#!/usr/bin/env python3
"""tierbench: crun versus gVisor (runsc), measured through the product.

Starts a real `sbxd` (managed Podman mode, both isolation tiers enabled) with
its own Postgres, host id, run dir and port, creates one template per tier for
every experiment, and drives the public REST API on keep-alive connections.
Python 3 standard library only. See bench/README.md for the commands and
bench/RESULTS.md for what was measured with it.

Every measurement is a "block": one tier, one cell, one round. A block records
the load average, MemAvailable and the host's CPU busy share at its start and
end and is written to bench/results/<run>/blocks/ the moment it ends, so an
interrupted run loses at most one block. `report` turns the blocks into
results.json, summary.csv and summary.md.
"""
from __future__ import annotations

import argparse
import base64
import contextlib
import csv
import glob
import http.client
import json
import os
import re
import shlex
import shutil
import signal
import socket
import statistics
import struct
import subprocess
import sys
import threading
import time
import urllib.request
from concurrent.futures import ThreadPoolExecutor

BENCH = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(BENCH)
WORK = os.path.join(BENCH, ".work")
STATE = os.path.join(WORK, "tb-state.json")
XDG = os.environ.get("XDG_RUNTIME_DIR", "")
HOME = os.path.expanduser("~")

PG_NAME = "sbx-bn-pg"
PG_PORT = 55446
PG_IMAGE = "postgres:17-alpine"

IMG_ALPINE = "docker.io/library/python:3.12-alpine"
IMG_SLIM = "docker.io/library/python:3.12-slim"
IMG_WL = {"alpine": "localhost/sbxbench-alpine:1", "slim": "localhost/sbxbench-slim:1"}
IMG_NET = {"alpine": IMG_ALPINE, "slim": IMG_SLIM}

TIERS = ("crun", "runsc")
SUFFIX = {"crun": "c", "runsc": "g"}

# name -> (port, run dir, host id, database)
SERVERS = {
    "main": (18461, "sbxb-m", "sbxbench-main", "sbx_main"),
    "su": (18462, "sbxb-s", "sbxbench-su", "sbx_su"),
}

HIGH_LOAD = 3.0  # load1 at the start of a block above which the block is flagged (4 hardware threads)
MEM_FLOOR_KB = 2_500_000  # never start something that grows memory below this MemAvailable


# --------------------------------------------------------------------------- utilities

def now() -> float:
    return time.perf_counter()


def sh(cmd, check=True, env=None, timeout=600, cwd=None) -> str:
    p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, env=env, timeout=timeout, cwd=cwd)
    out = p.stdout.decode(errors="replace")
    if check and p.returncode != 0:
        raise RuntimeError("%s: exit %d\n%s" % (" ".join(map(str, cmd)), p.returncode, out))
    return out


def loadavg():
    with open("/proc/loadavg") as f:
        return [float(x) for x in f.read().split()[:3]]


def meminfo(*keys):
    out = {}
    with open("/proc/meminfo") as f:
        for line in f:
            k, v = line.split(":", 1)
            if not keys or k in keys:
                out[k] = int(v.split()[0])
    return out


def mem_avail_kb() -> int:
    return meminfo("MemAvailable")["MemAvailable"]


def cpu_times():
    with open("/proc/stat") as f:
        v = [int(x) for x in f.readline().split()[1:]]
    idle = v[3] + v[4]
    return sum(v), idle


def pct(values, p):
    """Linear-interpolated percentile."""
    if not values:
        return None
    s = sorted(values)
    if len(s) == 1:
        return s[0]
    k = (len(s) - 1) * p / 100.0
    lo = int(k)
    hi = min(lo + 1, len(s) - 1)
    return s[lo] + (s[hi] - s[lo]) * (k - lo)


def stats(values):
    v = [x for x in values if isinstance(x, (int, float))]
    if not v:
        return {"n": 0}
    return {"n": len(v), "mean": statistics.fmean(v), "sd": statistics.stdev(v) if len(v) > 1 else 0.0,
            "min": min(v), "p50": pct(v, 50), "p95": pct(v, 95), "p99": pct(v, 99), "max": max(v)}


def parse_go_duration(s: str) -> float:
    """Go duration string -> milliseconds."""
    total = 0.0
    for num, unit in re.findall(r"([0-9.]+)(ns|µs|us|ms|s|m|h)", s):
        total += float(num) * {"ns": 1e-6, "µs": 1e-3, "us": 1e-3, "ms": 1.0, "s": 1e3, "m": 6e4, "h": 3.6e6}[unit]
    return total


def load_state() -> dict:
    with open(STATE) as f:
        return json.load(f)


def save_state(st: dict) -> None:
    tmp = STATE + ".tmp"
    with open(tmp, "w") as f:
        json.dump(st, f, indent=1)
    os.replace(tmp, STATE)


def log(*a):
    print(time.strftime("%H:%M:%S"), *a, flush=True)


def redact(text: str) -> str:
    """Nothing written under bench/ may name the operator or the machine: the home
    directory becomes ~, the user name `user`, the host name `host`."""
    import getpass
    user, host = getpass.getuser(), socket.gethostname()
    text = text.replace(HOME, "~")
    for word, repl in ((user, "user"), (host, "host"), (host.split(".")[0], "host")):
        if len(word) >= 3:
            text = text.replace(word, repl)
    return text


def write_json(path, obj, indent=None):
    with open(path, "w") as f:
        f.write(redact(json.dumps(obj, indent=indent)))


def check_redacted(root=BENCH):
    """Returns the files under bench/ (outside .work) that still name the user or the host."""
    import getpass
    words = [w.encode() for w in (getpass.getuser(), socket.gethostname()) if len(w) >= 3]
    bad = []
    for dp, dn, fn in os.walk(root):
        dn[:] = [d for d in dn if d not in (".work", "__pycache__")]
        for f in fn:
            with open(os.path.join(dp, f), "rb") as fh:
                data = fh.read()
            if any(w in data for w in words):
                bad.append(os.path.join(dp, f))
    return bad


# --------------------------------------------------------------------------- blocks

class Block:
    """One measurement block; written to disk when the `with` ends."""
    _seq_lock = threading.Lock()

    def __init__(self, st, group, name, rnd, **cell):
        self.st, self.group, self.name, self.round, self.cell = st, group, name, rnd, cell
        self.samples: dict[str, list] = {}
        self.extra: dict = {}
        self.errors: list[str] = []

    def add(self, metric, value):
        self.samples.setdefault(metric, []).append(value)

    def addmany(self, metric, values):
        self.samples.setdefault(metric, []).extend(values)

    def err(self, msg):
        self.errors.append(str(msg)[:600])

    def __enter__(self):
        self.t0 = time.time()
        self.load0, self.mem0, self.cpu0 = loadavg(), mem_avail_kb(), cpu_times()
        return self

    def __exit__(self, et, ev, tb):
        if et is not None:
            self.err("block aborted: %s: %s" % (et.__name__, ev))
        tot1, idle1 = cpu_times()
        tot0, idle0 = self.cpu0
        busy = 1.0 - (idle1 - idle0) / max(1, tot1 - tot0)
        rec = {"group": self.group, "name": self.name, "round": self.round, "cell": self.cell,
               "t_start": self.t0, "t_end": time.time(), "load_start": self.load0, "load_end": loadavg(),
               "mem_avail_kb_start": self.mem0, "mem_avail_kb_end": mem_avail_kb(), "host_cpu_busy": round(busy, 3),
               "high_load": self.load0[0] >= HIGH_LOAD, "samples": self.samples, "extra": self.extra, "errors": self.errors}
        d = os.path.join(self.st["results"], "blocks")
        os.makedirs(d, exist_ok=True)
        with Block._seq_lock:
            seq = len(os.listdir(d))
            cellname = "-".join(str(v).replace("/", "_") for v in self.cell.values())
            path = os.path.join(d, "%04d.%s.r%s.%s.%s.json" % (seq, self.group, self.round, self.name, cellname))
            write_json(path, rec)
        n = {k: len(v) for k, v in self.samples.items()}
        log("block %s/%s r%s %s: load %.2f->%.2f busy %.0f%% avail %d MB %s%s" % (
            self.group, self.name, self.round, self.cell, self.load0[0], rec["load_end"][0], busy * 100,
            rec["mem_avail_kb_end"] // 1024, n if len(str(n)) < 200 else "(%d metrics)" % len(n),
            " ERRORS=%d %s" % (len(self.errors), self.errors[0][:200]) if self.errors else ""))
        return False


# --------------------------------------------------------------------------- HTTP

class Api:
    """REST client with one keep-alive connection per thread."""

    def __init__(self, port, key):
        self.port, self.key = port, key
        self.local = threading.local()

    def _conn(self, fresh=False):
        c = getattr(self.local, "c", None)
        if c is None or fresh:
            if c is not None:
                c.close()
            c = http.client.HTTPConnection("127.0.0.1", self.port, timeout=300)
            c.connect()
            c.sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
            self.local.c = c
        return c

    def _send(self, method, path, body, token, headers=None):
        h = {"X-Access-Token": token} if token else {"X-API-Key": self.key}
        if headers:
            h.update(headers)
        data = None
        if body is not None:
            if isinstance(body, (bytes, bytearray)):
                data = bytes(body)
                h["Content-Type"] = "application/octet-stream"
            else:
                data = json.dumps(body).encode()
                h["Content-Type"] = "application/json"
        for attempt in (0, 1):
            c = self._conn(fresh=attempt == 1)
            try:
                c.request(method, path, body=data, headers=h)
                return c, c.getresponse()
            except (http.client.RemoteDisconnected, BrokenPipeError, ConnectionResetError, http.client.CannotSendRequest):
                if attempt == 1:
                    raise
        raise RuntimeError("unreachable")

    def call(self, method, path, body=None, token=None):
        """Returns (status, parsed JSON or bytes, elapsed ms)."""
        t = now()
        c, r = self._send(method, path, body, token)
        raw = r.read()
        ms = (now() - t) * 1000
        if r.getheader("Content-Type", "").startswith("application/json") and raw:
            try:
                return r.status, json.loads(raw), ms
            except ValueError:
                pass
        return r.status, raw, ms

    def must(self, want, method, path, body=None, token=None):
        s, d, ms = self.call(method, path, body, token)
        if s != want:
            raise RuntimeError("%s %s: %s %s" % (method, path, s, str(d)[:400]))
        return d, ms

    # --- sandboxes
    def claim(self, template, **kw):
        body = {"template_id": template}
        body.update(kw)
        d, ms = self.must(201, "POST", "/v1/sandboxes", body)
        return d, ms

    def delete(self, sid, wait=False):
        return self.call("DELETE", "/v1/sandboxes/%s%s" % (sid, "?wait=true" if wait else ""))

    def exec(self, sb, cmd, args=(), env=None, timeout_ms=0, cwd=None):
        """Runs a command with ?wait=true. Returns (result dict with decoded stdout/stderr, ms)."""
        body = {"cmd": cmd, "args": list(args)}
        if env:
            body["env"] = env
        if timeout_ms:
            body["timeout_ms"] = timeout_ms
        if cwd:
            body["cwd"] = cwd
        d, ms = self.must(200, "POST", "/v1/sandboxes/%s/commands?wait=true" % sb["id"], body, sb["access_token"])
        d["stdout"] = base64.b64decode(d.get("stdout") or "").decode(errors="replace")
        d["stderr"] = base64.b64decode(d.get("stderr") or "").decode(errors="replace")
        return d, ms

    def shell(self, sb, script, **kw):
        return self.exec(sb, "sh", ["-c", script], **kw)

    def put_file(self, sb, path, data: bytes, mode="0644"):
        self.must(204, "PUT", "/v1/sandboxes/%s/files?path=%s&mode=%s" % (sb["id"], urllib.request.quote(path), mode),
                  data, sb["access_token"])

    def stream(self, sb, body, decode=False):
        """POST commands?wait=stream. Returns timings (ms from the request) and byte counts."""
        t = now()
        c, r = self._send("POST", "/v1/sandboxes/%s/commands?wait=stream" % sb["id"], body, sb["access_token"])
        res = {"status": r.status, "headers_ms": (now() - t) * 1000, "first_out_ms": None, "out_bytes": 0,
               "dropped": 0, "exit": None, "events": 0, "error": None}
        if r.status != 200:
            res["error"] = r.read().decode(errors="replace")[:300]
            return res
        out = []
        while True:
            line = r.readline()
            if not line:
                break
            if res["first_out_ms"] is None and (b'"type":"stdout"' in line[:40] or b'"type":"pty"' in line[:40] or b'"type":"stderr"' in line[:40]):
                res["first_out_ms"] = (now() - t) * 1000
            ev = json.loads(line)
            res["events"] += 1
            ty = ev.get("type")
            if ty in ("stdout", "stderr", "pty"):
                raw = base64.b64decode(ev.get("data") or "")
                res["out_bytes"] += len(raw)
                if decode:
                    out.append(raw)
            elif ty == "truncated":
                res["dropped"] += ev.get("dropped", 0)
            elif ty == "exit":
                res["exit"] = ev.get("exit")
            elif ty == "error":
                res["error"] = ev.get("error")
        res["total_ms"] = (now() - t) * 1000
        if decode:
            res["output"] = b"".join(out)
        return res

    # --- metrics
    def metrics(self):
        s, d, _ = self.call("GET", "/metrics")
        text = d.decode() if isinstance(d, bytes) else str(d)
        m = {}
        for line in text.splitlines():
            if line and not line.startswith("#"):
                k, _, v = line.rpartition(" ")
                try:
                    m[k] = float(v)
                except ValueError:
                    pass
        return m

    def pool(self, template, m=None):
        """Counts by state for one template, plus creates in flight."""
        m = m or self.metrics()
        out = {}
        for k, v in m.items():
            if k.startswith("sbx_pool_sandboxes{") and 'template="%s"' % template in k:
                out[re.search(r'state="([^"]+)"', k).group(1)] = int(v)
        return out

    def wait_pool(self, template, warm, timeout=180, exact=False, settle=0.0):
        """Waits until the template has `warm` warm sandboxes and nothing is being created or destroyed."""
        t = now()
        while True:
            p = self.pool(template)
            busy = p.get("creating", 0) + p.get("recycling", 0) + p.get("failed", 0)
            w = p.get("warm", 0)
            if (w == warm if exact else w >= warm) and busy == 0:
                if settle:
                    time.sleep(settle)
                return (now() - t) * 1000
            if now() - t > timeout:
                raise RuntimeError("pool %s: %s after %ss, want warm=%s" % (template, p, timeout, warm))
            time.sleep(0.05)


class WS:
    """Minimal RFC 6455 client for the command WebSocket (binary frames only)."""

    def __init__(self, port, path, token):
        self.s = socket.create_connection(("127.0.0.1", port), timeout=120)
        self.s.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        key = base64.b64encode(os.urandom(16)).decode()
        req = ("GET %s HTTP/1.1\r\nHost: 127.0.0.1:%d\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
               "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: sbx.v1\r\n"
               "X-Access-Token: %s\r\n\r\n" % (path, port, key, token))
        self.s.sendall(req.encode())
        self.buf = b""
        while b"\r\n\r\n" not in self.buf:
            d = self.s.recv(65536)
            if not d:
                raise RuntimeError("websocket handshake: connection closed: %r" % self.buf[:200])
            self.buf += d
        head, self.buf = self.buf.split(b"\r\n\r\n", 1)
        if b" 101 " not in head.split(b"\r\n", 1)[0]:
            raise RuntimeError("websocket handshake refused: %r" % head[:300])

    def _need(self, n):
        while len(self.buf) < n:
            d = self.s.recv(262144)
            if not d:
                raise EOFError("websocket closed")
            self.buf += d

    def recv(self):
        """Returns (opcode, payload) of the next data or close frame; answers pings."""
        while True:
            self._need(2)
            b0, b1 = self.buf[0], self.buf[1]
            ln, off = b1 & 0x7F, 2
            if ln == 126:
                self._need(4)
                ln, off = struct.unpack(">H", self.buf[2:4])[0], 4
            elif ln == 127:
                self._need(10)
                ln, off = struct.unpack(">Q", self.buf[2:10])[0], 10
            self._need(off + ln)
            payload, self.buf = self.buf[off:off + ln], self.buf[off + ln:]
            op = b0 & 0x0F
            if op == 9:
                self.send(payload, op=10)
                continue
            if op == 10:
                continue
            return op, payload

    def send(self, payload: bytes, op=2):
        n = len(payload)
        if n < 126:
            head = struct.pack(">BB", 0x80 | op, 0x80 | n)
        elif n < 65536:
            head = struct.pack(">BBH", 0x80 | op, 0x80 | 126, n)
        else:
            head = struct.pack(">BBQ", 0x80 | op, 0x80 | 127, n)
        # A zero masking key leaves the payload as it is; the server only requires the bit.
        self.s.sendall(head + b"\0\0\0\0" + payload)

    def close(self):
        with contextlib.suppress(Exception):
            self.send(struct.pack(">H", 1000), op=8)
        self.s.close()


# --------------------------------------------------------------------------- servers

def runsc_path():
    return os.environ.get("SBX_BENCH_RUNSC") or os.path.join(HOME, ".local/opt/gvisor/runsc")


def db_url(db):
    return "postgres://postgres:sbx@127.0.0.1:%d/%s?sslmode=disable" % (PG_PORT, db)


def sbxd_env(db):
    env = dict(os.environ)
    for k in list(env):
        if k.startswith("SBX_"):
            del env[k]
    env["SBX_DATABASE_URL"] = db_url(db)
    env["SBX_RUNTIMES"] = "crun,runsc=" + runsc_path()
    return env


def admin(server, *args, check=True):
    return sh([os.path.join(WORK, "bin", "sbxd"), "admin", *args], env=sbxd_env(SERVERS[server][3]), check=check)


def template_apply(server, tid, tier, image, *, cpus=1, memory="256m", pids=128, workdir="64m", tmp="32m",
                   network="none", warm=0, max_total=0, refill=2, nofile=4096):
    admin(server, "template", "apply", "--id", tid, "--runtime", tier, "--image", image, "--cpus", str(cpus),
          "--memory", memory, "--pids", str(pids), "--workdir-size", workdir, "--tmp-size", tmp, "--network", network,
          "--warm-target", str(warm), "--max-total", str(max_total), "--refill-rate", str(refill), "--nofile", str(nofile),
          "--warm-ttl", "0", "--idle-timeout", "30m", "--max-lifetime", "2h")


def server_paths(name):
    port, rd, hid, db = SERVERS[name]
    return {"port": port, "run_dir": os.path.join(XDG, rd), "host_id": hid, "db": db,
            "log": os.path.join(WORK, "logs", name + ".log")}


def server_start(st, name, runsc_flags="", policy="destroy", claim_wait="5s", wait_ready=True):
    """Starts sbxd; returns (pid, ms from exec to /readyz 200)."""
    p = server_paths(name)
    os.makedirs(os.path.dirname(p["log"]), exist_ok=True)
    args = [os.path.join(WORK, "bin", "sbxd"), "serve", "--listen", "127.0.0.1:%d" % p["port"], "--run-dir", p["run_dir"],
            "--host-id", p["host_id"], "--log-level", "debug", "--shutdown-policy", policy, "--claim-wait", claim_wait]
    if runsc_flags:
        args += ["--runsc-flags", runsc_flags]
    logf = open(p["log"], "ab")
    t = now()
    proc = subprocess.Popen(args, env=sbxd_env(p["db"]), stdout=logf, stderr=logf, stdin=subprocess.DEVNULL,
                            start_new_session=True, cwd=WORK)
    logf.close()
    st.setdefault("servers", {})[name] = {"pid": proc.pid, "runsc_flags": runsc_flags, "policy": policy, **p}
    save_state(st)
    ready_ms = None
    if wait_ready:
        while True:
            if proc.poll() is not None:
                raise RuntimeError("sbxd %s exited with %s during start; see %s" % (name, proc.returncode, p["log"]))
            try:
                c = http.client.HTTPConnection("127.0.0.1", p["port"], timeout=2)
                c.request("GET", "/readyz")
                if c.getresponse().status == 200:
                    ready_ms = (now() - t) * 1000
                    break
            except OSError:
                pass
            if now() - t > 90:
                raise RuntimeError("sbxd %s not ready after 90 s; see %s" % (name, p["log"]))
            time.sleep(0.01)
    return proc.pid, ready_ms


def server_stop(st, name, sig=signal.SIGTERM, timeout=120):
    """Signals this harness's sbxd (by the pid it recorded) and waits. Returns ms."""
    s = st.get("servers", {}).get(name)
    if not s or not s.get("pid"):
        return None
    pid = s["pid"]
    t = now()
    try:
        with open("/proc/%d/cmdline" % pid, "rb") as f:
            cmd = f.read()
        if b"sbxd" not in cmd or s["host_id"].encode() not in cmd:
            raise ProcessLookupError  # the pid was reused by something that is not ours
        os.kill(pid, sig)
    except (ProcessLookupError, FileNotFoundError):
        s["pid"] = None
        save_state(st)
        return None
    while os.path.exists("/proc/%d" % pid):
        with contextlib.suppress(ChildProcessError, OSError):
            os.waitpid(pid, os.WNOHANG)
        try:
            with open("/proc/%d/stat" % pid) as f:
                if f.read().rsplit(")", 1)[1].split()[0] == "Z":
                    break
        except (FileNotFoundError, ProcessLookupError):
            break
        if now() - t > timeout:
            os.kill(pid, signal.SIGKILL)
            raise RuntimeError("sbxd %s did not stop within %ss" % (name, timeout))
        time.sleep(0.01)
    s["pid"] = None
    save_state(st)
    return (now() - t) * 1000


def api_for(st, name) -> Api:
    return Api(SERVERS[name][0], st["keys"][name])


def podman_cli(name, *args, check=True):
    """podman CLI with the runtime table of our own server (needed to see runsc containers)."""
    env = dict(os.environ)
    conf = os.path.join(server_paths(name)["run_dir"], "podman", "containers.conf")
    if os.path.exists(conf):
        env["CONTAINERS_CONF_OVERRIDE"] = conf
    return sh(["podman", *args], env=env, check=check)


def own_containers(name):
    """[(container id, sandbox id, template id, state)] of the server's host id only."""
    hid = SERVERS[name][2]
    out = podman_cli(name, "ps", "-a", "--no-trunc", "--filter", "label=sbx.host_id=" + hid, "--format",
                     '{{.ID}} {{.Names}} {{index .Labels "sbx.template_id"}} {{.State}}')
    res = []
    for line in out.splitlines():
        f = line.split()
        if len(f) == 4 and f[1].startswith("sbx-"):
            res.append((f[0], f[1][4:], f[2], f[3]))
    return res


def log_offset(name):
    try:
        return os.path.getsize(server_paths(name)["log"])
    except OSError:
        return 0


def log_since(name, off):
    with open(server_paths(name)["log"], "rb") as f:
        f.seek(off)
        return f.read().decode(errors="replace")


WARM_RE = re.compile(r'time=(\S+) .*msg="sandbox warm".* sandbox=(\S+) template=(\S+) .*took=(\S+)')


def warm_events(text, template=None):
    """[(unix time of the log line, sandbox, template, took ms)] from a debug log."""
    out = []
    for m in WARM_RE.finditer(text):
        if template and m.group(3) != template:
            continue
        ts = m.group(1)
        mm = re.match(r"(\d+-\d+-\d+T\d+:\d+:\d+)(\.\d+)?", ts)
        base = time.mktime(time.strptime(mm.group(1), "%Y-%m-%dT%H:%M:%S")) + float(mm.group(2) or 0)
        out.append((base, m.group(2), m.group(3), parse_go_duration(m.group(4))))
    return out


# --------------------------------------------------------------------------- setup / teardown

def host_facts(commit):
    def first(cmd):
        try:
            return sh(cmd, check=False).strip().splitlines()[0]
        except Exception as e:  # noqa: BLE001
            return "n/a (%s)" % e
    cpu = ""
    with open("/proc/cpuinfo") as f:
        for line in f:
            if line.startswith("model name"):
                cpu = line.split(":", 1)[1].strip()
                break
    gov = "n/a"
    with contextlib.suppress(OSError):
        with open("/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor") as f:
            gov = f.read().strip()
    drv = "n/a"
    with contextlib.suppress(OSError):
        with open("/sys/devices/system/cpu/cpu0/cpufreq/scaling_driver") as f:
            drv = f.read().strip()
    thp = {}
    for k in ("enabled", "shmem_enabled"):
        with contextlib.suppress(OSError):
            with open("/sys/kernel/mm/transparent_hugepage/" + k) as f:
                thp[k] = f.read().strip()
    info = {}
    with contextlib.suppress(Exception):
        j = json.loads(sh(["podman", "info", "--format", "json"]))
        info = {"graph_driver": j["store"]["graphDriverName"], "graph_root": j["store"]["graphRoot"],
                "cgroup_manager": j["host"]["cgroupManager"], "cgroup_version": j["host"]["cgroupVersion"],
                "cgroup_controllers": j["host"].get("cgroupControllers"), "conmon": j["host"]["conmon"]["version"],
                "oci_runtime": j["host"]["ociRuntime"]["version"].splitlines()[0]}
    mi = meminfo("MemTotal", "MemAvailable", "SwapTotal", "SwapFree")
    return {
        "commit": commit, "kernel": os.uname().release, "cpu_model": cpu, "cpu_threads": os.cpu_count(),
        "cpu_governor": gov, "cpu_scaling_driver": drv, "transparent_hugepage": thp, "meminfo_kb": mi,
        "podman": first(["podman", "--version"]), "crun": first(["crun", "--version"]),
        "runsc": sh([runsc_path(), "--version"], check=False).strip().splitlines(), "runsc_path": runsc_path(),
        "runsc_platform": "systrap (default; --platform not set)", "podman_info": info,
        "go": first([os.path.join(HOME, ".local/opt/go/bin/go"), "version"]) if os.path.exists(os.path.join(HOME, ".local/opt/go/bin/go")) else first(["go", "version"]),
        "python": sys.version.split()[0], "postgres_image": PG_IMAGE,
        "load_average_at_setup": loadavg(), "time": time.strftime("%Y-%m-%d %H:%M:%S %z"),
        "top_cpu_at_setup": sh(["sh", "-c", "ps -eo pcpu,pmem,comm --sort=-pcpu | head -9"], check=False).splitlines(),
    }


def build_snapshot():
    """Builds sbxd and sbx-agent once from `git archive HEAD`, never from the working tree."""
    binp = os.path.join(WORK, "bin", "sbxd")
    cfile = os.path.join(WORK, "COMMIT")
    if os.path.exists(binp) and os.path.exists(cfile):
        return open(cfile).read().strip()
    src = os.path.join(WORK, "src")
    os.makedirs(src, exist_ok=True)
    os.makedirs(os.path.join(WORK, "bin"), exist_ok=True)
    commit = sh(["git", "-C", REPO, "rev-parse", "HEAD"]).strip()
    subprocess.run("git -C %s archive HEAD | tar -x -C %s" % (shlex.quote(REPO), shlex.quote(src)), shell=True, check=True)
    env = dict(os.environ)
    env["PATH"] = os.path.join(HOME, ".local/opt/go/bin") + ":" + env["PATH"]
    sh(["go", "build", "-o", os.path.join(WORK, "bin") + "/", "./cmd/sbxd"], env=env, cwd=src, timeout=900)
    env["CGO_ENABLED"] = "0"
    sh(["go", "build", "-o", os.path.join(WORK, "bin") + "/", "./cmd/sbx-agent"], env=env, cwd=src, timeout=900)
    with open(cfile, "w") as f:
        f.write(commit + "\n")
    return commit


def main_templates():
    """Templates of the main server: (id stem, image, keyword arguments). One per tier is created."""
    wl = dict(cpus=1, memory="1g", pids=256, workdir="384m", tmp="128m", network="allowlist", warm=1)
    small = dict(cpus=1, memory="256m", pids=128, workdir="64m", tmp="32m")
    return [
        ("life", IMG_ALPINE, dict(small, warm=4)),
        ("cold", IMG_ALPINE, dict(small, warm=0)),
        ("thr", IMG_ALPINE, dict(small, warm=0)),
        ("dens", IMG_ALPINE, dict(small, warm=0)),
        ("wla", IMG_WL["alpine"], wl),
        ("wld", IMG_WL["slim"], wl),
        ("wla2", IMG_WL["alpine"], dict(wl, cpus=2, warm=0)),
        ("neta", IMG_NET["alpine"], wl),
        ("netd", IMG_NET["slim"], wl),
        ("lim1", IMG_ALPINE, dict(small, pids=64, warm=2)),
        ("lim05", IMG_ALPINE, dict(small, pids=64, cpus=0.5, warm=2)),
    ]


def cmd_setup(a):
    os.makedirs(WORK, exist_ok=True)
    if not XDG:
        sys.exit("XDG_RUNTIME_DIR is not set")
    commit = build_snapshot()
    log("binaries built from commit", commit)
    for img in (IMG_ALPINE, IMG_SLIM):
        if subprocess.run(["podman", "image", "exists", img]).returncode != 0:
            sh(["podman", "pull", "-q", img])
    for k, tag in IMG_WL.items():
        if subprocess.run(["podman", "image", "exists", tag]).returncode != 0:
            log("building", tag)
            sh(["podman", "build", "-q", "-t", tag, "-f", os.path.join(BENCH, "images", "Containerfile." + k),
                os.path.join(BENCH, "images")], timeout=900)
    if PG_NAME not in sh(["docker", "ps", "--format", "{{.Names}}"]).split():
        sh(["docker", "run", "-d", "--rm", "--name", PG_NAME, "-e", "POSTGRES_PASSWORD=sbx",
            "-p", "127.0.0.1:%d:5432" % PG_PORT, PG_IMAGE])
    for _ in range(120):
        if subprocess.run(["docker", "exec", PG_NAME, "psql", "-U", "postgres", "-h", "127.0.0.1", "-c", "select 1"],
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
            break
        time.sleep(0.5)
    else:
        sys.exit("postgres did not come up")
    run = a.run or time.strftime("%Y%m%d-%H%M%S")
    results = os.path.join(BENCH, "results", run)
    os.makedirs(os.path.join(results, "blocks"), exist_ok=True)
    st = {"run": run, "results": results, "commit": commit, "keys": {}, "servers": {}}
    facts = host_facts(commit)
    facts["postgres"] = sh(["docker", "exec", PG_NAME, "psql", "-U", "postgres", "-h", "127.0.0.1", "-Atc", "select version()"], check=False).strip()
    for name, (_, _, _, db) in SERVERS.items():
        sh(["docker", "exec", PG_NAME, "psql", "-U", "postgres", "-h", "127.0.0.1", "-c", "CREATE DATABASE " + db], check=False)
        admin(name, "tenant", "create", "--id", "bench", "--max-concurrent", "2000", "--rate", "0", "--burst", "1")
        st["keys"][name] = json.loads(admin(name, "key", "create", "--tenant", "bench", "--json"))["key"]
    for stem, image, kw in main_templates():
        for tier in TIERS:
            template_apply("main", stem + "-" + SUFFIX[tier], tier, image, **kw)
    facts["preflight"] = sh([os.path.join(WORK, "bin", "sbxd"), "preflight"], env=sbxd_env("sbx_main"), check=False).splitlines()
    write_json(os.path.join(results, "host.json"), facts, indent=1)
    save_state(st)
    pid, ready = server_start(st, "main")
    log("main server pid %d ready after %.0f ms; results in %s" % (pid, ready, results))
    api = api_for(st, "main")
    for stem, _, kw in main_templates():
        for tier in TIERS:
            api.wait_pool(stem + "-" + SUFFIX[tier], kw.get("warm", 0), timeout=300)
    log("all pools warm")


def cmd_start(a):
    st = load_state()
    pid, ready = server_start(st, a.server, runsc_flags=a.runsc_flags or "")
    log("%s pid %d ready after %.0f ms" % (a.server, pid, ready))


def cmd_stop(a):
    st = load_state()
    log("stopped %s in %s ms" % (a.server, server_stop(st, a.server)))


def cmd_teardown(a):
    st = load_state() if os.path.exists(STATE) else {"servers": {}}
    for name in SERVERS:
        with contextlib.suppress(Exception):
            server_stop(st, name)
        p = server_paths(name)
        if os.path.exists(p["run_dir"]):
            # Whatever a killed server left: its own host id only.
            out = sh([os.path.join(WORK, "bin", "sbxd"), "admin", "host", "destroy", "--run-dir", p["run_dir"],
                      "--host-id", p["host_id"]], env=sbxd_env(p["db"]), check=False)
            log("host destroy %s: %s" % (name, out.strip()[-300:]))
            shutil.rmtree(p["run_dir"], ignore_errors=True)
    sh(["docker", "rm", "-f", PG_NAME], check=False)
    if a.images:
        for tag in IMG_WL.values():
            sh(["podman", "rmi", tag], check=False)
    for name in SERVERS:
        left = sh(["podman", "ps", "-a", "--filter", "label=sbx.host_id=" + SERVERS[name][2], "--format", "{{.Names}}"], check=False).strip()
        log("containers left for %s: %r" % (SERVERS[name][2], left))
    if a.work:
        shutil.rmtree(WORK, ignore_errors=True)


# --------------------------------------------------------------------------- group: claim cycle

def order(rnd, i=0):
    """ABAB within a round, and the tier that goes first alternates between rounds and cells."""
    return TIERS if (rnd + i) % 2 == 1 else TIERS[::-1]


def wait_destroyed(api, sid, t0, timeout=30):
    """Polls until the sandbox is destroyed; returns ms since t0."""
    while True:
        s, d, _ = api.call("GET", "/v1/sandboxes/" + sid)
        if s == 200 and d.get("state") == "destroyed":
            return (now() - t0) * 1000
        if now() - t0 > timeout:
            raise RuntimeError("sandbox %s not destroyed after %ss: %s" % (sid, timeout, d))
        time.sleep(0.004)


def mdelta(m0, m1, prefix):
    """Change of a counter (summed over its label sets) between two metric snapshots."""
    return sum(v - m0.get(k, 0) for k, v in m1.items() if k.startswith(prefix))


def g_claim(st, a):
    """Warm claim, claim to first output byte, delete answered and delete finished.

    Two regimes, because a claim is two Postgres commits and a commit on this
    host is slower while Podman is writing: `quiet` claims 2 s after the pool
    last changed, `after_refill` claims 50 ms after the replacement of the
    previous sandbox turned warm."""
    api = api_for(st, "main")
    n = a.n or 15
    for rep, (regime, settle) in enumerate((("quiet", 2.0), ("after_refill", 0.05), ("quiet", 2.0), ("after_refill", 0.05))):
        for tier in order(a.round, rep // 2):
            tpl = "life-" + SUFFIX[tier]
            api.wait_pool(tpl, 4, settle=0.5)
            m0 = api.metrics()
            with Block(st, "lifecycle", "claim_cycle", a.round, tier=tier, image="alpine", regime=regime) as b:
                for i in range(n + 2):
                    api.wait_pool(tpl, 4)
                    time.sleep(settle)
                    _, floor = api.must(200, "GET", "/healthz")
                    t0 = now()
                    sb, claim_ms = api.claim(tpl)
                    r = api.stream(sb, {"cmd": "echo", "args": ["hello"]})
                    first = (now() - t0) * 1000 - (r["total_ms"] - r["first_out_ms"]) if r["first_out_ms"] else None
                    td = now()
                    s, _, del_ms = api.delete(sb["id"])
                    fin = wait_destroyed(api, sb["id"], td)
                    if i < 2:
                        continue  # warm-up
                    if s != 204 or r["first_out_ms"] is None:
                        b.err("sample %d: delete %s stream %s" % (i, s, r))
                        continue
                    b.add("http_floor_healthz_ms", floor)
                    b.add("claim_ms", claim_ms)
                    b.add("exec_first_byte_ms", r["first_out_ms"])
                    b.add("exec_echo_total_ms", r["total_ms"])
                    b.add("claim_to_first_byte_ms", first)
                    b.add("delete_answered_ms", del_ms)
                    b.add("delete_finished_ms", fin)
                m1 = api.metrics()
                b.extra["claims"] = mdelta(m0, m1, "sbx_pool_claims_total")
                b.extra["claims_waited"] = mdelta(m0, m1, "sbx_pool_claims_waited_total")


# --------------------------------------------------------------------------- group: cold create

def empty_pool(api, server, stem, tier, image, kw, timeout=300):
    """Sets warm-target 0 and bumps the template version, so every warm sandbox is recycled."""
    tid = stem + "-" + SUFFIX[tier]
    kw = dict(kw, warm=0, max_total=0)
    kw["nofile"] = 4097 if empty_pool.flip.get(tid) != 4097 else 4096
    empty_pool.flip[tid] = kw["nofile"]
    template_apply(server, tid, tier, image, **kw)
    t = now()
    while True:
        p = api.pool(tid)
        if sum(v for k, v in p.items() if k not in ("destroyed",)) == 0:
            return kw
        if now() - t > timeout:
            raise RuntimeError("pool %s not empty: %s" % (tid, p))
        time.sleep(0.1)


empty_pool.flip = {}


def fill_pool(api, server, stem, tier, image, kw, n, refill, timeout=600):
    """Raises warm-target to n and returns the create times (ms) from the server's debug log,
    plus the creates per second over the fill."""
    tid = stem + "-" + SUFFIX[tier]
    off = log_offset(server)
    template_apply(server, tid, tier, image, **dict(kw, warm=n, max_total=n, refill=refill))
    api.wait_pool(tid, n, timeout=timeout, exact=True)
    time.sleep(0.2)
    ev = [e for e in warm_events(log_since(server, off), tid)]
    took = [e[3] for e in ev]
    rate = None
    if len(ev) >= 2:
        span = max(e[0] for e in ev) - min(e[0] - e[3] / 1000 for e in ev)
        rate = len(ev) / span if span > 0 else None
    text = log_since(server, off)
    retries = len(re.findall(r'create attempt failed; retrying.*template=%s' % re.escape(tid), text))
    fails = len(re.findall(r'msg="sandbox create failed".*template=%s' % re.escape(tid), text))
    return took, rate, retries, fails


def g_create(st, a):
    """Cold create to WARM, one at a time and at refill concurrency 2 and 4."""
    api = api_for(st, "main")
    small = dict(cpus=1, memory="256m", pids=128, workdir="64m", tmp="32m")
    n = a.n or 12
    for ci, refill in enumerate((1, 2, 4)):
        for rep in range(2):
            for tier in order(a.round, ci + rep):
                kw = empty_pool(api, "main", "cold", tier, IMG_ALPINE, small)
                time.sleep(0.5)
                with Block(st, "lifecycle", "create_to_warm", a.round, tier=tier, image="alpine", refill=refill) as b:
                    took, rate, retries, fails = fill_pool(api, "main", "cold", tier, IMG_ALPINE, kw, n, refill)
                    b.addmany("create_to_warm_ms", took)
                    if rate:
                        b.add("creates_per_s", rate)
                    b.extra.update(retries=retries, failures=fails)
                    if len(took) != n:
                        b.err("expected %d warm events, saw %d" % (n, len(took)))
    for tier in TIERS:
        empty_pool(api, "main", "cold", tier, IMG_ALPINE, small)


# --------------------------------------------------------------------------- group: sustained claim rate

def g_rate(st, a):
    """Open-loop claims at a fixed arrival rate with immediate release."""
    api = api_for(st, "main")
    small = dict(cpus=1, memory="256m", pids=128, workdir="64m", tmp="32m")
    dur = a.seconds or 30
    rates = [float(x) for x in (a.rates or "1,2,3,5,10").split(",")]
    warm, refill = a.warm or 8, a.refill or 2
    for tier in TIERS:
        template_apply("main", "thr-" + SUFFIX[tier], tier, IMG_ALPINE, **dict(small, warm=warm, refill=refill))
    for ri, rate in enumerate(rates):
        for tier in order(a.round, ri):
            tpl = "thr-" + SUFFIX[tier]
            api.wait_pool(tpl, warm, timeout=300, settle=1.0)
            m0 = api.metrics()
            lat, codes, late = [], {}, []
            lock = threading.Lock()

            def one(due, tpl=tpl, lat=lat, codes=codes, late=late, lock=lock):
                start = now()
                s, d, ms = api.call("POST", "/v1/sandboxes", {"template_id": tpl})
                with lock:
                    codes[s] = codes.get(s, 0) + 1
                    late.append((start - due) * 1000)
                    if s == 201:
                        lat.append(ms)
                if s == 201:
                    api.delete(d["id"])

            total = int(rate * dur)
            with Block(st, "throughput", "claim_rate", a.round, tier=tier, image="alpine", rate=rate, warm=warm, refill=refill) as b:
                with ThreadPoolExecutor(max_workers=96) as ex:
                    t0 = now()
                    for k in range(total):
                        due = t0 + k / rate
                        d = due - now()
                        if d > 0:
                            time.sleep(d)
                        ex.submit(one, due)
                    sent_s = now() - t0
                wall = now() - t0
                m1 = api.metrics()

                claims, waited = mdelta(m0, m1, "sbx_pool_claims_total"), mdelta(m0, m1, "sbx_pool_claims_waited_total")
                ok = codes.get(201, 0)
                b.addmany("claim_ms", lat)
                b.extra.update(arrivals=total, ok=ok, codes={str(k): v for k, v in codes.items()}, wall_s=wall, sent_s=sent_s,
                               server_claims=claims, server_claims_waited=waited,
                               warm_fraction=(ok - waited) / total if total else None,
                               served_fraction=ok / total if total else None,
                               under_50ms_fraction=sum(1 for x in lat if x < 50) / total if total else None,
                               start_lateness_p95_ms=pct(late, 95), achieved_rate=ok / wall)
            try:
                api.wait_pool(tpl, warm, timeout=300)
            except RuntimeError as e:
                log("pool did not recover:", e)
    for tier in TIERS:
        empty_pool(api, "main", "thr", tier, IMG_ALPINE, small)


# --------------------------------------------------------------------------- group: agent workloads

G = "/workspace/.tb/g.py"
MODS = ("json,re,ssl,sqlite3,asyncio,http.client,decimal,datetime,subprocess,argparse,logging,unittest,"
        "email.parser,xml.etree.ElementTree,csv,hashlib,urllib.request,zipfile,tempfile,pathlib,"
        "multiprocessing,ctypes,uuid,random,statistics,gzip,shutil,socket,threading,dataclasses")
GIT_SCRIPT = ("set -e; d=/workspace/.tb/git$$; mkdir $d; cd $d; git init -q .; git config user.email b@example.com; "
              "git config user.name bench; git config gc.auto 0; i=0; while [ $i -lt 200 ]; do echo $i > f$((i % 20)).txt; "
              "git add -A; git commit -q -m c$i; i=$((i+1)); done; test $(git rev-list --count HEAD) = 200; cd /; rm -rf $d")

# name -> argv, samples per sandbox inside (driver) and through the API, options.
# "heavy" workloads take seconds per run under gVisor and get fewer samples; the tables say n.
WORKLOADS = [
    dict(name="py_start", argv=["python3", "-c", "pass"], n=10, api=10),
    dict(name="import30_cold", argv=["python3", "-c", "import " + MODS], n=10, api=3, uniq="PYTHONPYCACHEPREFIX"),
    dict(name="import30_cached", argv=["python3", "-c", "import " + MODS], n=10, api=10),
    dict(name="cpu_loop", argv=["python3", G, "cpu"], n=10, api=5),
    dict(name="numpy", argv=["python3", G, "numpy"], n=10, api=5),
    dict(name="json_re_sha", argv=["python3", G, "micro"], n=10, api=5),
    dict(name="files_ws_2000", argv=["python3", G, "files_ws"], n=10, api=5),
    dict(name="files_rootfs_1500", argv=["python3", G, "files_root"], n=10, api=5),
    dict(name="seq_100mb", argv=["python3", G, "seq100"], n=10, api=3),
    dict(name="fork_exec_500", argv=["sh", "-c", "i=0; while [ $i -lt 500 ]; do /bin/true; i=$((i+1)); done"], n=10, api=3),
    dict(name="subprocess_200", argv=["python3", G, "subproc"], n=10, api=3),
    dict(name="pipeline", argv=["sh", "-c", "seq 1 200000 | sort -rn | uniq | wc -l"], n=10, api=5),
    dict(name="git_200_commits", argv=["sh", "-c", GIT_SCRIPT], n=4, api=1),
    # The same with git's automatic maintenance off (git 2.54 in the Alpine image runs it after every commit).
    dict(name="git_200_noauto", argv=["sh", "-c", GIT_SCRIPT.replace("git config gc.auto 0;", "git config gc.auto 0; git config maintenance.auto false;")], n=4, api=0),
    dict(name="threads_io", argv=["python3", G, "threads"], n=10, api=5),
]


def guest_upload(api, sb):
    with open(os.path.join(BENCH, "tierbench_guest.py"), "rb") as f:
        api.put_file(sb, G, f.read())


def run_inner(api, sb, argv, n, uniq=None, env=None):
    """Runs argv n times inside the sandbox with the driver. Returns the driver's records."""
    spec = {"argv": argv, "n": n, "uniq_dir_env": uniq, "env": env or {}}
    api.put_file(sb, "/workspace/.tb/spec.json", json.dumps(spec).encode())
    r, ms = api.exec(sb, "python3", [G, "drv", "/workspace/.tb/spec.json"])
    if r["exit"]["exit_code"] != 0:
        raise RuntimeError("driver failed: %s %s" % (r["exit"], r["stderr"][-400:]))
    return json.loads(r["stdout"].strip().splitlines()[-1])


def add_core(b, core, prefix="core_"):
    for k, v in (core or {}).items():
        if isinstance(v, (int, float)) and k not in ("check", "files", "bytes", "procs", "status"):
            b.add(prefix + k + ("" if k.endswith(("_s", "_mb_s")) else "_ms"), v)


def measure_workload(api, sb, b, w, scale=1.0, api_mode=True):
    n = max(2, int(round(w["n"] * scale)))
    recs = run_inner(api, sb, w["argv"], n + 1, uniq=w.get("uniq"))
    for i, r in enumerate(recs):
        if r["rc"] != 0:
            b.err("inner rc=%s %s" % (r["rc"], r.get("err", "")))
            continue
        if i == 0:
            b.add("first_inner_ms", r["ms"])  # the first run of this workload in this sandbox
            add_core(b, r.get("core"), "first_core_")
            continue
        b.add("inner_ms", r["ms"])
        add_core(b, r.get("core"))
    if not api_mode:
        return
    for i in range(int(round(w["api"] * scale)) if w["api"] else 0):
        env = None
        if w.get("uniq"):
            env = {w["uniq"]: "/tmp/ua%d" % i}
        r, ms = api.exec(sb, w["argv"][0], w["argv"][1:], env=env)
        if w.get("uniq"):
            api.shell(sb, "rm -rf /tmp/ua%d" % i)
        if r["exit"]["exit_code"] != 0:
            b.err("api rc=%s %s" % (r["exit"], r["stderr"][-300:]))
            continue
        b.add("api_ms", ms)


def g_wl(st, a):
    """Agent workloads inside a claimed sandbox, timed inside (driver) and through the API."""
    api = api_for(st, "main")
    only = set(filter(None, a.only.split(",")))
    stem = {"alpine": "wla", "slim": "wld"}
    for image in a.images.split(","):
        sbs = {}
        for tier in TIERS:
            sbs[tier], _ = api.claim(stem[image] + "-" + SUFFIX[tier])
            guest_upload(api, sbs[tier])
        for tier in TIERS:
            api.wait_pool(stem[image] + "-" + SUFFIX[tier], 1, settle=1.0)
        try:
            for wi, w in enumerate(WORKLOADS):
                if only and w["name"] not in only:
                    continue
                for tier in order(a.round, wi):
                    with Block(st, "workload", w["name"], a.round, tier=tier, image=image) as b:
                        measure_workload(api, sbs[tier], b, w, scale=a.scale)
        finally:
            for sb in sbs.values():
                api.delete(sb["id"])
            time.sleep(1.5)


def g_wl2(st, a):
    """The CPU- and process-heavy workloads again with a 2-CPU template: how much of the
    gVisor cost on a 1-CPU template is the sentry sharing the sandbox's CPU quota."""
    api = api_for(st, "main")
    names = ("py_start", "import30_cached", "fork_exec_500", "pipeline", "threads_io", "files_ws_2000", "git_200_commits", "numpy")
    wl = dict(cpus=2, memory="1g", pids=256, workdir="384m", tmp="128m", network="allowlist", warm=1)
    for tier in TIERS:
        template_apply("main", "wla2-" + SUFFIX[tier], tier, IMG_WL["alpine"], **wl)
    sbs = {}
    for tier in TIERS:
        api.wait_pool("wla2-" + SUFFIX[tier], 1, timeout=120)
        sbs[tier], _ = api.claim("wla2-" + SUFFIX[tier])
        guest_upload(api, sbs[tier])
    for tier in TIERS:
        template_apply("main", "wla2-" + SUFFIX[tier], tier, IMG_WL["alpine"], **dict(wl, warm=0))
    time.sleep(2)
    try:
        for wi, w in enumerate([w for w in WORKLOADS if w["name"] in names]):
            for tier in order(a.round, wi):
                with Block(st, "workload_2cpu", w["name"], a.round, tier=tier, image="alpine", cpus=2) as b:
                    measure_workload(api, sbs[tier], b, w, scale=a.scale, api_mode=False)
    finally:
        for sb in sbs.values():
            api.delete(sb["id"])
        for tier in TIERS:
            empty_pool(api, "main", "wla2", tier, IMG_WL["alpine"], wl)


# --------------------------------------------------------------------------- group: stream throughput

STREAM_BYTES = 50 * 1024 * 1024
PTY_YES_BYTES = 256 * 1024       # `yes` through a PTY arrives in frames of a few dozen bytes: 50 MB would take half an hour under gVisor
PTY_LINE_BYTES = 8 * 1024 * 1024  # one 8 MiB line: the PTY path without the per-line cost
CMD_YES = "yes | head -c %d"
CMD_LINE = "head -c %d /dev/zero | tr '\\0' a"


def pty_run(api, sb, script, ack):
    """A command on a PTY, read through the WebSocket. Returns (seconds, bytes, dropped, frames, status frame)."""
    d, _ = api.must(201, "POST", "/v1/sandboxes/%s/commands" % sb["id"],
                    {"cmd": "sh", "args": ["-c", "read go; " + script], "pty": {"cols": 80, "rows": 24}}, sb["access_token"])
    ws = WS(api.port, "/v1/sandboxes/%s/commands/%s/ws?from=0" % (sb["id"], d["pid"]), sb["access_token"])
    ws.s.settimeout(300)
    got, dropped, frames, t0, exit_ = 0, 0, 0, None, None
    try:
        while True:
            op, p = ws.recv()
            if op == 8:
                break
            ch = p[0]
            if ch == 9:
                dropped += json.loads(p[1:]).get("dropped", 0)
                if t0 is None:
                    t0 = now()
                    ws.send(b"\x00\n")  # release the `read`
            elif ch in (1, 2):
                got += len(p) - 1
                frames += 1
                if ack:
                    ws.send(b"\x06" + json.dumps({"offset": got + dropped}).encode())
            elif ch == 3:
                exit_ = json.loads(p[1:])
                break
        el = now() - t0
    finally:
        ws.close()
    return el, got, dropped, frames, exit_


def g_stream(st, a):
    """Output throughput: PTY over the WebSocket (with and without ACK flow control) and NDJSON."""
    api = api_for(st, "main")
    n = a.n or 4
    sbs = {}
    for tier in TIERS:
        sbs[tier], _ = api.claim("wla-" + SUFFIX[tier])
    for tier in TIERS:
        api.wait_pool("wla-" + SUFFIX[tier], 1, settle=1.0)
    modes = (("pty_ws_yes_256k_ack", CMD_YES % PTY_YES_BYTES), ("pty_ws_yes_256k_noack", CMD_YES % PTY_YES_BYTES),
             ("pty_ws_one_line_8m_ack", CMD_LINE % PTY_LINE_BYTES), ("ndjson_pipe_yes_50m", CMD_YES % STREAM_BYTES),
             ("ndjson_pty_yes_256k", CMD_YES % PTY_YES_BYTES))
    try:
        for mi, (mode, script) in enumerate(modes):
            for tier in order(a.round, mi):
                sb = sbs[tier]
                with Block(st, "stream", mode, a.round, tier=tier, image="alpine") as b:
                    for i in range(n + 1):
                        if mode.startswith("pty_ws"):
                            el, got, dropped, frames, ex = pty_run(api, sb, script, ack=mode.endswith("_ack"))
                            ok = bool(ex) and ex.get("exit", {}).get("exit_code") == 0
                        else:
                            body = {"cmd": "sh", "args": ["-c", script]}
                            if "pty" in mode:
                                body["pty"] = {"cols": 80, "rows": 24}
                            r = api.stream(sb, body)
                            el, got, dropped, frames = r["total_ms"] / 1000, r["out_bytes"], r["dropped"], r["events"]
                            ok = bool(r["exit"]) and r["exit"]["exit_code"] == 0
                        if i == 0:
                            continue
                        if not ok:
                            b.err("run %d: not ok (got %d dropped %d)" % (i, got, dropped))
                            continue
                        b.add("seconds", el)
                        b.add("delivered_mb_s", got / 1e6 / el)
                        b.add("delivered_fraction", got / (got + dropped) if got + dropped else 0)
                        b.add("received_mb", got / 1e6)
                        b.add("bytes_per_frame", got / max(1, frames))
    finally:
        for sb in sbs.values():
            api.delete(sb["id"])
        time.sleep(1.5)


# --------------------------------------------------------------------------- group: memory and density

def cg_base():
    uid = os.getuid()
    return "/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/user.slice" % (uid, uid)


def cg_read(path):
    """memory.current, a few memory.stat fields, processes and threads of one scope; None if it is gone."""
    try:
        with open(path + "/memory.current") as f:
            cur = int(f.read())
        stat = {}
        with open(path + "/memory.stat") as f:
            for line in f:
                k, v = line.split()
                if k in ("anon", "file", "shmem", "kernel", "slab", "pagetables", "sock", "kernel_stack"):
                    stat[k] = int(v)
        procs, threads = [], 0
        for dp, _, _ in os.walk(path):  # crun keeps the container's processes in a child cgroup
            with open(dp + "/cgroup.procs") as f:
                procs += [int(x) for x in f.read().split()]
            with open(dp + "/cgroup.threads") as f:
                threads += len(f.read().split())
    except OSError:
        return None
    return {"current": cur, "stat": stat, "procs": procs, "threads": threads}


def proc_mem(pid):
    """(Pss kB, Rss kB) of a process, or None when it cannot be read."""
    try:
        pss = rss = 0
        with open("/proc/%d/smaps_rollup" % pid) as f:
            for line in f:
                if line.startswith("Pss:"):
                    pss = int(line.split()[1])
                elif line.startswith("Rss:"):
                    rss = int(line.split()[1])
        return pss, rss
    except OSError:
        return None


def service_procs(name):
    """{role: pid} of this server's own sbxd and Podman services, found by the run dir in their command line."""
    rd = server_paths(name)["run_dir"]
    out = {}
    for d in os.listdir("/proc"):
        if not d.isdigit():
            continue
        try:
            with open("/proc/%s/cmdline" % d, "rb") as f:
                cmd = f.read().replace(b"\0", b" ").decode(errors="replace")
        except OSError:
            continue
        if rd not in cmd:
            continue
        if "podman system service" in cmd or " system service " in cmd:
            out["podman_runsc" if "podman-runsc.sock" in cmd else "podman_crun"] = int(d)
        elif "sbxd serve" in cmd or cmd.split(" ")[0].endswith("sbxd") and " serve " in cmd:
            out["sbxd"] = int(d)
    return out


def density_snapshot(name, template, with_pss=True):
    base = cg_base()
    cons = [c for c in own_containers(name) if c[2] == template and c[3] == "running"]
    per, conmon, procs, threads, pss, rss, unreadable = [], [], 0, 0, 0, 0, 0
    stat_sum = {}
    cm_procs = 0
    for cid, sid, _, _ in cons:
        c = cg_read("%s/libpod-%s.scope" % (base, cid))
        m = cg_read("%s/libpod-conmon-%s.scope" % (base, cid))
        if not c:
            continue
        per.append((sid, c["current"]))
        procs += len(c["procs"])
        threads += c["threads"]
        for k, v in c["stat"].items():
            stat_sum[k] = stat_sum.get(k, 0) + v
        if m:
            conmon.append(m["current"])
            cm_procs += len(m["procs"])
        if with_pss:
            for pid in c["procs"] + (m["procs"] if m else []):
                pm = proc_mem(pid)
                if pm is None:
                    unreadable += 1
                else:
                    pss += pm[0]
                    rss += pm[1]
    svc = {}
    for role, pid in service_procs(name).items():
        pm = proc_mem(pid)
        if pm:
            svc[role] = {"pss_kb": pm[0], "rss_kb": pm[1]}
    mi = meminfo("MemAvailable", "MemFree", "Cached", "Shmem", "Slab", "AnonPages", "PageTables", "KernelStack")
    vals = [v for _, v in per]
    return {"n": len(per), "meminfo_kb": mi, "scope_bytes_sum": sum(vals), "conmon_bytes_sum": sum(conmon),
            "scope_bytes_mean": statistics.fmean(vals) if vals else 0, "scope_bytes_min": min(vals) if vals else 0,
            "scope_bytes_max": max(vals) if vals else 0, "stat_sum": stat_sum, "procs": procs, "threads": threads,
            "conmon_procs": cm_procs, "pss_kb_sum": pss, "rss_kb_sum": rss, "pss_unreadable": unreadable,
            "services": svc, "per": dict(per), "host_tasks": int(open("/proc/loadavg").read().split()[3].split("/")[1])}


def g_density(st, a):
    """Ramp of idle warm sandboxes per tier; then one Python command in 20 of them."""
    api = api_for(st, "main")
    small = dict(cpus=1, memory="256m", pids=128, workdir="64m", tmp="32m")
    steps = {"crun": [20, 40, 80, 160, 320], "runsc": [20, 40, 80, 160]}
    est_mb = {"crun": 4, "runsc": 24}
    for tier in order(a.round):
        tid = "dens-" + SUFFIX[tier]
        kw = empty_pool(api, "main", "dens", tier, IMG_ALPINE, small)
        time.sleep(4)
        with Block(st, "density", "ramp", a.round, tier=tier, image="alpine") as b:
            snaps = [density_snapshot("main", tid)]
            reached = 0
            for n in steps[tier]:
                need_kb = (n - reached) * est_mb[tier] * 1024
                if mem_avail_kb() - need_kb < MEM_FLOOR_KB:
                    b.extra["stopped_before"] = n
                    b.err("stopped before %d sandboxes: MemAvailable %d MB" % (n, mem_avail_kb() // 1024))
                    break
                took, rate, retries, fails = fill_pool(api, "main", "dens", tier, IMG_ALPINE, kw, n, 4)
                reached = n
                time.sleep(5)
                s = density_snapshot("main", tid)
                s["create_ms_p50"], s["creates_per_s"], s["retries"], s["failures"] = pct(took, 50), rate, retries, fails
                snaps.append(s)
                log("  %s n=%d avail %d MB scopes %.1f MB (%.2f MB each) conmon %.2f MB each, %d procs %d threads" % (
                    tier, n, s["meminfo_kb"]["MemAvailable"] // 1024, s["scope_bytes_sum"] / 1e6,
                    s["scope_bytes_mean"] / 1e6, s["conmon_bytes_sum"] / 1e6 / max(1, s["n"]), s["procs"], s["threads"]))
            for s in snaps:
                per = s.pop("per")
                s["per_sample"] = dict(list(per.items())[:3])
            b.extra["steps"] = snaps
            # First use: claim k sandboxes (max-total = n, so nothing is refilled) and run one command in each.
            k = min(20, reached)
            base = cg_base()
            cid = {sid: c for c, sid, t, _ in own_containers("main") if t == tid}
            before_avail = mem_avail_kb()
            sbs = [api.claim(tid)[0] for _ in range(k)]
            time.sleep(2)
            idle = {sb["id"]: cg_read("%s/libpod-%s.scope" % (base, cid[sb["id"]])) for sb in sbs}
            for sb in sbs:
                r, _ = api.exec(sb, "python3", ["-c", "import json,re,hashlib; print(42)"])
                if r["exit"]["exit_code"] != 0:
                    b.err("first-use command: %s" % r["stderr"][-200:])
            time.sleep(4)
            for sb in sbs:
                c0, c1 = idle[sb["id"]], cg_read("%s/libpod-%s.scope" % (base, cid[sb["id"]]))
                if c0 and c1:
                    b.add("idle_claimed_mb", c0["current"] / 1e6)
                    b.add("after_python_mb", c1["current"] / 1e6)
                    b.add("first_use_growth_mb", (c1["current"] - c0["current"]) / 1e6)
                    b.add("after_python_anon_mb", c1["stat"].get("anon", 0) / 1e6)
                    b.add("after_python_file_mb", c1["stat"].get("file", 0) / 1e6)
                    b.add("after_python_shmem_mb", c1["stat"].get("shmem", 0) / 1e6)
            b.extra["first_use_mem_avail_delta_mb_per_sandbox"] = (before_avail - mem_avail_kb()) / 1024 / max(1, k)
            t = now()
            for sb in sbs:
                api.delete(sb["id"])
            empty_pool(api, "main", "dens", tier, IMG_ALPINE, small)
            b.extra["teardown_s"] = now() - t
            b.extra["teardown_per_s"] = reached / (now() - t) if reached else None
            time.sleep(4)
            b.extra["mem_avail_after_teardown_kb"] = mem_avail_kb()
        time.sleep(3)


# --------------------------------------------------------------------------- group: network path

NET_ALLOW = ["pypi.org", "files.pythonhosted.org", "www.gstatic.com"]
SMALL_URL = "https://www.gstatic.com/generate_204"
PIP = ["python3", "-m", "pip", "install", "--disable-pip-version-check", "--no-warn-script-location", "--progress-bar", "off", "-q"]


def big_url():
    """A ~40 MB immutable file on files.pythonhosted.org: the scipy 1.14.1 cp312 manylinux x86_64 wheel."""
    with urllib.request.urlopen("https://pypi.org/pypi/scipy/1.14.1/json", timeout=30) as r:
        j = json.load(r)
    for f in j["urls"]:
        if "cp312-cp312-manylinux" in f["filename"] and "x86_64" in f["filename"]:
            return f["url"], f["size"]
    raise RuntimeError("scipy wheel not found")


def g_net(st, a):
    """pip installs and HTTPS through the egress proxy; the host without a proxy as the baseline."""
    api = api_for(st, "main")
    stem = {"alpine": "neta", "slim": "netd"}
    url, size = big_url()
    guest = os.path.join(BENCH, "tierbench_guest.py")
    policy = {"mode": "allowlist", "allow_domains": NET_ALLOW}

    def host_block():
        with Block(st, "network", "https", a.round, tier="host_no_proxy", image="host") as b:
            env = {k: v for k, v in os.environ.items() if "proxy" not in k.lower()}
            for mode in ("new", "keep"):
                j = json.loads(sh([sys.executable, "-I", guest, "https", SMALL_URL, "21", mode], env=env).strip().splitlines()[-1])
                b.addmany("get_%s_conn_ms" % mode, j["req"][1:])
            if a.download:
                for _ in range(a.download):
                    j = json.loads(sh([sys.executable, "-I", guest, "download", url], env=env).strip().splitlines()[-1])
                    b.add("download_mb_s", j["mb_s"])
                    b.add("download_s", j["total"] / 1000)
            b.extra.update(small_url=SMALL_URL, big_url=url, big_bytes=size)

    host_block()
    for image in a.images.split(","):
        for tier in order(a.round, 0 if image == "alpine" else 1):
            tpl = stem[image] + "-" + SUFFIX[tier]
            api.wait_pool(tpl, 1, settle=1.0)
            sb, _ = api.claim(tpl, network=policy)
            guest_upload(api, sb)
            api.wait_pool(tpl, 1, settle=1.0)
            try:
                with Block(st, "network", "pip", a.round, tier=tier, image=image) as b:
                    def pip(metric, args, sb=sb, b=b):
                        rec = run_inner(api, sb, PIP + args, 1)[0]
                        if rec["rc"] != 0:
                            b.err("%s: rc=%s %s" % (metric, rec["rc"], rec.get("err", "")))
                        else:
                            b.add(metric, rec["ms"] / 1000)
                    pip("requests_cold_s", ["--user", "requests"])
                    r, _ = api.exec(sb, "python3", ["-c", "import requests,sys; print(requests.__version__, requests.__file__)"])
                    b.extra["requests_import"] = (r["stdout"] + r["stderr"]).strip()[-200:]
                    pip("requests_noop_s", ["--user", "requests"])
                    pip("requests_repeat_cached_s", ["--target", "/workspace/t2", "requests"])
                    pip("numpy_cold_s", ["--user", "numpy"])
                    r, _ = api.exec(sb, "python3", ["-c", "import numpy; print(numpy.__version__, numpy.__file__)"])
                    b.extra["numpy_import"] = (r["stdout"] + r["stderr"]).strip()[-200:]
                    pip("numpy_repeat_cached_s", ["--target", "/workspace/t3", "numpy"])
                    r, _ = api.shell(sb, "df -k /workspace /tmp | tail -2; du -sk /workspace/.cache 2>/dev/null")
                    b.extra["disk_after"] = r["stdout"]
                    s, stj, _ = api.call("GET", "/v1/sandboxes/%s/stats" % sb["id"])
                    b.extra["stats_after"] = stj if s == 200 else str(stj)
                with Block(st, "network", "https", a.round, tier=tier, image=image) as b:
                    for mode in ("new", "keep"):
                        r, _ = api.exec(sb, "python3", [G, "https", SMALL_URL, "21", mode])
                        if r["exit"]["exit_code"] != 0:
                            b.err("https %s: %s" % (mode, r["stderr"][-300:]))
                            continue
                        b.addmany("get_%s_conn_ms" % mode, json.loads(r["stdout"].strip().splitlines()[-1])["req"][1:])
                    if a.download and image == "alpine":
                        for _ in range(a.download):
                            r, _ = api.exec(sb, "python3", [G, "download", url])
                            if r["exit"]["exit_code"] != 0:
                                b.err("download: %s" % r["stderr"][-300:])
                                continue
                            j = json.loads(r["stdout"].strip().splitlines()[-1])
                            b.add("download_mb_s", j["mb_s"])
                            b.add("download_s", j["total"] / 1000)
            finally:
                api.delete(sb["id"])
                time.sleep(1.5)
    host_block()


# --------------------------------------------------------------------------- group: limits

BOMB_MEM = "x=[]\nwhile True:\n    x.append(b'x'*(4<<20))"
BOMB_FORK = "b(){ b|b& }; b"


def g_limits(st, a):
    """Memory bomb to oom_killed, fork bomb containment, CPU throttling accuracy."""
    api = api_for(st, "main")
    n = a.n or 4
    # memory bomb
    for rep in range(2):
        for tier in order(a.round, rep):
            tpl = "lim1-" + SUFFIX[tier]
            with Block(st, "limits", "memory_bomb", a.round, tier=tier, image="alpine", memory="256m") as b:
                for i in range(n):
                    api.wait_pool(tpl, 2, settle=1.0)
                    sb, _ = api.claim(tpl)
                    t0 = now()
                    s, d, _ = api.call("POST", "/v1/sandboxes/%s/commands" % sb["id"], {"cmd": "python3", "args": ["-c", BOMB_MEM]}, sb["access_token"])
                    reason, state = None, None
                    while now() - t0 < 60:
                        _, g, _ = api.call("GET", "/v1/sandboxes/" + sb["id"])
                        state, reason = g.get("state"), g.get("reason")
                        if state != "running":
                            break
                        time.sleep(0.004)
                    el = (now() - t0) * 1000
                    b.add("bomb_to_reported_ms", el) if reason == "oom_killed" else b.err("state %s reason %s after %.0f ms (start %s)" % (state, reason, el, s))
                    b.extra.setdefault("reasons", []).append(reason)
                    api.delete(sb["id"])
    # fork bomb
    for tier in order(a.round):
        tpl = "lim1-" + SUFFIX[tier]
        with Block(st, "limits", "fork_bomb", a.round, tier=tier, image="alpine", pids=64) as b:
            for i in range(max(2, n // 2)):
                api.wait_pool(tpl, 2, settle=1.0)
                sb, _ = api.claim(tpl)
                r0, base_ms = api.exec(sb, "echo", ["ok"])
                b.add("echo_before_ms", base_ms)
                d, _ = api.must(201, "POST", "/v1/sandboxes/%s/commands" % sb["id"], {"cmd": "sh", "args": ["-c", BOMB_FORK]}, sb["access_token"])
                time.sleep(3)
                s, stj, _ = api.call("GET", "/v1/sandboxes/%s/stats" % sb["id"])
                if s == 200:
                    b.add("pids_current_during", stj["pids_current"])
                    b.add("pids_limit_hits", stj["pids_limit_hits"])
                ok = 0
                for _ in range(5):
                    s, e, ms = api.call("POST", "/v1/sandboxes/%s/commands?wait=true" % sb["id"], {"cmd": "echo", "args": ["ok"]}, sb["access_token"])
                    if s == 200 and e.get("exit", {}).get("exit_code") == 0:
                        ok += 1
                        b.add("echo_during_ok_ms", ms)
                    else:
                        b.extra.setdefault("echo_during_errors", []).append("%s %s" % (s, str(e)[:160]))
                b.add("echo_during_ok_of_5", ok)
                _, g, _ = api.call("GET", "/v1/sandboxes/" + sb["id"])
                b.extra.setdefault("state_during", []).append(g.get("state"))
                # Kill the bomb's process group and see how long until a command runs again.
                t0 = now()
                api.call("POST", "/v1/sandboxes/%s/commands/%s/signal" % (sb["id"], d["pid"]), {"signal": "SIGKILL"}, sb["access_token"])
                usable = None
                while now() - t0 < 20:
                    s, e, ms = api.call("POST", "/v1/sandboxes/%s/commands?wait=true" % sb["id"], {"cmd": "echo", "args": ["ok"]}, sb["access_token"])
                    if s == 200 and e.get("exit", {}).get("exit_code") == 0:
                        usable = (now() - t0) * 1000
                        break
                    time.sleep(0.05)
                if usable is None:
                    b.err("not usable 20 s after SIGKILL of the bomb's process group")
                else:
                    b.add("usable_after_kill_ms", usable)
                time.sleep(0.5)
                s, stj, _ = api.call("GET", "/v1/sandboxes/%s/stats" % sb["id"])
                if s == 200:
                    b.add("pids_current_after_kill", stj["pids_current"])
                td = now()
                api.delete(sb["id"])
                b.add("delete_finished_ms", wait_destroyed(api, sb["id"], td, timeout=60))
    # CPU throttling
    for ci, (stem, cpus) in enumerate((("lim1", 1.0), ("lim05", 0.5))):
        for tier in order(a.round, ci):
            tpl = stem + "-" + SUFFIX[tier]
            api.wait_pool(tpl, 2, settle=1.0)
            sb, _ = api.claim(tpl)
            guest_upload(api, sb)
            api.wait_pool(tpl, 2, settle=1.0)
            with Block(st, "limits", "cpu_throttle", a.round, tier=tier, image="alpine", cpus=cpus) as b:
                for i in range(max(2, n // 2) + 1):
                    _, s0, _ = api.call("GET", "/v1/sandboxes/%s/stats" % sb["id"])
                    t0 = now()
                    r, _ = api.exec(sb, "python3", [G, "burn", "5", "2"])
                    wall = now() - t0
                    _, s1, _ = api.call("GET", "/v1/sandboxes/%s/stats" % sb["id"])
                    if r["exit"]["exit_code"] != 0:
                        b.err("burn: %s" % r["stderr"][-200:])
                        continue
                    if i == 0:
                        continue
                    j = json.loads(r["stdout"].strip().splitlines()[-1])
                    cg = (s1["cpu_usage_usec"] - s0["cpu_usage_usec"]) / 1e6
                    b.add("cgroup_cpus_used", cg / wall)
                    b.add("cgroup_vs_limit", cg / wall / cpus)
                    b.add("app_cpus_seen", j["app_cpu_s"] / j["wall_s"])
                    b.add("app_vs_limit", j["app_cpu_s"] / j["wall_s"] / cpus)
                    b.add("throttled_fraction_of_wall", (s1["cpu_throttled_usec"] - s0["cpu_throttled_usec"]) / 1e6 / wall)
            api.delete(sb["id"])
            time.sleep(1)


# --------------------------------------------------------------------------- group: start-up paths (second server)

SU_SMALL = dict(cpus=1, memory="256m", pids=128, workdir="64m", tmp="32m")
KNOB_SHAPE = dict(cpus=1, memory="1g", pids=256, workdir="384m", tmp="128m", network="none")


def su_templates(active_tier, warm, stem="su", image=None, kw=None):
    for tier in TIERS:
        template_apply("su", stem + "-" + SUFFIX[tier], tier, image or IMG_ALPINE,
                       **dict(kw or SU_SMALL, warm=warm if tier == active_tier else 0, max_total=0, refill=2))


def su_destroy(st):
    """Stops the second server and removes everything of its host id."""
    server_stop(st, "su")
    p = server_paths("su")
    if os.path.exists(p["run_dir"]):
        t = now()
        sh([os.path.join(WORK, "bin", "sbxd"), "admin", "host", "destroy", "--run-dir", p["run_dir"], "--host-id", p["host_id"]],
           env=sbxd_env(p["db"]), check=False)
        return now() - t
    return 0.0


def g_startup(st, a):
    """First claims after a cold server start, and restart-to-ready with N warm sandboxes to re-adopt."""
    su_destroy(st)
    api = api_for(st, "su")
    warm = 4
    for tier in TIERS:  # the knob templates live in the same database: keep their pools empty here
        template_apply("su", "kn-" + SUFFIX[tier], tier, IMG_WL["alpine"], **dict(KNOB_SHAPE, warm=0))
    for rep in range(a.n or 2):
        for tier in order(a.round, rep):
            tpl = "su-" + SUFFIX[tier]
            su_templates(tier, warm)
            with Block(st, "startup", "cold_start", a.round, tier=tier, image="alpine", warm_target=warm) as b:
                off = log_offset("su")
                t0 = now()
                _, ready = server_start(st, "su", policy="destroy", claim_wait="30s")
                b.add("exec_to_ready_ms", ready)
                claims = []
                for i in range(6):
                    s, d, ms = api.call("POST", "/v1/sandboxes", {"template_id": tpl})
                    if s != 201:
                        b.err("claim %d after start: %s %s" % (i, s, str(d)[:200]))
                        break
                    claims.append(ms)
                    if i == 0:
                        b.add("exec_to_first_claim_served_ms", (now() - t0) * 1000)
                        r = api.stream(d, {"cmd": "echo", "args": ["hello"]})
                        b.add("exec_to_first_output_ms", (now() - t0) * 1000)
                    api.delete(d["id"])
                if claims:
                    b.add("first_claim_ms", claims[0])
                    b.addmany("claims_2_to_6_ms", claims[1:])
                api.wait_pool(tpl, warm, timeout=120)
                b.add("exec_to_pool_full_ms", (now() - t0) * 1000)
                ev = warm_events(log_since("su", off), tpl)
                if ev:
                    b.add("first_create_ms", ev[0][3])
                    b.addmany("later_creates_ms", [e[3] for e in ev[1:]])
                for i in range(5):
                    api.wait_pool(tpl, warm)
                    time.sleep(2)
                    d, ms = api.claim(tpl)
                    b.add("steady_claim_ms", ms)
                    api.delete(d["id"])
                api.wait_pool(tpl, warm)
                b.add("stop_destroy_ms", server_stop(st, "su"))
    for n in [int(x) for x in (a.adopt or "10,100").split(",")]:
        for tier in order(a.round, n):
            tpl = "su-" + SUFFIX[tier]
            if tier == "runsc" and mem_avail_kb() - n * 24 * 1024 < MEM_FLOOR_KB:
                log("skipping re-adopt runsc n=%d: MemAvailable %d MB" % (n, mem_avail_kb() // 1024))
                continue
            for t2 in TIERS:
                template_apply("su", "su-" + SUFFIX[t2], t2, IMG_ALPINE, **dict(SU_SMALL, warm=n if t2 == tier else 0, max_total=n, refill=4))
            with Block(st, "startup", "restart_readopt", a.round, tier=tier, image="alpine", warm=n) as b:
                t0 = now()
                server_start(st, "su", policy="drain")
                api.wait_pool(tpl, n, timeout=600, exact=True)
                b.add("cold_fill_s", now() - t0)
                time.sleep(2)
                for i in range(a.restarts or 3):
                    b.add("stop_drain_ms", server_stop(st, "su"))
                    left = len([c for c in own_containers("su") if c[3] == "running"])
                    if left != n:
                        b.err("after a drain stop %d containers run, want %d" % (left, n))
                    off = log_offset("su")
                    t0 = now()
                    _, ready = server_start(st, "su", policy="drain")
                    b.add("exec_to_ready_ms", ready)
                    api.wait_pool(tpl, n, timeout=600, exact=True)
                    b.add("exec_to_all_warm_ms", (now() - t0) * 1000)
                    d, ms = api.claim(tpl)
                    b.add("first_claim_after_restart_ms", ms)
                    b.add("exec_to_first_claim_served_ms", (now() - t0) * 1000)
                    api.delete(d["id"])
                    api.wait_pool(tpl, n, timeout=120, exact=True)
                    text = log_since("su", off)
                    b.add("readopted", text.count("re-adopted warm sandbox"))
                    b.add("created_after_restart", len(warm_events(text, tpl)))
                    time.sleep(1)
                server_stop(st, "su")
                b.add("host_destroy_s", su_destroy(st))
                b.add("host_destroy_per_s", n / b.samples["host_destroy_s"][-1])
            time.sleep(3)


# --------------------------------------------------------------------------- group: gVisor knobs (second server)

KNOBS = {
    "default": "",
    "directfs_off": "--directfs=false",
    "overlay2_none": "--overlay2=none",
    "overlay2_root_memory": "--overlay2=root:memory",
}
KNOB_WORKLOADS = ("py_start", "import30_cold", "import30_cached", "files_ws_2000", "files_rootfs_1500", "fork_exec_500")


def g_knobs(st, a):
    """runsc flag variants on the file-heavy and start-up workloads (runsc only)."""
    su_destroy(st)
    api = api_for(st, "su")
    wl = KNOB_SHAPE
    su_templates(None, 0)
    names = list(KNOBS)
    only = set(filter(None, a.only.split(",")))
    if a.round % 2 == 0:
        names.reverse()
    for knob in names:
        if only and knob not in only:
            continue
        flags = KNOBS[knob]
        for tier in TIERS:
            template_apply("su", "kn-" + SUFFIX[tier], tier, IMG_WL["alpine"], **dict(wl, warm=6 if tier == "runsc" else 0, max_total=7, refill=1))
        off = log_offset("su")
        with Block(st, "knobs", "runsc_flags", a.round, tier="runsc", image="alpine", knob=knob) as b:
            b.extra["flags"] = flags
            try:
                _, ready = server_start(st, "su", runsc_flags=flags, policy="destroy")
                b.add("exec_to_ready_ms", ready)
                api.wait_pool("kn-g", 6, timeout=180, exact=True)
            except Exception as e:  # noqa: BLE001
                b.err("server with %r: %s; log: %s" % (flags, e, log_since("su", off)[-600:]))
                su_destroy(st)
                continue
            ev = warm_events(log_since("su", off), "kn-g")
            b.addmany("create_to_warm_ms", [e[3] for e in ev])
            time.sleep(4)
            snap = density_snapshot("su", "kn-g")
            b.add("idle_scope_mb", snap["scope_bytes_mean"] / 1e6)
            b.extra["idle"] = {k: snap[k] for k in ("n", "procs", "threads", "stat_sum")}
            # Proof that the flag reached runsc: the command line of one sandbox process.
            cons = [c for c in own_containers("su") if c[2] == "kn-g"]
            if cons:
                c = cg_read("%s/libpod-%s.scope" % (cg_base(), cons[0][0]))
                if c and c["procs"]:
                    with contextlib.suppress(OSError):
                        with open("/proc/%d/cmdline" % c["procs"][0], "rb") as f:
                            b.extra["sandbox_cmdline"] = f.read().replace(b"\0", b" ").decode(errors="replace")[:700]
            sb, _ = api.claim("kn-g")
            guest_upload(api, sb)
            api.wait_pool("kn-g", 6, timeout=60)
            time.sleep(1)
            for w in WORKLOADS:
                if w["name"] not in KNOB_WORKLOADS:
                    continue
                try:
                    recs = run_inner(api, sb, w["argv"], max(2, int(round(w["n"] * a.scale))) + 1, uniq=w.get("uniq"))
                except Exception as e:  # noqa: BLE001
                    b.err("%s: %s" % (w["name"], e))
                    continue
                for i, r in enumerate(recs):
                    if r["rc"] != 0:
                        b.err("%s rc=%s %s" % (w["name"], r["rc"], r.get("err", "")))
                    elif i == 0:
                        b.add(w["name"] + "_first_ms", r["ms"])
                    else:
                        b.add(w["name"] + "_ms", r["ms"])
                        if w["name"] in ("files_ws_2000", "files_rootfs_1500"):
                            add_core(b, r.get("core"), w["name"] + "_core_")
            api.delete(sb["id"])
            b.add("stop_destroy_ms", server_stop(st, "su"))
        su_destroy(st)
        time.sleep(2)


GROUPS = {"claim": g_claim, "create": g_create, "rate": g_rate, "wl": g_wl, "wl2": g_wl2, "stream": g_stream,
          "density": g_density, "net": g_net, "limits": g_limits, "startup": g_startup, "knobs": g_knobs}


# --------------------------------------------------------------------------- report

def fmt(v):
    if v is None:
        return ""
    return "%.0f" % v if abs(v) >= 100 else "%.1f" % v if abs(v) >= 10 else "%.2f" % v


def cmd_report(a):
    """blocks/*.json -> results.json, summary.csv, summary.md. Round 0 (pilot) blocks are left out."""
    results = os.path.join(BENCH, "results", a.run) if a.run else load_state()["results"]
    rows, errors, flagged, nblocks = {}, {}, {}, 0
    for path in sorted(glob.glob(os.path.join(results, "blocks", "*.json"))):
        with open(path) as f:
            d = json.load(f)
        if d["round"] < 1:
            continue
        nblocks += 1
        cell = dict(d["cell"])
        tier = cell.pop("tier")
        cellname = " ".join("%s=%s" % kv for kv in cell.items())
        base = (d["group"], d["name"], cellname)
        samples = dict(d["samples"])
        # Scalar results a block keeps under `extra` (served fraction, teardown rate, ...) are one sample each.
        for k, v in d["extra"].items():
            if isinstance(v, (int, float)) and not isinstance(v, bool):
                samples.setdefault(k, []).append(v)
        for metric, vals in samples.items():
            vals = [x for x in vals if isinstance(x, (int, float))]
            if vals:
                rows.setdefault(base + (metric,), {}).setdefault(tier, {}).setdefault(d["round"], []).extend(vals)
        if d["errors"]:
            errors.setdefault(base + (tier,), []).extend("r%s: %s" % (d["round"], e) for e in d["errors"])
        if d["high_load"]:
            flagged[base + (tier,)] = flagged.get(base + (tier,), 0) + 1
    out = []
    for (group, name, cellname, metric), tiers in rows.items():
        rec = {"group": group, "name": name, "cell": cellname, "metric": metric}
        for tier, rounds in tiers.items():
            pooled = [x for r in sorted(rounds) for x in rounds[r]]
            s = stats(pooled)
            s["rounds"] = len(rounds)
            s["round_p50"] = {str(r): pct(rounds[r], 50) for r in sorted(rounds)}
            s["samples"] = {str(r): rounds[r] for r in sorted(rounds)}
            rec[tier] = s
        if "crun" in rec and "runsc" in rec and rec["crun"]["p50"]:
            rec["runsc_over_crun_p50"] = rec["runsc"]["p50"] / rec["crun"]["p50"]
        out.append(rec)
    write_json(os.path.join(results, "results.json"),
               {"run": os.path.basename(results), "blocks": nblocks, "rows": out,
                "errors": {" | ".join(k): v for k, v in errors.items()},
                "high_load_blocks": {" | ".join(k): v for k, v in flagged.items()}})
    cols = ("n", "rounds", "p50", "p95", "min", "max")
    with open(os.path.join(results, "summary.csv"), "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["group", "name", "cell", "metric"] + ["%s_%s" % (t, c) for t in TIERS for c in cols]
                   + ["crun_round_p50", "runsc_round_p50", "runsc_over_crun_p50"])
        for r in out:
            line = [r["group"], r["name"], r["cell"], r["metric"]]
            for t in TIERS:
                line += [r[t][c] if t in r else "" for c in cols]
            line += [";".join(fmt(v) for v in r[t]["round_p50"].values()) if t in r else "" for t in TIERS]
            w.writerow(line + [r.get("runsc_over_crun_p50", "")])
    md = ["# tierbench summary: run %s" % os.path.basename(results), "",
          "%d blocks. Each cell is p50 (p95) of the samples pooled over the rounds; `rounds` is the per-round p50, "
          "in round order; ratio is runsc p50 over crun p50. Rows with cell `image=host` are the host itself with no sandbox and "
          "no proxy, shown in the runsc column. Raw samples are in `results.json`." % nblocks, ""]
    last = None
    for r in out:
        if r["group"] != last:
            last = r["group"]
            md += ["", "## %s" % last, "", "| block | cell | metric | crun | runsc | ratio | n | crun rounds | runsc rounds |",
                   "|---|---|---|---|---|---|---|---|---|"]
        c, g = r.get("crun"), r.get("runsc") or r.get("host_no_proxy")  # the host baseline has no tier; it sits in the runsc column
        one = lambda s: "%s (%s)" % (fmt(s["p50"]), fmt(s["p95"])) if s else ""  # noqa: E731
        rp = lambda s: " / ".join(fmt(v) for v in s["round_p50"].values()) if s else ""  # noqa: E731
        md.append("| %s | %s | %s | %s | %s | %s | %s | %s | %s |" % (
            r["name"], r["cell"], r["metric"], one(c), one(g),
            "%.2fx" % r["runsc_over_crun_p50"] if "runsc_over_crun_p50" in r else "",
            "/".join(str(s["n"]) for s in (c, g) if s), rp(c), rp(g)))
    if errors:
        md += ["", "## Errors", ""] + ["- `%s`: %d (%s)" % (" | ".join(k), len(v), v[0][:160].replace("\n", " ")) for k, v in errors.items()]
    if flagged:
        md += ["", "## Blocks started under high load (load1 >= %.1f)" % HIGH_LOAD, ""] + ["- `%s`: %d" % (" | ".join(k), v) for k, v in flagged.items()]
    with open(os.path.join(results, "summary.md"), "w") as f:
        f.write(redact("\n".join(md) + "\n"))
    log("report: %d blocks, %d rows, %d cells with errors, %d flagged; in %s" % (nblocks, len(out), len(errors), len(flagged), results))
    bad = check_redacted()
    if bad:
        log("NOT REDACTED:", bad)


def cmd_run(a):
    st = load_state()
    for g in a.group.split(","):
        log("=== group %s round %s" % (g, a.round))
        GROUPS[g](st, a)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("setup", help="build from git HEAD, start Postgres and the main server, create templates")
    p.add_argument("--run", help="results directory name (default: timestamp)")
    p.set_defaults(fn=cmd_setup)
    p = sub.add_parser("run", help="run measurement groups against the running server")
    p.add_argument("group", help="comma-separated: " + ",".join(GROUPS))
    p.add_argument("--round", type=int, default=1)
    p.add_argument("--n", type=int, default=0, help="samples per block (default: per group)")
    p.add_argument("--seconds", type=int, default=0)
    p.add_argument("--rates", default="")
    p.add_argument("--warm", type=int, default=0)
    p.add_argument("--refill", type=int, default=0)
    p.add_argument("--only", default="", help="restrict to these workloads / cells (comma-separated)")
    p.add_argument("--images", default="alpine,slim")
    p.add_argument("--scale", type=float, default=1.0, help="multiplies the per-workload sample counts")
    p.add_argument("--download", type=int, default=2, help="downloads of the ~40 MB file per network block (0 disables)")
    p.add_argument("--adopt", default="", help="re-adopt sizes for the startup group (default 10,100)")
    p.add_argument("--restarts", type=int, default=0, help="restarts per re-adopt block (default 3)")
    p.set_defaults(fn=cmd_run)
    p = sub.add_parser("report", help="aggregate the blocks of a run into results.json, summary.csv and summary.md")
    p.add_argument("--run", help="results directory name (default: the current run)")
    p.set_defaults(fn=cmd_report)
    p = sub.add_parser("start")
    p.add_argument("server", choices=list(SERVERS))
    p.add_argument("--runsc-flags", default="")
    p.set_defaults(fn=cmd_start)
    p = sub.add_parser("stop")
    p.add_argument("server", choices=list(SERVERS))
    p.set_defaults(fn=cmd_stop)
    p = sub.add_parser("teardown", help="stop servers (destroy policy), remove Postgres; --images and --work remove the rest")
    p.add_argument("--images", action="store_true")
    p.add_argument("--work", action="store_true")
    p.set_defaults(fn=cmd_teardown)
    a = ap.parse_args()
    a.fn(a)


if __name__ == "__main__":
    main()
