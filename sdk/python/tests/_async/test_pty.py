"""sandbox.pty: the WebSocket framing, flow control, keepalive and re-attach."""

from __future__ import annotations

import json
import socket
from typing import Any

import pytest
from websockets.sync.server import ServerConnection

import sbx
from sbx import AsyncSandbox

from ..fake import (
    API_KEY,
    FakeServer,
    FakeWSServer,
    WSSession,
    envelope,
    wait_until,
    ws_exit,
    ws_offset,
    ws_output,
)
from .helpers import eventually


def drop(ws: ServerConnection) -> None:
    """Kill the TCP connection without a closing handshake."""
    ws.socket.shutdown(socket.SHUT_RDWR)


def simple(output: bytes = b"hello\r\n", exit_code: int = 0) -> Any:
    def script(ws: ServerConnection, session: WSSession) -> None:
        ws_offset(ws, session.start)
        ws_output(ws, session, output)
        ws_exit(ws, exit_code)

    return script


async def test_create_attaches_and_delivers_output(
    sandbox: AsyncSandbox, server: FakeServer, ws: FakeWSServer
) -> None:
    ws.scripts.append(simple(b"$ stty size\r\n24 80\r\n", exit_code=0))
    chunks: list[bytes] = []
    handle = await sandbox.pty.create(
        80, 24, chunks.append, envs={"TERM": "xterm"}, cwd="/tmp", timeout=30, tag="tty"
    )
    result = await handle.wait()
    assert b"".join(chunks) == b"$ stty size\r\n24 80\r\n"
    assert (result.exit_code, result.reason, result.pid, result.truncated) == (0, "exited", "p1", False)
    assert handle.exit_code == 0 and handle.finished and handle.offset == 20 and handle.pid == "p1"

    start = server.seen("POST", r"/commands$")[0]
    assert json.loads(start.content) == {
        "cmd": "/bin/sh",
        "pty": {"cols": 80, "rows": 24},
        "env": {"TERM": "xterm"},
        "cwd": "/tmp",
        "timeout_ms": 30_000,
        "tag": "tty",
    }
    assert "wait" not in start.url.params

    session = ws.sessions[0]
    assert session.path == f"/v1/sandboxes/{sandbox.sandbox_id}/commands/p1/ws?from=0"
    assert session.subprotocol == "sbx.v1"
    headers = {k.lower(): v for k, v in session.headers.items()}
    assert headers["x-access-token"] == sandbox.access_token
    assert "x-api-key" not in headers  # the tenant key does not travel on the data plane
    assert headers["user-agent"].startswith("sbx-python/")
    assert "token=" not in session.path


async def test_create_command_forms(sandbox: AsyncSandbox, server: FakeServer, ws: FakeWSServer) -> None:
    for kwargs, wanted in (
        ({"cmd": "python3"}, {"cmd": "python3"}),
        ({"cmd": "python3", "args": ["-i"]}, {"cmd": "python3", "args": ["-i"]}),
        ({"cmd": "ls -la | head"}, {"cmd": "/bin/sh", "args": ["-c", "ls -la | head"]}),
        ({"args": ["-l"]}, {"cmd": "/bin/sh", "args": ["-l"]}),
    ):
        ws.scripts.append(simple())
        handle = await sandbox.pty.create(100, 30, lambda data: None, **kwargs)  # type: ignore[arg-type]
        await handle.wait()
        assert json.loads(server.seen("POST", r"/commands$")[-1].content) == {
            **wanted,
            "pty": {"cols": 100, "rows": 30},
        }


async def test_input_resize_and_signal_frames(sandbox: AsyncSandbox, ws: FakeWSServer) -> None:
    def script(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, 0)
        assert wait_until(lambda: bool(session.signals))
        ws_exit(conn, 130, "killed")

    ws.scripts.append(script)
    handle = await sandbox.pty.create(80, 24, lambda data: None)
    await handle.send_input("stty size\n")
    await handle.send_input(b"\x03")
    big = bytes(range(256)) * 400  # 102400 bytes: more than one frame
    await handle.send_input(big)
    await handle.resize(120, 40)
    await handle.signal("SIGINT")
    result = await handle.wait()
    assert (result.exit_code, result.reason) == (130, "killed")  # returned, not raised, for a terminal

    session = ws.sessions[0]
    assert bytes(session.stdin) == b"stty size\n\x03" + big
    assert session.resizes == [(120, 40)]
    assert session.signals == ["SIGINT"]
    stdin_frames = [f for f in session.frames if f[0] == 0]
    assert len(stdin_frames) == 2 + 4 and max(len(f) for f in stdin_frames) == 32768 + 1
    assert [f for f in session.frames if f[0] == 4] == [b'\x04{"cols":120,"rows":40}']
    assert [f for f in session.frames if f[0] == 5] == [b'\x05{"signal":"SIGINT"}']

    with pytest.raises(sbx.StreamError):  # the session is over
        await handle.send_input("late")


async def test_output_is_acknowledged_in_small_steps(sandbox: AsyncSandbox, ws: FakeWSServer) -> None:
    total = 60_000

    def script(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, 0)
        ws_output(conn, session, b"x" * total, frame_size=1000)
        assert wait_until(lambda: bool(session.acks) and session.acks[-1] > total - 10_000)
        ws_exit(conn)

    ws.scripts.append(script)
    received: list[bytes] = []
    handle = await sandbox.pty.create(80, 24, received.append)
    await handle.wait()
    assert sum(len(c) for c in received) == total
    acks = ws.sessions[0].acks
    assert acks == sorted(acks) and len(acks) >= 6
    # Never more than the agent's low watermark (10000 bytes) between two
    # acknowledgements, so a paused command always resumes.
    steps = [b - a for a, b in zip([0, *acks], acks, strict=False)]
    assert max(steps) < 10_000 and min(steps) > 0
    assert total - acks[-1] < 10_000
    assert next(f for f in ws.sessions[0].frames if f[0] == 6) == b'\x06{"offset":9000}'


async def test_large_frames_are_acknowledged_one_by_one(sandbox: AsyncSandbox, ws: FakeWSServer) -> None:
    def script(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, 0)
        ws_output(conn, session, b"y" * (32768 * 3))  # the largest frame the server sends
        assert wait_until(lambda: len(session.acks) == 3)
        ws_exit(conn)

    ws.scripts.append(script)
    handle = await sandbox.pty.create(80, 24, lambda data: None)
    await handle.wait()
    assert ws.sessions[0].acks == [32768, 65536, 98304]


async def test_acknowledgement_follows_the_callback(sandbox: AsyncSandbox, ws: FakeWSServer) -> None:
    """An ACK means "consumed": it is sent after on_data returned, not on receipt."""
    order: list[str] = []

    def script(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, 0)
        ws_output(conn, session, b"z" * 9000)
        assert wait_until(lambda: bool(session.acks))
        order.append("ack")
        ws_exit(conn)

    ws.scripts.append(script)
    handle = await sandbox.pty.create(80, 24, lambda data: order.append("callback"))
    await handle.wait()
    assert order == ["callback", "ack"]


async def test_flow_control_can_be_switched_off(sandbox: AsyncSandbox, ws: FakeWSServer) -> None:
    ws.scripts.append(simple(b"x" * 50_000))
    handle = await sandbox.pty.create(80, 24, lambda data: None, flow_control=False)
    await handle.wait()
    assert ws.sessions[0].acks == [] and handle.offset == 50_000


async def test_application_pings_are_answered(sandbox: AsyncSandbox, ws: FakeWSServer) -> None:
    def script(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, 0)
        conn.send(b"\x07ping-1")
        conn.send(b"\x08unsolicited pong")
        conn.send(b"\x2afrom the future")  # an unknown channel is ignored
        conn.send("text frames are ignored")
        conn.ping().wait(5)  # a protocol-level ping, answered by the library
        assert wait_until(lambda: session.pongs == [b"ping-1"])
        ws_exit(conn)

    ws.scripts.append(script)
    handle = await sandbox.pty.create(80, 24, lambda data: None)
    assert (await handle.wait()).exit_code == 0
    assert ws.sessions[0].pongs == [b"ping-1"]


async def test_reattach_after_a_dropped_connection(
    sandbox: AsyncSandbox, ws: FakeWSServer, sleeps: list[float]
) -> None:
    def first(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, 0)
        ws_output(conn, session, b"A" * 5000)
        drop(conn)

    def second(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, session.start)
        ws_output(conn, session, b"B" * 100)
        ws_exit(conn, 7)

    ws.scripts += [first, second]
    received: list[bytes] = []
    handle = await sandbox.pty.create(80, 24, received.append)
    result = await handle.wait()
    assert b"".join(received) == b"A" * 5000 + b"B" * 100  # nothing lost, nothing twice
    assert (result.exit_code, result.truncated) == (7, False)
    assert [s.start for s in ws.sessions] == [0, 5000]
    assert ws.sessions[1].path.endswith("/ws?from=5000")
    assert handle.reconnects == 1 and len(sleeps) == 1


async def test_reattach_reports_lost_output(sandbox: AsyncSandbox, ws: FakeWSServer) -> None:
    def first(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, 0)
        ws_output(conn, session, b"A" * 5000)
        drop(conn)

    def second(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, 6000, dropped=1000)  # the replay buffer starts later than asked
        ws_output(conn, session, b"B" * 100)
        ws_offset(conn, 6600, dropped=500)  # and the client fell behind once more
        ws_output(conn, session, b"C" * 10)
        ws_exit(conn)

    ws.scripts += [first, second]
    received: list[bytes] = []
    gaps: list[tuple[int, int]] = []
    handle = await sandbox.pty.create(
        80, 24, received.append, on_truncated=lambda offset, n: gaps.append((offset, n))
    )
    result = await handle.wait()
    assert b"".join(received) == b"A" * 5000 + b"B" * 100 + b"C" * 10
    assert gaps == [(6000, 1000), (6600, 500)]
    assert (result.truncated, result.dropped_bytes, handle.dropped_bytes, handle.offset) == (
        True,
        1500,
        1500,
        6610,
    )


async def test_replayed_bytes_are_not_delivered_twice(sandbox: AsyncSandbox, ws: FakeWSServer) -> None:
    def first(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, 0)
        ws_output(conn, session, b"0123456789")
        drop(conn)

    def second(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, 4)  # a server that replays from earlier than asked
        ws_output(conn, session, b"456", frame_size=3)
        ws_output(conn, session, b"789ab", frame_size=5)
        ws_output(conn, session, b"cd")
        ws_exit(conn)

    ws.scripts += [first, second]
    received: list[bytes] = []
    handle = await sandbox.pty.create(80, 24, received.append)
    await handle.wait()
    assert b"".join(received) == b"0123456789abcd"


async def test_reattach_learns_why_the_sandbox_is_gone(sandbox: AsyncSandbox, ws: FakeWSServer) -> None:
    def first(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, 0)
        ws_output(conn, session, b"working...")
        gone = envelope(
            "sandbox_not_running", "sandbox is destroyed", state="destroyed", reason="max_lifetime"
        )
        ws.rejections.append((409, gone, {}))
        conn.close(1001, "sandbox terminated")

    ws.scripts.append(first)
    handle = await sandbox.pty.create(80, 24, lambda data: None)
    with pytest.raises(sbx.SandboxNotRunningError) as e:
        await handle.wait()
    assert (e.value.reason, e.value.state, e.value.status_code, e.value.request_id) == (
        "max_lifetime",
        "destroyed",
        409,
        "req-test",
    )
    assert handle.exit_code is None and handle.finished


async def test_reattach_is_bounded(sandbox: AsyncSandbox, ws: FakeWSServer, sleeps: list[float]) -> None:
    def dies(conn: ServerConnection, session: WSSession) -> None:
        drop(conn)

    ws.scripts += [dies] * 10
    handle = await sandbox.pty.create(80, 24, lambda data: None)
    with pytest.raises(sbx.StreamError, match="after 5 attempts"):
        await handle.wait()
    assert len(ws.sessions) == 6 and len(sleeps) == 5


async def test_reattach_retries_through_rate_limiting(sandbox: AsyncSandbox, ws: FakeWSServer) -> None:
    def first(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, 0)
        ws_output(conn, session, b"a")
        ws.rejections.append((429, envelope("rate_limited", "slow down"), {"Retry-After": "1"}))
        drop(conn)

    ws.scripts += [first, simple(b"b")]
    received: list[bytes] = []
    handle = await sandbox.pty.create(80, 24, received.append)
    assert (await handle.wait()).exit_code == 0
    assert b"".join(received) == b"ab"


async def test_status_frame_with_an_error(sandbox: AsyncSandbox, ws: FakeWSServer) -> None:
    def script(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, 0)
        conn.send(
            b"\x03"
            + json.dumps(
                {"error": {"code": "sandbox_terminated", "message": "sandbox has terminated: oom_killed"}}
            ).encode()
        )
        conn.close(1001, "sandbox_terminated")

    ws.scripts.append(script)
    handle = await sandbox.pty.create(80, 24, lambda data: None)
    with pytest.raises(sbx.SandboxTerminatedError, match="oom_killed"):
        await handle.wait()
    assert len(ws.sessions) == 1  # a terminal frame is final: no re-attach


async def test_handshake_errors_are_mapped(
    sandbox: AsyncSandbox, server: FakeServer, ws: FakeWSServer, sleeps: list[float]
) -> None:
    ws.rejections.append((404, envelope("process_not_found", "no such process"), {}))
    with pytest.raises(sbx.CommandNotFoundError) as e:
        await sandbox.pty.connect("p9", lambda data: None)
    assert (e.value.status_code, e.value.code, e.value.request_id) == (404, "process_not_found", "req-test")

    ws.token = "sbt_something_else"
    ws.api_key = "nope"
    with pytest.raises(sbx.AuthenticationError):
        await sandbox.pty.connect("p1", lambda data: None)
    ws.token = sandbox.access_token or ""

    # 429 on the first attach is retried like an idempotent request.
    ws.rejections.append((429, envelope("rate_limited", "slow down"), {"Retry-After": "2"}))
    ws.scripts.append(simple(b"late"))
    handle = await sandbox.pty.connect("p1", lambda data: None)
    await handle.wait()
    assert sleeps == [2.0]


async def test_connection_refused_is_a_transport_error(server: FakeServer, sleeps: list[float]) -> None:
    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    port = listener.getsockname()[1]
    listener.close()  # nothing listens here any more
    sb = await AsyncSandbox.create(
        "python", api_key=API_KEY, transport=server.transport(), ws_base_url=f"ws://127.0.0.1:{port}"
    )
    with pytest.raises(sbx.TransportError):
        await sb.pty.connect("p1", lambda data: None)
    assert len(sleeps) == 3  # the retry policy applies to the first attach


async def test_ws_url_is_derived_from_the_base_url(server: FakeServer) -> None:
    for base, wanted in (
        ("http://sbx.test:8080", "ws://sbx.test:8080/v1/x"),
        ("https://sbx.example.com", "wss://sbx.example.com/v1/x"),
        ("https://example.com/prefix/", "wss://example.com/prefix/v1/x"),
    ):
        assert sbx.ConnectionConfig.resolve(base_url=base).ws_url("/v1/x") == wanted
    sb = await AsyncSandbox.create(
        "python", api_key=API_KEY, base_url="http://h:1", transport=server.transport()
    )
    assert sb.config.ws_url("/v1/x") == "ws://h:1/v1/x"


async def test_connect_by_pid_and_pipe_mode_channels(sandbox: AsyncSandbox, ws: FakeWSServer) -> None:
    def script(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, session.start)
        ws_output(conn, session, b"out", channel=1)
        ws_output(conn, session, b"err", channel=2)
        ws_exit(conn, 1)

    ws.scripts += [script, script]
    out: list[bytes] = []
    err: list[bytes] = []
    handle = await sandbox.pty.connect("p7", out.append, on_stderr=err.append, from_offset=10)
    result = await handle.wait()
    assert ws.sessions[0].path.endswith("/commands/p7/ws?from=10")
    assert (out, err, result.exit_code, handle.offset) == ([b"out"], [b"err"], 1, 16)
    merged: list[bytes] = []
    await (await sandbox.pty.connect("p7", merged.append)).wait()
    assert merged == [b"out", b"err"]


async def test_wait_timeout_and_disconnect(sandbox: AsyncSandbox, ws: FakeWSServer) -> None:
    def script(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, 0)
        ws_output(conn, session, b"prompt$ ")
        wait_until(lambda: False, timeout=2)  # an idle shell

    ws.scripts.append(script)
    received: list[bytes] = []
    handle = await sandbox.pty.create(80, 24, received.append)
    await eventually(lambda: received == [b"prompt$ "])
    with pytest.raises(sbx.SandboxTimeoutError) as e:
        await handle.wait(timeout=0.05)
    assert isinstance(e.value, TimeoutError)
    await handle.disconnect()
    assert handle.finished and handle.offset == 8
    with pytest.raises(sbx.StreamError, match="disconnected"):
        await handle.wait()
    assert len(ws.sessions) == 1  # detaching on purpose does not re-attach
    await handle.disconnect()


async def test_callback_exception_ends_the_session(sandbox: AsyncSandbox, ws: FakeWSServer) -> None:
    def bad(data: bytes) -> None:
        raise RuntimeError("callback failed")

    ws.scripts.append(simple())
    handle = await sandbox.pty.create(80, 24, bad)
    with pytest.raises(RuntimeError, match="callback failed"):
        await handle.wait()


async def test_rest_helpers(sandbox: AsyncSandbox, server: FakeServer, ws: FakeWSServer) -> None:
    def script(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, 0)
        wait_until(lambda: False, timeout=1)

    ws.scripts.append(script)
    server.programs.append(lambda cmd: None)  # a shell that keeps running
    handle = await sandbox.pty.create(80, 24, lambda data: None)
    fake = server.sandbox().commands[handle.pid]
    await sandbox.pty.send_input(handle.pid, "ls\n")
    await sandbox.pty.resize(handle.pid, 132, 43)
    assert bytes(fake.stdin) == b"ls\n" and fake.sizes == [(132, 43)]
    assert json.loads(server.seen("POST", r"/resize$")[0].content) == {"cols": 132, "rows": 43}
    assert await handle.kill() is True
    assert await sandbox.pty.kill(handle.pid) is False
    assert fake.signals == ["SIGKILL"]
    await handle.disconnect()

    plain = await sandbox.commands.run("x", background=True)
    with pytest.raises(sbx.ConflictError) as e:
        await sandbox.pty.resize(plain.pid, 1, 1)
    assert e.value.code == "not_a_pty"


async def test_final_close_codes_are_typed_without_a_status_frame(
    sandbox: AsyncSandbox, ws: FakeWSServer
) -> None:
    for code, reason, expected in (
        (4000, "sandbox_terminated", sbx.SandboxTerminatedError),
        (4000, "sandbox_not_running", sbx.SandboxNotRunningError),
        (4000, "", sbx.SandboxTerminatedError),  # no reason: the code's own meaning
        (4001, "unauthorized", sbx.AuthenticationError),
        (4004, "process_not_found", sbx.CommandNotFoundError),
        (4005, "bad_request", sbx.InvalidArgumentError),
    ):

        def script(
            conn: ServerConnection, session: WSSession, code: int = code, reason: str = reason
        ) -> None:
            ws_offset(conn, 0)
            conn.close(code, reason)

        before = len(ws.sessions)
        ws.scripts.append(script)
        handle = await sandbox.pty.create(80, 24, lambda data: None)
        with pytest.raises(expected):
            await handle.wait()
        assert len(ws.sessions) == before + 1, f"close {code} {reason!r} was followed by a re-attach"


async def test_transient_close_codes_and_status_frames_reattach(
    sandbox: AsyncSandbox, ws: FakeWSServer
) -> None:
    def agent_lost(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, 0)
        ws_output(conn, session, b"a")
        conn.send(
            b"\x03"
            + json.dumps(
                {"error": {"code": "agent_unavailable", "message": "the agent did not answer"}}
            ).encode()
        )
        conn.close(4003, "agent_unavailable")

    def shutting_down(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, session.start)
        ws_output(conn, session, b"b")
        conn.close(4002, "shutting_down")  # the close alone, without a status frame

    def unknown_code(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, session.start)
        ws_output(conn, session, b"c")
        conn.close(4777, "something_new")

    def last(conn: ServerConnection, session: WSSession) -> None:
        ws_offset(conn, session.start)
        ws_output(conn, session, b"d")
        ws_exit(conn)

    ws.scripts += [agent_lost, shutting_down, unknown_code, last]
    received: list[bytes] = []
    handle = await sandbox.pty.create(80, 24, received.append)
    result = await handle.wait()
    assert result.exit_code == 0 and b"".join(received) == b"abcd"
    assert [s.start for s in ws.sessions] == [0, 1, 2, 3] and handle.reconnects == 3


def test_close_code_table() -> None:
    from sbx import _wire as wire

    assert wire.close_error(4000, "sandbox_not_running") == {
        "code": "sandbox_not_running",
        "message": "the server closed the connection: sandbox_not_running (close code 4000)",
    }
    assert wire.close_error(4004, None)["code"] == "process_not_found"  # type: ignore[index]
    assert wire.close_error(4001, "Not A Token!")["code"] == "unauthorized"  # type: ignore[index]
    for code in (4002, 4003, 4029, 4099, 4321, 1000, 1001, 1006, None):
        assert wire.close_error(code, "x") is None
    # The frame sizes the server documents.
    assert wire.MAX_CLIENT_FRAME_PAYLOAD == 1 << 20 and wire.MAX_SERVER_FRAME_PAYLOAD == 32 << 10
    assert (
        wire.STDIN_CHUNK < wire.MAX_CLIENT_FRAME_PAYLOAD
        and wire.MAX_SERVER_FRAME > wire.MAX_SERVER_FRAME_PAYLOAD
    )
