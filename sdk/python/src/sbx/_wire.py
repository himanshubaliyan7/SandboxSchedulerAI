"""The WebSocket framing of the PTY / interactive route (no I/O here).

Every message is binary: one channel byte followed by the payload.
"""

from __future__ import annotations

import json
from typing import Any

from .errors import ProtocolError

CH_STDIN = 0  # client -> server: input bytes
CH_STDOUT = 1  # server -> client: stdout, and all output of a PTY
CH_STDERR = 2  # server -> client: stderr (pipe mode)
CH_STATUS = 3  # server -> client: JSON {"exit": ...} or {"error": ...}; the last frame
CH_RESIZE = 4  # client -> server: JSON {"cols", "rows"}
CH_SIGNAL = 5  # client -> server: JSON {"signal"}
CH_ACK = 6  # client -> server: JSON {"offset"}
CH_PING = 7  # either: opaque payload, echoed on CH_PONG
CH_PONG = 8
CH_OFFSET = 9  # server -> client: JSON {"offset", "dropped"}; first frame, and after a truncation

SUBPROTOCOL = "sbx.v1"

# The server sends at most 1 + 32768 bytes per frame; allow generous headroom
# so a larger server-side frame size does not break old clients.
MAX_SERVER_FRAME = 4 * 1024 * 1024
# The server accepts at most 1 MiB + 1 per client frame; stay far below it.
STDIN_CHUNK = 32 * 1024

# The agent pauses the command above 100000 unacknowledged bytes and resumes
# below 10000, so acknowledgements must come in steps smaller than 10000.
FLOW_LOW_WATERMARK = 10_000
ACK_STEP = 8192


def frame(channel: int, payload: bytes = b"") -> bytes:
    return bytes((channel,)) + payload


def json_frame(channel: int, value: Any) -> bytes:
    return bytes((channel,)) + json.dumps(value, separators=(",", ":")).encode()


def split(message: bytes) -> tuple[int, bytes]:
    if not message:
        raise ProtocolError("empty WebSocket frame")
    return message[0], message[1:]


def json_payload(payload: bytes) -> dict[str, Any]:
    try:
        obj = json.loads(payload)
    except ValueError as e:
        raise ProtocolError(f"invalid JSON in a WebSocket control frame: {e}") from None
    if not isinstance(obj, dict):
        raise ProtocolError("WebSocket control frame is not a JSON object")
    return obj


def stdin_frames(data: bytes) -> list[bytes]:
    """Split input into frames the server accepts."""
    return [frame(CH_STDIN, data[i : i + STDIN_CHUNK]) for i in range(0, len(data), STDIN_CHUNK)]


class FlowControl:
    """Decides when to acknowledge consumed output.

    ``consumed(n)`` is called after ``n`` bytes have been handed to the
    application; it returns the offset to acknowledge once at least
    ``step`` bytes are outstanding, else ``None``. Because every
    acknowledgement covers everything consumed so far, fewer than ``step``
    bytes are ever left unacknowledged by a client that keeps up.
    """

    def __init__(self, step: int = ACK_STEP) -> None:
        if not 0 < step < FLOW_LOW_WATERMARK:
            raise ValueError(f"the ACK step must be between 1 and {FLOW_LOW_WATERMARK - 1}")
        self.step = step
        self.offset = 0
        self.acked = 0

    def reset(self, offset: int) -> None:
        """The server announced a new position (first frame, or a truncation)."""
        self.offset = offset
        self.acked = offset

    def consumed(self, n: int) -> int | None:
        self.offset += n
        if self.offset - self.acked >= self.step:
            self.acked = self.offset
            return self.offset
        return None
