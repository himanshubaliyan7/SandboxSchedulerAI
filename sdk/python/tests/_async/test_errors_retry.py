"""Error mapping and automatic retries."""

from __future__ import annotations

from typing import Any

import httpx
import pytest

import sbx
from sbx import AsyncSandbox, RetryPolicy

from ..fake import API_KEY, FakeServer, envelope, error
from .helpers import BASE_URL

CASES: list[tuple[int, str, dict[str, Any], type[sbx.SbxError]]] = [
    (400, "bad_request", {}, sbx.InvalidArgumentError),
    (400, "invalid_timeout", {"max_timeout_ms": 5}, sbx.InvalidArgumentError),
    (400, "network_not_allowed", {}, sbx.InvalidArgumentError),
    (400, "bad_signal", {}, sbx.InvalidArgumentError),
    (400, "command_not_found", {}, sbx.CommandStartError),
    (400, "exec_format", {}, sbx.CommandStartError),
    (400, "bad_cwd", {}, sbx.CommandStartError),
    (401, "unauthorized", {}, sbx.AuthenticationError),
    (403, "permission_denied", {}, sbx.PermissionDeniedError),
    (404, "not_found", {}, sbx.SandboxNotFoundError),
    (404, "template_not_found", {}, sbx.TemplateNotFoundError),
    (404, "process_not_found", {}, sbx.CommandNotFoundError),
    (404, "file_not_found", {}, sbx.PathNotFoundError),
    (405, "method_not_allowed", {}, sbx.ApiError),
    (409, "conflict", {}, sbx.ConflictError),
    (409, "is_directory", {}, sbx.ConflictError),
    (409, "tag_in_use", {}, sbx.ConflictError),
    (
        409,
        "sandbox_not_running",
        {"state": "destroyed", "reason": "idle_timeout"},
        sbx.SandboxNotRunningError,
    ),
    (409, "sandbox_terminated", {"state": "failed", "reason": "oom_killed"}, sbx.SandboxTerminatedError),
    (413, "payload_too_large", {}, sbx.PayloadTooLargeError),
    (413, "too_large", {}, sbx.PayloadTooLargeError),
    (429, "rate_limited", {"class": "default"}, sbx.RateLimitError),
    (429, "quota_exceeded", {}, sbx.QuotaExceededError),
    (500, "internal", {}, sbx.ServerError),
    (500, "start_failed", {}, sbx.CommandStartError),
    (500, "io_error", {}, sbx.ServerError),
    (501, "not_implemented", {}, sbx.ServerError),
    (502, "agent_unavailable", {}, sbx.AgentUnavailableError),
    (503, "pool_empty", {}, sbx.PoolEmptyError),
    (503, "shutting_down", {}, sbx.ServiceUnavailableError),
    (504, "timeout", {}, sbx.SandboxTimeoutError),
    (507, "no_space", {}, sbx.NoSpaceError),
    (418, "some_future_code", {}, sbx.ApiError),
    (409, "some_future_conflict", {}, sbx.ConflictError),
    (500, "some_future_failure", {}, sbx.ServerError),
]


@pytest.mark.parametrize(("status", "code", "details", "cls"), CASES, ids=[f"{c[0]}-{c[1]}" for c in CASES])
async def test_error_mapping(
    server: FakeServer, status: int, code: str, details: dict[str, Any], cls: type[sbx.SbxError]
) -> None:
    sandbox = await AsyncSandbox.create(
        "python", api_key=API_KEY, transport=server.transport(), retry=RetryPolicy.none()
    )
    server.inject("POST", r"/signal$", error(status, code, "the message", retry_after=7, **details))
    with pytest.raises(sbx.SbxError) as e:
        await sandbox.commands.signal("p1", "SIGINT")
    err = e.value
    assert type(err) is cls
    assert "the message" in str(err)
    if isinstance(err, sbx.ApiError):
        assert (err.status_code, err.code, err.request_id, err.retry_after) == (status, code, "req-test", 7.0)
        assert err.details == details
        assert code in str(err) and "req-test" in str(err)
    if isinstance(err, sbx.SandboxNotRunningError):
        assert (err.state, err.reason) == (details["state"], details["reason"])
    if cls is sbx.SandboxTimeoutError:
        assert isinstance(err, TimeoutError)


async def test_hierarchy() -> None:
    assert issubclass(sbx.SandboxTerminatedError, sbx.SandboxNotRunningError)
    assert issubclass(sbx.SandboxNotRunningError, sbx.ConflictError)
    assert issubclass(sbx.PoolEmptyError, sbx.ServiceUnavailableError)
    assert issubclass(sbx.ServiceUnavailableError, sbx.ServerError)
    assert issubclass(sbx.CommandTimeoutError, sbx.CommandExitError)
    assert issubclass(sbx.CommandTimeoutError, TimeoutError)
    assert issubclass(sbx.StreamError, sbx.TransportError)
    for name in sbx.errors.__all__:
        assert issubclass(getattr(sbx, name), sbx.SbxError)


async def test_unknown_route_is_a_plain_not_found(sandbox: AsyncSandbox, server: FakeServer) -> None:
    server.inject("GET", r"/stats$", error(404, "not_found", "no such route: GET /v1/x"))
    with pytest.raises(sbx.NotFoundError) as e:
        await sandbox.stats()
    assert type(e.value) is sbx.NotFoundError


async def test_error_that_is_not_json(sandbox: AsyncSandbox, server: FakeServer, sleeps: list[float]) -> None:
    server.inject("POST", r"/signal$", httpx.Response(502, text="<html>Bad Gateway</html>"))
    with pytest.raises(sbx.ServiceUnavailableError) as e:
        await sandbox.commands.signal("p1", "SIGINT")
    assert e.value.status_code == 502 and e.value.code == "" and "Bad Gateway" in str(e.value)
    server.inject("POST", r"/signal$", httpx.Response(500))
    with pytest.raises(sbx.ServerError, match="HTTP 500"):
        await sandbox.commands.signal("p1", "SIGINT")


async def test_create_retries_rate_limit_with_retry_after(server: FakeServer, sleeps: list[float]) -> None:
    server.inject(
        "POST",
        r"^/v1/sandboxes$",
        error(429, "rate_limited", retry_after=2, **{"class": "create"}),
        error(503, "pool_empty", retry_after=1),
    )
    sb = await AsyncSandbox.create("python", api_key=API_KEY, base_url=BASE_URL, transport=server.transport())
    assert sleeps == [2.0, 1.0]
    assert len(server.seen("POST", r"^/v1/sandboxes$")) == 3
    assert sb.sandbox_id


async def test_retries_are_bounded(server: FakeServer, sleeps: list[float]) -> None:
    server.inject("POST", r"^/v1/sandboxes$", *[error(503, "pool_empty", retry_after=1) for _ in range(10)])
    with pytest.raises(sbx.PoolEmptyError) as e:
        await AsyncSandbox.create("python", api_key=API_KEY, transport=server.transport())
    assert e.value.retry_after == 1.0
    assert sleeps == [1.0, 1.0, 1.0]  # the default: three retries
    assert len(server.requests) == 4

    server.requests.clear()
    sleeps.clear()
    with pytest.raises(sbx.PoolEmptyError):
        await AsyncSandbox.create(
            "python", api_key=API_KEY, transport=server.transport(), retry=RetryPolicy(max_retries=1)
        )
    assert sleeps == [1.0] and len(server.requests) == 2


async def test_retries_can_be_switched_off(server: FakeServer, sleeps: list[float]) -> None:
    server.inject(
        "POST", r"^/v1/sandboxes$", error(429, "rate_limited", retry_after=3, **{"class": "create"})
    )
    with pytest.raises(sbx.RateLimitError) as e:
        await AsyncSandbox.create(
            "python", api_key=API_KEY, transport=server.transport(), retry=RetryPolicy.none()
        )
    assert (e.value.retry_after, e.value.limit_class) == (3.0, "create")
    assert sleeps == [] and len(server.requests) == 1


async def test_a_long_retry_after_is_not_waited_for(server: FakeServer, sleeps: list[float]) -> None:
    server.inject("POST", r"^/v1/sandboxes$", error(429, "rate_limited", retry_after=3600))
    with pytest.raises(sbx.RateLimitError):
        await AsyncSandbox.create("python", api_key=API_KEY, transport=server.transport())
    assert sleeps == []


async def test_backoff_without_retry_after(server: FakeServer, sleeps: list[float]) -> None:
    server.inject("POST", r"^/v1/sandboxes$", *[error(503, "pool_empty") for _ in range(3)])
    await AsyncSandbox.create("python", api_key=API_KEY, transport=server.transport())
    assert len(sleeps) == 3
    # Exponential from 0.25 s with +-25 % jitter.
    for delay, base in zip(sleeps, (0.25, 0.5, 1.0), strict=True):
        assert base * 0.75 <= delay <= base * 1.25


async def test_quota_exceeded_is_not_retried_by_default(server: FakeServer, sleeps: list[float]) -> None:
    server.inject("POST", r"^/v1/sandboxes$", error(429, "quota_exceeded", retry_after=5))
    with pytest.raises(sbx.QuotaExceededError) as e:
        await AsyncSandbox.create("python", api_key=API_KEY, transport=server.transport())
    assert e.value.retry_after == 5.0 and sleeps == []
    server.inject("POST", r"^/v1/sandboxes$", error(429, "quota_exceeded", retry_after=5))
    await AsyncSandbox.create(
        "python", api_key=API_KEY, transport=server.transport(), retry=RetryPolicy(retry_quota_exceeded=True)
    )
    assert sleeps == [5.0]


async def test_what_is_retried_depends_on_the_call(
    sandbox: AsyncSandbox, server: FakeServer, sleeps: list[float]
) -> None:
    # Idempotent GET: 502, 503 and 429 are all retried.
    server.inject(
        "GET",
        r"/commands$",
        error(502, "agent_unavailable", retry_after=1),
        error(503, "shutting_down", retry_after=5),
        error(429, "rate_limited", retry_after=1),
    )
    assert await sandbox.commands.list() == []
    assert sleeps == [1.0, 5.0, 1.0]
    sleeps.clear()

    # Starting a command is not idempotent: only the rate limiter (which
    # answers before anything happens) is retried.
    server.inject("POST", r"/commands$", error(429, "rate_limited", retry_after=1))
    assert (await sandbox.commands.run("true")).exit_code == 0
    assert sleeps == [1.0]
    sleeps.clear()
    for status, code in ((502, "agent_unavailable"), (503, "shutting_down")):
        server.inject("POST", r"/commands$", error(status, code, retry_after=1))
        with pytest.raises(sbx.ServiceUnavailableError):
            await sandbox.commands.run("true")
        server.inject("POST", r"/commands$", error(status, code, retry_after=1))
        with pytest.raises(sbx.ServiceUnavailableError):
            await sandbox.commands.run("true", background=True)
    assert sleeps == []
    assert len(server.sandbox().commands) == 1


async def test_connection_errors(sandbox: AsyncSandbox, server: FakeServer, sleeps: list[float]) -> None:
    server.inject("GET", r"/commands$", httpx.ConnectError("refused"), httpx.ReadTimeout("slow"))
    assert await sandbox.commands.list() == []
    assert len(sleeps) == 2

    server.inject("POST", r"/signal$", httpx.ConnectError("refused"))
    with pytest.raises(sbx.TransportError, match="ConnectError: refused") as e:
        await sandbox.commands.signal("p1", "SIGINT")
    assert not isinstance(e.value, sbx.ApiError)

    server.inject("POST", r"/signal$", httpx.ReadTimeout("slow"))
    with pytest.raises(sbx.SandboxTimeoutError) as t:
        await sandbox.commands.signal("p1", "SIGINT")
    assert isinstance(t.value, TimeoutError)

    server.inject("GET", r"/commands$", *[httpx.ConnectError("refused") for _ in range(4)])
    with pytest.raises(sbx.TransportError):
        await sandbox.commands.list()


async def test_error_event_envelope_helper() -> None:
    err = sbx.errors.error_from_envelope(
        envelope("sandbox_terminated", "gone", state="failed", reason="oom_killed")
    )
    assert isinstance(err, sbx.SandboxTerminatedError)
    assert (err.status_code, err.reason, err.request_id) == (None, "oom_killed", "req-test")
    assert isinstance(sbx.errors.error_from_envelope("garbage", status_code=503), sbx.ServiceUnavailableError)
    assert isinstance(sbx.errors.error_from_envelope(None), sbx.ApiError)
