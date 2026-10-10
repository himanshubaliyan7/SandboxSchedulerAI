"""Connection settings, credentials and the retry policy (no I/O here)."""

from __future__ import annotations

import os
import platform
import random
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from urllib.parse import quote, urlsplit, urlunsplit

from ._version import __version__
from .errors import (
    AgentUnavailableError,
    ConfigurationError,
    QuotaExceededError,
    RateLimitError,
    SbxError,
    ServiceUnavailableError,
)

DEFAULT_BASE_URL = "http://127.0.0.1:8080"
ENV_API_KEY = "SBX_API_KEY"
ENV_BASE_URL = "SBX_BASE_URL"

USER_AGENT = f"sbx-python/{__version__} (python {platform.python_version()}; {platform.system().lower()})"


@dataclass(frozen=True)
class RetryPolicy:
    """When and how often a request is retried automatically.

    A request is retried only when it is safe to:

    * ``429 rate_limited`` on any call (the limiter runs before the request
      has any effect);
    * ``503`` (``pool_empty``, ``shutting_down``) on ``Sandbox.create`` and on
      idempotent calls;
    * ``502 agent_unavailable`` and connection errors on idempotent calls.

    The server's ``Retry-After`` is honoured; without it the delay grows
    exponentially from ``backoff_initial`` to ``backoff_max`` with jitter.
    A ``Retry-After`` longer than ``max_retry_after`` is not waited for: the
    error is raised instead. ``429 quota_exceeded`` is not retried unless
    ``retry_quota_exceeded`` is set, because it only clears when a sandbox
    is deleted or expires.

    ``RetryPolicy(max_retries=0)`` (or :meth:`none`) switches retries off.
    """

    max_retries: int = 3
    backoff_initial: float = 0.25
    backoff_max: float = 8.0
    max_retry_after: float = 30.0
    retry_quota_exceeded: bool = False

    @classmethod
    def none(cls) -> RetryPolicy:
        return cls(max_retries=0)

    def backoff(self, attempt: int) -> float:
        """Delay before retry number ``attempt`` (1-based) when the server gave none."""
        base = min(self.backoff_initial * (2 ** max(attempt - 1, 0)), self.backoff_max)
        return base * (0.75 + random.random() * 0.5)

    def delay_for(
        self,
        err: BaseException,
        attempt: int,
        *,
        idempotent: bool,
        is_create: bool = False,
        transport_failure: bool = False,
    ) -> float | None:
        """Seconds to wait before retrying after ``err``, or ``None`` to give up.

        ``attempt`` is the number of retries already made.
        """
        if attempt >= self.max_retries:
            return None
        if transport_failure:
            ok = idempotent
        elif isinstance(err, RateLimitError):
            ok = True
        elif isinstance(err, QuotaExceededError):
            ok = self.retry_quota_exceeded and is_create
        elif isinstance(err, AgentUnavailableError):
            ok = idempotent
        elif isinstance(err, ServiceUnavailableError):
            ok = idempotent or is_create
        else:
            ok = False
        if not ok:
            return None
        retry_after = getattr(err, "retry_after", None)
        if isinstance(retry_after, (int, float)) and retry_after >= 0:
            if retry_after > self.max_retry_after:
                return None
            return float(retry_after)
        return self.backoff(attempt + 1)


#: Default per-command timeout of ``commands.run``, in seconds. The same as
#: the TypeScript SDK's ``commandTimeoutMs`` (60000) and as E2B.
DEFAULT_COMMAND_TIMEOUT = 60.0


@dataclass(frozen=True)
class ConnectionConfig:
    """Everything a client needs to talk to one server.

    The API key and the access token are excluded from ``repr`` and are
    never logged by the SDK.
    """

    base_url: str = DEFAULT_BASE_URL
    api_key: str | None = field(default=None, repr=False)
    access_token: str | None = field(default=None, repr=False)
    #: Total budget of one ordinary request, in seconds.
    request_timeout: float = 60.0
    #: A stream or WebSocket that is silent for this long is considered dead
    #: and reconnected. The server sends a keepalive every 25 s.
    stream_idle_timeout: float = 90.0
    #: How many times in a row a broken stream is reconnected before giving up.
    max_reconnects: int = 5
    retry: RetryPolicy = field(default_factory=RetryPolicy)
    headers: Mapping[str, str] = field(default_factory=lambda: dict[str, str]())
    #: Override for the WebSocket origin (``ws://host:port``). By default it is
    #: derived from ``base_url``.
    ws_base_url: str | None = None
    #: How ``commands.run("a | b")`` is turned into an argv when ``args`` is not given.
    shell: Sequence[str] = ("/bin/sh", "-c")
    #: Wall-clock limit, in seconds, of a command started by ``commands.run``
    #: without a ``timeout`` (E2B's default). 0 means no limit.
    command_timeout: float = DEFAULT_COMMAND_TIMEOUT

    @classmethod
    def resolve(
        cls,
        *,
        api_key: str | None = None,
        base_url: str | None = None,
        access_token: str | None = None,
        request_timeout: float | None = None,
        retry: RetryPolicy | None = None,
        headers: Mapping[str, str] | None = None,
        ws_base_url: str | None = None,
    ) -> ConnectionConfig:
        """Fill in defaults and the ``SBX_API_KEY`` / ``SBX_BASE_URL`` environment fallbacks."""
        key = api_key or os.environ.get(ENV_API_KEY) or None
        url = (base_url or os.environ.get(ENV_BASE_URL) or DEFAULT_BASE_URL).rstrip("/")
        parts = urlsplit(url)
        if parts.scheme not in ("http", "https") or not parts.netloc:
            raise ConfigurationError(f"base_url must be an http(s) URL, got {url!r}")
        kwargs: dict[str, object] = {}
        if request_timeout is not None:
            if request_timeout <= 0:
                raise ConfigurationError("request_timeout must be positive")
            kwargs["request_timeout"] = float(request_timeout)
        return cls(
            base_url=url,
            api_key=key,
            access_token=access_token or None,
            retry=retry if retry is not None else RetryPolicy(),
            headers=dict(headers or {}),
            ws_base_url=ws_base_url.rstrip("/") if ws_base_url else None,
            **kwargs,  # type: ignore[arg-type]
        )

    def auth_headers(self, plane: str) -> dict[str, str]:
        """Credentials for a ``"control"`` or ``"data"`` plane request.

        Data-plane calls use the sandbox's access token when there is one,
        so the tenant API key travels only where it is required.
        """
        if plane == "data":
            if self.access_token:
                return {"X-Access-Token": self.access_token}
            if self.api_key:
                return {"X-API-Key": self.api_key}
            raise ConfigurationError("no credentials: pass access_token= or api_key= (or set SBX_API_KEY)")
        if not self.api_key:
            raise ConfigurationError(
                "this call needs a tenant API key: pass api_key= or set SBX_API_KEY "
                "(a sandbox access token is only valid for commands and files)"
            )
        return {"X-API-Key": self.api_key}

    def base_headers(self) -> dict[str, str]:
        out = {"User-Agent": USER_AGENT, "Accept": "application/json"}
        out.update(self.headers)
        return out

    def url(self, path: str) -> str:
        return self.base_url + path

    def ws_url(self, path: str) -> str:
        if self.ws_base_url:
            return self.ws_base_url + path
        parts = urlsplit(self.base_url)
        scheme = "wss" if parts.scheme == "https" else "ws"
        return urlunsplit((scheme, parts.netloc, parts.path.rstrip("/") + path, "", ""))


def seg(value: str) -> str:
    """Escape one URL path segment."""
    return quote(value, safe="")


def ms(seconds: float | None, what: str = "timeout") -> int | None:
    """Convert seconds to the API's integer milliseconds (at least 1)."""
    if seconds is None:
        return None
    if seconds < 0:
        raise ValueError(f"{what} must not be negative")
    if seconds == 0:
        return 0
    return max(round(seconds * 1000), 1)


def describe(exc: BaseException) -> str:
    """A short description of a low-level exception that is safe to show."""
    text = str(exc).strip()
    return f"{type(exc).__name__}: {text}" if text else type(exc).__name__


def is_sbx_error(exc: BaseException) -> bool:
    return isinstance(exc, SbxError)
