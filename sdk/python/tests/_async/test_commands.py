"""commands.run, handles, the NDJSON stream and its reconnection."""

from __future__ import annotations

import base64
import json
from typing import Any

import httpx
import pytest

import sbx
from sbx import AsyncSandbox, ExitEvent, StderrEvent, StdoutEvent, TruncatedEvent

from ..fake import ChunkStream, FakeCommand, FakeServer, envelope, program, split_every


def body_of(request: httpx.Request) -> dict[str, Any]:
    return json.loads(request.content)


async def test_run_is_a_shell_command_line_by_default(sandbox: AsyncSandbox, server: FakeServer) -> None:
    server.programs.append(program(stdout=b"42\n"))
    result = await sandbox.commands.run('python3 -c "print(6*7)"')
    request = server.seen("POST", r"/commands$")[0]
    assert body_of(request) == {"cmd": "/bin/sh", "args": ["-c", 'python3 -c "print(6*7)"']}
    assert request.url.params["wait"] == "stream"
    assert (result.stdout, result.stderr, result.exit_code, result.reason) == ("42\n", "", 0, "exited")
    assert result.pid == "p1" and result.ok and result.error is None
    assert not result.truncated and result.dropped_bytes == 0
    assert result.stdout_bytes == b"42\n"


async def test_run_with_args_starts_the_program_directly(sandbox: AsyncSandbox, server: FakeServer) -> None:
    await sandbox.commands.run(
        "python3", args=["-c", "print(1)"], envs={"K": "v"}, cwd="/tmp", timeout=1.5, stdin=True, tag="job"
    )
    assert body_of(server.seen("POST", r"/commands$")[0]) == {
        "cmd": "python3",
        "args": ["-c", "print(1)"],
        "env": {"K": "v"},
        "cwd": "/tmp",
        "timeout_ms": 1500,
        "stdin": True,
        "tag": "job",
    }
    await sandbox.commands.run("true", args=[])
    assert body_of(server.requests[-1]) == {"cmd": "true", "args": []}
    with pytest.raises(TypeError):
        await sandbox.commands.run("echo", args="hello")  # type: ignore[arg-type]
    with pytest.raises(ValueError):
        await sandbox.commands.run("")


async def test_non_zero_exit_raises_like_e2b(sandbox: AsyncSandbox, server: FakeServer) -> None:
    server.programs.append(program(stdout=b"partial\n", stderr=b"boom\n", exit_code=3))
    with pytest.raises(sbx.CommandExitError) as e:
        await sandbox.commands.run("false")
    err = e.value
    assert (err.exit_code, err.stdout, err.stderr, err.reason, err.signal) == (
        3,
        "partial\n",
        "boom\n",
        "exited",
        None,
    )
    assert err.result.error == "exit status 3" and not err.result.ok
    assert "code 3" in str(err) and "boom" in str(err)
    assert not isinstance(err, sbx.ApiError)


async def test_check_false_returns_the_result(sandbox: AsyncSandbox, server: FakeServer) -> None:
    server.programs.append(program(stderr=b"boom\n", exit_code=3))
    result = await sandbox.commands.run("false", check=False)
    assert (result.exit_code, result.stderr) == (3, "boom\n")
    server.programs.append(program(exit_code=137, reason="killed", signal="SIGKILL"))
    result = await sandbox.commands.run("x", check=False)
    assert (result.exit_code, result.reason, result.signal, result.error) == (
        137,
        "killed",
        "SIGKILL",
        "killed",
    )


async def test_command_timeout(sandbox: AsyncSandbox, server: FakeServer) -> None:
    server.programs.append(program(stdout=b"tick\n", exit_code=137, reason="timeout", signal="SIGKILL"))
    with pytest.raises(sbx.CommandTimeoutError) as e:
        await sandbox.commands.run("sleep 60", timeout=0.2)
    assert body_of(server.seen("POST", r"/commands$")[0])["timeout_ms"] == 200
    assert (e.value.reason, e.value.exit_code, e.value.stdout) == ("timeout", 137, "tick\n")
    assert isinstance(e.value, TimeoutError) and isinstance(e.value, sbx.CommandExitError)
    server.programs.append(program(exit_code=137, reason="timeout"))
    assert (await sandbox.commands.run("sleep 60", timeout=0.2, check=False)).reason == "timeout"


async def test_start_failures(sandbox: AsyncSandbox) -> None:
    with pytest.raises(sbx.CommandStartError) as e:
        await sandbox.commands.run("/no/such/binary", args=[])
    assert e.value.code == "command_not_found" and e.value.status_code == 400
    with pytest.raises(sbx.CommandStartError):
        await sandbox.commands.run("/no/such/binary", args=[], background=True)


def idle(cmd: FakeCommand) -> None:
    """A command that keeps running and prints nothing."""


def chatty(cmd: FakeCommand) -> None:
    cmd.output += [
        ("stdout", b"one\n"),
        ("stderr", b"warn\n"),
        ("stdout", "héllo 世".encode()[:8]),  # ends in the middle of a 3-byte character
        ("stdout", "héllo 世".encode()[8:] + b"\n"),
        ("stdout", bytes(range(256)) * 200),  # 51200 bytes: several base64 lines' worth
        ("stderr", b"done\n"),
    ]
    cmd.exit = {"exit_code": 0, "reason": "exited"}


CHATTY_STDOUT = b"one\n" + "héllo 世\n".encode() + bytes(range(256)) * 200


async def test_streaming_callbacks(sandbox: AsyncSandbox, server: FakeServer) -> None:
    server.programs.append(chatty)
    out: list[str] = []
    err: list[str] = []
    result = await sandbox.commands.run("x", on_stdout=out.append, on_stderr=err.append)
    assert err == ["warn\n", "done\n"]
    assert out[0] == "one\n"
    # A character split across two events reaches the callback whole.
    assert "".join(out[1:3]) == "héllo 世\n" and "�" not in "".join(out[1:3])
    assert "".join(out) == result.stdout == CHATTY_STDOUT.decode("utf-8", "replace")
    assert result.stdout_bytes == CHATTY_STDOUT
    assert result.stderr == "warn\ndone\n"


@pytest.mark.parametrize("size", [1, 3, 7, 64, 1000, 4096, 65536])
async def test_chunk_boundaries_anywhere(sandbox: AsyncSandbox, server: FakeServer, size: int) -> None:
    """Lines and base64 payloads split at arbitrary byte positions decode the same."""
    server.programs.append(chatty)
    server.chunker = split_every(size)
    result = await sandbox.commands.run("x")
    assert result.stdout_bytes == CHATTY_STDOUT and result.stderr == "warn\ndone\n"
    assert not result.truncated


async def test_reconnect_resumes_at_the_offset(
    sandbox: AsyncSandbox, server: FakeServer, sleeps: list[float]
) -> None:
    server.programs.append(chatty)
    # Connection 1 (the POST) dies after "start" + 3 events and half a line;
    # connection 2 after one more event; connection 3 completes.
    server.cuts = [4, 1, None]
    seen: list[str] = []
    result = await sandbox.commands.run("x", on_stdout=seen.append)
    assert result.stdout_bytes == CHATTY_STDOUT  # complete, nothing twice
    assert result.stderr == "warn\ndone\n"
    assert "".join(seen) == result.stdout
    assert not result.truncated
    first_three = len(b"one\n") + len(b"warn\n") + 8
    assert server.stream_requests == [("p1", 0), ("p1", first_three), ("p1", first_three + 3)]
    resumed = server.seen("GET", r"/commands/p1/stream$")
    assert [r.url.params["from"] for r in resumed] == [str(first_three), str(first_three + 3)]
    assert all(r.headers["x-access-token"] == sandbox.access_token for r in resumed)
    assert len(sleeps) == 2  # one backoff per reconnect
    assert len(server.seen("POST", r"/commands$")) == 1  # the command was started once


async def test_reconnect_reports_a_gap(sandbox: AsyncSandbox, server: FakeServer) -> None:
    server.programs.append(chatty)
    server.cuts = [2, None]  # start + "one\n", then cut

    def lose_buffer(cmd: FakeCommand, connection: int) -> None:
        if connection == 2:
            cmd.ring_start = 1000  # the sandbox no longer holds bytes 4..999

    server.before_stream = lose_buffer
    handle = await sandbox.commands.run("x", background=True)
    server.stream_requests.clear()
    server.cuts = [1, None]
    events = [event async for event in handle]
    gaps = [e for e in events if isinstance(e, TruncatedEvent)]
    assert gaps == [TruncatedEvent(offset=1000, dropped=996)]
    result = await handle.wait()
    assert result.truncated and result.dropped_bytes == 996
    assert handle.dropped_bytes == 996
    assert server.stream_requests == [("p1", 0), ("p1", 4)]
    # What was delivered is exactly what the server still had.
    everything = b"".join(d for _, d in server.sandbox().commands["p1"].output)
    delivered = b"".join(e.data for e in events if isinstance(e, (StdoutEvent, StderrEvent)))
    assert delivered == everything[:4] + everything[1000:]


async def test_truncated_at_attach(sandbox: AsyncSandbox, server: FakeServer) -> None:
    def old(cmd: FakeCommand) -> None:
        cmd.output.append(("stdout", b"x" * 3000))
        cmd.ring_start = 2000
        cmd.exit = {"exit_code": 0, "reason": "exited"}

    server.programs.append(old)
    started = await sandbox.commands.run("x", background=True)
    handle = await sandbox.commands.connect(started.pid)
    result = await handle.wait()
    assert (result.truncated, result.dropped_bytes, len(result.stdout)) == (True, 2000, 1000)
    # Asking for what is still there is not a truncation.
    late = await sandbox.commands.connect(started.pid, from_offset=2500)
    result = await late.wait()
    assert (result.truncated, len(result.stdout)) == (False, 500)
    assert server.stream_requests[-1] == ("p1", 2500)


async def test_stream_gives_up_after_bounded_reconnects(
    sandbox: AsyncSandbox, server: FakeServer, sleeps: list[float]
) -> None:
    server.programs.append(chatty)
    handle = await sandbox.commands.run("x", background=True)
    server.cuts = [0] * 50  # every connection dies before a complete line
    with pytest.raises(sbx.StreamError, match="could not be resumed after 5 attempts at offset 0") as e:
        await handle.wait()
    assert isinstance(e.value, sbx.TransportError)
    assert len(server.stream_requests) == 6  # the first try and five reconnects
    assert len(sleeps) == 5
    # The handle is still usable: the next wait resumes where it stopped.
    server.cuts = []
    assert (await handle.wait()).stdout_bytes == CHATTY_STDOUT


async def test_progress_resets_the_reconnect_budget(sandbox: AsyncSandbox, server: FakeServer) -> None:
    def slow(cmd: FakeCommand) -> None:
        cmd.output += [("stdout", bytes([65 + i])) for i in range(20)]
        cmd.exit = {"exit_code": 0, "reason": "exited"}

    server.programs.append(slow)
    server.cuts = [2] + [1] * 19 + [None]  # one event per connection: 20 reconnects in total
    result = await sandbox.commands.run("x")
    assert result.stdout == "ABCDEFGHIJKLMNOPQRST"
    assert len(server.stream_requests) == 21


async def test_lost_before_the_command_id_is_known(sandbox: AsyncSandbox, server: FakeServer) -> None:
    server.cuts = [0]
    with pytest.raises(sbx.StreamError, match="before the server reported the command id"):
        await sandbox.commands.run("x")
    assert len(server.seen("POST", r"/commands$")) == 1  # never started twice


async def test_keepalives_and_a_stream_that_ends_early(sandbox: AsyncSandbox, server: FakeServer) -> None:
    def running(cmd: FakeCommand) -> None:
        cmd.output.append(("stdout", b"a"))

    def advance(cmd: FakeCommand, connection: int) -> None:
        if connection == 3:
            cmd.output.append(("stdout", b"b"))
        if connection == 4:
            cmd.exit = {"exit_code": 0, "reason": "exited"}

    server.programs.append(running)
    server.before_stream = advance
    # Each stream of a running command ends (cleanly) after a keepalive:
    # the client must treat that as a disconnect, not as the end.
    result = await sandbox.commands.run("x")
    assert result.stdout == "ab"
    assert server.stream_requests == [("p1", 0), ("p1", 1), ("p1", 1), ("p1", 2)]


async def test_error_event_ends_the_stream_with_the_mapped_error(
    sandbox: AsyncSandbox, server: FakeServer
) -> None:
    handle = await sandbox.commands.run("x", background=True)
    lines = [
        {"type": "stdout", "offset": 0, "data": base64.b64encode(b"ok").decode()},
        {"type": "from_the_future", "offset": 2},
        {
            "type": "error",
            "offset": 0,
            "error": envelope("sandbox_terminated", "gone", state="failed", reason="oom_killed")["error"],
        },
    ]
    body = b"".join(json.dumps(line).encode() + b"\n" for line in lines)
    server.inject("GET", r"/stream$", httpx.Response(200, stream=ChunkStream([body])))
    with pytest.raises(sbx.SandboxTerminatedError) as e:
        await handle.wait()
    assert (e.value.reason, e.value.state, e.value.status_code) == ("oom_killed", "failed", None)
    assert handle.stdout == "ok" and handle.offset == 2


async def test_sandbox_gone_while_reconnecting(sandbox: AsyncSandbox, server: FakeServer) -> None:
    server.programs.append(chatty)
    handle = await sandbox.commands.run("x", background=True)
    server.cuts = [1]
    fake = server.sandbox()

    def die(cmd: FakeCommand, connection: int) -> None:
        if connection == 1:
            fake.state, fake.reason = "destroyed", "idle_timeout"

    server.before_stream = die
    with pytest.raises(sbx.SandboxNotRunningError) as e:
        await handle.wait()
    assert e.value.reason == "idle_timeout"


async def test_background_handle(sandbox: AsyncSandbox, server: FakeServer) -> None:
    def running(cmd: FakeCommand) -> None:
        cmd.output.append(("stdout", b"ready\n"))

    server.programs.append(running)
    handle = await sandbox.commands.run("server", background=True, stdin=True, tag="srv")
    request = server.seen("POST", r"/commands$")[0]
    assert "wait" not in request.url.params
    assert handle.pid == "p1" and handle.result is None and handle.exit_code is None
    fake = server.sandbox().commands["p1"]

    await handle.send_stdin("hello ")
    await handle.send_stdin(b"world\n")
    assert bytes(fake.stdin) == b"hello world\n"
    assert server.seen("POST", r"/stdin$")[0].headers["content-type"] == "application/octet-stream"
    await handle.close_stdin()
    assert fake.stdin_closed
    with pytest.raises(sbx.ConflictError) as e:
        await handle.send_stdin("more")
    assert e.value.code == "stdin_closed"

    assert await handle.kill() is True
    assert fake.signals == ["SIGKILL"]
    assert await handle.kill() is False  # already exited
    with pytest.raises(sbx.CommandExitError) as x:
        await handle.wait()
    assert (x.value.exit_code, x.value.reason, x.value.signal, x.value.stdout) == (
        137,
        "killed",
        "SIGKILL",
        "ready\n",
    )
    assert handle.exit_code == 137
    # wait() is repeatable and does not read the stream again.
    n = len(server.stream_requests)
    with pytest.raises(sbx.CommandExitError):
        await handle.wait()
    assert len(server.stream_requests) == n


async def test_iterating_a_handle(sandbox: AsyncSandbox, server: FakeServer) -> None:
    server.programs.append(program(stdout=b"out", stderr=b"err", exit_code=2))
    handle = await sandbox.commands.run("x", background=True, check=False)
    events = [event async for event in handle]
    assert events == [
        StdoutEvent(data=b"out", offset=0),
        StderrEvent(data=b"err", offset=3),
        ExitEvent(exit=sbx.ExitStatus(exit_code=2, reason="exited"), offset=6),
    ]
    assert events[0].text == "out"  # type: ignore[union-attr]
    assert [event async for event in handle] == []  # finished: nothing more
    result = await handle.wait()
    assert (result.stdout, result.stderr, result.exit_code) == ("out", "err", 2)
    assert (handle.stdout, handle.stderr, handle.offset) == ("out", "err", 6)


async def test_disconnect_then_resume(sandbox: AsyncSandbox, server: FakeServer) -> None:
    server.programs.append(chatty)
    seen: list[str] = []
    handle = await sandbox.commands.run("x", background=True, on_stdout=seen.append)
    async for event in handle:
        assert event == StdoutEvent(data=b"one\n", offset=0)
        break
    await handle.disconnect()
    assert handle.offset == 4 and handle.result is None
    await handle.disconnect()  # harmless twice
    result = await handle.wait()
    assert server.stream_requests == [("p1", 0), ("p1", 4)]
    assert result.stdout_bytes == CHATTY_STDOUT
    # Callbacks given to run() saw what wait() read (the rest, after the manual iteration).
    assert "".join(seen) == CHATTY_STDOUT[4:].decode("utf-8", "replace")


async def test_wait_callbacks_override(sandbox: AsyncSandbox, server: FakeServer) -> None:
    server.programs.append(program(stdout=b"o", stderr=b"e"))
    handle = await sandbox.commands.run("x", background=True)
    out: list[str] = []
    err: list[str] = []
    await handle.wait(on_stdout=out.append, on_stderr=err.append)
    assert (out, err) == (["o"], ["e"])


async def test_pty_output_in_the_ndjson_stream(sandbox: AsyncSandbox, server: FakeServer) -> None:
    def tty(cmd: FakeCommand) -> None:
        cmd.output.append(("pty", b"$ ls\r\n"))
        cmd.exit = {"exit_code": 0, "reason": "exited"}

    server.programs.append(tty)
    handle = await sandbox.commands.run("x", background=True)
    events = [event async for event in handle]
    assert isinstance(events[0], sbx.PtyOutputEvent)
    assert (await handle.wait()).stdout == "$ ls\r\n"


async def test_list_get_connect_kill_by_pid(sandbox: AsyncSandbox, server: FakeServer) -> None:
    server.programs += [program(stdout=b"first"), idle]
    await sandbox.commands.run("echo first")
    await sandbox.commands.run("sleep", args=["60"], background=True, tag="sleeper", stdin=True)
    listed = await sandbox.commands.list()
    assert [(c.pid, c.running, c.tag) for c in listed] == [("p1", False, None), ("p2", True, "sleeper")]
    assert listed[0].exit == sbx.ExitStatus(exit_code=0, reason="exited") and listed[0].next_offset == 5
    assert listed[1].cmd == "sleep" and tuple(listed[1].args) == ("60",) and not listed[1].pty
    assert listed[1].started_at is not None and listed[1].raw["some_future_field"] == {"ignored": True}

    assert (await sandbox.commands.get("p2")).tag == "sleeper"
    with pytest.raises(sbx.CommandNotFoundError):
        await sandbox.commands.get("nope")
    with pytest.raises(sbx.CommandNotFoundError):
        await sandbox.commands.connect("nope")

    again = await sandbox.commands.connect("p1")
    assert (await again.wait()).stdout == "first"

    await sandbox.commands.send_stdin("p2", "data")
    await sandbox.commands.signal("p2", "SIGINT")
    fake = server.sandbox().commands["p2"]
    assert bytes(fake.stdin) == b"data" and fake.signals == ["SIGINT"]
    with pytest.raises(sbx.InvalidArgumentError):
        await sandbox.commands.signal("p2", "SIGWHAT")
    await sandbox.commands.close_stdin("p2")
    assert await sandbox.commands.kill("p2") is True
    assert await sandbox.commands.kill("p2") is False
    assert await sandbox.commands.kill("nope") is False
    # A missing sandbox is not "the command was not running": with a token it is a 401.
    with pytest.raises(sbx.AuthenticationError):
        await AsyncSandbox("sb-missing", sandbox._api).commands.kill("p1")
