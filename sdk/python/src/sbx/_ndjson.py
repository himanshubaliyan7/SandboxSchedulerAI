"""Incremental NDJSON decoding."""

from __future__ import annotations

import json
from typing import Any

from .errors import ProtocolError

# One event carries at most 32768 bytes of output (44 KiB of base64). The
# bound only protects against a peer that never sends a newline.
MAX_LINE_BYTES = 8 * 1024 * 1024


class NDJSONDecoder:
    """Turns arbitrary byte chunks into JSON objects, one per line.

    Chunk boundaries may fall anywhere, including inside a multi-byte
    character or a base64 payload; an incomplete last line stays buffered
    until the rest arrives.
    """

    def __init__(self, max_line_bytes: int = MAX_LINE_BYTES) -> None:
        self._buf = bytearray()
        self._max = max_line_bytes

    @property
    def pending(self) -> int:
        """Bytes of an incomplete line currently buffered."""
        return len(self._buf)

    def feed(self, chunk: bytes) -> list[dict[str, Any]]:
        if not chunk:
            return []
        self._buf += chunk
        out: list[dict[str, Any]] = []
        start = 0
        while True:
            nl = self._buf.find(b"\n", start)
            if nl < 0:
                break
            line = bytes(self._buf[start:nl]).strip()
            start = nl + 1
            if not line:
                continue
            try:
                obj = json.loads(line)
            except ValueError as e:
                raise ProtocolError(f"invalid JSON line in stream: {e}") from None
            if not isinstance(obj, dict):
                raise ProtocolError("stream line is not a JSON object")
            out.append(obj)
        if start:
            del self._buf[:start]
        if len(self._buf) > self._max:
            raise ProtocolError(f"stream line exceeds {self._max} bytes")
        return out
