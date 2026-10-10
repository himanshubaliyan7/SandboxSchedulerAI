"""Interactive sessions over the WebSocket route (PTY, and pipe-mode attach)."""

from __future__ import annotations

import json as jsonlib
from collections.abc import Mapping, Sequence
from typing import Any

from websockets.exceptions import ConnectionClosed, InvalidHandshake, InvalidStatus
from websockets.typing import Subprotocol

from .. import _wire as wire
from .._config import USER_AGENT, describe, ms, seg
from ..errors import (
    ProtocolError,
    RateLimitError,
    SandboxTimeoutError,
    SbxError,
    ServiceUnavailableError,
    StreamError,
    TransportError,
    error_from_envelope,
)
from ..models import CommandInfo, CommandResult, ExitStatus
from ._compat import (
    ClientConnection,
    DataHandler,
    Event,
    Task,
    TruncatedHandler,
    invoke,
    sleep,
    spawn,
    ws_connect,
)
from .api import AsyncApiClient, decode_json
from .commands import AsyncCommands


def _handshake_error(e: InvalidStatus) -> SbxError:
    response = e.response
    body = response.body or b""
    parsed: Any
    try:
        parsed = jsonlib.loads(body) if body else None
    except ValueError:
        parsed = None
    retry_after: float | None = None
    try:
        value = response.headers.get("Retry-After")
        retry_after = float(value) if value else None
    except ValueError:
        retry_after = None
    return error_from_envelope(
        parsed,
        status_code=response.status_code,
        request_id=response.headers.get("X-Request-Id"),
        retry_after=retry_after,
    )


class AsyncPtyHandle:
    """A live attachment to a command's terminal.

    Output is delivered to ``on_data`` by a background reader as it arrives.
    The reader acknowledges what the callback has consumed (flow control: a
    slow callback pauses the command instead of losing output), answers
    pings, and re-attaches at the right offset if the connection drops.
    Call :meth:`wait` for the exit status.
    """

    def __init__(
        self,
        api: AsyncApiClient,
        sandbox_id: str,
        pid: str,
        on_data: DataHandler,
        *,
        on_stderr: DataHandler | None = None,
        on_truncated: TruncatedHandler | None = None,
        offset: int = 0,
        flow_control: bool = True,
        info: CommandInfo | None = None,
    ) -> None:
        self._api = api
        self._sandbox_id = sandbox_id
        self._pid = pid
        self._info = info
        self._on_data = on_data
        self._on_stderr = on_stderr
        self._on_truncated = on_truncated
        self._offset = offset  # output delivered to the callbacks so far
        self._position = offset  # where the server's next frame starts
        self._dropped = 0
        self._flow = wire.FlowControl() if flow_control else None
        self._ws: ClientConnection | None = None
        self._reader: Task | None = None
        self._done = Event()
        self._closing = False
        self._exit: ExitStatus | None = None
        self._error: BaseException | None = None
        self.reconnects = 0

    def __repr__(self) -> str:
        return f"<{type(self).__name__} pid={self._pid!r} sandbox={self._sandbox_id!r} offset={self._offset}>"

    @property
    def pid(self) -> str:
        return self._pid

    @property
    def offset(self) -> int:
        """Bytes of output delivered so far; pass it to ``pty.connect(from_offset=...)`` to resume."""
        return self._offset

    @property
    def dropped_bytes(self) -> int:
        return self._dropped

    @property
    def exit_code(self) -> int | None:
        return self._exit.exit_code if self._exit is not None else None

    @property
    def finished(self) -> bool:
        return self._done.is_set()

    # -- connection ---------------------------------------------------------

    async def _dial(self) -> ClientConnection:
        config = self._api.config
        path = f"/v1/sandboxes/{seg(self._sandbox_id)}/commands/{seg(self._pid)}/ws?from={self._offset}"
        headers = dict(config.headers)
        headers.update(config.auth_headers("data"))
        try:
            return await ws_connect(
                config.ws_url(path),
                additional_headers=headers,
                user_agent_header=USER_AGENT,
                subprotocols=[Subprotocol(wire.SUBPROTOCOL)],
                # The server's frames are small today (32769 bytes), but the
                # limit is raised well above that on purpose.
                max_size=wire.MAX_SERVER_FRAME,
                compression=None,
                open_timeout=config.request_timeout,
                # Protocol-level pings from the server are answered by the
                # library; ours detect a dead peer between its pings.
                ping_interval=20,
                ping_timeout=config.stream_idle_timeout,
                close_timeout=5,
            )
        except InvalidStatus as e:
            raise _handshake_error(e) from None
        except TimeoutError as e:
            raise SandboxTimeoutError(f"WebSocket handshake timed out ({describe(e)})") from e
        except (InvalidHandshake, OSError, EOFError) as e:
            raise TransportError(f"WebSocket connection failed: {describe(e)}") from e

    async def _connect(self) -> ClientConnection:
        """Dial with the retry policy of an idempotent request."""
        policy = self._api.config.retry
        attempt = 0
        while True:
            try:
                return await self._dial()
            except (RateLimitError, ServiceUnavailableError) as e:
                delay = policy.delay_for(e, attempt, idempotent=True)
                if delay is None:
                    raise
            except (TransportError, SandboxTimeoutError) as e:
                delay = policy.delay_for(e, attempt, idempotent=True, transport_failure=True)
                if delay is None:
                    raise
            attempt += 1
            await sleep(delay)

    async def _start(self) -> None:
        self._ws = await self._connect()
        self._reader = spawn(self._run, f"sbx-pty-{self._pid}")

    async def _send(self, data: bytes) -> None:
        ws = self._ws
        if ws is None or self._done.is_set():
            raise StreamError(f"not attached to command {self._pid}")
        try:
            await ws.send(data)
        except ConnectionClosed as e:
            raise StreamError(f"the connection to command {self._pid} is closed: {describe(e)}") from e
        except OSError as e:
            raise StreamError(f"sending to command {self._pid} failed: {describe(e)}") from e

    # -- reader ---------------------------------------------------------------

    async def _on_offset(self, payload: bytes) -> None:
        announced = wire.json_payload(payload)
        position = announced.get("offset")
        if not isinstance(position, int):
            raise ProtocolError("offset frame without an offset")
        if self._flow is not None:
            self._flow.reset(position)
        self._position = position
        if position > self._offset:
            lost = position - self._offset
            self._dropped += lost
            self._offset = position
            if self._on_truncated is not None:
                await invoke(self._on_truncated, position, lost)

    async def _on_output(self, channel: int, payload: bytes) -> None:
        start = self._position
        self._position = start + len(payload)
        if self._position > self._offset:
            fresh = payload[max(self._offset - start, 0) :]
            handler = (
                self._on_stderr
                if channel == wire.CH_STDERR and self._on_stderr is not None
                else self._on_data
            )
            await invoke(handler, fresh)
            self._offset = self._position
        if self._flow is not None:
            ack = self._flow.consumed(len(payload))
            if ack is not None and self._ws is not None:
                try:
                    await self._ws.send(wire.json_frame(wire.CH_ACK, {"offset": ack}))
                except (ConnectionClosed, OSError):
                    pass  # the read loop notices and re-attaches

    async def _read(self, ws: ClientConnection) -> bool:
        """Relay frames until the status frame (True) or the connection ends (False)."""
        while True:
            try:
                message = await ws.recv()
            except ConnectionClosed as e:
                # The close code says whether re-attaching can work: a
                # sandbox that is gone or a credential that is wrong is
                # final, and needs no further request to find out.
                rcvd = e.rcvd
                final = wire.close_error(rcvd.code, rcvd.reason) if rcvd is not None else None
                if final is not None:
                    self._error = error_from_envelope(final)
                    return True
                return False
            except (OSError, EOFError):
                return False
            if isinstance(message, str):
                continue  # the protocol is binary; ignore anything else
            channel, payload = wire.split(message)
            if channel in (wire.CH_STDOUT, wire.CH_STDERR):
                await self._on_output(channel, payload)
            elif channel == wire.CH_OFFSET:
                await self._on_offset(payload)
            elif channel == wire.CH_STATUS:
                status = wire.json_payload(payload)
                exit_status = status.get("exit")
                error = status.get("error")
                if isinstance(exit_status, Mapping):
                    self._exit = ExitStatus.from_json(exit_status)
                elif isinstance(error, Mapping):
                    if error.get("code") in wire.TRANSIENT_STATUS_CODES:
                        # The link behind the server broke, or the server is
                        # stopping: the command may live on, so attach again.
                        return False
                    self._error = error_from_envelope(error)
                else:
                    self._error = ProtocolError("status frame with neither exit nor error")
                return True
            elif channel == wire.CH_PING:
                try:
                    await ws.send(wire.frame(wire.CH_PONG, payload))
                except (ConnectionClosed, OSError):
                    return False
            # CH_PONG and channels this SDK does not know are ignored.

    async def _run(self) -> None:
        config = self._api.config
        ws = self._ws
        failures = 0
        try:
            while True:
                if ws is not None:
                    before = self._position
                    finished = await self._read(ws)
                    try:
                        await ws.close()
                    except (ConnectionClosed, OSError):
                        pass
                    if finished or self._closing:
                        return
                    if self._position != before:
                        failures = 0
                # The connection dropped while the command may still be
                # running: attach again where the output stopped.
                failures += 1
                if failures > config.max_reconnects:
                    self._error = StreamError(
                        f"the connection to command {self._pid} broke and could not be "
                        f"re-established after {config.max_reconnects} attempts"
                    )
                    return
                await sleep(config.retry.backoff(failures))
                if self._closing:
                    return
                try:
                    ws = self._ws = await self._dial()
                    self.reconnects += 1
                except (TransportError, SandboxTimeoutError, RateLimitError, ServiceUnavailableError):
                    ws = self._ws = None
        except BaseException as e:
            # An API error on re-attach (the sandbox or the command is gone),
            # a protocol violation, or an exception from a callback.
            self._error = e
            if not isinstance(e, Exception):
                raise
        finally:
            current, self._ws = self._ws, None
            if current is not None:
                try:
                    await current.close()
                except (ConnectionClosed, OSError):
                    pass
            self._done.set()

    # -- public API -----------------------------------------------------------

    async def send_input(self, data: str | bytes) -> None:
        """Type into the terminal. ``b"\\x03"`` is Ctrl+C, ``b"\\x04"`` Ctrl+D."""
        payload = data.encode() if isinstance(data, str) else bytes(data)
        for message in wire.stdin_frames(payload):
            await self._send(message)

    async def resize(self, cols: int, rows: int) -> None:
        await self._send(wire.json_frame(wire.CH_RESIZE, {"cols": int(cols), "rows": int(rows)}))

    async def signal(self, signal: str) -> None:
        """Signal the command's process group, e.g. ``"SIGINT"``."""
        await self._send(wire.json_frame(wire.CH_SIGNAL, {"signal": signal}))

    async def kill(self) -> bool:
        """SIGKILL the command. ``False`` if it had already exited."""
        return await AsyncCommands(self._api, self._sandbox_id).kill(self._pid)

    async def wait(self, timeout: float | None = None) -> CommandResult:
        """Wait for the command to exit and return its status.

        The output went to ``on_data``, so ``stdout`` of the result is empty.
        A non-zero exit status is returned, not raised: for an interactive
        shell it is just the status of the last command typed. Raises the
        error that ended the session otherwise (for example
        :class:`~sbx.SandboxNotRunningError`), or
        :class:`~sbx.SandboxTimeoutError` after ``timeout`` seconds.
        """
        if not await self._done.wait(timeout):
            raise SandboxTimeoutError(f"command {self._pid} did not exit within {timeout} s")
        if self._exit is not None:
            return CommandResult(
                pid=self._pid,
                exit_code=self._exit.exit_code,
                reason=self._exit.reason,
                signal=self._exit.signal,
                truncated=self._dropped > 0,
                dropped_bytes=self._dropped,
            )
        if self._error is not None:
            raise self._error
        raise StreamError(f"disconnected from command {self._pid} before it exited")

    async def disconnect(self) -> None:
        """Detach. The command keeps running; ``pty.connect(pid, ..., from_offset=handle.offset)`` resumes."""
        self._closing = True
        ws = self._ws
        if ws is not None:
            try:
                await ws.close()
            except (ConnectionClosed, OSError):
                pass
        if self._reader is not None and not self._reader.is_current():
            await self._done.wait(5)


class AsyncPty:
    """``sandbox.pty``: interactive terminals."""

    def __init__(self, api: AsyncApiClient, sandbox_id: str) -> None:
        self._api = api
        self._sandbox_id = sandbox_id
        self._commands = AsyncCommands(api, sandbox_id)

    async def create(
        self,
        cols: int,
        rows: int,
        on_data: DataHandler,
        *,
        cmd: str | None = None,
        args: Sequence[str] | None = None,
        envs: Mapping[str, str] | None = None,
        cwd: str | None = None,
        timeout: float | None = None,
        tag: str | None = None,
        on_truncated: TruncatedHandler | None = None,
        flow_control: bool = True,
    ) -> AsyncPtyHandle:
        """Start a command on a pseudo-terminal and attach to it.

        Args:
            cols, rows: terminal size in character cells.
            on_data: called with raw output bytes as they arrive.
            cmd: the program to run, default ``/bin/sh``. With ``args`` it is
                started directly; without, a ``cmd`` containing spaces is
                run by the shell (``/bin/sh -c``).
            timeout: wall-clock limit in seconds; ``None`` (default) for none.
            on_truncated: called with ``(offset, dropped)`` if output is lost.
            flow_control: acknowledge output so that a slow ``on_data`` pauses
                the command. Switch it off to never pause the command; a
                slow consumer then loses output (reported as truncated).
        """
        body: dict[str, Any]
        if cmd is None:
            body = {"cmd": "/bin/sh", "args": list(args or ())}
        elif args is None and any(c.isspace() for c in cmd):
            shell = list(self._api.config.shell)
            body = {"cmd": shell[0], "args": [*shell[1:], cmd]}
        else:
            body = {"cmd": cmd, "args": list(args or ())}
        if not body["args"]:
            del body["args"]
        body["pty"] = {"cols": int(cols), "rows": int(rows)}
        if envs:
            body["env"] = dict(envs)
        if cwd:
            body["cwd"] = cwd
        timeout_ms = ms(timeout)
        if timeout_ms:
            body["timeout_ms"] = timeout_ms
        if tag:
            body["tag"] = tag
        path = f"/v1/sandboxes/{seg(self._sandbox_id)}/commands"
        info = CommandInfo.from_json(
            decode_json(await self._api.request("POST", path, plane="data", json=body))
        )
        handle = AsyncPtyHandle(
            self._api,
            self._sandbox_id,
            info.pid,
            on_data,
            on_truncated=on_truncated,
            flow_control=flow_control,
            info=info,
        )
        await handle._start()
        return handle

    async def connect(
        self,
        pid: str,
        on_data: DataHandler,
        *,
        on_stderr: DataHandler | None = None,
        on_truncated: TruncatedHandler | None = None,
        from_offset: int = 0,
        flow_control: bool = True,
    ) -> AsyncPtyHandle:
        """Attach to a running (or finished) command by pid.

        Output is replayed from ``from_offset`` (default: everything the
        sandbox still holds). It also works for a command that is not a PTY;
        its stderr then goes to ``on_stderr``, or to ``on_data`` if that is
        not given.
        """
        handle = AsyncPtyHandle(
            self._api,
            self._sandbox_id,
            pid,
            on_data,
            on_stderr=on_stderr,
            on_truncated=on_truncated,
            offset=from_offset,
            flow_control=flow_control,
        )
        await handle._start()
        return handle

    async def send_input(self, pid: str, data: str | bytes) -> None:
        """Type into a terminal without attaching to it."""
        await self._commands.send_stdin(pid, data)

    async def resize(self, pid: str, cols: int, rows: int) -> None:
        """Resize a terminal without attaching to it."""
        path = f"/v1/sandboxes/{seg(self._sandbox_id)}/commands/{seg(pid)}/resize"
        await self._api.request("POST", path, plane="data", json={"cols": int(cols), "rows": int(rows)})

    async def kill(self, pid: str) -> bool:
        """SIGKILL a terminal's command. ``False`` if it had already exited."""
        return await self._commands.kill(pid)
