# Generated from src/sbx/_async/commands.py by scripts/unasync.py. Do not edit.
"""Commands: run, stream, reconnect, signal."""

from __future__ import annotations

import codecs
from builtins import list as List  # the classes below define a method named ``list``
from collections.abc import Iterator, Mapping, Sequence
from typing import Any, Literal, overload

import httpx

from .._config import describe, ms, seg
from .._ndjson import NDJSONDecoder
from .._stream import StreamTracker
from ..errors import (
    CommandExitError,
    CommandNotFoundError,
    CommandTimeoutError,
    ConflictError,
    SandboxTimeoutError,
    StreamError,
    TransportError,
)
from ..models import (
    CommandEvent,
    CommandInfo,
    CommandResult,
    ExitEvent,
    StderrEvent,
    TruncatedEvent,
)
from ._compat import TextHandler, invoke, sleep
from .api import ApiClient, decode_json


def raise_for_exit(result: CommandResult) -> None:
    if result.reason == "timeout":
        raise CommandTimeoutError(result)
    if result.exit_code != 0:
        raise CommandExitError(result)


class CommandHandle:
    """A command in a sandbox, running or finished.

    Iterate over the handle to receive its output events as they happen, or
    call :meth:`wait` to collect everything until it exits. The handle
    remembers how far it has read: if the connection breaks, it reconnects
    and resumes at that offset; output that the sandbox no longer has is
    reported as a :class:`~sbx.TruncatedEvent` and in
    ``CommandResult.truncated`` rather than skipped silently.
    """

    def __init__(
        self,
        api: ApiClient,
        sandbox_id: str,
        *,
        info: CommandInfo | None = None,
        pid: str | None = None,
        offset: int = 0,
        on_stdout: TextHandler | None = None,
        on_stderr: TextHandler | None = None,
        check: bool = True,
        response: httpx.Response | None = None,
    ) -> None:
        self._api = api
        self._sandbox_id = sandbox_id
        self._info = info
        self._pid = info.pid if info is not None else pid
        self._tracker = StreamTracker(offset)
        self._on_stdout = on_stdout
        self._on_stderr = on_stderr
        self._check = check
        self._pending = response  # the response of POST ?wait=stream, read first
        self._active: httpx.Response | None = None
        self._gen: Iterator[CommandEvent] | None = None
        self._disconnected = False
        self._stdout: List[bytes] = []
        self._stderr: List[bytes] = []
        self._decoders = (
            codecs.getincrementaldecoder("utf-8")("replace"),
            codecs.getincrementaldecoder("utf-8")("replace"),
        )
        self._result: CommandResult | None = None

    def __repr__(self) -> str:
        return f"<{type(self).__name__} pid={self._pid!r} sandbox={self._sandbox_id!r} offset={self.offset}>"

    @property
    def pid(self) -> str:
        """The command id (an opaque string issued by the sandbox, not a kernel pid)."""
        if self._pid is None:
            raise StreamError("the command id is not known yet")
        return self._pid

    @property
    def offset(self) -> int:
        """How many bytes of the command's output this handle has consumed."""
        return self._tracker.offset

    @property
    def dropped_bytes(self) -> int:
        """Bytes of output that were lost before this handle could read them."""
        return self._tracker.dropped

    @property
    def result(self) -> CommandResult | None:
        """The result once the exit event has been read, else ``None``."""
        return self._result

    @property
    def exit_code(self) -> int | None:
        return self._result.exit_code if self._result is not None else None

    @property
    def stdout(self) -> str:
        """Stdout consumed so far."""
        return b"".join(self._stdout).decode("utf-8", "replace")

    @property
    def stderr(self) -> str:
        """Stderr consumed so far."""
        return b"".join(self._stderr).decode("utf-8", "replace")

    def _path(self, suffix: str = "") -> str:
        return f"/v1/sandboxes/{seg(self._sandbox_id)}/commands/{seg(self.pid)}{suffix}"

    def _record(self, event: CommandEvent) -> None:
        if isinstance(event, ExitEvent):
            out, err = b"".join(self._stdout), b"".join(self._stderr)
            self._result = CommandResult(
                pid=self._pid or "",
                exit_code=event.exit.exit_code,
                reason=event.exit.reason,
                signal=event.exit.signal,
                stdout=out.decode("utf-8", "replace"),
                stderr=err.decode("utf-8", "replace"),
                stdout_bytes=out,
                stderr_bytes=err,
                truncated=self._tracker.dropped > 0,
                dropped_bytes=self._tracker.dropped,
            )
        elif isinstance(event, StderrEvent):
            self._stderr.append(event.data)
        elif not isinstance(event, TruncatedEvent):
            self._stdout.append(event.data)  # stdout, or the merged output of a PTY

    def _events(self) -> Iterator[CommandEvent]:
        """Read events from the current offset until the command exits.

        Reconnects with ``from=<offset>`` when the stream is cut, at most
        ``max_reconnects`` times without any progress in between.
        """
        config = self._api.config
        failures = 0
        while self._result is None:
            response, self._pending = self._pending, None
            cause: BaseException | None = None
            if response is None:
                if self._pid is None:
                    raise StreamError(
                        "the connection was lost before the server reported the command id; "
                        "the command may be running (commands.list() shows it)"
                    )
                try:
                    response = self._api.open_stream(
                        "GET",
                        self._path("/stream"),
                        plane="data",
                        params={"from": self._tracker.offset},
                        idempotent=True,
                    )
                except (TransportError, SandboxTimeoutError) as e:
                    cause = e
            if response is not None:
                self._active = response
                try:
                    decoder = NDJSONDecoder()
                    for chunk in response.iter_bytes():
                        for obj in decoder.feed(chunk):
                            before = self._tracker.offset, self._tracker.keepalives
                            events = self._tracker.accept(obj)
                            if self._pid is None and self._tracker.command is not None:
                                self._info = self._tracker.command
                                self._pid = self._tracker.command.pid
                            if events or before != (self._tracker.offset, self._tracker.keepalives):
                                failures = 0  # the connection works
                            for event in events:
                                self._record(event)
                                yield event
                            if self._result is not None:
                                return
                except (httpx.TransportError, httpx.StreamError) as e:
                    cause = e
                finally:
                    self._active = None
                    response.close()
            if self._disconnected:
                return
            failures += 1
            if failures > config.max_reconnects:
                why = describe(cause) if cause is not None else "the stream ended without an exit event"
                raise StreamError(
                    f"the output stream of command {self._pid} broke and could not be resumed "
                    f"after {config.max_reconnects} attempts at offset {self._tracker.offset}: {why}"
                ) from cause
            sleep(config.retry.backoff(failures))

    def __iter__(self) -> CommandHandle:
        return self

    def __next__(self) -> CommandEvent:
        if self._gen is None:
            self._disconnected = False
            self._gen = self._events()
        try:
            return self._gen.__next__()
        except BaseException:
            # Finished or failed: a later iteration starts a fresh stream at
            # the offset reached so far.
            self._gen = None
            raise

    def wait(
        self,
        on_stdout: TextHandler | None = None,
        on_stderr: TextHandler | None = None,
    ) -> CommandResult:
        """Read until the command exits and return its result.

        Raises :class:`~sbx.CommandExitError` for a non-zero exit status and
        :class:`~sbx.CommandTimeoutError` when the per-command timeout killed
        it, unless the command was started with ``check=False``.
        """
        on_stdout = on_stdout or self._on_stdout
        on_stderr = on_stderr or self._on_stderr
        if self._result is None:
            for event in self:
                if isinstance(event, (ExitEvent, TruncatedEvent)):
                    continue
                is_err = isinstance(event, StderrEvent)
                handler = on_stderr if is_err else on_stdout
                if handler is None:
                    continue
                text = self._decoders[1 if is_err else 0].decode(event.data)
                if text:
                    invoke(handler, text)
        if self._result is None:
            raise StreamError(f"disconnected from command {self._pid} before it exited")
        if self._check:
            raise_for_exit(self._result)
        return self._result

    def disconnect(self) -> None:
        """Stop receiving output. The command keeps running; iterate or ``wait()`` again to resume."""
        self._disconnected = True
        gen, self._gen = self._gen, None
        pending, self._pending = self._pending, None
        for response in (self._active, pending):
            if response is not None:
                response.close()
        close = getattr(gen, "close", None)
        if close is not None:
            try:
                close()
            except (RuntimeError, ValueError):
                pass  # it is being iterated right now; closing the response ends it

    def kill(self) -> bool:
        """SIGKILL the command's process group. ``False`` if it had already exited."""
        return Commands(self._api, self._sandbox_id).kill(self.pid)

    def send_stdin(self, data: str | bytes) -> None:
        Commands(self._api, self._sandbox_id).send_stdin(self.pid, data)

    def close_stdin(self) -> None:
        Commands(self._api, self._sandbox_id).close_stdin(self.pid)


class Commands:
    """``sandbox.commands``: start and manage commands."""

    def __init__(self, api: ApiClient, sandbox_id: str) -> None:
        self._api = api
        self._sandbox_id = sandbox_id

    def _path(self, pid: str | None = None, suffix: str = "") -> str:
        base = f"/v1/sandboxes/{seg(self._sandbox_id)}/commands"
        return base if pid is None else f"{base}/{seg(pid)}{suffix}"

    def _start_body(
        self,
        cmd: str,
        args: Sequence[str] | None,
        envs: Mapping[str, str] | None,
        cwd: str | None,
        timeout: float | None,
        stdin: bool,
        tag: str | None,
    ) -> dict[str, Any]:
        if not cmd:
            raise ValueError("cmd must not be empty")
        body: dict[str, Any]
        if args is None:
            # E2B-style: a command line, interpreted by a shell.
            shell = List(self._api.config.shell)
            body = {"cmd": shell[0], "args": [*shell[1:], cmd]}
        else:
            if isinstance(args, str):
                raise TypeError("args must be a sequence of strings, not a string")
            body = {"cmd": cmd, "args": List(args)}
        if envs:
            body["env"] = dict(envs)
        if cwd:
            body["cwd"] = cwd
        timeout_ms = ms(timeout)
        if timeout_ms:
            body["timeout_ms"] = timeout_ms
        if stdin:
            body["stdin"] = True
        if tag:
            body["tag"] = tag
        return body

    @overload
    def run(
        self,
        cmd: str,
        *,
        args: Sequence[str] | None = None,
        envs: Mapping[str, str] | None = None,
        cwd: str | None = None,
        timeout: float | None = None,
        on_stdout: TextHandler | None = None,
        on_stderr: TextHandler | None = None,
        background: Literal[False] = False,
        stdin: bool = False,
        tag: str | None = None,
        check: bool = True,
    ) -> CommandResult: ...

    @overload
    def run(
        self,
        cmd: str,
        *,
        args: Sequence[str] | None = None,
        envs: Mapping[str, str] | None = None,
        cwd: str | None = None,
        timeout: float | None = None,
        on_stdout: TextHandler | None = None,
        on_stderr: TextHandler | None = None,
        background: Literal[True],
        stdin: bool = False,
        tag: str | None = None,
        check: bool = True,
    ) -> CommandHandle: ...

    def run(
        self,
        cmd: str,
        *,
        args: Sequence[str] | None = None,
        envs: Mapping[str, str] | None = None,
        cwd: str | None = None,
        timeout: float | None = None,
        on_stdout: TextHandler | None = None,
        on_stderr: TextHandler | None = None,
        background: bool = False,
        stdin: bool = False,
        tag: str | None = None,
        check: bool = True,
    ) -> CommandResult | CommandHandle:
        """Run a command in the sandbox.

        Args:
            cmd: without ``args``, a shell command line run by ``/bin/sh -c``
                (pipes, ``&&`` and quoting work as in E2B). With ``args``,
                the executable, started directly with exactly those
                arguments and no shell.
            args: argument vector; switches the shell off.
            envs: extra environment variables for this command.
            cwd: working directory (default ``/workspace``).
            timeout: wall-clock limit in seconds, enforced by the sandbox:
                the process group is killed and the reason is ``timeout``.
                ``None`` or 0 means no limit.
            on_stdout, on_stderr: called with decoded text as it arrives.
            background: return a handle as soon as the command has started
                instead of waiting for it.
            stdin: keep stdin open so that ``send_stdin`` works; by default
                stdin is ``/dev/null``.
            tag: optional name, unique among the sandbox's running commands.
            check: raise :class:`~sbx.CommandExitError` on a non-zero exit
                status (E2B's behaviour). With ``check=False`` the result is
                returned whatever the status.
        """
        body = self._start_body(cmd, args, envs, cwd, timeout, stdin, tag)
        if background:
            # A 429 is answered before the command exists, so it is retried;
            # nothing else is, because the command may have started.
            response = self._api.request("POST", self._path(), plane="data", json=body)
            return CommandHandle(
                self._api,
                self._sandbox_id,
                info=CommandInfo.from_json(decode_json(response)),
                on_stdout=on_stdout,
                on_stderr=on_stderr,
                check=check,
            )
        stream = self._api.open_stream(
            "POST", self._path(), plane="data", params={"wait": "stream"}, json=body
        )
        handle = CommandHandle(
            self._api,
            self._sandbox_id,
            on_stdout=on_stdout,
            on_stderr=on_stderr,
            check=check,
            response=stream,
        )
        try:
            return handle.wait()
        finally:
            handle.disconnect()

    def list(self) -> List[CommandInfo]:
        """Running and exited commands of the sandbox, in start order."""
        data = self._api.json("GET", self._path(), plane="data", idempotent=True)
        items = data.get("commands")
        return (
            [CommandInfo.from_json(c) for c in items if isinstance(c, Mapping)]
            if isinstance(items, List)
            else []
        )

    def get(self, pid: str) -> CommandInfo:
        """Look up one command."""
        return CommandInfo.from_json(self._api.json("GET", self._path(pid), plane="data", idempotent=True))

    def connect(
        self,
        pid: str,
        *,
        on_stdout: TextHandler | None = None,
        on_stderr: TextHandler | None = None,
        from_offset: int = 0,
        check: bool = True,
    ) -> CommandHandle:
        """Attach to a command started earlier (also after it has exited).

        Output is replayed from ``from_offset``; what the sandbox's 1 MiB
        replay buffer no longer holds is reported as truncated.
        """
        info = self.get(pid)
        return CommandHandle(
            self._api,
            self._sandbox_id,
            info=info,
            offset=from_offset,
            on_stdout=on_stdout,
            on_stderr=on_stderr,
            check=check,
        )

    def signal(self, pid: str, signal: str) -> None:
        """Send ``SIGINT``, ``SIGTERM``, ``SIGKILL``, ``SIGHUP`` or ``SIGQUIT`` to the process group."""
        self._api.request("POST", self._path(pid, "/signal"), plane="data", json={"signal": signal})

    def kill(self, pid: str) -> bool:
        """SIGKILL a command. ``False`` if it does not exist or has already exited."""
        try:
            self.signal(pid, "SIGKILL")
        except CommandNotFoundError:
            return False
        except ConflictError as e:
            if e.code == "process_exited":
                return False
            raise
        return True

    def send_stdin(self, pid: str, data: str | bytes) -> None:
        """Write to a command's stdin (started with ``stdin=True``, or a PTY)."""
        payload = data.encode() if isinstance(data, str) else bytes(data)
        self._api.request("POST", self._path(pid, "/stdin"), plane="data", content=payload)

    def close_stdin(self, pid: str) -> None:
        """Close a command's stdin (EOF). For a PTY send Ctrl+D (``b"\\x04"``) instead."""
        self._api.request("POST", self._path(pid, "/close-stdin"), plane="data")
