"""Offset bookkeeping for a command's output stream (no I/O here).

The server numbers every output byte of a command with one counter across
stdout, stderr and PTY output. :class:`StreamTracker` follows that counter
so that a broken stream can be resumed with ``from=<offset>`` without
delivering a byte twice, and so that any gap is reported.
"""

from __future__ import annotations

import base64
import binascii
from collections.abc import Mapping
from typing import Any

from .errors import ProtocolError, error_from_envelope
from .models import (
    CommandEvent,
    CommandInfo,
    ExitEvent,
    ExitStatus,
    PtyOutputEvent,
    StderrEvent,
    StdoutEvent,
    TruncatedEvent,
)

_OUTPUT = {"stdout": StdoutEvent, "stderr": StderrEvent, "pty": PtyOutputEvent}


class StreamTracker:
    """Consumes decoded stream events and produces what the caller should see."""

    def __init__(self, offset: int = 0) -> None:
        #: Offset of the next output byte wanted; the value for ``from=``.
        self.offset = offset
        #: Total bytes known to be lost.
        self.dropped = 0
        self.command: CommandInfo | None = None
        self.exit: ExitStatus | None = None
        #: Keepalive events seen (they prove the connection is alive).
        self.keepalives = 0

    def _gap(self, upto: int) -> list[CommandEvent]:
        if upto <= self.offset:
            return []
        lost = upto - self.offset
        self.dropped += lost
        self.offset = upto
        return [TruncatedEvent(offset=upto, dropped=lost)]

    def accept(self, obj: Mapping[str, Any]) -> list[CommandEvent]:
        """Process one event. Raises the mapped error for an ``error`` event."""
        kind = obj.get("type")
        raw_offset = obj.get("offset", 0)
        offset = raw_offset if isinstance(raw_offset, int) and not isinstance(raw_offset, bool) else 0

        if kind in _OUTPUT:
            raw = obj.get("data") or ""
            try:
                data = base64.b64decode(raw, validate=True) if isinstance(raw, str) else b""
            except (binascii.Error, ValueError) as e:
                raise ProtocolError(f"invalid base64 in a {kind} event: {e}") from None
            end = offset + len(data)
            if end <= self.offset:
                return []  # replayed after a reconnect; already delivered
            out = self._gap(offset)
            if offset < self.offset:
                data = data[self.offset - offset :]
                offset = self.offset
            self.offset = end
            if data:
                out.append(_OUTPUT[kind](data=data, offset=offset))
            return out
        if kind == "truncated":
            # ``dropped`` bytes before ``offset`` are gone. What matters to this
            # client is the distance from where it stood.
            return self._gap(offset)
        if kind == "exit":
            raw_exit = obj.get("exit")
            out = self._gap(offset)
            self.exit = ExitStatus.from_json(raw_exit if isinstance(raw_exit, Mapping) else {})
            out.append(ExitEvent(exit=self.exit, offset=self.offset))
            return out
        if kind == "start":
            cmd = obj.get("command")
            if isinstance(cmd, Mapping):
                self.command = CommandInfo.from_json(cmd)
            return []
        if kind == "keepalive":
            self.keepalives += 1
            return []
        if kind == "error":
            raise error_from_envelope(obj.get("error") or {"code": "internal", "message": "stream failed"})
        return []  # an event type this SDK does not know yet
