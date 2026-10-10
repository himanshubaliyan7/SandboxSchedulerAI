"""Integration tests against a real sbxd.

Skipped unless ``SBX_LIVE_BASE_URL`` and ``SBX_LIVE_API_KEY`` are set
(``scripts/live_test.sh`` starts a server and sets them). Optional:
``SBX_LIVE_OTHER_API_KEY`` (a key of a second tenant) and
``SBX_LIVE_TEMPLATE`` (default ``python``; the image needs python3 and a
POSIX shell, and the template network mode ``allowlist``).
"""

from __future__ import annotations

import hashlib
import io
import os
import threading
import time
from collections.abc import Callable, Iterator

import pytest

import sbx
from sbx import AsyncSandbox, Sandbox

BASE_URL = os.environ.get("SBX_LIVE_BASE_URL", "")
API_KEY = os.environ.get("SBX_LIVE_API_KEY", "")
OTHER_KEY = os.environ.get("SBX_LIVE_OTHER_API_KEY", "")
TEMPLATE = os.environ.get("SBX_LIVE_TEMPLATE", "python")

pytestmark = [
    pytest.mark.live,
    pytest.mark.skipif(not (BASE_URL and API_KEY), reason="set SBX_LIVE_BASE_URL and SBX_LIVE_API_KEY"),
]


def create(**kwargs: object) -> Sandbox:
    return Sandbox.create(TEMPLATE, api_key=API_KEY, base_url=BASE_URL, **kwargs)  # type: ignore[arg-type]


def until(condition: Callable[[], bool], timeout: float = 15.0, step: float = 0.05) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if condition():
            return True
        time.sleep(step)
    return condition()


@pytest.fixture(scope="module")
def sandbox() -> Iterator[Sandbox]:
    with create(metadata={"suite": "sbx-python-live"}, envs={"GREETING": "hello from create"}) as sb:
        yield sb


class Screen:
    """Collects PTY output and lets a test wait for text to appear."""

    def __init__(self) -> None:
        self._data = bytearray()
        self._changed = threading.Condition()

    def feed(self, data: bytes) -> None:
        with self._changed:
            self._data += data
            self._changed.notify_all()

    @property
    def text(self) -> str:
        return self._data.decode("utf-8", "replace")

    def __len__(self) -> int:
        return len(self._data)

    def wait_for(self, needle: str, timeout: float = 15.0) -> None:
        deadline = time.monotonic() + timeout
        with self._changed:
            while needle not in self.text:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise AssertionError(f"{needle!r} did not appear; last output: {self.text[-300:]!r}")
                self._changed.wait(remaining)


# -- lifecycle ----------------------------------------------------------------


def test_create_and_info(sandbox: Sandbox) -> None:
    assert sandbox.sandbox_id.startswith("sb-")
    assert sandbox.access_token and sandbox.access_token.startswith("sbt_")
    info = sandbox.get_info()
    assert info.state == "running" and info.template_id == TEMPLATE
    assert info.metadata == {"suite": "sbx-python-live"}
    assert info.created_at is not None and info.started_at is not None
    assert sandbox.is_running()


def test_run_python(sandbox: Sandbox) -> None:
    result = sandbox.commands.run('python3 -c "print(6*7)"')
    assert (result.stdout, result.stderr, result.exit_code, result.reason) == ("42\n", "", 0, "exited")
    direct = sandbox.commands.run(
        "python3", args=["-c", "import os; print(os.getcwd(), os.environ['GREETING'], os.getuid())"]
    )
    assert direct.stdout == "/workspace hello from create 1000\n"
    assert sandbox.commands.run("echo $A-$PWD", envs={"A": "b"}, cwd="/tmp").stdout == "b-/tmp\n"


def test_stderr_and_non_zero_exit(sandbox: Sandbox) -> None:
    with pytest.raises(sbx.CommandExitError) as e:
        sandbox.commands.run("echo out; echo err >&2; exit 3")
    assert (e.value.exit_code, e.value.stdout, e.value.stderr, e.value.reason) == (
        3,
        "out\n",
        "err\n",
        "exited",
    )
    result = sandbox.commands.run("echo err >&2; exit 4", check=False)
    assert (result.exit_code, result.stderr, result.error) == (4, "err\n", "exit status 4")
    with pytest.raises(sbx.CommandStartError) as s:
        sandbox.commands.run("/no/such/binary", args=[])
    assert s.value.code == "command_not_found"


def test_streaming_callbacks(sandbox: Sandbox) -> None:
    out: list[tuple[float, str]] = []
    err: list[str] = []
    started = time.monotonic()
    result = sandbox.commands.run(
        "for i in 1 2 3; do echo line $i; echo warn $i >&2; sleep 0.3; done",
        on_stdout=lambda text: out.append((time.monotonic() - started, text)),
        on_stderr=err.append,
    )
    assert "".join(t for _, t in out) == result.stdout == "line 1\nline 2\nline 3\n"
    assert "".join(err) == result.stderr == "warn 1\nwarn 2\nwarn 3\n"
    # Delivered while the command ran, not all at the end.
    assert len(out) >= 2 and out[0][0] < 0.5 < out[-1][0]


def test_large_output_and_unicode(sandbox: Sandbox) -> None:
    code = "import sys; sys.stdout.write('世界é' * 100000)"
    result = sandbox.commands.run("python3", args=["-c", code], envs={"PYTHONIOENCODING": "utf-8"})
    assert result.stdout == "世界é" * 100000 and not result.truncated  # 800 kB, multi-byte characters


def test_background_wait_and_kill(sandbox: Sandbox) -> None:
    handle = sandbox.commands.run("echo started; sleep 60", background=True, tag="sleeper")
    assert handle.pid
    for event in handle:
        assert isinstance(event, sbx.StdoutEvent) and event.text == "started\n"
        break
    listed = {c.pid: c for c in sandbox.commands.list()}
    assert listed[handle.pid].running and listed[handle.pid].tag == "sleeper"
    assert handle.kill() is True
    with pytest.raises(sbx.CommandExitError) as e:
        handle.wait()
    assert (e.value.exit_code, e.value.reason, e.value.signal, e.value.stdout) == (
        137,
        "killed",
        "SIGKILL",
        "started\n",
    )
    assert until(lambda: handle.kill() is False, timeout=3)
    assert sandbox.commands.kill("no-such-pid") is False

    quick = sandbox.commands.run("echo done", background=True)
    assert quick.wait().stdout == "done\n"
    again = sandbox.commands.connect(quick.pid)  # replay after exit
    assert again.wait().stdout == "done\n"
    tail = sandbox.commands.connect(quick.pid, from_offset=2)
    assert tail.wait().stdout == "ne\n"


def test_stdin_round_trip(sandbox: Sandbox) -> None:
    handle = sandbox.commands.run("cat", args=[], background=True, stdin=True)
    handle.send_stdin("hello ")
    sandbox.commands.send_stdin(handle.pid, b"world\n")
    handle.close_stdin()
    assert handle.wait().stdout == "hello world\n"
    closed = sandbox.commands.run("cat", args=[])  # stdin is /dev/null by default
    assert closed.stdout == ""
    with pytest.raises(sbx.ConflictError):
        handle.send_stdin("too late")


def test_command_timeout(sandbox: Sandbox) -> None:
    started = time.monotonic()
    with pytest.raises(sbx.CommandTimeoutError) as e:
        sandbox.commands.run("echo before; sleep 30", timeout=1)
    assert time.monotonic() - started < 10
    assert (e.value.reason, e.value.stdout) == ("timeout", "before\n")
    assert sandbox.commands.run("echo still alive").stdout == "still alive\n"


def test_files(sandbox: Sandbox) -> None:
    files = sandbox.files
    assert files.write("notes/hello.txt", "héllo\n").size == 7
    assert files.read("notes/hello.txt") == "héllo\n"
    assert files.read("/workspace/notes/hello.txt", format="bytes") == "héllo\n".encode()
    assert sandbox.commands.run("cat notes/hello.txt").stdout == "héllo\n"

    blob = os.urandom(5 * 1024 * 1024 + 123)  # a few MB of binary, streamed both ways
    digest = hashlib.sha256(blob).hexdigest()
    assert files.write("data/blob.bin", io.BytesIO(blob), mode=0o600).size == len(blob)
    assert sandbox.commands.run("sha256sum data/blob.bin").stdout.split()[0] == digest
    assert files.read("data/blob.bin", format="bytes") == blob
    stream = files.read("data/blob.bin", format="stream")
    assert stream.size == len(blob)
    hasher, chunks = hashlib.sha256(), 0
    for chunk in stream:
        hasher.update(chunk)
        chunks += 1
    assert hasher.hexdigest() == digest and chunks > 1

    written = files.write_files([("multi/a.txt", "A"), {"path": "multi/b.bin", "data": b"\x00\x01"}])
    assert [w.size for w in written] == [1, 2]
    entries = {e.name: e for e in files.list("data")}
    assert (
        entries["blob.bin"].size == len(blob)
        and entries["blob.bin"].mode == "0600"
        and not entries["blob.bin"].is_dir
    )
    assert (
        entries["blob.bin"].path == "/workspace/data/blob.bin"
        and entries["blob.bin"].modified_time is not None
    )
    assert {e.name for e in files.list()} >= {"notes", "data", "multi"}
    assert all(e.is_dir for e in files.list() if e.name in ("notes", "data", "multi"))

    assert files.make_dir("made/deep/er") is True
    assert files.make_dir("made/deep/er") is False
    assert files.make_dir("made") is False
    info = files.get_info("made/deep")
    assert (info.name, info.path, info.is_dir) == ("deep", "/workspace/made/deep", True)
    blob = files.get_info("data/blob.bin")
    assert not blob.is_dir and blob.size > 0
    with pytest.raises(sbx.PathNotFoundError):
        files.get_info("made/nope")
    assert (
        files.exists("made/deep/er")
        and files.exists("data/blob.bin")
        and files.exists("/workspace")
        and files.exists(".")
    )
    assert (
        not files.exists("data/nope")
        and not files.exists("nope/x")
        and not files.exists("data/blob.bin/child")
    )

    files.remove("data/blob.bin")
    assert not files.exists("data/blob.bin")
    files.remove("multi")
    assert not files.exists("multi")
    with pytest.raises(sbx.PathNotFoundError):
        files.read("data/blob.bin")
    with pytest.raises(sbx.PathNotFoundError):
        files.remove("data/blob.bin")
    with pytest.raises(sbx.PathNotFoundError):
        files.list("nope")
    with pytest.raises(sbx.ConflictError):
        files.read("made")
    with pytest.raises(sbx.NoSpaceError):  # /workspace is a 64 MiB tmpfs
        files.write("too-big.bin", io.BytesIO(b"\0" * (70 * 1024 * 1024)))
    assert sandbox.commands.run("echo ok").stdout == "ok\n"


def test_pty_session(sandbox: Sandbox) -> None:
    screen = Screen()
    gaps: list[tuple[int, int]] = []
    handle = sandbox.pty.create(
        80, 24, screen.feed, envs={"PS1": "", "TERM": "xterm"}, on_truncated=lambda o, n: gaps.append((o, n))
    )
    assert any(c.pid == handle.pid and c.pty for c in sandbox.commands.list())

    handle.send_input("stty size\n")
    screen.wait_for("24 80")
    handle.resize(100, 40)
    handle.send_input("stty size\n")
    screen.wait_for("40 100")
    sandbox.pty.resize(handle.pid, 132, 43)  # the REST route, without the socket
    handle.send_input("stty size\n")
    screen.wait_for("43 132")

    # A flood of output, interrupted with Ctrl+C.
    before = len(screen)
    handle.send_input("yes\n")
    assert until(lambda: len(screen) - before > 300_000)
    handle.send_input(b"\x03")
    handle.send_input("echo interrupted-$?\n")
    screen.wait_for("interrupted-130")
    assert gaps == []  # flow control: nothing was dropped although `yes` outruns any reader

    handle.send_input("exit 7\n")
    result = handle.wait(timeout=15)
    assert (result.exit_code, result.reason, result.truncated) == (7, "exited", False)
    assert handle.offset == len(screen) and handle.reconnects == 0
    with pytest.raises(sbx.StreamError):
        handle.send_input("late")


def test_pty_reattach_and_kill(sandbox: Sandbox) -> None:
    first = Screen()
    handle = sandbox.pty.create(80, 24, first.feed, cmd="python3", args=["-q"])
    handle.send_input("print('a' * 3)\n")
    first.wait_for("aaa")
    handle.disconnect()
    seen = handle.offset

    replay = Screen()
    second = sandbox.pty.connect(handle.pid, replay.feed)  # everything so far is replayed
    replay.wait_for("aaa")
    second.send_input("print('b' * 3)\n")
    replay.wait_for("bbb")
    second.disconnect()

    tail = Screen()
    third = sandbox.pty.connect(handle.pid, tail.feed, from_offset=seen)  # only what came after
    tail.wait_for("bbb")
    assert "aaa" not in tail.text
    sandbox.pty.send_input(handle.pid, "print('c' * 3)\n")
    tail.wait_for("ccc")
    assert third.kill() is True
    result = third.wait(timeout=15)
    assert (result.exit_code, result.reason) == (137, "killed")
    with pytest.raises(sbx.CommandNotFoundError):
        sandbox.pty.connect("no-such-pid", lambda data: None)


def test_output_beyond_the_replay_buffer_is_reported(sandbox: Sandbox) -> None:
    """The sandbox keeps 1 MiB per command; attaching later must say what is gone."""
    total = 3 * 1024 * 1024
    handle = sandbox.commands.run(
        "python3", args=["-c", f"import sys; sys.stdout.write('x' * {total})"], background=True
    )
    assert until(lambda: not sandbox.commands.get(handle.pid).running)
    assert sandbox.commands.get(handle.pid).next_offset == total

    late = sandbox.commands.connect(handle.pid)
    events = list(late)
    gaps = [e for e in events if isinstance(e, sbx.TruncatedEvent)]
    assert len(gaps) == 1 and gaps[0].dropped == gaps[0].offset >= total - 1024 * 1024
    result = late.wait()
    assert result.truncated and result.dropped_bytes == gaps[0].dropped
    assert result.dropped_bytes + len(result.stdout) == total and late.offset == total

    received: list[bytes] = []
    lost: list[tuple[int, int]] = []
    attached = sandbox.pty.connect(
        handle.pid, received.append, on_truncated=lambda offset, n: lost.append((offset, n))
    )
    outcome = attached.wait(timeout=15)
    assert outcome.exit_code == 0 and outcome.truncated
    assert lost == [(gaps[0].offset, gaps[0].dropped)]
    assert sum(len(chunk) for chunk in received) + outcome.dropped_bytes == total


def test_events_and_stats(sandbox: Sandbox) -> None:
    sandbox.commands.run("echo recorded", tag="rec")
    stats = sandbox.stats()
    assert stats.memory_bytes > 0 and stats.pids_current >= 1 and stats.oom_kills == 0

    def finished() -> list[sbx.SandboxEvent]:
        return [e for e in sandbox.events(type="command_finished") if e.data.get("tag") == "rec"]

    assert until(lambda: bool(finished()))
    record = finished()[0]
    assert record.data["exit_code"] == 0 and record.data["stdout_bytes"] == 9 and record.at is not None
    types = [e.type for e in sandbox.events()]
    assert types[0] == "claimed" and "command_started" in types


def test_list(sandbox: Sandbox) -> None:
    mine = Sandbox.list(metadata={"suite": "sbx-python-live"}, api_key=API_KEY, base_url=BASE_URL)
    assert sandbox.sandbox_id in [s.sandbox_id for s in mine]
    assert all(s.state in ("running", "assigned") for s in mine)
    assert Sandbox.list(metadata={"suite": "nobody"}, api_key=API_KEY, base_url=BASE_URL) == []
    assert sandbox.sandbox_id in [
        s.sandbox_id
        for s in Sandbox.list(state="all", template=TEMPLATE, limit=1000, api_key=API_KEY, base_url=BASE_URL)
    ]
    templates = Sandbox.list_templates(api_key=API_KEY, base_url=BASE_URL)
    assert TEMPLATE in [t.template_id for t in templates]


def test_connect_from_a_second_client_with_id_and_token(
    sandbox: Sandbox, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.delenv("SBX_API_KEY", raising=False)
    assert sandbox.access_token
    sandbox.files.write("shared.txt", "from the first client")
    other = Sandbox.connect(sandbox.sandbox_id, access_token=sandbox.access_token, base_url=BASE_URL)
    assert other.files.read("shared.txt") == "from the first client"
    assert other.commands.run("echo second").stdout == "second\n"
    assert other.is_running()
    with pytest.raises(sbx.ConfigurationError):
        other.get_info()  # the token is not a control-plane credential
    other.close()
    with pytest.raises(sbx.AuthenticationError):
        Sandbox.connect(sandbox.sandbox_id, access_token="sbt_" + "0" * 64, base_url=BASE_URL)
    with pytest.raises(sbx.AuthenticationError):
        Sandbox.connect("sb-0000000000000000", access_token=sandbox.access_token, base_url=BASE_URL)
    with pytest.raises(sbx.AuthenticationError):
        Sandbox.list(api_key="sbx_wrong_key", base_url=BASE_URL)


@pytest.mark.skipif(not OTHER_KEY, reason="set SBX_LIVE_OTHER_API_KEY to a second tenant's key")
def test_another_tenant_gets_not_found(sandbox: Sandbox) -> None:
    with pytest.raises(sbx.SandboxNotFoundError):
        Sandbox.connect(sandbox.sandbox_id, api_key=OTHER_KEY, base_url=BASE_URL)
    assert sandbox.sandbox_id not in [
        s.sandbox_id for s in Sandbox.list(state="all", api_key=OTHER_KEY, base_url=BASE_URL)
    ]
    theirs = Sandbox.connect(sandbox.sandbox_id, api_key=API_KEY, base_url=BASE_URL)
    theirs._api.config = sbx.ConnectionConfig.resolve(api_key=OTHER_KEY, base_url=BASE_URL)
    for call in (
        theirs.get_info,
        theirs.kill,
        theirs.stats,
        theirs.commands.list,
        lambda: theirs.files.read("shared.txt"),
    ):
        try:
            outcome = call()
        except sbx.SandboxNotFoundError:
            continue
        assert outcome is False, call  # kill() reports a missing sandbox as False
    assert sandbox.is_running()


def test_errors_from_the_real_server(sandbox: Sandbox) -> None:
    with pytest.raises(sbx.TemplateNotFoundError):
        Sandbox.create("no-such-template", api_key=API_KEY, base_url=BASE_URL)
    with pytest.raises(sbx.InvalidArgumentError) as e:
        create(timeout=10 * 365 * 24 * 3600)
    assert e.value.code == "invalid_timeout" and e.value.details["max_timeout_ms"] > 0 and e.value.request_id
    with pytest.raises(sbx.InvalidArgumentError) as n:
        create(network="full")
    assert n.value.code == "network_not_allowed"
    with pytest.raises(sbx.CommandNotFoundError):
        sandbox.commands.connect("no-such-pid")
    with pytest.raises(sbx.InvalidArgumentError):
        sandbox.commands.signal(sandbox.commands.run("sleep 5", background=True).pid, "SIGWHAT")


def test_network_none_and_allowlist() -> None:
    fetch = "import urllib.request; urllib.request.urlopen('http://example.com', timeout=5)"
    with create(network={"mode": "allowlist", "allow_domains": ["pypi.org"]}) as allow:
        info = allow.get_info()
        assert (
            info.network is not None
            and info.network.mode == "allowlist"
            and list(info.network.allow_domains) == ["pypi.org"]
        )
        proxy_env = allow.commands.run("env | grep -i _proxy || true").stdout
        assert "http" in proxy_env.lower()  # commands are pointed at the egress proxy
        denied = allow.commands.run("python3", args=["-c", fetch], check=False)
        assert denied.exit_code != 0 and "403" in denied.stderr  # example.com is not on the list
        updated = allow.update_network(
            sbx.NetworkPolicy(mode="allowlist", allow_domains=["example.com", "*.example.org"])
        )
        assert updated.network is not None and list(updated.network.allow_domains) == [
            "example.com",
            "*.example.org",
        ]
        assert allow.update_network("none").network == sbx.NetworkPolicy(mode="none")
        with pytest.raises(sbx.InvalidArgumentError):
            allow.update_network("full")
    with create(network="none") as none:
        assert none.commands.run("env | grep -i _proxy || true").stdout == ""
        assert none.commands.run("python3", args=["-c", fetch], check=False).exit_code != 0
        with pytest.raises(sbx.ConflictError) as e:
            none.update_network("allowlist")
        assert e.value.code == "network_not_attached"


def test_set_timeout_and_expiry() -> None:
    sb = create(timeout=300)
    try:
        first = sb.get_info().expires_at
        assert first is not None
        longer = sb.set_timeout(600).expires_at
        assert longer is not None and (longer - first).total_seconds() > 200
        handle = sb.commands.run("sleep 60", background=True)
        soon = sb.set_timeout(1.5).expires_at
        assert soon is not None and soon < first
        assert until(lambda: not sb.is_running(), timeout=20)
        info = sb.get_info()
        assert info.reason == "max_lifetime" and info.state in ("recycling", "destroyed")
        with pytest.raises(sbx.SandboxNotRunningError) as e:
            sb.commands.run("echo too late")
        assert e.value.reason == "max_lifetime" and e.value.state in ("recycling", "destroyed")
        with pytest.raises(sbx.SandboxNotRunningError):
            handle.wait()
        with pytest.raises(sbx.SandboxNotRunningError):
            sb.set_timeout(60)
    finally:
        sb.kill()
        sb.close()


def test_kill() -> None:
    sb = create()
    token = sb.access_token
    assert token and sb.commands.run("echo alive").stdout == "alive\n"
    assert sb.kill() is True
    assert sb.is_running() is False  # immediately: the sandbox is fenced before the 204
    assert until(lambda: sb.get_info().state == "destroyed")  # the container goes in the background
    assert sb.get_info().reason == "released"
    assert sb.kill() is True and sb.kill(wait=True) is True  # idempotent
    with pytest.raises(sbx.SandboxNotRunningError) as e:
        sb.commands.run("echo dead")
    assert e.value.reason == "released"
    with pytest.raises(sbx.SandboxNotRunningError):  # the token of a deleted sandbox opens nothing
        Sandbox.connect(sb.sandbox_id, access_token=token, base_url=BASE_URL)
    assert sb.sandbox_id not in [s.sandbox_id for s in Sandbox.list(api_key=API_KEY, base_url=BASE_URL)]
    sb.close()

    waited = create()
    assert waited.kill(wait=True) is True
    assert waited.get_info().state == "destroyed"
    assert Sandbox("sb-0000000000000000", waited._api).kill() is False
    waited.close()


def test_context_manager_kills() -> None:
    with create() as sb:
        sandbox_id = sb.sandbox_id
        assert sb.commands.run("echo in").stdout == "in\n"
    listed = Sandbox.list(state="all", api_key=API_KEY, base_url=BASE_URL, limit=1000)
    assert {s.sandbox_id: s.state for s in listed}[sandbox_id] in ("recycling", "destroyed")


async def test_async_client_smoke() -> None:
    async with await AsyncSandbox.create(TEMPLATE, api_key=API_KEY, base_url=BASE_URL) as sb:
        out: list[str] = []

        async def on_stdout(text: str) -> None:
            out.append(text)

        result = await sb.commands.run('python3 -c "print(6*7)"', on_stdout=on_stdout)
        assert result.stdout == "42\n" and "".join(out) == "42\n"
        with pytest.raises(sbx.CommandExitError):
            await sb.commands.run("exit 2")

        await sb.files.write("a.bin", io.BytesIO(b"\x00\xff" * 500_000))
        assert await sb.files.read("a.bin", format="bytes") == b"\x00\xff" * 500_000
        assert [e.name for e in await sb.files.list()] == ["a.bin"]

        handle = await sb.commands.run("echo bg; sleep 30", background=True)
        async for event in handle:
            assert isinstance(event, sbx.StdoutEvent)
            break
        assert await handle.kill()
        assert (await sb.commands.connect(handle.pid, check=False)).pid == handle.pid

        screen = Screen()
        pty = await sb.pty.create(90, 30, screen.feed, envs={"PS1": ""})
        await pty.send_input("stty size; exit 5\n")
        assert (await pty.wait(timeout=15)).exit_code == 5 and "30 90" in screen.text

        other = await AsyncSandbox.connect(sb.sandbox_id, access_token=sb.access_token, base_url=BASE_URL)
        assert (await other.commands.run("echo token")).stdout == "token\n"
        await other.close()
        assert (await sb.get_info()).state == "running" and await sb.is_running()
        sandbox_id = sb.sandbox_id
    listed = await AsyncSandbox.list(state="all", limit=1000, api_key=API_KEY, base_url=BASE_URL)
    assert {s.sandbox_id: s.state for s in listed}[sandbox_id] in ("recycling", "destroyed")


def test_kill_by_id_and_pty_close_code() -> None:
    sb = create()
    try:
        handle = sb.pty.create(80, 24, lambda data: None)
        # Sandbox.kill(id): the class form, with nothing but the id and the API key.
        assert Sandbox.kill(sb.sandbox_id, api_key=API_KEY, base_url=BASE_URL) is True
        # The open terminal is told why it ended, by status frame and close code 4000.
        with pytest.raises(sbx.SandboxTerminatedError) as e:
            handle.wait(timeout=15)
        assert e.value.code == "sandbox_terminated" and e.value.reason == "released"
        assert handle.reconnects == 0
        assert Sandbox.kill("sb-0000000000000000", api_key=API_KEY, base_url=BASE_URL) is False
    finally:
        sb.close()


def test_stream_headers_idempotency_and_cors(sandbox: Sandbox) -> None:
    import httpx

    token = sandbox.access_token or ""
    base = f"{BASE_URL}/v1/sandboxes/{sandbox.sandbox_id}"
    handle = sandbox.commands.run("sleep 2; echo late", background=True)
    with httpx.Client(timeout=httpx.Timeout(10, read=1.0)) as http:
        # The headers of a silent command's stream arrive at once (read timeout 1 s < 2 s of silence).
        started = time.monotonic()
        with http.stream(
            "GET", f"{base}/commands/{handle.pid}/stream", headers={"X-Access-Token": token}
        ) as r:
            took = time.monotonic() - started
            assert r.status_code == 200
        print(f"[measured] stream headers of a command silent for 2 s: {took * 1000:.0f} ms")
        assert took < 0.5

        # CORS: the script starts the server with --cors-allowed-origins http://app.sbx.test.
        ok = http.get(f"{base}/commands", headers={"X-Access-Token": token, "Origin": "http://app.sbx.test"})
        assert (
            ok.status_code == 200 and ok.headers.get("access-control-allow-origin") == "http://app.sbx.test"
        )
        assert "access-control-allow-credentials" not in ok.headers
        bad = http.get(f"{base}/commands", headers={"X-Access-Token": token, "Origin": "http://evil.test"})
        assert bad.status_code == 403 and "access-control-allow-origin" not in bad.headers
        key = http.get(f"{base}/commands", headers={"X-API-Key": API_KEY, "Origin": "http://app.sbx.test"})
        assert key.status_code == 403 and key.json()["error"]["code"] == "api_key_not_allowed"
        pre = http.request(
            "OPTIONS",
            f"{base}/commands",
            headers={"Origin": "http://app.sbx.test", "Access-Control-Request-Method": "POST"},
        )
        assert pre.status_code == 204 and "X-Access-Token" in pre.headers["access-control-allow-headers"]
    handle.wait()

    # A repeated start with the same key attaches to the first command.
    key_ = f"live-{time.time_ns()}"
    first = sandbox.commands.run("echo once; sleep 0.3", idempotency_key=key_)
    again = sandbox.commands.run("echo once; sleep 0.3", idempotency_key=key_)
    assert again.pid == first.pid and again.stdout == "once\n"
    with pytest.raises(sbx.ConflictError) as e:
        sandbox.commands.run("echo other", idempotency_key=key_)
    assert e.value.code == "idempotency_key_reused"
