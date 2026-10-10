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

#: Largest output payload of one server frame (the frame is one channel byte longer).
MAX_SERVER_FRAME_PAYLOAD = 32 * 1024
# The read limit is far above that on purpose, so that a server that one
# day sends larger frames does not break this client.
MAX_SERVER_FRAME = 4 * 1024 * 1024
#: Largest payload of one client frame the server accepts: 1 MiB after the
#: channel byte. A larger frame is answered with close code 1009.
MAX_CLIENT_FRAME_PAYLOAD = 1024 * 1024
# Input is sent in much smaller frames, so keystrokes interleave with a paste.
STDIN_CHUNK = 32 * 1024

# Application close codes of the WebSocket route (see "How a WebSocket ends"
# in api/openapi.yaml). The close reason is the API error code.
CLOSE_SANDBOX_TERMINATED = 4000
CLOSE_UNAUTHORIZED = 4001
CLOSE_SHUTTING_DOWN = 4002
CLOSE_AGENT_UNAVAILABLE = 4003
CLOSE_NOT_FOUND = 4004
CLOSE_BAD_REQUEST = 4005
CLOSE_RATE_LIMITED = 4029
CLOSE_ERROR = 4099

_CLOSE_DEFAULT_REASON = {
    CLOSE_SANDBOX_TERMINATED: "sandbox_terminated",
    CLOSE_UNAUTHORIZED: "unauthorized",
    CLOSE_NOT_FOUND: "process_not_found",
    CLOSE_BAD_REQUEST: "bad_request",
}

#: Error codes of a status frame that end one connection but not the
#: session: the reader re-attaches instead of failing.
TRANSIENT_STATUS_CODES = frozenset(
    {"agent_unavailable", "shutting_down", "rate_limited", "timeout", "internal"}
)


def close_error(code: int | None, reason: str | None) -> dict[str, str] | None:
    """The error body for a close code after which re-attaching cannot succeed.

    ``None`` for every other close: the transient application codes
    (shutting down, agent unavailable, rate limited, other) and everything
    that is not an application code, after which the caller re-attaches.
    """
    if code is None or code not in _CLOSE_DEFAULT_REASON:
        return None
    token = reason or ""
    if not token or len(token) > 64 or not all(c == "_" or "a" <= c <= "z" for c in token):
        token = _CLOSE_DEFAULT_REASON[code]
    return {"code": token, "message": f"the server closed the connection: {token} (close code {code})"}


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
