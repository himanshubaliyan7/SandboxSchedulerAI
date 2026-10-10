"""An in-memory imitation of sbxd for the unit tests.

``FakeServer.transport()`` is an ``httpx.MockTransport`` (usable by both the
blocking and the asyncio client) that implements the routes, credentials,
error envelope and NDJSON stream of ``api/openapi.yaml``. ``FakeWSServer``
is a real WebSocket server on a loopback port that speaks the channel
framing of the PTY route.

Both record what they receive and can be told to misbehave: cut a stream
after N lines, answer an error for the next matching request, split the
NDJSON body at arbitrary byte boundaries, close a WebSocket mid-output.
"""

from __future__ import annotations

import base64
import json
import re
import threading
import time
from collections.abc import AsyncIterator, Callable, Iterator
from dataclasses import dataclass, field
from typing import Any

import httpx
from websockets.datastructures import Headers
from websockets.exceptions import ConnectionClosed
from websockets.http11 import Request, Response
from websockets.sync.server import Server, ServerConnection, serve

API_KEY = "sbx_test_0123456789abcdef"
OTHER_KEY = "sbx_other_fedcba9876543210"
NOW = "2026-10-10T08:00:00.123456789Z"


def envelope(code: str, message: str = "", **details: Any) -> dict[str, Any]:
    err: dict[str, Any] = {"code": code, "message": message or code, "request_id": "req-test"}
    if details:
        err["details"] = details
    return {"error": err}


def error(
    status: int, code: str, message: str = "", *, retry_after: int | None = None, **details: Any
) -> httpx.Response:
    headers = {"X-Request-Id": "req-test"}
    if retry_after is not None:
        headers["Retry-After"] = str(retry_after)
    return httpx.Response(status, json=envelope(code, message, **details), headers=headers)


class ChunkStream(httpx.SyncByteStream, httpx.AsyncByteStream):
    """A response body made of explicit chunks, optionally ending in a broken connection."""

    def __init__(self, chunks: list[bytes], fail: bool = False) -> None:
        self.chunks = chunks
        self.fail = fail
        self.closed = False

    def __iter__(self) -> Iterator[bytes]:
        yield from self.chunks
        if self.fail:
            raise httpx.ReadError("connection reset by test")

    async def __aiter__(self) -> AsyncIterator[bytes]:
        for chunk in self.chunks:
            yield chunk
        if self.fail:
            raise httpx.ReadError("connection reset by test")

    def close(self) -> None:
        self.closed = True

    async def aclose(self) -> None:
        self.closed = True


def split_every(n: int) -> Callable[[bytes], list[bytes]]:
    """A chunker that cuts the body every ``n`` bytes, wherever that falls."""

    def chunker(body: bytes) -> list[bytes]:
        return [body[i : i + n] for i in range(0, len(body), n)] or [b""]

    return chunker


@dataclass
class FakeCommand:
    pid: str
    cmd: str
    args: list[str]
    body: dict[str, Any]
    #: Output so far: (kind, bytes) with kind stdout, stderr or pty.
    output: list[tuple[str, bytes]] = field(default_factory=lambda: list[tuple[str, bytes]]())
    exit: dict[str, Any] | None = None
    #: Offset of the oldest byte still replayable (simulates the ring buffer).
    ring_start: int = 0
    stdin: bytearray = field(default_factory=bytearray)
    stdin_closed: bool = False
    signals: list[str] = field(default_factory=lambda: list[str]())
    sizes: list[tuple[int, int]] = field(default_factory=lambda: list[tuple[int, int]]())

    @property
    def total(self) -> int:
        return sum(len(d) for _, d in self.output)

    def view(self) -> dict[str, Any]:
        v: dict[str, Any] = {
            "pid": self.pid,
            "cmd": self.cmd,
            "pty": "pty" in self.body,
            "started_at": NOW,
            "next_offset": self.total,
            "some_future_field": {"ignored": True},
        }
        if self.args:
            v["args"] = self.args
        if self.body.get("tag"):
            v["tag"] = self.body["tag"]
        if self.exit is not None:
            v["exit"] = self.exit
        return v

    def events(self, start: int) -> list[dict[str, Any]]:
        """The stream events from offset ``start``, like the agent produces them."""
        out: list[dict[str, Any]] = []
        if start < self.ring_start:
            out.append({"type": "truncated", "offset": self.ring_start, "dropped": self.ring_start - start})
            start = self.ring_start
        offset = 0
        for kind, data in self.output:
            end = offset + len(data)
            if end > start:
                piece = data[max(start - offset, 0) :]
                out.append(
                    {"type": kind, "offset": max(offset, start), "data": base64.b64encode(piece).decode()}
                )
            offset = end
        if self.exit is not None:
            out.append({"type": "exit", "offset": offset, "exit": self.exit})
        return out


@dataclass
class FakeSandbox:
    id: str
    tenant: str
    token: str
    template_id: str
    body: dict[str, Any]
    state: str = "running"
    reason: str = ""
    expires_at: str = "2026-10-10T09:00:00Z"
    network: dict[str, Any] | None = None
    files: dict[str, bytes] = field(default_factory=lambda: dict[str, bytes]())
    modes: dict[str, str] = field(default_factory=lambda: dict[str, str]())
    dirs: set[str] = field(default_factory=lambda: {"/workspace"})
    commands: dict[str, FakeCommand] = field(default_factory=lambda: dict[str, FakeCommand]())

    def view(self) -> dict[str, Any]:
        v: dict[str, Any] = {
            "id": self.id,
            "template_id": self.template_id,
            "template_version": 3,
            "state": self.state,
            "created_at": NOW,
            "started_at": NOW,
            "last_activity_at": NOW,
            "expires_at": self.expires_at,
            "field_from_the_future": 1,
        }
        if self.reason:
            v["reason"] = self.reason
        if self.network is not None:
            v["network"] = self.network
        if self.body.get("metadata"):
            v["metadata"] = self.body["metadata"]
        return v


Program = Callable[[FakeCommand], None]


def program(
    stdout: bytes = b"", stderr: bytes = b"", exit_code: int = 0, reason: str = "exited", signal: str = ""
) -> Program:
    """A command that prints and exits immediately."""

    def run(cmd: FakeCommand) -> None:
        if stdout:
            cmd.output.append(("pty" if "pty" in cmd.body else "stdout", stdout))
        if stderr:
            cmd.output.append(("stderr", stderr))
        cmd.exit = {"exit_code": exit_code, "reason": reason}
        if signal:
            cmd.exit["signal"] = signal

    return run


def abspath(path: str) -> str:
    import posixpath

    return posixpath.normpath(path if path.startswith("/") else "/workspace/" + path)


class FakeServer:
    def __init__(self) -> None:
        self.keys = {API_KEY: "acme", OTHER_KEY: "other"}
        self.templates = {"python": {"max_lifetime_ms": 3_600_000, "network": "allowlist"}}
        self.sandboxes: dict[str, FakeSandbox] = {}
        self.requests: list[httpx.Request] = []
        #: Errors to answer instead of handling: (method, path regex, responses left).
        self.injected: list[tuple[str, re.Pattern[str], list[httpx.Response | Exception]]] = []
        #: What the next started commands do, in order; then ``default_program``.
        self.programs: list[Program] = []
        self.default_program: Program = program(stdout=b"ok\n")
        #: Splits an NDJSON body into the chunks the client will receive.
        self.chunker: Callable[[bytes], list[bytes]] = lambda body: [body]
        #: Per stream connection, in order: the number of lines after which
        #: the connection breaks (None: do not break).
        self.cuts: list[int | None] = []
        #: Called with (command, connection number) before each stream is answered.
        self.before_stream: Callable[[FakeCommand, int], None] | None = None
        self.stream_requests: list[tuple[str, int]] = []
        self.delete_status = 204
        self._n = 0

    # -- test controls --------------------------------------------------------

    def transport(self) -> httpx.MockTransport:
        return httpx.MockTransport(self.handle)

    def inject(self, method: str, pattern: str, *responses: httpx.Response | Exception) -> None:
        self.injected.append((method, re.compile(pattern), list(responses)))

    def seen(self, method: str, pattern: str) -> list[httpx.Request]:
        rx = re.compile(pattern)
        return [r for r in self.requests if r.method == method and rx.search(r.url.path)]

    def sandbox(self, sandbox_id: str | None = None) -> FakeSandbox:
        if sandbox_id is None:
            return next(iter(self.sandboxes.values()))
        return self.sandboxes[sandbox_id]

    def _id(self) -> str:
        self._n += 1
        return f"{self._n:016x}"

    # -- request handling -----------------------------------------------------

    def handle(self, request: httpx.Request) -> httpx.Response:
        request.read()
        self.requests.append(request)
        path = request.url.path
        for method, rx, responses in self.injected:
            if responses and method == request.method and rx.search(path):
                answer = responses.pop(0)
                if isinstance(answer, Exception):
                    raise answer
                return answer
        try:
            return self.route(request)
        except _Fail as f:
            return f.response

    def _tenant(self, request: httpx.Request) -> str:
        key = request.headers.get("x-api-key", "")
        auth = request.headers.get("authorization", "")
        if not key and auth.lower().startswith("bearer ") and not auth[7:].startswith("sbt_"):
            key = auth[7:]
        tenant = self.keys.get(key)
        if tenant is None:
            raise _Fail(error(401, "unauthorized", "missing or invalid credentials"))
        return tenant

    def _owned(self, request: httpx.Request, sandbox_id: str) -> FakeSandbox:
        tenant = self._tenant(request)
        sb = self.sandboxes.get(sandbox_id)
        if sb is None or sb.tenant != tenant:
            raise _Fail(error(404, "not_found", f'sandbox "{sandbox_id}" not found'))
        return sb

    def _data(self, request: httpx.Request, sandbox_id: str) -> FakeSandbox:
        if request.headers.get("x-api-key"):
            sb = self._owned(request, sandbox_id)
        else:
            token = request.headers.get("x-access-token", "")
            sb = self.sandboxes.get(sandbox_id)
            if sb is None or not token or sb.token != token:
                raise _Fail(error(401, "unauthorized", "missing or invalid credentials"))
        if sb.state != "running":
            raise _Fail(not_running(sb))
        return sb

    def _json(self, request: httpx.Request, allowed: set[str]) -> dict[str, Any]:
        try:
            body = json.loads(request.content)
        except ValueError:
            raise _Fail(error(400, "bad_request", "invalid JSON body")) from None
        unknown = set(body) - allowed
        if unknown:
            raise _Fail(error(400, "bad_request", f"invalid JSON body: unknown field {sorted(unknown)[0]!r}"))
        return body

    def route(self, request: httpx.Request) -> httpx.Response:
        method, path, q = request.method, request.url.path, request.url.params
        if (method, path) == ("POST", "/v1/sandboxes"):
            return self.create(request)
        if (method, path) == ("GET", "/v1/sandboxes"):
            tenant = self._tenant(request)
            states = (q.get("state") or "assigned,running").split(",")
            want_md = dict(p.split("=", 1) for p in q.get_list("metadata"))
            rows = [
                sb.view()
                for sb in reversed(self.sandboxes.values())
                if sb.tenant == tenant
                and (states == ["all"] or sb.state in states)
                and (not q.get("template_id") or sb.template_id == q["template_id"])
                and all((sb.body.get("metadata") or {}).get(k) == v for k, v in want_md.items())
            ]
            return httpx.Response(200, json={"sandboxes": rows[: int(q.get("limit") or 100)]})
        if (method, path) == ("GET", "/v1/templates"):
            self._tenant(request)
            return httpx.Response(
                200,
                json={
                    "templates": [
                        {
                            "id": "python",
                            "version": 3,
                            "image": "docker.io/library/python:3.12-alpine",
                            "runtime": "crun",
                            "resources": {"cpus": 1, "memory_bytes": 268435456},
                            "network": "allowlist",
                            "idle_timeout_ms": 600000,
                            "max_lifetime_ms": 3600000,
                        }
                    ]
                },
            )
        m = re.fullmatch(r"/v1/sandboxes/([^/]+)(/.*)?", path)
        if not m:
            return error(404, "not_found", f"no such route: {method} {path}")
        sid, rest = m.group(1), m.group(2) or ""
        if rest in ("", "/timeout", "/network", "/events", "/stats"):
            return self.control(request, sid, rest)
        if rest.startswith("/commands"):
            return self.commands(request, self._data(request, sid), rest[len("/commands") :])
        if rest.startswith("/files"):
            return self.files(request, self._data(request, sid), rest[len("/files") :])
        return error(404, "not_found", f"no such route: {method} {path}")

    def create(self, request: httpx.Request) -> httpx.Response:
        tenant = self._tenant(request)
        body = self._json(request, {"template_id", "timeout_ms", "env", "metadata", "network"})
        tmpl = self.templates.get(body.get("template_id", ""))
        if tmpl is None:
            return error(404, "template_not_found", f'template "{body.get("template_id")}" not found')
        if body.get("timeout_ms", 0) > tmpl["max_lifetime_ms"]:
            return error(400, "invalid_timeout", "timeout_ms exceeds the template's max lifetime",
                         max_timeout_ms=tmpl["max_lifetime_ms"])  # fmt: skip
        net = body.get("network")
        if net and net.get("mode") == "full":
            return error(400, "network_not_allowed", "the template does not allow the requested network mode")
        n = self._id()
        sb = FakeSandbox(
            id=f"sb-{n}", tenant=tenant, token="sbt_" + n * 4, template_id=body["template_id"], body=body,
            network=net or {"mode": tmpl["network"]},
        )  # fmt: skip
        self.sandboxes[sb.id] = sb
        return httpx.Response(
            201, json={**sb.view(), "access_token": sb.token}, headers={"Location": f"/v1/sandboxes/{sb.id}"}
        )

    def control(self, request: httpx.Request, sid: str, rest: str) -> httpx.Response:
        sb = self._owned(request, sid)
        method = request.method
        if rest == "" and method == "GET":
            return httpx.Response(200, json=sb.view())
        if rest == "" and method == "DELETE":
            if sb.state == "running":
                # An asynchronous delete leaves the sandbox on its way out.
                sb.state, sb.reason = ("recycling" if self.delete_status == 202 else "destroyed"), "released"
            return httpx.Response(self.delete_status)
        if rest == "/timeout" and method == "POST":
            body = self._json(request, {"timeout_ms"})
            if body.get("timeout_ms", 0) <= 0:
                return error(400, "invalid_timeout", "timeout_ms must be positive")
            if sb.state != "running":
                return not_running(sb)
            sb.expires_at = f"2026-10-10T08:00:{min(body['timeout_ms'] // 1000, 59):02d}Z"
            return httpx.Response(200, json=sb.view())
        if rest == "/network" and method == "PUT":
            body = self._json(request, {"mode", "allow_domains", "allow_cidrs", "deny_cidrs"})
            if sb.state != "running":
                return not_running(sb)
            if (sb.body.get("network") or {}).get("mode") == "none":
                return error(409, "network_not_attached", "created with network mode none")
            sb.network = body
            return httpx.Response(200, json=sb.view())
        if rest == "/events" and method == "GET":
            events: list[dict[str, Any]] = [
                {"id": 1, "type": "claimed", "at": NOW},
                {"id": 2, "type": "transition", "from_state": "assigned", "to_state": "running", "at": NOW},
            ]
            for i, c in enumerate(sb.commands.values()):
                events.append({"id": 3 + i, "type": "command_finished", "at": NOW,
                               "data": {"pid": c.pid, "cmd": c.cmd, "exit_code": (c.exit or {}).get("exit_code")}})  # fmt: skip
            want = request.url.params.get("type")
            return httpx.Response(200, json={"events": [e for e in events if not want or e["type"] == want]})
        if rest == "/stats" and method == "GET":
            if sb.state != "running":
                return not_running(sb)
            return httpx.Response(200, json={
                "memory_bytes": 2_600_000, "oom_kills": 0, "cpu_usage_usec": 12345, "cpu_throttled_periods": 1,
                "cpu_throttled_usec": 20, "pids_current": 2, "pids_limit_hits": 0, "memory_high_hits": 0,
            })  # fmt: skip
        return error(405, "method_not_allowed", f"method {method} is not allowed")

    # -- commands -------------------------------------------------------------

    def ndjson(self, cmd: FakeCommand, events: list[dict[str, Any]]) -> httpx.Response:
        number = len(self.stream_requests)
        cut = self.cuts.pop(0) if self.cuts else None
        lines = [json.dumps(e).encode() + b"\n" for e in events]
        body = b"".join(lines if cut is None else lines[:cut])
        if cut is not None and cut < len(lines):
            body += lines[cut][: len(lines[cut]) // 2]  # half a line, then the connection dies
        del number
        return httpx.Response(
            200,
            headers={"Content-Type": "application/x-ndjson", "X-Request-Id": "req-stream"},
            stream=ChunkStream(self.chunker(body), fail=cut is not None),
        )

    def commands(self, request: httpx.Request, sb: FakeSandbox, rest: str) -> httpx.Response:
        method, q = request.method, request.url.params
        if rest == "" and method == "GET":
            return httpx.Response(200, json={"commands": [c.view() for c in sb.commands.values()]})
        if rest == "" and method == "POST":
            body = self._json(request, {"cmd", "args", "env", "cwd", "timeout_ms", "stdin", "tag", "pty"})
            if not body.get("cmd"):
                return error(400, "bad_request", "cmd is required")
            if body["cmd"] == "/no/such/binary":
                return error(400, "command_not_found", "executable not found")
            cmd = FakeCommand(
                pid=f"p{len(sb.commands) + 1}", cmd=body["cmd"], args=body.get("args") or [], body=body
            )
            sb.commands[cmd.pid] = cmd
            (self.programs.pop(0) if self.programs else self.default_program)(cmd)
            wait = q.get("wait") or "false"
            if wait == "stream":
                self.stream_requests.append((cmd.pid, 0))
                if self.before_stream:
                    self.before_stream(cmd, len([s for s in self.stream_requests if s[0] == cmd.pid]))
                start = {"type": "start", "offset": 0, "command": {**cmd.view(), "next_offset": 0}}
                start["command"].pop("exit", None)
                return self.ndjson(cmd, [start, *cmd.events(0)])
            if wait == "true":
                out = b"".join(d for k, d in cmd.output if k != "stderr")
                err = b"".join(d for k, d in cmd.output if k == "stderr")
                return httpx.Response(200, json={**cmd.view(), "stdout": base64.b64encode(out).decode(),
                                                 "stderr": base64.b64encode(err).decode(), "truncated": False})  # fmt: skip
            return httpx.Response(201, json=cmd.view(), headers={"Location": f"{request.url.path}/{cmd.pid}"})
        m = re.fullmatch(r"/([^/]+)(/[a-z-]+)?", rest)
        if not m:
            return error(404, "not_found", "no such route: " + request.url.path)
        cmd = sb.commands.get(m.group(1))
        if cmd is None:
            return error(404, "process_not_found", "no such process")
        action = m.group(2) or ""
        if action == "" and method == "GET":
            return httpx.Response(200, json=cmd.view())
        if action == "/stream" and method == "GET":
            try:
                start_at = int(q.get("from") or 0)
            except ValueError:
                return error(400, "bad_request", "from must be a non-negative integer")
            self.stream_requests.append((cmd.pid, start_at))
            if self.before_stream:
                self.before_stream(cmd, len([s for s in self.stream_requests if s[0] == cmd.pid]))
            events = cmd.events(start_at)
            if cmd.exit is None:
                events.append({"type": "keepalive", "offset": cmd.total})
            return self.ndjson(cmd, events)
        if action == "/stdin" and method == "POST":
            if cmd.exit is not None:
                return error(409, "process_exited", "the process has exited")
            if (not cmd.body.get("stdin") and "pty" not in cmd.body) or cmd.stdin_closed:
                return error(409, "stdin_closed", "stdin is closed")
            cmd.stdin += request.content
            return httpx.Response(204)
        if action == "/close-stdin" and method == "POST":
            if "pty" in cmd.body:
                return error(409, "is_a_pty", "send Ctrl+D instead")
            cmd.stdin_closed = True
            return httpx.Response(204)
        if action == "/signal" and method == "POST":
            body = self._json(request, {"signal"})
            if body.get("signal") not in ("SIGINT", "SIGTERM", "SIGKILL", "SIGHUP", "SIGQUIT"):
                return error(400, "bad_signal", "unknown signal")
            if cmd.exit is not None:
                return error(409, "process_exited", "the process has exited")
            cmd.signals.append(body["signal"])
            if body["signal"] == "SIGKILL":
                cmd.exit = {"exit_code": 137, "reason": "killed", "signal": "SIGKILL"}
            return httpx.Response(204)
        if action == "/resize" and method == "POST":
            body = self._json(request, {"cols", "rows"})
            if "pty" not in cmd.body:
                return error(409, "not_a_pty", "not a PTY")
            cmd.sizes.append((body["cols"], body["rows"]))
            return httpx.Response(204)
        return error(405, "method_not_allowed", f"method {method} is not allowed")

    # -- files ------------------------------------------------------------------

    def files(self, request: httpx.Request, sb: FakeSandbox, rest: str) -> httpx.Response:
        method, q = request.method, request.url.params
        raw = q.get("path")
        if rest == "/list" and method == "GET":
            path = abspath(raw or ".")
            if path in sb.files:
                return error(409, "conflict", "not a directory")
            if path != "/" and path not in sb.dirs:
                return error(404, "file_not_found", "no such file or directory")
            prefix = path.rstrip("/") + "/"
            names: dict[str, dict[str, Any]] = {}
            for p in sorted([*sb.files, *sb.dirs]):
                if p.startswith(prefix) and p != path:
                    name = p[len(prefix) :].split("/")[0]
                    full = prefix + name
                    names.setdefault(name, {
                        "name": name, "path": full, "size": len(sb.files.get(full, b"")),
                        "mode": sb.modes.get(full, "0755" if full in sb.dirs else "0644"),
                        "is_dir": full in sb.dirs, "mod_time": NOW,
                    })  # fmt: skip
            return httpx.Response(200, json={"files": list(names.values())})
        if not raw:
            return error(400, "bad_request", "the path query parameter is required")
        path = abspath(raw)
        if rest == "/mkdir" and method == "POST":
            parts = path.strip("/").split("/")
            for i in range(1, len(parts) + 1):
                sb.dirs.add("/" + "/".join(parts[:i]))
            return httpx.Response(204)
        if rest != "":
            return error(404, "not_found", "no such route: " + request.url.path)
        if method == "GET":
            if path in sb.dirs:
                return error(409, "is_directory", f"{path} is a directory")
            if path not in sb.files:
                return error(404, "file_not_found", "no such file or directory")
            return httpx.Response(
                200, content=sb.files[path], headers={"Content-Type": "application/octet-stream"}
            )
        if method == "PUT":
            if path in sb.dirs:
                return error(409, "is_directory", f"{path} is a directory")
            if path.startswith("/etc/"):
                return error(403, "permission_denied", "permission denied")
            if len(request.content) > 8 * 1024 * 1024:
                return error(507, "no_space", "no space left on device")
            parts = path.strip("/").split("/")[:-1]
            for i in range(1, len(parts) + 1):
                sb.dirs.add("/" + "/".join(parts[:i]))
            sb.files[path] = request.content
            sb.modes[path] = q.get("mode") or "0644"
            return httpx.Response(204)
        if method == "DELETE":
            if path not in sb.files and path not in sb.dirs:
                return error(404, "file_not_found", "no such file or directory")
            for p in [p for p in sb.files if p == path or p.startswith(path + "/")]:
                del sb.files[p]
            sb.dirs = {d for d in sb.dirs if d != path and not d.startswith(path + "/")}
            return httpx.Response(204)
        return error(405, "method_not_allowed", f"method {method} is not allowed")


def not_running(sb: FakeSandbox) -> httpx.Response:
    details = {"state": sb.state}
    if sb.reason:
        details["reason"] = sb.reason
    return error(409, "sandbox_not_running", f"sandbox {sb.id} is {sb.state}", **details)


class _Fail(Exception):
    def __init__(self, response: httpx.Response) -> None:
        self.response = response


# ---------------------------------------------------------------------------
# WebSocket
# ---------------------------------------------------------------------------


@dataclass
class WSSession:
    """What one WebSocket connection did, as seen by the server."""

    path: str
    start: int
    headers: dict[str, str]
    subprotocol: str | None = None
    stdin: bytearray = field(default_factory=bytearray)
    acks: list[int] = field(default_factory=lambda: list[int]())
    resizes: list[tuple[int, int]] = field(default_factory=lambda: list[tuple[int, int]]())
    signals: list[str] = field(default_factory=lambda: list[str]())
    pongs: list[bytes] = field(default_factory=lambda: list[bytes]())
    frames: list[bytes] = field(default_factory=lambda: list[bytes]())
    sent: int = 0


#: A connection script: called with the connection and its session; sends
#: frames and returns when the connection should end.
WSScript = Callable[[ServerConnection, WSSession], None]


class FakeWSServer:
    """A WebSocket server speaking the PTY framing, with one script per connection."""

    def __init__(self, token: str = "", api_key: str = API_KEY) -> None:
        self.token = token
        self.api_key = api_key
        self.sessions: list[WSSession] = []
        self.scripts: list[WSScript] = []
        #: HTTP answers for the next handshakes instead of upgrading.
        self.rejections: list[tuple[int, dict[str, Any], dict[str, str]]] = []
        self._server: Server = serve(
            self._handle, "127.0.0.1", 0, process_request=self._process_request, subprotocols=["sbx.v1"],  # type: ignore[list-item]
            compression=None, max_size=1 << 20 | 1,
        )  # fmt: skip
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()

    @property
    def url(self) -> str:
        host, port = self._server.socket.getsockname()[:2]
        return f"ws://{host}:{port}"

    def close(self) -> None:
        self._server.shutdown()
        self._thread.join(5)

    def _process_request(self, connection: ServerConnection, request: Request) -> Response | None:
        if self.rejections:
            status, body, headers = self.rejections.pop(0)
            payload = json.dumps(body).encode()
            hdrs = Headers({"Content-Type": "application/json", "Content-Length": str(len(payload)),
                            "X-Request-Id": "req-ws", **headers})  # fmt: skip
            return Response(status, "Error", hdrs, payload)
        token = request.headers.get("X-Access-Token", "")
        key = request.headers.get("X-API-Key", "")
        if not ((self.token and token == self.token) or key == self.api_key):
            payload = json.dumps(envelope("unauthorized", "missing or invalid credentials")).encode()
            return Response(401, "Unauthorized", Headers({"Content-Length": str(len(payload))}), payload)
        return None

    def _handle(self, ws: ServerConnection) -> None:
        assert ws.request is not None
        path = ws.request.path
        m = re.search(r"[?&]from=(\d+)", path)
        session = WSSession(
            path=path, start=int(m.group(1)) if m else 0, headers=dict(ws.request.headers.raw_items()),
            subprotocol=ws.subprotocol,
        )  # fmt: skip
        self.sessions.append(session)
        script = self.scripts.pop(0) if self.scripts else None
        reader = threading.Thread(target=self._read, args=(ws, session), daemon=True)
        reader.start()
        try:
            if script is not None:
                script(ws, session)
        except ConnectionClosed:
            pass

    @staticmethod
    def _read(ws: ServerConnection, session: WSSession) -> None:
        try:
            for message in ws:
                if isinstance(message, str) or not message:
                    continue
                session.frames.append(message)
                channel, payload = message[0], message[1:]
                if channel == 0:
                    session.stdin += payload
                elif channel == 4:
                    size = json.loads(payload)
                    session.resizes.append((size["cols"], size["rows"]))
                elif channel == 5:
                    session.signals.append(json.loads(payload)["signal"])
                elif channel == 6:
                    session.acks.append(json.loads(payload)["offset"])
                elif channel == 7:
                    ws.send(b"\x08" + payload)
                elif channel == 8:
                    session.pongs.append(payload)
        except ConnectionClosed:
            pass


def ws_offset(ws: ServerConnection, offset: int, dropped: int = 0) -> None:
    frame: dict[str, int] = {"offset": offset}
    if dropped:
        frame["dropped"] = dropped
    ws.send(b"\x09" + json.dumps(frame).encode())


def ws_output(
    ws: ServerConnection, session: WSSession, data: bytes, channel: int = 1, frame_size: int = 32768
) -> None:
    for i in range(0, len(data), frame_size):
        piece = data[i : i + frame_size]
        ws.send(bytes((channel,)) + piece)
        session.sent += len(piece)


def ws_exit(ws: ServerConnection, exit_code: int = 0, reason: str = "exited") -> None:
    ws.send(b"\x03" + json.dumps({"exit": {"exit_code": exit_code, "reason": reason}}).encode())
    ws.close(1000)


def wait_until(condition: Callable[[], bool], timeout: float = 5.0) -> bool:
    """Poll from a test thread (never from the event loop)."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if condition():
            return True
        time.sleep(0.005)
    return condition()
