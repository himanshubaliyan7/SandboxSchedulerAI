#!/usr/bin/env python3
"""The part of tierbench that runs inside a sandbox (and, for the network
baseline, on the host). Uploaded to /workspace/.tb/g.py by bench/tierbench.py.

    g.py drv SPEC.json      run a command N times, time each spawn-to-exit
    g.py <workload> [...]   run one workload; the last stdout line is JSON with
                            the workload's own timings in milliseconds

Standard library only (numpy only in the `numpy` workload).
"""
import json
import os
import shutil
import subprocess
import sys
import time

pc = time.perf_counter


def ms(t):
    return (pc() - t) * 1000.0


def drv(spec_path):
    """Times `argv` n times from inside the sandbox: fork+exec to exit, as a
    process in the sandbox sees it. No API, no agent."""
    spec = json.load(open(spec_path))
    out = []
    for i in range(spec["n"]):
        env = dict(os.environ)
        env.update(spec.get("env") or {})
        uniq = spec.get("uniq_dir_env")
        d = None
        if uniq:
            d = "/tmp/u%d_%d" % (os.getpid(), i)
            env[uniq] = d
        t = pc()
        p = subprocess.run(spec["argv"], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=spec.get("cwd"))
        rec = {"ms": ms(t), "rc": p.returncode}
        try:
            core = json.loads(p.stdout.decode(errors="replace").strip().splitlines()[-1])
            if isinstance(core, dict):
                rec["core"] = core
        except Exception:  # noqa: BLE001
            pass
        if p.returncode != 0:
            rec["err"] = (p.stderr.decode(errors="replace")[-300:] + "|" + p.stdout.decode(errors="replace")[-200:])
        out.append(rec)
        if d:
            shutil.rmtree(d, ignore_errors=True)
    print(json.dumps(out))


def w_cpu():
    t = pc()
    s = 0
    for i in range(2_000_000):
        s += i * i % 7
    return {"loop": ms(t)}


def w_numpy():
    t = pc()
    import numpy as np
    imp = ms(t)
    rng = np.random.default_rng(0)
    a = rng.random((400, 400))
    b = rng.random((400, 400))
    t = pc()
    for _ in range(20):
        c = a @ b
    mat = ms(t)
    t = pc()
    for _ in range(3):
        np.linalg.svd(a[:200, :200])
    svd = ms(t)
    return {"import": imp, "matmul": mat, "svd": svd, "check": float(c[0, 0])}


def w_micro():
    import hashlib
    import re
    doc = {"items": [{"id": i, "name": "item-%d" % i, "tags": ["a", "b", "c"], "v": i * 1.5, "nested": {"k": [i, i + 1]}}
                     for i in range(2000)]}
    t = pc()
    for _ in range(10):
        s = json.dumps(doc)
        json.loads(s)
    tj = ms(t)
    text = ("The quick brown fox jumps over the lazy dog 12345 user@example.com 2026-10-11 10.0.0.1\n" * 12000)
    t = pc()
    n = len(re.findall(r"\b\w+@\w+\.\w+\b", text)) + len(re.findall(r"\d{4}-\d{2}-\d{2}", text)) + len(re.findall(r"\b[a-z]{5}\b", text))
    tr = ms(t)
    blob = b"\xa5" * (1 << 20)
    t = pc()
    h = hashlib.sha256()
    for _ in range(64):
        h.update(blob)
    h.hexdigest()
    th = ms(t)
    return {"json": tj, "re": tr, "sha256_64MiB": th, "check": n}


def w_files_ws(n=2000):
    d = "/workspace/.tb/f%d" % os.getpid()
    os.makedirs(d)
    data = b"x" * 1024
    names = [os.path.join(d, "f%04d" % i) for i in range(n)]
    t = pc()
    for p in names:
        with open(p, "wb") as f:
            f.write(data)
    tw = ms(t)
    t = pc()
    for p in names:
        os.stat(p)
    ts = ms(t)
    t = pc()
    total = 0
    for p in names:
        with open(p, "rb") as f:
            total += len(f.read())
    tr = ms(t)
    t = pc()
    for p in names:
        os.unlink(p)
    os.rmdir(d)
    td = ms(t)
    assert total == n * 1024
    return {"write": tw, "stat": ts, "read": tr, "delete": td, "all": tw + ts + tr + td}


def w_files_root(n=1500):
    root = os.path.join(sys.prefix, "lib", "python%d.%d" % sys.version_info[:2])
    names = []
    for dp, dn, fn in os.walk(root):
        dn.sort()
        if "site-packages" in dn:
            dn.remove("site-packages")
        for f in sorted(fn):
            if f.endswith(".py"):
                names.append(os.path.join(dp, f))
                if len(names) >= n:
                    break
        if len(names) >= n:
            break
    t = pc()
    total = 0
    for p in names:
        with open(p, "rb") as f:
            total += len(f.read())
    return {"read": ms(t), "files": len(names), "bytes": total}


def w_seq100(mb=100):
    p = "/workspace/.tb/big%d.bin" % os.getpid()
    buf = os.urandom(1 << 20)
    t = pc()
    with open(p, "wb", buffering=0) as f:
        for _ in range(mb):
            f.write(buf)
    tw = ms(t)
    t = pc()
    total = 0
    with open(p, "rb", buffering=0) as f:
        while True:
            b = f.read(1 << 20)
            if not b:
                break
            total += len(b)
    tr = ms(t)
    os.unlink(p)
    assert total == mb << 20
    return {"write": tw, "read": tr, "write_mb_s": mb / (tw / 1000), "read_mb_s": mb / (tr / 1000)}


def w_subproc(n=200):
    t = pc()
    for _ in range(n):
        subprocess.run(["/bin/true"], check=True)
    return {"run": ms(t)}


def w_threads(tasks=4000, workers=16):
    from concurrent.futures import ThreadPoolExecutor
    d = "/workspace/.tb/t%d" % os.getpid()
    os.makedirs(d)
    data = b"y" * 1024

    def job(i):
        p = os.path.join(d, "t%05d" % i)
        with open(p, "wb") as f:
            f.write(data)
        with open(p, "rb") as f:
            n = len(f.read())
        os.unlink(p)
        return n

    t = pc()
    with ThreadPoolExecutor(max_workers=workers) as ex:
        total = sum(ex.map(job, range(tasks)))
    el = ms(t)
    os.rmdir(d)
    assert total == tasks * 1024
    return {"pool": el}


def w_burn(seconds="5", procs="2"):
    """Busy loops in `procs` processes for `seconds` of wall time; reports the CPU time they got."""
    seconds, procs = float(seconds), int(procs)
    code = "import time\nend=time.time()+%r\nwhile time.time()<end:\n    for _ in range(20000): pass\nprint(time.process_time())" % seconds
    t = pc()
    ps = [subprocess.Popen([sys.executable, "-c", code], stdout=subprocess.PIPE) for _ in range(procs)]
    cpu = sum(float(p.communicate()[0]) for p in ps)
    wall = pc() - t
    return {"wall_s": wall, "app_cpu_s": cpu, "procs": procs}


def w_https(url, n="20", mode="new"):
    """GETs through the environment's proxy. mode new: one TLS connection per request;
    keep: one connection, n requests."""
    import http.client
    import ssl
    import urllib.parse
    import urllib.request
    n = int(n)
    out = []
    if mode == "new":
        for _ in range(n):
            t = pc()
            with urllib.request.urlopen(url, timeout=30) as r:
                r.read()
                st = r.status
            out.append(ms(t))
    else:
        u = urllib.parse.urlparse(url)
        proxy = os.environ.get("HTTPS_PROXY") or os.environ.get("https_proxy")
        t = pc()
        if proxy:
            pu = urllib.parse.urlparse(proxy)
            c = http.client.HTTPSConnection(pu.hostname, pu.port, timeout=30, context=ssl.create_default_context())
            c.set_tunnel(u.hostname, 443)
        else:
            c = http.client.HTTPSConnection(u.hostname, 443, timeout=30)
        c.connect()
        connect = ms(t)
        for _ in range(n):
            t = pc()
            c.request("GET", u.path or "/")
            r = c.getresponse()
            r.read()
            st = r.status
            out.append(ms(t))
        c.close()
        return {"req": out, "connect": connect, "status": st}
    return {"req": out, "status": st}


def w_download(url):
    import urllib.request
    t = pc()
    total = 0
    first = None
    with urllib.request.urlopen(url, timeout=120) as r:
        while True:
            b = r.read(1 << 16)
            if not b:
                break
            if first is None:
                first = ms(t)
            total += len(b)
    el = ms(t)
    return {"total": el, "first_byte": first, "bytes": total, "mb_s": total / 1e6 / (el / 1000)}


WORKLOADS = {"cpu": w_cpu, "numpy": w_numpy, "micro": w_micro, "files_ws": w_files_ws, "files_root": w_files_root,
             "seq100": w_seq100, "subproc": w_subproc, "threads": w_threads, "burn": w_burn, "https": w_https,
             "download": w_download}

if __name__ == "__main__":
    if sys.argv[1] == "drv":
        drv(sys.argv[2])
    else:
        print(json.dumps(WORKLOADS[sys.argv[1]](*sys.argv[2:])))
