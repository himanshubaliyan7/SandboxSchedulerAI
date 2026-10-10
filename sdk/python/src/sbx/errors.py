"""Exception hierarchy of the SDK.

Every exception the SDK raises on purpose derives from :class:`SbxError`.
Errors answered by the server derive from :class:`ApiError` and carry the
fields of the server's error envelope
(``{"error": {"code", "message", "request_id", "details"}}``).

::

    SbxError
    ├── ConfigurationError            missing API key, bad argument combination
    ├── TransportError                the server could not be reached / connection lost
    │   └── StreamError               a stream could not be resumed after retries
    ├── ProtocolError                 the server answered something the SDK cannot parse
    ├── SandboxTimeoutError           client-side deadline or server ``timeout`` (also a TimeoutError)
    ├── CommandExitError              a command exited with a non-zero status
    │   └── CommandTimeoutError       ... because its per-command timeout expired
    └── ApiError                      any error envelope from the server
        ├── InvalidArgumentError      400 bad_request, invalid_timeout, network_not_allowed, bad_signal
        │   └── PayloadTooLargeError  413
        ├── AuthenticationError       401
        ├── PermissionDeniedError     403
        ├── NotFoundError             404
        │   ├── SandboxNotFoundError
        │   ├── TemplateNotFoundError
        │   ├── CommandNotFoundError  no command with that pid
        │   └── PathNotFoundError     no such file or directory in the sandbox
        ├── ConflictError             409
        │   └── SandboxNotRunningError      the sandbox is not running (state, reason)
        │       └── SandboxTerminatedError  it died while the request was served
        ├── CommandStartError         the command could not be started
        ├── RateLimitError            429 rate_limited (retry_after)
        ├── QuotaExceededError        429 quota_exceeded (retry_after)
        ├── NoSpaceError              507
        └── ServerError               5xx
            └── ServiceUnavailableError   502/503 (retry_after)
                ├── PoolEmptyError
                └── AgentUnavailableError
"""

from __future__ import annotations

import builtins
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from .models import CommandResult

__all__ = [
    "AgentUnavailableError",
    "ApiError",
    "AuthenticationError",
    "CommandExitError",
    "CommandNotFoundError",
    "CommandStartError",
    "CommandTimeoutError",
    "ConfigurationError",
    "ConflictError",
    "InvalidArgumentError",
    "NoSpaceError",
    "NotFoundError",
    "PathNotFoundError",
    "PayloadTooLargeError",
    "PermissionDeniedError",
    "PoolEmptyError",
    "ProtocolError",
    "QuotaExceededError",
    "RateLimitError",
    "SandboxNotFoundError",
    "SandboxNotRunningError",
    "SandboxTerminatedError",
    "SandboxTimeoutError",
    "SbxError",
    "ServerError",
    "ServiceUnavailableError",
    "StreamError",
    "TemplateNotFoundError",
    "TransportError",
]


class SbxError(Exception):
    """Base class of every error raised by the SDK."""


class ConfigurationError(SbxError):
    """The client is not configured for the call (for example no API key)."""


class TransportError(SbxError):
    """The server could not be reached, or the connection broke."""


class StreamError(TransportError):
    """A command stream or WebSocket broke and could not be resumed."""


class ProtocolError(SbxError):
    """The server answered something the SDK does not understand."""


class SandboxTimeoutError(SbxError, builtins.TimeoutError):
    """A deadline expired: a client-side timeout, or the server's ``timeout`` error."""


class ApiError(SbxError):
    """An error envelope answered by the server.

    Attributes:
        status_code: HTTP status, or ``None`` when the error arrived inside a
            stream (an NDJSON ``error`` event or a WebSocket status frame).
        code: the server's stable error code, e.g. ``sandbox_not_running``.
        message: the human-readable message.
        request_id: the ``X-Request-Id`` of the failed request, for the logs.
        details: code-specific extra fields.
        retry_after: seconds from the ``Retry-After`` header, if any.
    """

    def __init__(
        self,
        message: str,
        *,
        code: str = "",
        status_code: int | None = None,
        request_id: str | None = None,
        details: Mapping[str, Any] | None = None,
        retry_after: float | None = None,
    ) -> None:
        super().__init__(message)
        self.message = message
        self.code = code
        self.status_code = status_code
        self.request_id = request_id
        self.details: dict[str, Any] = dict(details or {})
        self.retry_after = retry_after

    def __str__(self) -> str:
        parts = [self.message]
        meta: list[str] = []
        if self.code:
            meta.append(self.code)
        if self.status_code is not None:
            meta.append(f"HTTP {self.status_code}")
        if self.request_id:
            meta.append(f"request {self.request_id}")
        if meta:
            parts.append("(" + ", ".join(meta) + ")")
        return " ".join(parts)


class InvalidArgumentError(ApiError):
    """The server rejected the request as invalid (HTTP 400/413)."""


class PayloadTooLargeError(InvalidArgumentError):
    """The request body, upload or stdin write exceeds the server's limit."""


class AuthenticationError(ApiError):
    """Missing or invalid credentials (HTTP 401)."""


class PermissionDeniedError(ApiError):
    """The sandbox user may not access the path or execute the file (HTTP 403)."""


class NotFoundError(ApiError):
    """Something addressed by the request does not exist (HTTP 404)."""


class SandboxNotFoundError(NotFoundError):
    """No such sandbox for this tenant. Another tenant's sandbox looks the same."""


class TemplateNotFoundError(NotFoundError):
    """No such template."""


class CommandNotFoundError(NotFoundError):
    """No command with that pid in the sandbox."""


class PathNotFoundError(NotFoundError):
    """No such file or directory in the sandbox."""


class ConflictError(ApiError):
    """The request conflicts with the current state (HTTP 409)."""


class SandboxNotRunningError(ConflictError):
    """The sandbox is not running any more.

    Attributes:
        state: the sandbox state, e.g. ``destroyed``.
        reason: why it ended, e.g. ``released``, ``idle_timeout``,
            ``max_lifetime``, ``oom_killed``, ``instance_lost``, ``host_drain``.
    """

    @property
    def state(self) -> str | None:
        v = self.details.get("state")
        return v if isinstance(v, str) else None

    @property
    def reason(self) -> str | None:
        v = self.details.get("reason")
        return v if isinstance(v, str) else None


class SandboxTerminatedError(SandboxNotRunningError):
    """The sandbox died while the request or stream was being served."""


class CommandStartError(ApiError):
    """The command could not be started (executable not found, bad cwd, ...)."""


class RateLimitError(ApiError):
    """The tenant's request rate limit is exhausted (HTTP 429 ``rate_limited``)."""

    @property
    def limit_class(self) -> str | None:
        """Which bucket was empty: ``create`` or ``default``."""
        v = self.details.get("class")
        return v if isinstance(v, str) else None


class QuotaExceededError(ApiError):
    """The tenant is at its concurrent-sandbox quota (HTTP 429 ``quota_exceeded``)."""


class NoSpaceError(ApiError):
    """The sandbox's writable storage is full (HTTP 507)."""


class ServerError(ApiError):
    """The server failed (HTTP 5xx)."""


class ServiceUnavailableError(ServerError):
    """The server cannot serve the request right now (HTTP 502/503); retry later."""


class PoolEmptyError(ServiceUnavailableError):
    """No warm sandbox of the template became available in time."""


class AgentUnavailableError(ServiceUnavailableError):
    """The in-sandbox agent did not answer although the sandbox is running."""


class CommandExitError(SbxError):
    """A command finished with a non-zero exit status.

    Raised by ``commands.run(..., check=True)`` (the default) and by
    ``handle.wait()``. The complete result is available on the exception.
    """

    def __init__(self, result: CommandResult) -> None:
        self.result = result
        detail = f"command exited with code {result.exit_code}"
        if result.reason and result.reason != "exited":
            detail += f" ({result.reason}" + (f", {result.signal}" if result.signal else "") + ")"
        tail = result.stderr.strip()[-500:]
        if tail:
            detail += f": {tail}"
        super().__init__(detail)

    @property
    def exit_code(self) -> int:
        return self.result.exit_code

    @property
    def stdout(self) -> str:
        return self.result.stdout

    @property
    def stderr(self) -> str:
        return self.result.stderr

    @property
    def reason(self) -> str:
        return self.result.reason

    @property
    def signal(self) -> str | None:
        return self.result.signal


class CommandTimeoutError(CommandExitError, builtins.TimeoutError):
    """The command was killed because its per-command ``timeout`` expired."""


_BY_CODE: dict[str, type[ApiError]] = {
    "bad_request": InvalidArgumentError,
    "invalid_timeout": InvalidArgumentError,
    "network_not_allowed": InvalidArgumentError,
    "bad_signal": InvalidArgumentError,
    "bad_frame": InvalidArgumentError,
    "payload_too_large": PayloadTooLargeError,
    "too_large": PayloadTooLargeError,
    "stdin_overflow": PayloadTooLargeError,
    "unauthorized": AuthenticationError,
    "permission_denied": PermissionDeniedError,
    "not_found": SandboxNotFoundError,
    "template_not_found": TemplateNotFoundError,
    "process_not_found": CommandNotFoundError,
    "file_not_found": PathNotFoundError,
    "sandbox_not_running": SandboxNotRunningError,
    "sandbox_terminated": SandboxTerminatedError,
    "command_not_found": CommandStartError,
    "exec_format": CommandStartError,
    "bad_cwd": CommandStartError,
    "start_failed": CommandStartError,
    "pty_failed": CommandStartError,
    "rate_limited": RateLimitError,
    "quota_exceeded": QuotaExceededError,
    "no_space": NoSpaceError,
    "pool_empty": PoolEmptyError,
    "shutting_down": ServiceUnavailableError,
    "not_ready": ServiceUnavailableError,
    "agent_unavailable": AgentUnavailableError,
}

_BY_STATUS: dict[int, type[ApiError]] = {
    400: InvalidArgumentError,
    401: AuthenticationError,
    403: PermissionDeniedError,
    404: NotFoundError,
    409: ConflictError,
    413: PayloadTooLargeError,
    429: RateLimitError,
    502: ServiceUnavailableError,
    503: ServiceUnavailableError,
    507: NoSpaceError,
}


def error_from_envelope(
    body: Any,
    *,
    status_code: int | None = None,
    request_id: str | None = None,
    retry_after: float | None = None,
) -> SbxError:
    """Build the exception for a server error.

    ``body`` is the decoded JSON of the response (the envelope, or the bare
    ``error`` object of a stream event), or anything else if the body was
    not JSON.
    """
    err: Any = body
    if isinstance(body, Mapping) and isinstance(body.get("error"), Mapping):
        err = body["error"]
    code = ""
    message = ""
    details: Mapping[str, Any] | None = None
    if isinstance(err, Mapping):
        raw_code = err.get("code")
        code = raw_code if isinstance(raw_code, str) else ""
        raw_msg = err.get("message")
        message = raw_msg if isinstance(raw_msg, str) else ""
        raw_details = err.get("details")
        details = raw_details if isinstance(raw_details, Mapping) else None
        rid = err.get("request_id")
        if isinstance(rid, str) and rid:
            request_id = rid
    if not message:
        message = f"HTTP {status_code}" if status_code is not None else (code or "unknown error")

    if code == "timeout" or status_code == 504:
        return SandboxTimeoutError(
            f"{message} (timeout" + (f", request {request_id})" if request_id else ")")
        )

    cls = _BY_CODE.get(code)
    if code == "not_found" and message.startswith("no such route"):
        cls = NotFoundError
    if cls is None and status_code is not None:
        cls = _BY_STATUS.get(status_code)
        if cls is None and status_code >= 500:
            cls = ServerError
    if cls is None:
        cls = ApiError
    return cls(
        message,
        code=code,
        status_code=status_code,
        request_id=request_id,
        details=details,
        retry_after=retry_after,
    )
