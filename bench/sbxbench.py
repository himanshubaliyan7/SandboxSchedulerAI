#!/usr/bin/env python3
"""sbxbench: phase-0 benchmark harness for the warm-pool sandbox scheduler.

Standard library only. One entry point:

    python3 bench/sbxbench.py            # everything, default sample counts
    python3 bench/sbxbench.py --quick    # smoke test (minutes, numbers meaningless)
    python3 bench/sbxbench.py --only lifecycle,api

Results go to bench/results/<timestamp>/ (host.json, results.json,
summary.csv, summary.md). See bench/README.md.

Safety rules the harness enforces on itself:
  * every container it creates is named sbxbench-*; it only ever lists and
    removes by that prefix, in the default store and in its own isolated
    stores;
  * it never resets, prunes or reconfigures the user's Podman; other
    storage drivers are exercised through isolated --root/--runroot stores
    under bench/.work and $XDG_RUNTIME_DIR, removed at exit;
  * the density ramp stops at a memory floor and before per-user kernel
    quotas (session keyrings, inotify instances) are exhausted.
"""
import argparse
import atexit
import concurrent.futures as cf
import csv
import http.client
import json
import math
import os
import platform
import re
import shutil
import signal
import socket
import statistics
import subprocess
import sys
import threading
import time

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)
PREFIX = "sbxbench-"
ALPINE = "docker.io/library/alpine:3.20"
PYIMG = "docker.io/library/python:3.12-alpine"
SECCOMP_SRC = os.path.join(REPO, "internal/runtime/podman/seccomp/sbx-seccomp.json")
UID, GID = os.getuid(), os.getgid()
XDG = os.environ.get("XDG_RUNTIME_DIR", f"/run/user/{UID}")
TAG = f"{os.getpid():x}"
WORK = os.path.join(HERE, ".work", TAG)          # ext4, same fs as the real graph root here
RUN = os.path.join(XDG, f"sbxb-{TAG}")           # tmpfs, short paths (Unix socket limit is 108 bytes)
AGENT_ARGV = ["/usr/local/bin/sbx-agent", "serve", "--socket", "/run/sbx/agent.sock"]
IDLE = ["sleep", "86400"]

# Resource shape of one sandbox. Mirrors internal/runtime/podman/spec.go
# with the resources used by the project's integration tests, at 1 CPU.
RES = dict(cpus=1.0, memory=256 << 20, pids=128, workdir=64 << 20, tmp=32 << 20, nofile=1024, shm=16 << 20)
CUID, CGID = 1000, 1000  # podman.DefaultContainerUID/GID

LOG_LOCK = threading.Lock()


def log(*a):
    with LOG_LOCK:
        print(time.strftime("%H:%M:%S"), *a, flush=True)


def loadavg():
    with open("/proc/loadavg") as f:
        return [float(x) for x in f.read().split()[:3]]


def sh(cmd, timeout=180, env=None, stdin=None):
    """Run cmd; return (ms, rc, stdout, stderr). Wall time includes the
    harness's own fork/exec (see the 'harness_floor' baseline)."""
    t = time.perf_counter()
    try:
        p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout, env=env,
                           input=stdin, stdin=None if stdin is not None else subprocess.DEVNULL)
        rc, out, err = p.returncode, p.stdout.decode(errors="replace"), p.stderr.decode(errors="replace")
    except subprocess.TimeoutExpired as e:
        rc, out, err = 124, "", f"timeout after {timeout}s: {e}"
    return (time.perf_counter() - t) * 1000.0, rc, out, err


def pct(xs, p):
    if not xs:
        return float("nan")
    s = sorted(xs)
    k = (len(s) - 1) * p / 100.0
    lo, hi = math.floor(k), math.ceil(k)
    return s[lo] + (s[hi] - s[lo]) * (k - lo)


def stats(xs):
    if not xs:
        return dict(n=0)
    return dict(n=len(xs), mean=statistics.fmean(xs), stddev=statistics.stdev(xs) if len(xs) > 1 else 0.0,
                min=min(xs), p50=pct(xs, 50), p95=pct(xs, 95), p99=pct(xs, 99), max=max(xs))


class Rec:
    """Collects samples as rows keyed by (bench, cell, op, round)."""

    def __init__(self):
        self.rows = {}
        self.extra = {}
        self.lock = threading.Lock()

    def row(self, bench, cell, op, rnd):
        key = (bench, cell, op, rnd)
        with self.lock:
            if key not in self.rows:
                self.rows[key] = dict(bench=bench, cell=cell, op=op, round=rnd, unit="ms", samples=[], errors=[],
                                      load_start=loadavg(), load_end=None)
            return self.rows[key]

    def add(self, bench, cell, op, rnd, value):
        r = self.row(bench, cell, op, rnd)
        with self.lock:
            r["samples"].append(round(value, 3))
            r["load_end"] = loadavg()

    def err(self, bench, cell, op, rnd, msg):
        r = self.row(bench, cell, op, rnd)
        with self.lock:
            r["errors"].append(msg.strip()[-600:])
            r["load_end"] = loadavg()

    def merged(self):
        """One entry per (bench, cell, op): all rounds pooled plus per-round medians."""
        out = {}
        for (bench, cell, op, rnd), r in self.rows.items():
            m = out.setdefault((bench, cell, op), dict(bench=bench, cell=cell, op=op, samples=[], errors=[],
                                                       round_p50=[], round_mean=[], load1=[]))
            m["samples"] += r["samples"]
            m["errors"] += r["errors"]
            if r["samples"]:
                m["round_p50"].append(round(pct(r["samples"], 50), 2))
                m["round_mean"].append(round(statistics.fmean(r["samples"]), 2))
            m["load1"] += [r["load_start"][0]] + ([r["load_end"][0]] if r["load_end"] else [])
        for m in out.values():
            m["stats"] = stats(m["samples"])
        return list(out.values())


REC = Rec()

# --------------------------------------------------------------------------
# Stores


class Store:
    """A Podman store: the user's default one, or an isolated one of ours."""

    def __init__(self, name, args=(), root=None, runroot=None):
        self.name, self.args, self.root, self.runroot = name, list(args), root, runroot

    def p(self, *a):
        return ["podman"] + self.args + list(a)


MAIN = Store("default")
STORES = []          # isolated stores, for cleanup
_SERVICE = []        # podman system service processes
IMAGE_TARS = {}


def image_tar(image):
    if image not in IMAGE_TARS:
        path = os.path.join(WORK, re.sub(r"[^a-z0-9]+", "_", image) + ".tar")
        ms, rc, out, err = sh(["podman", "save", "-o", path, image], timeout=600)
        if rc != 0:
            raise RuntimeError(f"podman save {image}: {err}")
        IMAGE_TARS[image] = path
    return IMAGE_TARS[image]


_store_seq = [0]


def make_store(driver="native", transient=False, images=(ALPINE,)):
    """Create an isolated store under bench/.work and load images into it.
    driver: 'native' (kernel overlayfs) or 'fuse' (fuse-overlayfs)."""
    _store_seq[0] += 1
    name = f"iso{_store_seq[0]}-{driver}{'-transient' if transient else ''}"
    root = os.path.join(WORK, f"s{_store_seq[0]}")
    runroot = os.path.join(RUN, f"r{_store_seq[0]}")
    os.makedirs(root)
    os.makedirs(runroot)
    args = ["--root", root, "--runroot", runroot, "--storage-driver", "overlay"]
    if driver == "fuse":
        args += ["--storage-opt", "overlay.mount_program=/usr/bin/fuse-overlayfs"]
    if transient:
        args += ["--transient-store"]
    st = Store(name, args, root, runroot)
    STORES.append(st)
    for img in images:
        ms, rc, out, err = sh(st.p("load", "-q", "-i", image_tar(img)), timeout=600)
        if rc != 0:
            raise RuntimeError(f"load {img} into {name}: {err}")
    return st


def store_info(st):
    ms, rc, out, err = sh(st.p("info", "--format", "json"))
    try:
        s = json.loads(out)["store"]
        return dict(graphDriverName=s.get("graphDriverName"), graphStatus=s.get("graphStatus"),
                    transientStore=s.get("transientStore"), graphRoot=s.get("graphRoot"), runRoot=s.get("runRoot"),
                    graphOptions=s.get("graphOptions"))
    except Exception as e:  # noqa
        return dict(error=f"{e}: {err[-300:]}")


def our_containers(st):
    ms, rc, out, err = sh(st.p("ps", "-a", "--filter", f"name=^{PREFIX}", "--format", "{{.Names}}"))
    return [n for n in out.split() if n.startswith(PREFIX)]


def remove_ours(st):
    names = our_containers(st)
    for i in range(0, len(names), 40):
        sh(st.p("rm", "-f", "-t", "0", *names[i:i + 40]), timeout=600)
    return len(names)


def drop_store(st):
    remove_ours(st)
    sh(["podman", "unshare", "rm", "-rf", st.root, st.runroot], timeout=600)
    if st in STORES:
        STORES.remove(st)


_cleaned = [False]


def cleanup():
    if _cleaned[0]:
        return
    _cleaned[0] = True
    log("cleanup: removing sbxbench- containers, service, temp stores")
    for p in _SERVICE:
        try:
            p.terminate()
            p.wait(10)
        except Exception:  # noqa
            pass
    remove_ours(MAIN)
    for st in list(STORES):
        drop_store(st)
    for d in (WORK, RUN):
        if os.path.isdir(d):
            sh(["podman", "unshare", "rm", "-rf", d], timeout=600)
    try:
        os.rmdir(os.path.join(HERE, ".work"))
    except OSError:
        pass
    left = our_containers(MAIN)
    log("cleanup: remaining sbxbench- containers in default store:", left or "none")


def _sig(signum, frame):
    log(f"signal {signum}: cleaning up")
    cleanup()
    os._exit(130)


# --------------------------------------------------------------------------
# Specs (CLI flags and the REST create body)


def seccomp_path():
    """Copy of the product seccomp profile under RUN (short stable path)."""
    dst = os.path.join(RUN, "sbx-seccomp.json")
    if not os.path.exists(dst):
        shutil.copyfile(SECCOMP_SRC, dst)
    return dst


def plain_flags(runtime="crun", network="none"):
    """Podman defaults; only the two matrix dimensions are set."""
    return ["--runtime", runtime, "--network", network]


def hardened_flags(runtime="crun", network="none", userns=f"keep-id:uid={CUID},gid={CGID}", seccomp="custom",
                   rundir=None, agent=None, agent_cmd=False, res=RES):
    """CLI equivalent of podman.BuildCreateSpec (internal/runtime/podman/spec.go)."""
    f = ["--runtime", runtime, "--network", network,
         "--cap-drop", "all", "--security-opt", "no-new-privileges",
         "--read-only", "--read-only-tmpfs=true",
         "--user", f"{CUID}:{CGID}",
         "--cpus", str(res["cpus"]), "--memory", str(res["memory"]), "--memory-swap", str(res["memory"]),
         "--pids-limit", str(res["pids"]),
         "--cgroup-conf", f"memory.high={res['memory'] // 100 * 90 // 4096 * 4096}",
         "--cgroup-conf", "memory.oom.group=1",
         "--ulimit", f"nofile={res['nofile']}:{res['nofile']}",
         "--log-driver", "none", "--shm-size", str(res["shm"]), "--hostname", "sandbox",
         "--stop-timeout", "0", "--restart", "no", "--image-volume", "ignore",
         "--workdir", "/workspace", "-e", "HOME=/workspace", "-e", "TMPDIR=/tmp",
         "--mount", f"type=tmpfs,destination=/workspace,tmpfs-size={res['workdir']},tmpfs-mode=0755,U=true",
         "--mount", f"type=tmpfs,destination=/tmp,tmpfs-size={res['tmp']},tmpfs-mode=1777",
         "--mount", "type=tmpfs,destination=/var/tmp,tmpfs-size=1048576,tmpfs-mode=1777,noexec",
         "--mount", "type=tmpfs,destination=/run,tmpfs-size=1048576,tmpfs-mode=0755,noexec"]
    if userns:
        f += ["--userns", userns]
    if seccomp == "custom":
        f += ["--security-opt", f"seccomp={seccomp_path()}"]
    if rundir:
        f += ["--mount", f"type=bind,source={rundir},destination=/run/sbx,rw,nosuid,nodev,noexec"]
    if agent:
        f += ["--mount", f"type=bind,source={agent},destination=/usr/local/bin/sbx-agent,ro,nosuid,nodev"]
    if agent_cmd:
        f += ["--entrypoint", json.dumps(AGENT_ARGV)]
    return f


def api_spec(name, image, rundir, agent, runtime="crun", command=None, res=RES):
    """The Libpod SpecGenerator body the product sends (spec.go createSpec),
    keep-id mapping, product seccomp profile."""
    command = command or AGENT_ARGV

    def tmpfs(dest, size, mode, *extra):
        return dict(destination=dest, type="tmpfs", source="tmpfs",
                    options=["rw", "nosuid", "nodev", f"size={size}", f"mode={mode}", *extra])
    mounts = [tmpfs("/workspace", res["workdir"], "0755", "exec", "tmpcopyup", "U"),
              tmpfs("/tmp", res["tmp"], "1777", "exec", "notmpcopyup"),
              tmpfs("/var/tmp", 1 << 20, "1777", "noexec", "notmpcopyup"),
              tmpfs("/run", 1 << 20, "0755", "noexec", "notmpcopyup"),
              dict(destination="/run/sbx", type="bind", source=rundir,
                   options=["rbind", "rw", "nosuid", "nodev", "noexec", "rprivate"])]
    if agent:
        mounts.append(dict(destination="/usr/local/bin/sbx-agent", type="bind", source=agent,
                           options=["rbind", "ro", "nosuid", "nodev", "exec", "rprivate"]))
    ns = lambda m: dict(nsmode=m)  # noqa: E731
    return dict(
        name=name, image=image, entrypoint=command[:1], command=command[1:],
        env={"HOME": "/workspace", "TMPDIR": "/tmp"}, work_dir="/workspace", hostname="sandbox",
        labels={"sbxbench": "true"},
        terminal=False, stdin=False, remove=False, env_host=False, httpproxy=False, init=False,
        stop_timeout=0, restart_policy="no", log_configuration=dict(driver="none"), oci_runtime=runtime,
        image_volume_mode="ignore", mounts=mounts, shm_size=res["shm"], user=f"{CUID}:{CGID}",
        privileged=False, cap_add=[], cap_drop=["ALL"], no_new_privileges=True,
        seccomp_profile_path=seccomp_path(),
        userns=dict(nsmode="keep-id", value=f"uid={CUID},gid={CGID}"),
        idmappings=dict(HostUIDMapping=False, HostGIDMapping=False),
        read_only_filesystem=True, read_write_tmpfs=True,
        pidns=ns("private"), ipcns=ns("private"), utsns=ns("private"), cgroupns=ns("private"), netns=ns("none"),
        cgroups_mode="enabled",
        resource_limits=dict(
            cpu=dict(quota=int(res["cpus"] * 100000), period=100000),
            memory=dict(limit=res["memory"], swap=res["memory"]),
            pids=dict(limit=res["pids"]),
            unified={"memory.high": str(res["memory"] // 100 * 90 // 4096 * 4096), "memory.oom.group": "1"}),
        r_limits=[dict(type="nofile", hard=res["nofile"], soft=res["nofile"])],
    )


# --------------------------------------------------------------------------
# HTTP over Unix sockets (Podman REST API and the in-sandbox agent)


class UConn(http.client.HTTPConnection):
    def __init__(self, path, timeout=120):
        super().__init__("localhost", timeout=timeout)
        self.upath = path

    def connect(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.settimeout(self.timeout)
        s.connect(self.upath)
        self.sock = s


class Api:
    """Keep-alive client for one Unix socket, one per thread."""

    def __init__(self, path, prefix=""):
        self.path, self.prefix, self.conn = path, prefix, None

    def call(self, method, url, body=None, fresh=False, timeout=120):
        data = json.dumps(body).encode() if body is not None else None
        hdr = {"Content-Type": "application/json"} if data is not None else {}
        for attempt in (0, 1):
            conn = UConn(self.path, timeout) if fresh else (self.conn or UConn(self.path, timeout))
            try:
                conn.request(method, self.prefix + url, body=data, headers=hdr)
                r = conn.getresponse()
                payload = r.read()
                if fresh or r.will_close:
                    conn.close()
                    if not fresh:
                        self.conn = None
                else:
                    self.conn = conn
                return r.status, payload
            except (http.client.HTTPException, OSError) as e:
                conn.close()
                if not fresh:
                    self.conn = None
                if attempt == 1 or fresh:
                    return 599, str(e).encode()
        return 599, b""

    def close(self):
        if self.conn:
            self.conn.close()
            self.conn = None


_tls = threading.local()
SERVICE_SOCK = [None]
API_PREFIX = [None]


def start_service():
    """Our own `podman system service --time 0` on the default store, as the
    research notes recommend for the product (no idle exit, no socket
    activation on the request path). The user's podman.socket is untouched."""
    if SERVICE_SOCK[0]:
        return
    sock = os.path.join(RUN, "podman.sock")
    p = subprocess.Popen(["podman", "system", "service", "--time", "0", f"unix://{sock}"],
                         stdout=subprocess.DEVNULL, stderr=open(os.path.join(RUN, "service.log"), "wb"))
    _SERVICE.append(p)
    for _ in range(200):
        if os.path.exists(sock):
            break
        time.sleep(0.05)
    ms, rc, out, err = sh(["podman", "version", "--format", "{{.Client.APIVersion}}"])
    API_PREFIX[0] = f"/v{out.strip()}/libpod"
    SERVICE_SOCK[0] = sock
    st, body = papi().call("GET", "/_ping")
    if st != 200:
        raise RuntimeError(f"podman service not answering: {st} {body!r}")


def papi():
    if not getattr(_tls, "api", None):
        _tls.api = Api(SERVICE_SOCK[0], API_PREFIX[0])
    return _tls.api


def api_create(spec):
    st, body = papi().call("POST", "/containers/create", spec)
    if st != 201:
        raise RuntimeError(f"create {st}: {body[-400:].decode(errors='replace')}")
    return json.loads(body)["Id"]


def api_start(name):
    st, body = papi().call("POST", f"/containers/{name}/start")
    if st not in (204, 304):
        raise RuntimeError(f"start {st}: {body[-400:].decode(errors='replace')}")


def api_rm(name):
    st, body = papi().call("DELETE", f"/containers/{name}?force=true&timeout=0&v=true")
    if st not in (200, 204, 404):
        raise RuntimeError(f"rm {st}: {body[-400:].decode(errors='replace')}")


def api_exec(name, argv):
    """exec create + attached start, read the hijacked stream to EOF, then
    inspect for the exit code (three round trips, as a client must)."""
    st, body = papi().call("POST", f"/containers/{name}/exec",
                           dict(Cmd=argv, AttachStdout=True, AttachStderr=True))
    if st != 201:
        raise RuntimeError(f"exec create {st}: {body[-300:].decode(errors='replace')}")
    eid = json.loads(body)["Id"]
    st, out = papi().call("POST", f"/exec/{eid}/start", dict(Detach=False, Tty=False), fresh=True)
    if st != 200:
        raise RuntimeError(f"exec start {st}: {out[-300:].decode(errors='replace')}")
    st, body = papi().call("GET", f"/exec/{eid}/json")
    code = json.loads(body).get("ExitCode") if st == 200 else None
    if code != 0:
        raise RuntimeError(f"exec exit code {code}")
    return eid


def api_inspect(name):
    st, body = papi().call("GET", f"/containers/{name}/json")
    if st != 200:
        raise RuntimeError(f"inspect {st}: {body[-300:].decode(errors='replace')}")
    return json.loads(body)


def agent_wait_ready(rundir, timeout=30.0):
    """Poll like agentclient.WaitReady but tighter (1 ms): socket exists and
    GET /v1/health says ready."""
    sock = os.path.join(rundir, "agent.sock")
    end = time.perf_counter() + timeout
    last = "no socket"
    while time.perf_counter() < end:
        if os.path.exists(sock):
            st, body = Api(sock).call("GET", "/v1/health", fresh=True, timeout=5)
            if st == 200 and json.loads(body).get("ready"):
                return
            last = f"{st} {body[-200:]!r}"
        time.sleep(0.001)
    raise RuntimeError(f"agent not ready in {timeout}s: {last}")


def agent_init(rundir, sid):
    st, body = Api(os.path.join(rundir, "agent.sock")).call(
        "POST", "/v1/init", dict(sandbox_id=sid, generation=1, egress_proxy=False), fresh=True)
    if st // 100 != 2:
        raise RuntimeError(f"agent init {st}: {body[-200:]!r}")


def agent_exec_true(rundir):
    """One command through the in-sandbox agent: POST /v1/processes then read
    the NDJSON stream to the exit event. New connection each time (a claim's
    first RPC)."""
    a = Api(os.path.join(rundir, "agent.sock"))
    st, body = a.call("POST", "/v1/processes", dict(cmd="true"))
    if st // 100 != 2:
        raise RuntimeError(f"agent start {st}: {body[-200:]!r}")
    pid = json.loads(body)["id"]
    st, body = a.call("GET", f"/v1/processes/{pid}/stream?from=0")
    a.close()
    for line in body.splitlines():
        ev = json.loads(line)
        if ev.get("type") == "exit":
            if ev["exit"].get("exit_code") != 0:
                raise RuntimeError(f"agent exec exit {ev['exit']}")
            return
    raise RuntimeError(f"agent stream had no exit event: {st} {body[-200:]!r}")


AGENT_BIN = [None]


def build_agent():
    """Static agent binary, built into our temp dir (nothing in the repo changes)."""
    if AGENT_BIN[0] is not None:
        return AGENT_BIN[0]
    dst = os.path.join(RUN, "sbx-agent")
    env = dict(os.environ, CGO_ENABLED="0")
    env["PATH"] = os.path.expanduser("~/.local/opt/go/bin") + ":" + env.get("PATH", "")
    ms, rc, out, err = sh(["go", "build", "-o", dst, "./cmd/sbx-agent"], timeout=600, env=dict(env, PWD=REPO))
    if rc != 0:
        # go build needs cwd = module root
        p = subprocess.run(["go", "build", "-o", dst, "./cmd/sbx-agent"], cwd=REPO, env=env,
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        rc, err = p.returncode, p.stderr.decode(errors="replace")
    AGENT_BIN[0] = dst if rc == 0 and os.path.exists(dst) else ""
    if not AGENT_BIN[0]:
        log("agent build FAILED, falling back to `sleep` as the idle process:", err[-400:])
    REC.extra["agent_binary"] = dict(built=bool(AGENT_BIN[0]), error=None if AGENT_BIN[0] else err[-800:])
    return AGENT_BIN[0]


_seq = [0]
_seq_lock = threading.Lock()


def new_name(kind):
    with _seq_lock:
        _seq[0] += 1
        return f"{PREFIX}{kind}-{TAG}-{_seq[0]}"


def new_rundir(name):
    d = os.path.join(RUN, "rd", name[len(PREFIX):])
    os.makedirs(d, exist_ok=True)
    return d


# --------------------------------------------------------------------------
# 0. harness floor


def bench_floor(a):
    """What the timing method itself costs: spawning /bin/true and
    `podman --version` (CLI start-up with no store access)."""
    for rnd in range(a.rounds):
        for i in range(-a.warmup, a.samples):
            for op, cmd in (("spawn_bin_true", ["/bin/true"]), ("podman_version", ["podman", "--version"]),
                            ("podman_ps_noop", ["podman", "ps", "-q", "--filter", "name=^sbxbench-none$"])):
                ms, rc, out, err = sh(cmd)
                if i >= 0:
                    REC.add("floor", "harness", op, rnd, ms)


# --------------------------------------------------------------------------
# 1. lifecycle latency matrix (CLI)


def lifecycle_cell(bench, cell, st, flags, image, rnd, n, warm):
    P = st.p
    fails = 0
    for i in range(-warm, n):
        name = new_name("lc")
        steps = [("create", P("create", "--name", name, *flags, image, *IDLE)),
                 ("start", P("start", name)),
                 ("exec_first", P("exec", name, "true")),
                 ("exec_again", P("exec", name, "true")),
                 ("rm_f_running", P("rm", "-f", "-t", "0", name)),
                 ("run_rm_true", P("run", "--rm", "--name", name + "r", *flags, image, "true"))]
        ok = True
        tot = 0.0
        for op, cmd in steps:
            if not ok and op not in ("rm_f_running", "run_rm_true"):
                continue
            ms, rc, out, err = sh(cmd)
            if rc != 0:
                if op == "rm_f_running" and not ok:
                    continue
                ok = False if op != "run_rm_true" else ok
                REC.err(bench, cell, op, rnd, f"rc={rc}: {err or out}")
                if op == "create":
                    fails += 1
                continue
            if i >= 0:
                REC.add(bench, cell, op, rnd, ms)
            if op in ("create", "start"):
                tot += ms
        if ok and i >= 0:
            REC.add(bench, cell, "create_plus_start", rnd, tot)
        if fails >= 3 and i < 0 or fails >= 5:
            REC.err(bench, cell, "create", rnd, "cell aborted after repeated create failures")
            sh(P("rm", "-f", "-t", "0", name))
            return False
    return True


def bench_lifecycle(a):
    nets = ["none"] + [n for n in ("pasta", "slirp4netns") if shutil.which(n)]
    runtimes = [r for r in ("crun", "runc") if shutil.which(r)]
    rd = new_rundir("lc")
    agent = build_agent()
    cells = []
    for rt in runtimes:
        for net in nets:
            cells.append((f"{rt}/{net}/plain", plain_flags(rt, net)))
            cells.append((f"{rt}/{net}/hardened", hardened_flags(rt, net, rundir=rd, agent=agent or None)))
    REC.extra["lifecycle_cells"] = [c for c, _ in cells]
    dead = set()
    for rnd in range(a.rounds):
        for cell, flags in cells:
            if cell in dead:
                continue
            log(f"lifecycle round {rnd + 1}/{a.rounds} {cell} load={loadavg()[0]}")
            if not lifecycle_cell("lifecycle", cell, MAIN, flags, ALPINE, rnd, a.samples, a.warmup):
                dead.add(cell)


def bench_storage(a):
    """Storage driver x transient store, through isolated stores. The
    'native/persistent' isolated cell is the control for the default store."""
    rd = new_rundir("st")
    agent = build_agent()
    variants = [("native", False), ("native", True)]
    if shutil.which("fuse-overlayfs"):
        variants += [("fuse", False), ("fuse", True)]
    stores = []
    for drv, tr in variants:
        try:
            st = make_store(drv, tr)
            REC.extra.setdefault("isolated_stores", {})[st.name] = store_info(st)
            stores.append((drv, tr, st))
        except Exception as e:  # noqa
            REC.err("storage", f"{drv}/{'transient' if tr else 'persistent'}", "setup", 0, str(e))
    dead = set()
    for rnd in range(a.rounds):
        for drv, tr, st in stores:
            for spec, flags in (("plain", plain_flags()), ("hardened", hardened_flags(rundir=rd, agent=agent or None))):
                cell = f"{drv}/{'transient' if tr else 'persistent'}/crun/none/{spec}"
                if cell in dead:
                    continue
                log(f"storage round {rnd + 1}/{a.rounds} {cell} load={loadavg()[0]}")
                if not lifecycle_cell("storage", cell, st, flags, ALPINE, rnd, a.samples, a.warmup):
                    dead.add(cell)
    for _, _, st in stores:
        drop_store(st)


# --------------------------------------------------------------------------
# 1b. REST API versus CLI on the product's real spec (hardened, crun, none, agent)


def api_cycle(image, agent, record, has_agent=True):
    """create -> start -> agent ready -> init -> agent exec -> podman exec -> rm, over the socket."""
    name = new_name("api")
    rd = new_rundir(name)
    spec = api_spec(name, image, rd, agent or None, command=None if has_agent else IDLE)
    t0 = time.perf_counter()
    api_create(spec)
    t1 = time.perf_counter()
    record("create", (t1 - t0) * 1e3)
    try:
        api_start(name)
        t2 = time.perf_counter()
        record("start", (t2 - t1) * 1e3)
        record("create_plus_start", (t2 - t0) * 1e3)
        if has_agent:
            agent_wait_ready(rd)
            t3 = time.perf_counter()
            record("start_return_to_agent_ready", (t3 - t2) * 1e3)
            record("create_to_agent_ready", (t3 - t0) * 1e3)
            t = time.perf_counter()
            agent_init(rd, name)
            record("agent_init_rpc", (time.perf_counter() - t) * 1e3)
            for op in ("agent_exec_true_first", "agent_exec_true_again"):
                t = time.perf_counter()
                agent_exec_true(rd)
                record(op, (time.perf_counter() - t) * 1e3)
        for op in ("podman_exec_true_first", "podman_exec_true_again"):
            t = time.perf_counter()
            api_exec(name, ["true"])
            record(op, (time.perf_counter() - t) * 1e3)
    finally:
        t = time.perf_counter()
        api_rm(name)
        record("rm_f_running", (time.perf_counter() - t) * 1e3)
        shutil.rmtree(rd, ignore_errors=True)


def cli_cycle(image, agent, record, has_agent=True):
    name = new_name("cli")
    rd = new_rundir(name)
    flags = hardened_flags(rundir=rd, agent=agent or None, agent_cmd=has_agent)
    cmd = ["podman", "create", "--name", name, *flags, image] + ([] if has_agent else IDLE)
    t0 = time.perf_counter()
    ms, rc, out, err = sh(cmd)
    if rc != 0:
        raise RuntimeError(f"cli create: {err}")
    record("create", ms)
    try:
        ms2, rc, out, err = sh(["podman", "start", name])
        if rc != 0:
            raise RuntimeError(f"cli start: {err}")
        t2 = time.perf_counter()
        record("start", ms2)
        record("create_plus_start", (t2 - t0) * 1e3)
        if has_agent:
            agent_wait_ready(rd)
            t3 = time.perf_counter()
            record("start_return_to_agent_ready", (t3 - t2) * 1e3)
            record("create_to_agent_ready", (t3 - t0) * 1e3)
        for op in ("podman_exec_true_first", "podman_exec_true_again"):
            ms, rc, out, err = sh(["podman", "exec", name, "true"])
            if rc != 0:
                raise RuntimeError(f"cli exec: {err}")
            record(op, ms)
    finally:
        ms, rc, out, err = sh(["podman", "rm", "-f", "-t", "0", name])
        record("rm_f_running", ms)
        shutil.rmtree(rd, ignore_errors=True)


def bench_api(a):
    start_service()
    agent = build_agent()
    for rnd in range(a.rounds):
        log(f"api-vs-cli round {rnd + 1}/{a.rounds} load={loadavg()[0]}")
        for i in range(-a.warmup, a.samples):
            for transport, fn in (("rest-api", api_cycle), ("cli", cli_cycle)):
                cell = f"{transport}/crun/none/hardened+agent" if agent else f"{transport}/crun/none/hardened+sleep"

                def record(op, ms, cell=cell, i=i):
                    if i >= 0:
                        REC.add("api_vs_cli", cell, op, rnd, ms)
                try:
                    fn(ALPINE, agent, record, has_agent=bool(agent))
                except Exception as e:  # noqa
                    REC.err("api_vs_cli", cell, "cycle", rnd, str(e))


# --------------------------------------------------------------------------
# 2. user-namespace mapping cost


USERNS = [("host(default)", None), ("keep-id", "keep-id"), (f"keep-id:uid={CUID},gid={CGID}", f"keep-id:uid={CUID},gid={CGID}"),
          ("nomap", "nomap"), ("auto", "auto")]


def bench_userns(a):
    """First use of an image under a mapping versus later uses. Each
    repetition gets a brand new isolated store, so 'first' really is the
    first container of that image with that mapping."""
    drivers = ["native"] + (["fuse"] if shutil.which("fuse-overlayfs") else [])
    reps = a.userns_reps
    n_sub = a.userns_subsequent
    for rep in range(reps):
        for drv in drivers:
            for image, short in ((PYIMG, "python:3.12-alpine"), (ALPINE, "alpine:3.20")):
                for label, mode in USERNS:
                    cell = f"{drv}/{short}/{label}"
                    log(f"userns rep {rep + 1}/{reps} {cell} load={loadavg()[0]}")
                    try:
                        st = make_store(drv, False, images=(image,))
                    except Exception as e:  # noqa
                        REC.err("userns", cell, "setup", rep, str(e))
                        continue
                    flags = ["--network", "none", "--log-driver", "none", "--stop-timeout", "0"]
                    if mode:
                        flags += ["--userns", mode]
                    for i in range(1 + n_sub):
                        name = new_name("un")
                        phase = "first" if i == 0 else "subsequent"
                        ms_c, rc, out, err = sh(st.p("create", "--name", name, *flags, image, *IDLE), timeout=600)
                        if rc != 0:
                            REC.err("userns", cell, f"{phase}_create", rep, err)
                            break
                        ms_s, rc, out, err = sh(st.p("start", name))
                        if rc != 0:
                            REC.err("userns", cell, f"{phase}_start", rep, err)
                        else:
                            REC.add("userns", cell, f"{phase}_create", rep, ms_c)
                            REC.add("userns", cell, f"{phase}_start", rep, ms_s)
                            REC.add("userns", cell, f"{phase}_create_plus_start", rep, ms_c + ms_s)
                        sh(st.p("rm", "-f", "-t", "0", name))
                    if mode == "auto":
                        # auto with other auto containers alive: each new container
                        # needs a range nobody holds, so the mapping changes every time.
                        held = []
                        for i in range(a.userns_auto_concurrent):
                            name = new_name("ua")
                            ms_c, rc, out, err = sh(st.p("create", "--name", name, *flags, image, *IDLE), timeout=600)
                            if rc != 0:
                                REC.err("userns", cell, "auto_while_others_alive_create", rep, err)
                                break
                            held.append(name)
                            REC.add("userns", cell, "auto_while_others_alive_create", rep, ms_c)
                        ms, rc, out, err = sh(st.p("ps", "-a", "--format", "{{.Names}} {{.IDMappings}}"))
                        ms, rc, out, err = sh(st.p("inspect", "--format", "{{.Name}} {{json .HostConfig.IDMappings}}", *held)) if held else (0, 0, "", "")
                        REC.extra.setdefault("userns_auto_mappings", {})[f"{cell}#rep{rep}"] = out.strip().splitlines()[:12]
                    ms, rc, out, err = sh(["podman", "unshare", "du", "-sm", os.path.join(st.root, "overlay")])
                    REC.extra.setdefault("userns_store_mb_after", {})[f"{cell}#rep{rep}"] = out.split()[0] if out.split() else err[-200:]
                    drop_store(st)
    # how many --userns=auto containers fit in the subordinate ID budget?
    try:
        st = make_store("fuse" if "fuse" in drivers else "native", False, images=(ALPINE,))
        n, first_err = 0, None
        for i in range(a.userns_auto_max):
            name = new_name("ux")
            ms_c, rc, out, err = sh(st.p("create", "--name", name, "--network", "none", "--userns", "auto", ALPINE, *IDLE), timeout=600)
            if rc != 0:
                first_err = err.strip()[-500:]
                break
            n += 1
            REC.add("userns", "auto-exhaustion/alpine:3.20", "create_nth", 0, ms_c)
        ms, rc, sub, err = sh(["sh", "-c", f"grep -E '^({UID}|'\"$(id -un)\"'):' /etc/subuid /etc/subgid"])
        REC.extra["userns_auto_exhaustion"] = dict(store=st.name, created_before_error=n, first_error=first_err,
                                                   ceiling_tried=a.userns_auto_max, subid=sub.strip().splitlines())
        drop_store(st)
    except Exception as e:  # noqa
        REC.extra["userns_auto_exhaustion"] = dict(error=str(e))


# --------------------------------------------------------------------------
# 3. warm state: pause / unpause versus exec into an idle running container


def scope_dir(cgroup_path):
    return "/sys/fs/cgroup" + cgroup_path


def bench_pause(a):
    start_service()
    agent = build_agent()
    name = new_name("pz")
    rd = new_rundir(name)
    try:
        api_create(api_spec(name, ALPINE, rd, agent or None, command=None if agent else IDLE))
        api_start(name)
        if agent:
            agent_wait_ready(rd)
            agent_init(rd, name)
    except Exception as e:  # noqa
        REC.err("pause", "setup", "setup", 0, str(e))
        return
    info = api_inspect(name)
    cg = scope_dir(info["State"]["CgroupPath"])
    cell = "crun/none/hardened"
    # does exec into a paused container work?
    sh(["podman", "pause", name])
    ms, rc, out, err = sh(["podman", "exec", name, "true"])
    REC.extra["pause_exec_into_paused"] = dict(rc=rc, stderr=err.strip()[-300:], ms=round(ms, 1))
    ms, rc, out, err = sh(["podman", "inspect", "--format", "{{.State.Status}}", name])
    REC.extra["pause_state_while_paused"] = out.strip()
    sh(["podman", "unpause", name])
    for rnd in range(a.rounds):
        log(f"pause round {rnd + 1}/{a.rounds} load={loadavg()[0]}")
        for i in range(-a.warmup, a.samples):
            def rec(op, ms, i=i):
                if i >= 0:
                    REC.add("pause", cell, op, rnd, ms)
            for op, cmd in (("cli_pause", ["podman", "pause", name]), ("cli_unpause", ["podman", "unpause", name]),
                            ("cli_exec_true_idle_running", ["podman", "exec", name, "true"])):
                ms, rc, out, err = sh(cmd)
                if rc != 0:
                    REC.err("pause", cell, op, rnd, err)
                else:
                    rec(op, ms)
            for op in ("pause", "unpause"):
                t = time.perf_counter()
                st, body = papi().call("POST", f"/containers/{name}/{op}")
                if st != 204:
                    REC.err("pause", cell, f"api_{op}", rnd, f"{st} {body[-200:]!r}")
                else:
                    rec(f"api_{op}", (time.perf_counter() - t) * 1e3)
            try:
                t = time.perf_counter()
                api_exec(name, ["true"])
                rec("api_exec_true_idle_running", (time.perf_counter() - t) * 1e3)
                if agent:
                    t = time.perf_counter()
                    agent_exec_true(rd)
                    rec("agent_exec_true_idle_running", (time.perf_counter() - t) * 1e3)
                    # the paused warm state's request path: unpause, then one agent RPC
                    papi().call("POST", f"/containers/{name}/pause")
                    t = time.perf_counter()
                    st, body = papi().call("POST", f"/containers/{name}/unpause")
                    agent_exec_true(rd)
                    rec("api_unpause_then_agent_exec_true", (time.perf_counter() - t) * 1e3)
            except Exception as e:  # noqa
                REC.err("pause", cell, "exec", rnd, str(e))
            # the kernel primitive without Podman: cgroup.freeze written by the rootless user
            try:
                fz = os.path.join(cg, "cgroup.freeze")
                for val, want, op in (("1", "frozen 1", "cgroup_freeze_write_until_frozen"),
                                      ("0", "frozen 0", "cgroup_thaw_write_until_thawed")):
                    t = time.perf_counter()
                    with open(fz, "w") as f:
                        f.write(val)
                    while want not in open(os.path.join(cg, "cgroup.events")).read():
                        if time.perf_counter() - t > 5:
                            raise RuntimeError("freeze state not reached in 5s")
                    rec(op, (time.perf_counter() - t) * 1e3)
            except Exception as e:  # noqa
                REC.err("pause", cell, "cgroup_freeze", rnd, str(e))
    # memory of a paused sandbox is not released: record it for the write-up
    try:
        cur = int(open(os.path.join(cg, "memory.current")).read())
        papi().call("POST", f"/containers/{name}/pause")
        time.sleep(1)
        cur2 = int(open(os.path.join(cg, "memory.current")).read())
        papi().call("POST", f"/containers/{name}/unpause")
        REC.extra["pause_memory_current_bytes"] = dict(running=cur, paused=cur2)
    except Exception as e:  # noqa
        REC.extra["pause_memory_current_bytes"] = dict(error=str(e))
    api_rm(name)


# --------------------------------------------------------------------------
# 4. density


def meminfo():
    out = {}
    with open("/proc/meminfo") as f:
        for line in f:
            k, v = line.split(":")
            out[k] = int(v.split()[0])  # kB
    return out


def key_usage(uid=UID):
    """(keys in use, quota) for a uid from /proc/key-users (qnkeys/maxkeys)."""
    try:
        with open("/proc/key-users") as f:
            for line in f:
                p = line.split()
                if p[0] == f"{uid}:":
                    q, m = p[3].split("/")
                    return int(q), int(m)
    except OSError:
        pass
    return None, None


def user_proc_totals():
    """Counts over every process of this uid: processes, threads, open fds, inotify instances."""
    procs = threads = fds = ino = 0
    for d in os.listdir("/proc"):
        if not d.isdigit():
            continue
        try:
            if os.stat(f"/proc/{d}").st_uid != UID:
                continue
            procs += 1
            with open(f"/proc/{d}/status") as f:
                for line in f:
                    if line.startswith("Threads:"):
                        threads += int(line.split()[1])
                        break
            for fd in os.listdir(f"/proc/{d}/fd"):
                fds += 1
                try:
                    if os.readlink(f"/proc/{d}/fd/{fd}") == "anon_inode:inotify":
                        ino += 1
                except OSError:
                    pass
        except OSError:
            continue
    return dict(processes=procs, threads=threads, fds=fds, inotify_instances=ino)


def read_int(path):
    try:
        with open(path) as f:
            return int(f.read().split()[0])
    except (OSError, ValueError, IndexError):
        return None


def smaps(pid):
    out = {}
    try:
        with open(f"/proc/{pid}/smaps_rollup") as f:
            for line in f:
                p = line.split()
                if p[0] in ("Rss:", "Pss:"):
                    out[p[0][:-1].lower() + "_kb"] = int(p[1])
        out["fds"] = len(os.listdir(f"/proc/{pid}/fd"))
    except OSError:
        pass
    return out


def mean_or_none(xs):
    xs = [x for x in xs if x is not None]
    return round(statistics.fmean(xs), 1) if xs else None


MEMKEYS = ("MemFree", "Cached", "AnonPages", "Shmem", "Slab", "SUnreclaim", "SReclaimable", "KernelStack", "PageTables", "Percpu")


def density_snapshot(boxes, base):
    mi = meminfo()
    tot = user_proc_totals()
    keys, keymax = key_usage()
    scope, leaf, conmon_cg, cpss, crss, cfds, apss, arss, pids = [], [], [], [], [], [], [], [], []
    for b in boxes:
        cg = b["cg"]
        scope.append(read_int(cg + "/memory.current"))
        leaf.append(read_int(cg + "/container/memory.current"))
        pids.append(read_int(cg + "/container/pids.current") or read_int(cg + "/pids.current"))
        conmon_cg.append(read_int(cg.replace("/libpod-", "/libpod-conmon-") + "/memory.current"))
        s = smaps(b["conmon_pid"])
        cpss.append(s.get("pss_kb")); crss.append(s.get("rss_kb")); cfds.append(s.get("fds"))  # noqa: E702
        s = smaps(b["pid"])
        apss.append(s.get("pss_kb")); arss.append(s.get("rss_kb"))  # noqa: E702
    n = len(boxes)
    snap = dict(
        n=n, load=loadavg(), mem_available_kb=mi["MemAvailable"], swap_free_kb=mi.get("SwapFree"),
        meminfo_kb={k: mi.get(k) for k in MEMKEYS},
        cgroup_scope_memory_current_mean_kb=None if not n else round((mean_or_none(scope) or 0) / 1024, 1),
        cgroup_leaf_memory_current_mean_kb=None if not n else round((mean_or_none(leaf) or 0) / 1024, 1),
        conmon_scope_memory_current_mean_kb=None if not n else round((mean_or_none(conmon_cg) or 0) / 1024, 1),
        conmon_pss_mean_kb=mean_or_none(cpss), conmon_rss_mean_kb=mean_or_none(crss), conmon_fds_mean=mean_or_none(cfds),
        sandbox_pid1_pss_mean_kb=mean_or_none(apss), sandbox_pid1_rss_mean_kb=mean_or_none(arss),
        cgroup_pids_current_mean=mean_or_none(pids),
        keys_used=keys, keys_max=keymax, **{"user_" + k: v for k, v in tot.items()})
    if _SERVICE:
        snap["podman_service_rss_kb"] = smaps(_SERVICE[0].pid).get("rss_kb")
    st, body = papi().call("GET", "/info")
    try:
        snap["podman_free_locks"] = json.loads(body)["host"]["freeLocks"]
    except Exception:  # noqa
        pass
    if base and n:
        snap["mem_available_delta_per_sandbox_kb"] = round((base["mem_available_kb"] - mi["MemAvailable"]) / n, 1)
        # where the host-level growth sits (positive = grew), kB per sandbox
        snap["meminfo_growth_per_sandbox_kb"] = {k: round((mi[k] - base["meminfo_kb"][k]) / n, 1)
                                                 for k in MEMKEYS if mi.get(k) is not None and base["meminfo_kb"].get(k) is not None}
        for k in ("processes", "threads", "fds", "inotify_instances"):
            snap[f"user_{k}_per_sandbox"] = round((tot[k] - base["user_" + k]) / n, 2)
    return snap


def make_box(agent, image=ALPINE, timings=None):
    """One hardened idle sandbox through the REST API; returns its record."""
    name = new_name("dn")
    rd = new_rundir(name)
    t0 = time.perf_counter()
    api_create(api_spec(name, image, rd, agent or None, command=None if agent else IDLE))
    t1 = time.perf_counter()
    try:
        api_start(name)
        t2 = time.perf_counter()
        if agent:
            agent_wait_ready(rd)
    except Exception:
        try:
            api_rm(name)
        finally:
            shutil.rmtree(rd, ignore_errors=True)
        raise
    t3 = time.perf_counter()
    if timings is not None:
        timings.append(dict(create=(t1 - t0) * 1e3, start=(t2 - t1) * 1e3, ready=(t3 - t2) * 1e3, total=(t3 - t0) * 1e3))
    return dict(name=name, rundir=rd)


def fill_box_info(b):
    info = api_inspect(b["name"])
    b["cg"] = scope_dir(info["State"]["CgroupPath"])
    b["conmon_pid"] = info["State"]["ConmonPid"]
    b["pid"] = info["State"]["Pid"]


def foreign_containers():
    ms, rc, out, err = sh(["podman", "ps", "-a", "--format", "{{.Names}}"])
    return [n for n in out.split() if not n.startswith(PREFIX)]


def bench_density(a):
    start_service()
    agent = build_agent()
    REC.extra["density"] = []
    for rnd in range(a.density_rounds):
        REC.extra["density"].append(density_round(a, agent, rnd, push=a.push_to_limit and rnd == a.density_rounds - 1))
        time.sleep(5)


def density_round(a, agent, rnd, push):
    idle_kind = "sbx-agent serve (real idle sandbox)" if agent else "sleep 86400"
    time.sleep(3)
    base = density_snapshot([], None)
    steps = [base]
    boxes, create_errors = [], []
    stop = None
    floor_kb = a.density_mem_floor_mb * 1024
    ino_max = read_int("/proc/sys/fs/inotify/max_user_instances")
    log(f"density round {rnd + 1}: idle process = {idle_kind}; baseline MemAvailable {base['mem_available_kb'] // 1024} MB, "
        f"keys {base['keys_used']}/{base['keys_max']}")
    while stop is None:
        timings = []
        streak = 0
        while len(timings) < a.density_step:
            if len(boxes) >= a.density_ceiling:
                stop = f"fixed ceiling of {a.density_ceiling} sandboxes reached"
                break
            keys, keymax = key_usage()
            if keys is not None and keymax - keys <= a.density_key_headroom:
                stop = (f"safety stop {keymax - keys} keys short of the per-user session-keyring quota "
                        f"(kernel.keys.maxkeys={keymax}, {keys} in use; each running container holds one key)")
                break
            if meminfo()["MemAvailable"] < floor_kb:
                stop = f"memory floor: MemAvailable below {a.density_mem_floor_mb} MB"
                break
            try:
                b = make_box(agent, timings=timings)
                fill_box_info(b)
                boxes.append(b)
                streak = 0
            except Exception as e:  # noqa
                create_errors.append(dict(at_n=len(boxes), error=str(e)[-400:]))
                streak += 1
                if streak >= 3:
                    stop = f"3 consecutive create/start errors at {len(boxes)} sandboxes: {e}"
                    break
        if not timings:
            break
        time.sleep(a.density_settle)
        snap = density_snapshot(boxes, base)
        for k in ("create", "start", "ready", "total"):
            snap[f"step_{k}_ms"] = stats([t[k] for t in timings])
            for t in timings:
                REC.add("density_ramp", f"n<={len(boxes):03d}", k if k != "total" else "create_to_ready", rnd, t[k])
        if stop is None and ino_max and ino_max - snap["user_inotify_instances"] <= a.density_inotify_headroom:
            stop = f"safety stop {ino_max - snap['user_inotify_instances']} short of fs.inotify.max_user_instances={ino_max}"
        steps.append(snap)
        log(f"density n={snap['n']} MemAvail={snap['mem_available_kb'] // 1024}MB "
            f"per-sbx={snap.get('mem_available_delta_per_sandbox_kb')}kB scope={snap['cgroup_scope_memory_current_mean_kb']}kB "
            f"conmonPss={snap['conmon_pss_mean_kb']}kB keys={snap['keys_used']}/{snap['keys_max']} "
            f"create-to-ready p50={f1(snap['step_total_ms'].get('p50'))}")
    peak = len(boxes)
    log("density stop:", stop)
    cell = f"n={peak}"
    # request-path latency with the host full
    if boxes:
        b = boxes[len(boxes) // 2]
        for i in range(-3, a.samples):
            try:
                if agent:
                    t = time.perf_counter()
                    agent_exec_true(b["rundir"])
                    if i >= 0:
                        REC.add("density_at_peak", cell, "agent_exec_true", rnd, (time.perf_counter() - t) * 1e3)
                t = time.perf_counter()
                api_exec(b["name"], ["true"])
                if i >= 0:
                    REC.add("density_at_peak", cell, "api_podman_exec_true", rnd, (time.perf_counter() - t) * 1e3)
                ms, rc, out, err = sh(["podman", "exec", b["name"], "true"])
                if i >= 0 and rc == 0:
                    REC.add("density_at_peak", cell, "cli_podman_exec_true", rnd, ms)
                ms, rc, out, err = sh(["podman", "ps", "-q"])
                if i >= 0 and rc == 0:
                    REC.add("density_at_peak", cell, "cli_podman_ps", rnd, ms)
                t = time.perf_counter()
                st, body = papi().call("GET", "/containers/json?all=true")
                if i >= 0 and st == 200:
                    REC.add("density_at_peak", cell, "api_list_containers", rnd, (time.perf_counter() - t) * 1e3)
            except Exception as e:  # noqa
                REC.err("density_at_peak", cell, "exec", rnd, str(e))
    limit = push_to_limit(a) if push and boxes else None
    # teardown rate at the peak
    t = time.perf_counter()
    errs = par_map(lambda b: api_rm(b["name"]), boxes, a.density_teardown_conc)
    dt = time.perf_counter() - t
    for b in boxes:
        shutil.rmtree(b["rundir"], ignore_errors=True)
    time.sleep(3)
    after = density_snapshot([], None)
    pm = f"/sys/fs/cgroup/user.slice/user-{UID}.slice/pids.max"
    return dict(
        round=rnd, idle_process=idle_kind, image=ALPINE, step=a.density_step, stop_reason=stop, peak_sandboxes=peak,
        mem_floor_mb=a.density_mem_floor_mb, key_headroom=a.density_key_headroom, steps=steps, create_errors=create_errors,
        teardown=dict(n=peak, concurrency=a.density_teardown_conc, seconds=round(dt, 3),
                      removals_per_second=round(peak / dt, 2) if dt else None, errors=errs[:5]),
        after_teardown=dict(mem_available_kb=after["mem_available_kb"], keys_used=after["keys_used"],
                            user_inotify_instances=after["user_inotify_instances"]),
        limit_confirmation=limit,
        limits=dict(inotify_max_user_instances=ino_max, keys_maxkeys=read_int("/proc/sys/kernel/keys/maxkeys"),
                    pid_max=read_int("/proc/sys/kernel/pid_max"),
                    user_slice_pids_max=open(pm).read().strip() if os.path.exists(pm) else None,
                    podman_num_locks_default=2048))


def push_to_limit(a):
    """Opt-in (--push-to-limit): keep creating cheap containers until the
    first real error, record it, remove them at once. Refuses to run while
    containers that are not ours exist, because exhausting a per-user quota
    would fail somebody else's container starts."""
    foreign = foreign_containers()
    if foreign:
        return dict(skipped=f"{len(foreign)} foreign container(s) present; not exhausting a shared per-user quota")
    made, error = [], None
    before = key_usage()
    for i in range(a.density_key_headroom + 15):
        name = new_name("lim")
        ms, rc, out, err = sh(["podman", "run", "-d", "--name", name, "--network", "none", "--log-driver", "none",
                               "--stop-timeout", "0", ALPINE, *IDLE])
        if rc != 0:
            error = err.strip()[-600:]
            sh(["podman", "rm", "-f", "-t", "0", name])
            break
        made.append(name)
    at = key_usage()
    for i in range(0, len(made), 40):
        sh(["podman", "rm", "-f", "-t", "0", *made[i:i + 40]])
    return dict(extra_containers_started=len(made), keys_before=before, keys_at_failure=at, first_error=error)


def par_map(fn, items, conc):
    errs = []
    with cf.ThreadPoolExecutor(max_workers=conc) as ex:
        for f in cf.as_completed([ex.submit(fn, it) for it in items]):
            try:
                f.result()
            except Exception as e:  # noqa
                errs.append(str(e)[-300:])
    return errs


def bench_parallel(a):
    """Create throughput (create + start + agent ready) and teardown rate at
    several concurrencies, REST API and CLI."""
    start_service()
    agent = build_agent()
    out = []
    n = a.parallel_n
    for rnd in range(a.rounds):
        for transport in ("rest-api", "cli"):
            for conc in a.parallel_conc:
                made, lock = [], threading.Lock()

                def one(_, transport=transport):
                    if transport == "rest-api":
                        tm = []
                        b = make_box(agent, timings=tm)
                        with lock:
                            made.append(b)
                        return tm[0]["total"]
                    name = new_name("pc")
                    rd = new_rundir(name)
                    t = time.perf_counter()
                    flags = hardened_flags(rundir=rd, agent=agent or None, agent_cmd=bool(agent))
                    ms, rc, o, err = sh(["podman", "run", "-d", "--name", name, *flags, ALPINE] + ([] if agent else IDLE))
                    with lock:
                        made.append(dict(name=name, rundir=rd))
                    if rc != 0:
                        raise RuntimeError(err[-300:])
                    if agent:
                        agent_wait_ready(rd)
                    return (time.perf_counter() - t) * 1e3

                lat, errs = [], []
                l0 = loadavg()
                t = time.perf_counter()
                with cf.ThreadPoolExecutor(max_workers=conc) as ex:
                    for f in cf.as_completed([ex.submit(one, i) for i in range(n)]):
                        try:
                            lat.append(f.result())
                        except Exception as e:  # noqa
                            errs.append(str(e)[-300:])
                wall = time.perf_counter() - t
                t = time.perf_counter()
                if transport == "rest-api":
                    rerr = par_map(lambda b: api_rm(b["name"]), made, conc)
                else:
                    def rm(b):
                        ms, rc, o, err = sh(["podman", "rm", "-f", "-t", "0", b["name"]])
                        if rc != 0:
                            raise RuntimeError(err[-300:])
                    rerr = par_map(rm, made, conc)
                rwall = time.perf_counter() - t
                for b in made:
                    shutil.rmtree(b["rundir"], ignore_errors=True)
                row = dict(round=rnd, transport=transport, concurrency=conc, n=n, ok=len(lat),
                           wall_s=round(wall, 3), creates_per_s=round(len(lat) / wall, 2),
                           per_create_ms=stats(lat), create_errors=errs[:3], n_create_errors=len(errs),
                           teardown_wall_s=round(rwall, 3), removals_per_s=round(len(made) / rwall, 2),
                           teardown_errors=rerr[:3], load_start=l0, load_end=loadavg())
                out.append(row)
                log(f"parallel r{rnd + 1} {transport} c={conc}: {row['creates_per_s']} creates/s, "
                    f"{row['removals_per_s']} rm/s, errors={len(errs)}/{len(rerr)}")
    REC.extra["parallel"] = out


# --------------------------------------------------------------------------
# 5. workload overhead

WORKLOAD_DRIVER = r'''
import json, os, subprocess, sys, time
REPS = int(sys.argv[1]) if len(sys.argv) > 1 else 5
WD = "/workspace"
res = {}
def t(name, fn, reps):
    xs = []
    fn()  # one discarded warm-up call
    for _ in range(reps):
        a = time.perf_counter(); fn(); xs.append((time.perf_counter() - a) * 1000)
    res[name] = xs
MODS = ("json,re,ssl,sqlite3,asyncio,http.client,decimal,datetime,subprocess,argparse,logging,unittest,"
        "email.parser,xml.etree.ElementTree,csv,hashlib,urllib.request,zipfile,tempfile,pathlib,"
        "multiprocessing,ctypes,uuid,random,statistics,gzip,shutil,socket,threading,dataclasses")
def cpu():
    s = 0
    for i in range(2_000_000):
        s += i * i % 7
def files():
    d = os.path.join(WD, "bench_files")
    os.makedirs(d, exist_ok=True)
    blob = b"x" * 1024
    for i in range(2000):
        with open(os.path.join(d, "f%d" % i), "wb") as f:
            f.write(blob)
    n = 0
    for i in range(2000):
        with open(os.path.join(d, "f%d" % i), "rb") as f:
            n += len(f.read())
    for i in range(2000):
        os.unlink(os.path.join(d, "f%d" % i))
    os.rmdir(d)
    assert n == 2000 * 1024
t("python_start_pass", lambda: subprocess.run([sys.executable, "-c", "pass"], check=True), REPS * 6)
t("python_import_30_stdlib_modules", lambda: subprocess.run([sys.executable, "-c", "import " + MODS], check=True), REPS * 3)
t("cpu_loop_2M_iterations", cpu, REPS)
t("small_files_2000x1KiB_write_read_unlink", files, REPS)
t("fork_shell_300x_bin_true", lambda: subprocess.run(
    ["sh", "-c", "i=0; while [ $i -lt 300 ]; do /bin/true; i=$((i+1)); done"], check=True), REPS)
t("python_subprocess_100x_true", lambda: [subprocess.run(["/bin/true"], check=True) for _ in range(100)], REPS)
print(json.dumps(res))
'''


def bench_workload(a):
    """Agent-style workloads timed *inside* the sandbox (one `podman exec`
    carries the driver in; the exec cost is not in the numbers)."""
    rt = a.runtime
    if not shutil.which(rt):
        REC.extra["workload_skipped"] = f"runtime {rt!r} is not installed on this host"
        log("workload:", REC.extra["workload_skipped"])
        return
    rd = new_rundir("wl")
    variants = [("plain (overlay workdir)", plain_flags(rt) + ["--stop-timeout", "0"]),
                ("plain + tmpfs workdir", plain_flags(rt) + ["--stop-timeout", "0", "--tmpfs", "/workspace"]),
                ("hardened, Podman default seccomp", hardened_flags(rt, rundir=rd, seccomp="default")),
                ("hardened, product seccomp profile", hardened_flags(rt, rundir=rd, seccomp="custom")),
                ("hardened, product seccomp + PYTHONPYCACHEPREFIX=/tmp/pycache",
                 hardened_flags(rt, rundir=rd, seccomp="custom") + ["-e", "PYTHONPYCACHEPREFIX=/tmp/pycache"])]
    names = {}
    for label, flags in variants:
        name = new_name("wl")
        ms, rc, out, err = sh(["podman", "run", "-d", "--name", name, *flags, PYIMG, *IDLE])
        if rc != 0:
            REC.err("workload", f"{rt}/{label}", "setup", 0, err)
            continue
        sh(["podman", "exec", name, "mkdir", "-p", "/workspace"])
        names[label] = name
    for rnd in range(a.workload_rounds):
        for label, name in names.items():
            cell = f"{rt}/{label}"
            log(f"workload round {rnd + 1}/{a.workload_rounds} {cell} load={loadavg()[0]}")
            ms, rc, out, err = sh(["podman", "exec", "-i", "-w", "/workspace", name, "python3", "-", str(a.workload_reps)],
                                  stdin=WORKLOAD_DRIVER.encode(), timeout=900)
            if rc != 0:
                REC.err("workload", cell, "driver", rnd, err or out)
                continue
            try:
                data = json.loads(out.strip().splitlines()[-1])
            except Exception as e:  # noqa
                REC.err("workload", cell, "driver", rnd, f"{e}: {out[-300:]}")
                continue
            for op, xs in data.items():
                for x in xs:
                    REC.add("workload", cell, op, rnd, x)
    for name in names.values():
        sh(["podman", "rm", "-f", "-t", "0", name])


# --------------------------------------------------------------------------
# 6. host capability probes

SECCOMP_PROBE = r'''
import ctypes, errno, json, os, platform, socket
libc = ctypes.CDLL(None, use_errno=True)
libc.syscall.restype = ctypes.c_long
out = {"arch": platform.machine()}
def sc(name, nr, *args):
    ctypes.set_errno(0)
    r = libc.syscall(ctypes.c_long(nr), *args)
    e = ctypes.get_errno()
    out[name] = {"ret": r, "errno": e if r < 0 else 0, "err": errno.errorcode.get(e, str(e)) if r < 0 else "OK"}
    return r
def in_child(fn):
    rfd, wfd = os.pipe()
    pid = os.fork()
    if pid == 0:
        os.close(rfd)
        try:
            fn()
        finally:
            os.write(wfd, json.dumps(out).encode()); os._exit(0)
    os.close(wfd)
    data = b""
    while True:
        b = os.read(rfd, 65536)
        if not b: break
        data += b
    os.waitpid(pid, 0)
    out.update(json.loads(data))
L = ctypes.c_long
buf = ctypes.create_string_buffer(256)
if platform.machine() == "x86_64":
    in_child(lambda: sc("unshare(CLONE_NEWUSER)", 272, L(0x10000000)))
    in_child(lambda: sc("unshare(CLONE_NEWNS)", 272, L(0x00020000)))
    in_child(lambda: sc("unshare(CLONE_NEWNET)", 272, L(0x40000000)))
    sc("bpf(BPF_MAP_CREATE)", 321, L(0), buf, L(72))
    r = sc("io_uring_setup(1)", 425, L(1), buf)
    if r >= 0: os.close(r)
    r = sc("userfaultfd(0)", 323, L(0))
    if r >= 0: os.close(r)
    sc("keyctl(GET_KEYRING_ID, session)", 250, L(0), L(-3), L(0))
    sc("add_key(user)", 248, b"user", b"sbxbench", b"x", L(1), L(-3))
    in_child(lambda: sc("ptrace(PTRACE_TRACEME)", 101, L(0), L(0), L(0), L(0)))
    sc("mount(tmpfs on /tmp)", 165, b"none", b"/tmp", b"tmpfs", L(0), L(0))
    r = sc("perf_event_open", 298, buf, L(0), L(-1), L(-1), L(0))
    if r >= 0: os.close(r)
    sc("clone3(NULL)", 435, L(0), L(0))
    sc("personality(query)", 135, L(0xffffffff))
    sc("open_by_handle_at", 304, L(-1), L(0), L(0))
    sc("kexec_load", 246, L(0), L(0), L(0), L(0))
    sc("init_module", 175, L(0), L(0), L(0))
    sc("setns(-1)", 308, L(-1), L(0))
    sc("pivot_root", 155, b"/", b"/")
    sc("process_vm_readv(pid 1)", 310, L(1), L(0), L(0), L(0), L(0), L(0))
def sock(name, fam, typ, proto=0):
    try:
        s = socket.socket(fam, typ, proto); s.close()
        out[name] = {"ret": 0, "errno": 0, "err": "OK"}
    except OSError as e:
        out[name] = {"ret": -1, "errno": e.errno, "err": errno.errorcode.get(e.errno, str(e.errno))}
sock("socket(AF_ALG, SOCK_SEQPACKET)", 38, 5)
sock("socket(AF_NETLINK, SOCK_RAW)", 16, 3)
sock("socket(AF_PACKET, SOCK_RAW)", 17, 3)
sock("socket(AF_INET, SOCK_RAW, ICMP)", 2, 3, 1)
sock("socket(AF_VSOCK, SOCK_STREAM)", 40, 1)
sock("socket(AF_UNIX, SOCK_STREAM)", 1, 1)
try:
    out["status"] = {l.split(":")[0]: l.split(":")[1].strip() for l in open("/proc/self/status")
                     if l.split(":")[0] in ("Seccomp", "Seccomp_filters", "NoNewPrivs", "CapEff", "CapBnd", "Uid")}
except OSError as e:
    out["status"] = str(e)
print(json.dumps(out))
'''

SECCOMP_NAMES = {"unshare": "unshare(CLONE_NEWUSER)", "bpf": "bpf(BPF_MAP_CREATE)", "io_uring_setup": "io_uring_setup(1)",
                 "userfaultfd": "userfaultfd(0)", "keyctl": "keyctl(GET_KEYRING_ID, session)", "add_key": "add_key(user)",
                 "ptrace": "ptrace(PTRACE_TRACEME)", "mount": "mount(tmpfs on /tmp)", "perf_event_open": "perf_event_open",
                 "clone3": "clone3(NULL)", "personality": "personality(query)", "open_by_handle_at": "open_by_handle_at",
                 "kexec_load": "kexec_load", "init_module": "init_module", "setns": "setns(-1)", "pivot_root": "pivot_root",
                 "process_vm_readv": "process_vm_readv(pid 1)", "socket": "socket(AF_ALG, SOCK_SEQPACKET)"}


def seccomp_static(path):
    """What a profile says about each probed syscall, read from the JSON."""
    try:
        prof = json.load(open(path))
    except Exception as e:  # noqa
        return dict(error=str(e))
    out = dict(defaultAction=prof.get("defaultAction"), defaultErrnoRet=prof.get("defaultErrnoRet"), rules={})
    for sc_name in SECCOMP_NAMES:
        rules = []
        for r in prof.get("syscalls", []):
            if sc_name in r.get("names", []):
                d = dict(action=r.get("action"))
                if r.get("args"):
                    d["args"] = [f"arg{x.get('index')} {x.get('op')} {x.get('value')}" for x in r["args"]]
                if (r.get("includes") or {}).get("caps"):
                    d["only_with_caps"] = r["includes"]["caps"]
                if (r.get("excludes") or {}).get("caps"):
                    d["only_without_caps"] = r["excludes"]["caps"]
                if r.get("errnoRet") is not None:
                    d["errnoRet"] = r["errnoRet"]
                rules.append(d)
        out["rules"][sc_name] = rules or "not listed (default action applies)"
    return out


def probe(name, question):
    def deco(fn):
        def run(a):
            log("probe:", name)
            try:
                r = fn(a)
            except Exception as e:  # noqa
                r = dict(result="error", detail=f"probe raised: {e}")
            r["question"] = question
            REC.extra.setdefault("probes", {})[name] = r
            log(f"  -> {r.get('result')}: {str(r.get('detail'))[:160]}")
        PROBES.append(run)
        return run
    return deco


PROBES = []


def wait_state(name, want_not="running", timeout=20.0):
    t = time.perf_counter()
    while time.perf_counter() - t < timeout:
        ms, rc, out, err = sh(["podman", "inspect", "--format", "{{.State.Status}} {{.State.ExitCode}} {{.State.OOMKilled}}", name])
        if rc == 0 and out.split() and out.split()[0] != want_not:
            return out.strip()
        time.sleep(0.05)
    return "timeout:" + out.strip()


@probe("cgroup_kill", "Can the rootless user write cgroup.kill for a Podman-created container cgroup, and does it kill a fork-heavy tree?")
def probe_cgroup_kill(a):
    name = new_name("pk")
    rd = new_rundir(name)
    script = "i=0; while [ $i -lt 60 ]; do sleep 1000 & i=$((i+1)); done; while :; do /bin/true; done"
    ms, rc, out, err = sh(["podman", "run", "-d", "--name", name, *hardened_flags(rundir=rd), ALPINE, "sh", "-c", script])
    if rc != 0:
        return dict(result="error", detail=err.strip()[-400:])
    time.sleep(2)
    ms, rc, out, err = sh(["podman", "inspect", "--format", "{{.State.CgroupPath}}", name])
    cg = scope_dir(out.strip())
    leaf = cg + "/container"
    pids_before = read_int(leaf + "/pids.current")
    st = os.stat(leaf + "/cgroup.kill")
    owner = f"uid={st.st_uid} mode={oct(st.st_mode & 0o777)}"
    t = time.perf_counter()
    try:
        with open(leaf + "/cgroup.kill", "w") as f:
            f.write("1")
    except OSError as e:
        sh(["podman", "rm", "-f", "-t", "0", name])
        return dict(result="fails", detail=f"write {leaf}/cgroup.kill: {e}", owner=owner)
    while time.perf_counter() - t < 10:
        p = read_int(leaf + "/pids.current")
        if not p:
            break
    ms_empty = (time.perf_counter() - t) * 1e3
    state = wait_state(name)
    ms_state = (time.perf_counter() - t) * 1e3
    time.sleep(1)
    ms, rc, out, err = sh(["podman", "inspect", "--format", "{{.State.Status}} {{.State.ExitCode}}", name])
    final = out.strip()
    sh(["podman", "rm", "-f", "-t", "0", name])
    state = f"{state} -> {final}"
    ok = not p and ("exited" in state or "stopped" in state)
    return dict(result="works" if ok else "fails", pids_before=pids_before, cgroup=leaf, file_owner=owner,
                ms_until_cgroup_empty=round(ms_empty, 2), ms_until_podman_reports_exited=round(ms_state, 1),
                podman_state_after=state,
                detail=f"{pids_before} pids (60 sleeps + a fork loop) gone {ms_empty:.1f} ms after writing 1; Podman state '{state}' after {ms_state:.0f} ms")


@probe("memory_oom_group", "Does --cgroup-conf memory.oom.group=1 take effect?")
def probe_oom_group(a):
    res = {}
    script = "sleep 1000 & tail /dev/zero; echo SURVIVED; exit 42"
    for val in ("1", "0"):
        name = new_name("po")
        ms, rc, out, err = sh(["podman", "run", "-d", "--name", name, "--network", "none", "--memory", "64m", "--memory-swap", "64m",
                               "--cgroup-conf", f"memory.oom.group={val}", "--log-driver", "none", ALPINE, "sh", "-c", script])
        if rc != 0:
            res[val] = dict(error=err.strip()[-400:])
            continue
        ms, rc, out, err = sh(["podman", "inspect", "--format", "{{.State.CgroupPath}}", name])
        leaf = scope_dir(out.strip()) + "/container"
        file_val = read_int(leaf + "/memory.oom.group")
        state = wait_state(name, timeout=60)
        res[val] = dict(file_value_in_leaf_cgroup=file_val, final_state=state)
        sh(["podman", "rm", "-f", "-t", "0", name])
    g1, g0 = res.get("1", {}), res.get("0", {})
    ok = g1.get("file_value_in_leaf_cgroup") == 1 and "137" in str(g1.get("final_state")) and "42" in str(g0.get("final_state"))
    return dict(result="works" if ok else "partial/fails", runs=res,
                detail=f"oom.group=1: file reads {g1.get('file_value_in_leaf_cgroup')}, container ended '{g1.get('final_state')}' "
                       f"(status exitcode OOMKilled); control oom.group=0: ended '{g0.get('final_state')}' "
                       "(42 = only the hog was killed and the shell carried on)")


@probe("storage_opt_size", "Does --storage-opt size= work rootless on this filesystem?")
def probe_storage_opt(a):
    ms, rc, out, err = sh(["podman", "run", "--rm", "--name", new_name("ps"), "--network", "none", "--storage-opt", "size=10M", ALPINE, "true"])
    ms, rc2, fs, e2 = sh(["sh", "-c", "df -T \"$(podman info --format '{{.Store.GraphRoot}}')\" | tail -1"])
    return dict(result="works" if rc == 0 else "fails", rc=rc, detail=(err.strip() or "ok")[-400:], graphroot_fs=fs.strip())


@probe("exec_detached_reattach", "Can a detached `podman exec -d` session be re-attached?")
def probe_exec_reattach(a):
    start_service()
    name = new_name("pe")
    ms, rc, out, err = sh(["podman", "run", "-d", "--name", name, "--network", "none", "--stop-timeout", "0", ALPINE, *IDLE])
    if rc != 0:
        return dict(result="error", detail=err[-300:])
    r = {}
    ms, rc, out, err = sh(["podman", "exec", "-d", name, "sh", "-c", "sleep 20; echo done"])
    r["cli_exec_d"] = dict(rc=rc, stdout=out.strip()[:80], stderr=err.strip()[-200:])
    ms, rc, out, err = sh(["podman", "exec", "--help"])
    r["cli_has_exec_attach_subcommand"] = bool(re.search(r"\battach\b", out.split("Options:")[0]))
    st, body = papi().call("POST", f"/containers/{name}/exec", dict(Cmd=["sh", "-c", "sleep 20; echo done"], AttachStdout=True, AttachStderr=True))
    eid = json.loads(body)["Id"]
    st1, b1 = papi().call("POST", f"/exec/{eid}/start", dict(Detach=True, Tty=False), fresh=True)
    r["api_start_detached"] = dict(status=st1, body=b1[-200:].decode(errors="replace"))
    st2, b2 = papi().call("GET", f"/exec/{eid}/json")
    r["api_inspect_running"] = json.loads(b2).get("Running") if st2 == 200 else st2
    st3, b3 = papi().call("POST", f"/exec/{eid}/start", dict(Detach=False, Tty=False), fresh=True, timeout=30)
    r["api_second_start_attached"] = dict(status=st3, body=b3[-300:].decode(errors="replace"))
    st4, b4 = papi().call("POST", f"/containers/{name}/attach?stream=true&stdout=true&stderr=true&logs=false", fresh=True, timeout=3)
    r["note_container_attach"] = "POST /containers/{name}/attach attaches to the container's main process only, not to an exec session"
    sh(["podman", "rm", "-f", "-t", "0", name])
    ok = st3 == 200 and b"done" in b3
    return dict(result="works" if ok else "fails", evidence=r,
                detail=f"second start of a detached, still-running exec session -> HTTP {st3}: {b3[-200:].decode(errors='replace').strip()}")


@probe("seccomp_syscalls", "What do the installed default seccomp profile and the product profile do with unshare, bpf, io_uring_setup, AF_ALG and friends?")
def probe_seccomp(a):
    ms, rc, out, err = sh(["podman", "info", "--format", "{{.Host.Security.SECCOMPProfilePath}}"])
    default_profile = out.strip() or "/usr/share/containers/seccomp.json"
    variants = [("default container (default seccomp, default caps, root in userns)", ["--network", "none"]),
                ("default seccomp, cap-drop all, no-new-privileges, uid 1000 (hardened without the product profile)",
                 hardened_flags(seccomp="default")),
                ("product profile (hardened spec)", hardened_flags(seccomp="custom")),
                ("seccomp=unconfined, default caps (control: what the kernel itself refuses)",
                 ["--network", "none", "--security-opt", "seccomp=unconfined"])]
    res = {}
    for label, flags in variants:
        ms, rc, out, err = sh(["podman", "run", "--rm", "-i", "--name", new_name("sp"), *flags, PYIMG, "python3", "-"],
                              stdin=SECCOMP_PROBE.encode(), timeout=120)
        try:
            res[label] = json.loads(out.strip().splitlines()[-1])
        except Exception:  # noqa
            res[label] = dict(error=(err or out).strip()[-400:], rc=rc)
    sysctls = {}
    for k in ("kernel/unprivileged_bpf_disabled", "kernel/io_uring_disabled", "vm/unprivileged_userfaultfd",
              "kernel/apparmor_restrict_unprivileged_userns", "kernel/unprivileged_userns_clone", "kernel/perf_event_paranoid",
              "kernel/yama/ptrace_scope", "user/max_user_namespaces"):
        try:
            sysctls[k.replace("/", ".")] = open("/proc/sys/" + k).read().strip()
        except OSError as e:
            sysctls[k.replace("/", ".")] = f"n/a ({e.strerror})"
    d = res.get(variants[0][0], {})
    brief = ", ".join(f"{k}={d.get(k, {}).get('err')}" for k in
                      ("unshare(CLONE_NEWUSER)", "bpf(BPF_MAP_CREATE)", "io_uring_setup(1)", "socket(AF_ALG, SOCK_SEQPACKET)"))
    return dict(result="recorded", detail="default container: " + brief, runtime_results=res, host_sysctls=sysctls,
                default_profile_path=default_profile, default_profile_static=seccomp_static(default_profile),
                product_profile_static=seccomp_static(SECCOMP_SRC))


@probe("session_keyring_quota", "Does every container consume one kernel key from the user's quota, and does keyring=false avoid it?")
def probe_keyring(a):
    r = {}
    conf = os.path.join(RUN, "nokeyring.conf")
    with open(conf, "w") as f:
        f.write("[containers]\nkeyring = false\n")
    for label, env in (("default", None), ("CONTAINERS_CONF_OVERRIDE keyring=false", dict(os.environ, CONTAINERS_CONF_OVERRIDE=conf))):
        before = key_usage()[0]
        names = [new_name("kr") for _ in range(3)]
        errs = []
        for n in names:
            ms, rc, out, err = sh(["podman", "run", "-d", "--name", n, "--network", "none", "--stop-timeout", "0", ALPINE, *IDLE], env=env)
            if rc != 0:
                errs.append(err.strip()[-200:])
        during = key_usage()[0]
        sh(["podman", "rm", "-f", "-t", "0", *names])
        r[label] = dict(keys_before=before, keys_with_3_containers=during, delta=None if before is None else during - before, errors=errs)
    mx = read_int("/proc/sys/kernel/keys/maxkeys")
    d0, d1 = r["default"]["delta"], r["CONTAINERS_CONF_OVERRIDE keyring=false"]["delta"]
    return dict(result="recorded", runs=r, kernel_keys_maxkeys=mx, kernel_keys_maxbytes=read_int("/proc/sys/kernel/keys/maxbytes"),
                detail=f"3 containers add {d0} keys by default and {d1} with keyring=false; per-user quota kernel.keys.maxkeys={mx}")


@probe("checkpoint_rootless", "Does podman container checkpoint (CRIU) work rootless?")
def probe_checkpoint(a):
    name = new_name("pc")
    sh(["podman", "run", "-d", "--name", name, "--network", "none", "--stop-timeout", "0", ALPINE, *IDLE])
    ms, rc, out, err = sh(["podman", "container", "checkpoint", name])
    sh(["podman", "rm", "-f", "-t", "0", name])
    return dict(result="works" if rc == 0 else "fails", rc=rc, detail=(err.strip() or out.strip())[-300:])


@probe("cgroup_delegation", "Which cgroup v2 controllers are delegated to the rootless user, and do cpu/memory/pids limits land in the container cgroup?")
def probe_delegation(a):
    base = f"/sys/fs/cgroup/user.slice/user-{UID}.slice/user@{UID}.service"
    r = dict(user_service_cgroup_controllers=open(base + "/cgroup.controllers").read().split(),
             user_service_subtree_control=open(base + "/cgroup.subtree_control").read().split())
    ms, rc, out, err = sh(["podman", "info", "--format", "{{.Host.CgroupControllers}}"])
    r["podman_info_cgroupControllers"] = out.strip()
    name = new_name("pd")
    rd = new_rundir(name)
    ms, rc, out, err = sh(["podman", "run", "-d", "--name", name, *hardened_flags(rundir=rd), ALPINE, *IDLE])
    if rc != 0:
        return dict(result="fails", detail=err.strip()[-400:], **r)
    ms, rc, out, err = sh(["podman", "inspect", "--format", "{{.State.CgroupPath}}", name])
    leaf = scope_dir(out.strip()) + "/container"
    vals = {}
    for f in ("cpu.max", "memory.max", "memory.high", "memory.swap.max", "memory.oom.group", "pids.max"):
        try:
            vals[f] = open(f"{leaf}/{f}").read().strip()
        except OSError as e:
            vals[f] = f"missing: {e.strerror}"
    sh(["podman", "rm", "-f", "-t", "0", name])
    ok = vals["cpu.max"].startswith("100000") and vals["pids.max"] == str(RES["pids"]) and vals["memory.max"] == str(RES["memory"]) \
        and vals["memory.swap.max"] == "0"
    return dict(result="works" if ok else "partial/fails", leaf_cgroup_values=vals, detail=json.dumps(vals), **r)


def bench_probes(a):
    for p in PROBES:
        p(a)


# --------------------------------------------------------------------------
# host facts


def first_line(cmd):
    ms, rc, out, err = sh(cmd)
    return (out or err).strip().splitlines()[0] if (out or err).strip() else None


def host_facts():
    ms, rc, out, err = sh(["podman", "info", "--format", "json"])
    info = json.loads(out) if rc == 0 else {}
    host, store = info.get("host", {}), info.get("store", {})
    cpu = None
    with open("/proc/cpuinfo") as f:
        for line in f:
            if line.startswith("model name"):
                cpu = line.split(":", 1)[1].strip()
                break
    mi = meminfo()
    graphroot = store.get("graphRoot")
    ms, rc, fstype, err = sh(["stat", "-f", "-c", "%T", graphroot or "/"])
    ms, rc, dfl, err = sh(["df", "-T", graphroot or "/"])
    base = f"/sys/fs/cgroup/user.slice/user-{UID}.slice/user@{UID}.service"

    def rd(p):
        try:
            return open(p).read().strip()
        except OSError as e:
            return f"n/a ({e.strerror})"
    ms, rc, top, err = sh(["sh", "-c", "ps -eo pcpu,pmem,comm --sort=-pcpu | head -9"])
    ms, rc, sub, err = sh(["sh", "-c", "grep -E \"^($(id -un)|$(id -u)):\" /etc/subuid /etc/subgid"])
    return dict(
        timestamp=time.strftime("%Y-%m-%dT%H:%M:%S%z"), hostname_redacted=True,
        kernel=platform.release(), os=rd("/etc/os-release").split("PRETTY_NAME=")[-1].splitlines()[0].strip('"'),
        cpu_model=cpu, cpus=os.cpu_count(), mem_total_kb=mi["MemTotal"], mem_available_kb=mi["MemAvailable"],
        swap_total_kb=mi["SwapTotal"], swap_free_kb=mi["SwapFree"],
        podman_version=info.get("version", {}).get("Version"), podman_api_version=info.get("version", {}).get("APIVersion"),
        crun_version=first_line(["crun", "--version"]), runc_version=first_line(["runc", "--version"]),
        runsc_version=first_line(["runsc", "--version"]) if shutil.which("runsc") else "not installed",
        conmon_version=(host.get("conmon") or {}).get("version"),
        pasta=shutil.which("pasta"), pasta_package=(host.get("pasta") or {}).get("package"),
        slirp4netns=first_line(["slirp4netns", "--version"]) if shutil.which("slirp4netns") else None,
        fuse_overlayfs=first_line(["fuse-overlayfs", "--version"]) if shutil.which("fuse-overlayfs") else None,
        netavark=(host.get("networkBackendInfo") or {}).get("version"), hyperfine=shutil.which("hyperfine"),
        default_oci_runtime=(host.get("ociRuntime") or {}).get("name"),
        storage_driver=store.get("graphDriverName"), storage_graph_status=store.get("graphStatus"),
        storage_graph_options=store.get("graphOptions"), graph_root=graphroot, graph_root_fs_type=fstype.strip(),
        graph_root_df=dfl.strip().splitlines()[-1] if dfl.strip() else None,
        run_root=store.get("runRoot"), transient_store=store.get("transientStore"),
        database_backend=host.get("databaseBackend"), cgroup_version=host.get("cgroupVersion"),
        cgroup_manager=host.get("cgroupManager"), podman_info_cgroup_controllers=host.get("cgroupControllers"),
        user_service_cgroup_controllers=rd(base + "/cgroup.controllers").split(),
        user_service_cgroup_subtree_control=rd(base + "/cgroup.subtree_control").split(),
        event_logger=host.get("eventLogger"), default_log_driver=host.get("logDriver"),
        security=host.get("security"), id_mappings=host.get("idMappings"), subid=sub.strip().splitlines(),
        rootless=(host.get("security") or {}).get("rootless"),
        sysctl={k: rd("/proc/sys/" + k.replace(".", "/")) for k in (
            "fs.inotify.max_user_instances", "fs.inotify.max_user_watches", "kernel.keys.maxkeys", "kernel.keys.maxbytes",
            "kernel.pid_max", "kernel.threads-max", "user.max_user_namespaces",
            "kernel.apparmor_restrict_unprivileged_userns", "kernel.unprivileged_bpf_disabled", "vm.swappiness")},
        ulimit_nofile=first_line(["sh", "-c", "ulimit -n"]), ulimit_nproc=first_line(["sh", "-c", "ulimit -u"]),
        load_average=loadavg(), top_cpu_processes_at_start=top.strip().splitlines(),
        foreign_containers_at_start=len(foreign_containers()),
        images={i: first_line(["podman", "image", "inspect", "--format", "{{.Id}} {{.Size}}", i]) for i in (ALPINE, PYIMG)},
        python=sys.version.split()[0],
    )


# --------------------------------------------------------------------------
# output


def f1(x, nd=1):
    if x is None or (isinstance(x, float) and math.isnan(x)):
        return "-"
    return f"{x:.{nd}f}"


def lat_table(rows, bench, ops=None, cell_filter=None, first_col="cell"):
    rows = [r for r in rows if r["bench"] == bench and (ops is None or r["op"] in ops) and (cell_filter is None or cell_filter(r["cell"]))]
    if not rows:
        return "_no data_\n"
    order = {o: i for i, o in enumerate(ops or [])}
    rows.sort(key=lambda r: (r["cell"], order.get(r["op"], 99), r["op"]))
    out = [f"| {first_col} | op | n | mean | sd | min | p50 | p95 | p99 | max | per-round p50 | load1 | errors |",
           "|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|---:|"]
    for r in rows:
        s = r["stats"]
        if not s.get("n"):
            out.append(f"| {r['cell']} | {r['op']} | 0 | - | - | - | - | - | - | - | - | - | {len(r['errors'])} |")
            continue
        l1 = r["load1"]
        out.append(f"| {r['cell']} | {r['op']} | {s['n']} | {f1(s['mean'])} | {f1(s['stddev'])} | {f1(s['min'])} | {f1(s['p50'])} | "
                   f"{f1(s['p95'])} | {f1(s['p99'])} | {f1(s['max'])} | {' / '.join(f1(x) for x in r['round_p50'])} | "
                   f"{f1(min(l1), 2)}-{f1(max(l1), 2)} | {len(r['errors'])} |")
    return "\n".join(out) + "\n"


def write_summary(outdir, host, args, merged, extra):
    L = []
    w = L.append
    w(f"# sbxbench summary ({host['timestamp']})\n")
    w("All latencies are wall-clock milliseconds. `per-round p50` shows the median of each round separately "
      "(the spread between rounds is the noise from other work on the machine). `load1` is the range of the "
      "1-minute load average seen while the row was being measured.\n")
    w(f"Run configuration: samples/round={args.samples}, rounds={args.rounds}, warm-up cycles/round={args.warmup}, "
      f"quick={args.quick}, benchmarks={','.join(args.only)}.\n")
    w("## Host\n")
    for k in ("kernel", "os", "cpu_model", "cpus", "mem_total_kb", "swap_total_kb", "podman_version", "crun_version", "runc_version",
              "runsc_version", "conmon_version", "pasta_package", "slirp4netns", "fuse_overlayfs", "storage_driver", "graph_root_fs_type",
              "transient_store", "database_backend", "cgroup_version", "cgroup_manager", "podman_info_cgroup_controllers",
              "user_service_cgroup_subtree_control", "default_log_driver", "hyperfine", "load_average", "foreign_containers_at_start"):
        w(f"- **{k}**: {host.get(k)}")
    w(f"- **storage_graph_status**: {json.dumps(host.get('storage_graph_status'))}")
    w(f"- **sysctl**: {json.dumps(host.get('sysctl'))}")
    w(f"- **top CPU processes at start**: {host.get('top_cpu_processes_at_start')}\n")
    benches = {r["bench"] for r in merged}
    LC = ["run_rm_true", "create", "start", "create_plus_start", "exec_first", "exec_again", "rm_f_running"]
    if "floor" in benches:
        w("## 0. Harness floor\n")
        w(lat_table(merged, "floor"))
    if "lifecycle" in benches:
        w("## 1. Lifecycle latency matrix (podman CLI, default store)\n")
        w("Cell = runtime/network/spec. `create`..`rm_f_running` are one container's life; `run_rm_true` is a separate "
          "`podman run --rm IMAGE true`. Idle command `sleep 86400`, image alpine:3.20.\n")
        w(lat_table(merged, "lifecycle", LC))
    if "storage" in benches:
        w("## 1a. Storage driver x transient store (isolated stores, crun, network none)\n")
        w(lat_table(merged, "storage", LC))
        w(f"Isolated store facts: `{json.dumps(extra.get('isolated_stores'))}`\n")
    if "api_vs_cli" in benches:
        w("## 1b. REST API versus CLI, hardened crun/none with the real agent as PID 1\n")
        w(lat_table(merged, "api_vs_cli"))
    if "userns" in benches:
        w("## 2. User-namespace mapping cost (fresh isolated store per repetition)\n")
        w("Cell = storage/image/mapping. `first_*` is the first container of that image with that mapping in a brand-new "
          "store (n = repetitions); `subsequent_*` are later containers in the same store.\n")
        w(lat_table(merged, "userns", ["first_create", "first_start", "first_create_plus_start", "subsequent_create", "subsequent_start",
                                       "subsequent_create_plus_start", "auto_while_others_alive_create", "create_nth"]))
        w(f"`--userns=auto` exhaustion: `{json.dumps(extra.get('userns_auto_exhaustion'))}`\n")
        w(f"Store size (MB, overlay dir) after each cell: `{json.dumps(extra.get('userns_store_mb_after'))}`\n")
    if "pause" in benches:
        w("## 3. Warm state: pause/unpause versus exec into an idle running sandbox\n")
        w(lat_table(merged, "pause"))
        for k in ("pause_exec_into_paused", "pause_state_while_paused", "pause_memory_current_bytes"):
            w(f"- {k}: `{json.dumps(extra.get(k))}`")
        w("")
    for d in extra.get("density") or []:
        w(f"## 4. Density, round {d['round'] + 1}\n")
        w(f"Idle process: {d['idle_process']}; image {d['image']}; hardened spec via REST API. "
          f"Peak: **{d['peak_sandboxes']}** sandboxes. Stop reason: **{d['stop_reason']}**.\n")
        w("| n | MemAvailable MB | host delta/sbx kB | cgroup scope mem kB | conmon scope mem kB | conmon PSS kB | conmon RSS kB | PID1 PSS kB | "
          "pids/sbx | procs/sbx | threads/sbx | fds/sbx | inotify | keys | create p50 | start p50 | create-to-ready p50 | p95 | max | load1 |")
        w("|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
        for s in d["steps"]:
            tot = s.get("step_total_ms") or {}
            w(f"| {s['n']} | {s['mem_available_kb'] // 1024} | {f1(s.get('mem_available_delta_per_sandbox_kb'))} | "
              f"{f1(s.get('cgroup_scope_memory_current_mean_kb'))} | {f1(s.get('conmon_scope_memory_current_mean_kb'))} | "
              f"{f1(s.get('conmon_pss_mean_kb'))} | {f1(s.get('conmon_rss_mean_kb'))} | {f1(s.get('sandbox_pid1_pss_mean_kb'))} | "
              f"{f1(s.get('cgroup_pids_current_mean'))} | {f1(s.get('user_processes_per_sandbox'), 2)} | {f1(s.get('user_threads_per_sandbox'), 2)} | "
              f"{f1(s.get('user_fds_per_sandbox'), 2)} | {s['user_inotify_instances']} | {s['keys_used']}/{s['keys_max']} | "
              f"{f1((s.get('step_create_ms') or {}).get('p50'))} | {f1((s.get('step_start_ms') or {}).get('p50'))} | "
              f"{f1(tot.get('p50'))} | {f1(tot.get('p95'))} | {f1(tot.get('max'))} | {s['load'][0]} |")
        w("")
        last = d["steps"][-1]
        w(f"- host meminfo growth per sandbox at peak (kB): `{json.dumps(last.get('meminfo_growth_per_sandbox_kb'))}`")
        w(f"- podman service RSS kB: baseline {d['steps'][0].get('podman_service_rss_kb')}, at peak {last.get('podman_service_rss_kb')}; "
          f"free Podman locks at peak {last.get('podman_free_locks')}")
        w(f"- create/start errors during the ramp: `{json.dumps(d['create_errors'][:5])}` (total {len(d['create_errors'])})")
        w(f"- teardown at peak: `{json.dumps(d['teardown'])}`")
        w(f"- after teardown: `{json.dumps(d['after_teardown'])}`")
        w(f"- limits on this host: `{json.dumps(d['limits'])}`")
        w(f"- limit confirmation (--push-to-limit): `{json.dumps(d['limit_confirmation'])}`\n")
    if "density_at_peak" in benches:
        w("Request-path latency with the host at peak density:\n")
        w(lat_table(merged, "density_at_peak"))
    if "density_ramp" in benches:
        w("Create latency drift during the ramp (all rounds pooled; cell = sandboxes alive at the end of the step):\n")
        w(lat_table(merged, "density_ramp", ["create", "start", "ready", "create_to_ready"]))
    par = extra.get("parallel")
    if par:
        w("## 4a. Parallel create throughput and teardown rate\n")
        w("One create = create + start + agent ready, hardened spec. n per run = %d.\n" % par[0]["n"])
        w("| transport | concurrency | round | ok | creates/s | per-create p50 ms | p95 ms | max ms | removals/s | create errors | load1 start-end |")
        w("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|")
        for r in sorted(par, key=lambda r: (r["transport"], r["concurrency"], r["round"])):
            s = r["per_create_ms"]
            w(f"| {r['transport']} | {r['concurrency']} | {r['round'] + 1} | {r['ok']}/{r['n']} | {r['creates_per_s']} | {f1(s.get('p50'))} | "
              f"{f1(s.get('p95'))} | {f1(s.get('max'))} | {r['removals_per_s']} | {r['n_create_errors']} | {r['load_start'][0]}-{r['load_end'][0]} |")
        errs = [e for r in par for e in r["create_errors"] + r["teardown_errors"]]
        tot_err = sum(r["n_create_errors"] for r in par)
        w(f"\nCreate errors in total: {tot_err} of {sum(r['n'] for r in par)} attempts. Distinct messages: `{json.dumps(sorted(set(errs))[:6])}`\n")
    if "workload" in benches or extra.get("workload_skipped"):
        w("## 5. Workload overhead (timed inside the sandbox)\n")
        if extra.get("workload_skipped"):
            w(f"Skipped: {extra['workload_skipped']}\n")
        w(lat_table(merged, "workload"))
    pr = extra.get("probes")
    if pr:
        w("## 6. Host capability probes\n")
        w("| probe | question | result | detail |")
        w("|---|---|---|---|")
        for k, v in pr.items():
            w(f"| {k} | {v.get('question')} | **{v.get('result')}** | {str(v.get('detail')).replace('|', '/').replace(chr(10), ' ')} |")
        sp = pr.get("seccomp_syscalls", {})
        rr = sp.get("runtime_results")
        if rr:
            w("\n### Seccomp probe matrix (errno seen from inside the container; OK = the call succeeded)\n")
            labels = list(rr)
            w("| call | " + " | ".join(labels) + " |")
            w("|---|" + "---|" * len(labels))
            calls = [k for k in rr[labels[0]] if isinstance(rr[labels[0]][k], dict) and "err" in rr[labels[0]][k]] if isinstance(rr[labels[0]], dict) else []
            for c in calls:
                w(f"| `{c}` | " + " | ".join(str((rr[lab].get(c) or {}).get("err", "?")) if isinstance(rr[lab], dict) else "?" for lab in labels) + " |")
            w(f"\nProcess status per variant: `{json.dumps({lab: (rr[lab].get('status') if isinstance(rr[lab], dict) else None) for lab in labels})}`")
            w(f"\nHost sysctls that also gate these calls: `{json.dumps(sp.get('host_sysctls'))}`\n")
            w("Static reading of the two profiles (rules that name each syscall):\n")
            w("| syscall | installed default profile | product profile |")
            w("|---|---|---|")
            ds, ps = sp.get("default_profile_static", {}), sp.get("product_profile_static", {})
            for n in SECCOMP_NAMES:
                w(f"| {n} | `{json.dumps((ds.get('rules') or {}).get(n))}` | `{json.dumps((ps.get('rules') or {}).get(n))}` |")
            w(f"\nDefault actions: installed profile `{ds.get('defaultAction')}` errno {ds.get('defaultErrnoRet')}; "
              f"product profile `{ps.get('defaultAction')}` errno {ps.get('defaultErrnoRet')}.\n")
    errs = [(r["bench"], r["cell"], r["op"], r["errors"][0], len(r["errors"])) for r in merged if r["errors"]]
    if errs:
        w("## Errors recorded\n")
        for b, c, o, e, n in errs:
            w(f"- {b} / {c} / {o}: {n} error(s), first: `{e.strip()[:300]}`")
        w("")
    with open(os.path.join(outdir, "summary.md"), "w") as f:
        f.write("\n".join(L) + "\n")


def save(outdir, host, args):
    merged = REC.merged()
    rows = sorted(REC.rows.values(), key=lambda r: (r["bench"], r["cell"], r["op"], r["round"]))
    with open(os.path.join(outdir, "results.json"), "w") as f:
        json.dump(dict(config=vars(args), rows=rows, extra=REC.extra), f, indent=1, default=str)
    with open(os.path.join(outdir, "summary.csv"), "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["bench", "cell", "op", "n", "mean_ms", "stddev_ms", "min_ms", "p50_ms", "p95_ms", "p99_ms", "max_ms",
                    "per_round_p50_ms", "load1_min", "load1_max", "errors"])
        for r in sorted(merged, key=lambda r: (r["bench"], r["cell"], r["op"])):
            s = r["stats"]
            w.writerow([r["bench"], r["cell"], r["op"], s.get("n", 0)] + [round(s[k], 3) if s.get("n") else "" for k in
                       ("mean", "stddev", "min", "p50", "p95", "p99", "max")] +
                       ["/".join(str(x) for x in r["round_p50"]), min(r["load1"]), max(r["load1"]), len(r["errors"])])
    write_summary(outdir, host, args, merged, REC.extra)


BENCHES = [("floor", bench_floor), ("lifecycle", bench_lifecycle), ("storage", bench_storage), ("api", bench_api),
           ("userns", bench_userns), ("pause", bench_pause), ("density", bench_density), ("parallel", bench_parallel),
           ("workload", bench_workload), ("probes", bench_probes)]


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--only", default=",".join(n for n, _ in BENCHES), help="comma-separated: " + ",".join(n for n, _ in BENCHES))
    ap.add_argument("--samples", type=int, default=50, help="timed samples per cell per round (default 50)")
    ap.add_argument("--rounds", type=int, default=2, help="rounds; cells are interleaved across rounds (default 2)")
    ap.add_argument("--warmup", type=int, default=5, help="discarded warm-up cycles per cell per round (default 5)")
    ap.add_argument("--runtime", default="crun", help="OCI runtime for the workload suite (crun, runc, runsc)")
    ap.add_argument("--workload-rounds", type=int, default=3)
    ap.add_argument("--workload-reps", type=int, default=5)
    ap.add_argument("--userns-reps", type=int, default=3, help="fresh-store repetitions per mapping")
    ap.add_argument("--userns-subsequent", type=int, default=20)
    ap.add_argument("--userns-auto-concurrent", type=int, default=6)
    ap.add_argument("--userns-auto-max", type=int, default=80)
    ap.add_argument("--density-step", type=int, default=20)
    ap.add_argument("--density-rounds", type=int, default=2)
    ap.add_argument("--density-ceiling", type=int, default=400)
    ap.add_argument("--density-mem-floor-mb", type=int, default=2500, help="stop the ramp when MemAvailable drops below this")
    ap.add_argument("--density-key-headroom", type=int, default=20, help="stop this many keys short of kernel.keys.maxkeys")
    ap.add_argument("--density-inotify-headroom", type=int, default=64)
    ap.add_argument("--density-settle", type=float, default=3.0)
    ap.add_argument("--density-teardown-conc", type=int, default=4)
    ap.add_argument("--push-to-limit", action="store_true", help="after the ramp, create until the first real error (see README)")
    ap.add_argument("--parallel-n", type=int, default=32)
    ap.add_argument("--parallel-conc", default="1,2,4,8")
    ap.add_argument("--quick", action="store_true", help="smoke test: tiny sample counts, numbers are not meaningful")
    ap.add_argument("--out", default=None, help="results directory (default bench/results/<timestamp>)")
    a = ap.parse_args()
    a.only = [x for x in a.only.split(",") if x]
    a.parallel_conc = [int(x) for x in str(a.parallel_conc).split(",")]
    if a.quick:
        a.samples, a.rounds, a.warmup = 3, 1, 1
        a.workload_rounds, a.workload_reps = 1, 1
        a.userns_reps, a.userns_subsequent, a.userns_auto_concurrent, a.userns_auto_max = 1, 2, 2, 4
        a.density_step, a.density_ceiling, a.density_settle, a.density_rounds = 4, 8, 1.0, 1
        a.parallel_n, a.parallel_conc = 4, [1, 2]
    unknown = set(a.only) - {n for n, _ in BENCHES}
    if unknown:
        ap.error(f"unknown benchmark(s): {sorted(unknown)}")
    outdir = a.out or os.path.join(HERE, "results", time.strftime("%Y%m%d-%H%M%S"))
    os.makedirs(outdir, exist_ok=True)
    os.makedirs(WORK, exist_ok=True)
    os.makedirs(RUN, exist_ok=True)
    atexit.register(cleanup)
    signal.signal(signal.SIGTERM, _sig)
    signal.signal(signal.SIGINT, _sig)
    host = host_facts()
    with open(os.path.join(outdir, "host.json"), "w") as f:
        json.dump(host, f, indent=1)
    log(f"results -> {outdir}; load {host['load_average']}; foreign containers: {host['foreign_containers_at_start']}")
    t0 = time.time()
    for name, fn in BENCHES:
        if name not in a.only:
            continue
        log(f"=== {name} ===")
        t = time.time()
        try:
            fn(a)
        except Exception as e:  # noqa
            import traceback
            traceback.print_exc()
            REC.err(name, "harness", "exception", 0, f"{type(e).__name__}: {e}")
        REC.extra.setdefault("bench_wall_seconds", {})[name] = round(time.time() - t, 1)
        REC.extra.setdefault("bench_load_at_end", {})[name] = loadavg()
        remove_ours(MAIN)
        save(outdir, host, a)
    host["load_average_at_end"] = loadavg()
    host["wall_seconds"] = round(time.time() - t0, 1)
    with open(os.path.join(outdir, "host.json"), "w") as f:
        json.dump(host, f, indent=1)
    save(outdir, host, a)
    log(f"done in {host['wall_seconds']}s; summary: {os.path.join(outdir, 'summary.md')}")


if __name__ == "__main__":
    main()
