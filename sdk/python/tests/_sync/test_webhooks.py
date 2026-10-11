# Generated from tests/_async/test_webhooks.py by scripts/unasync.py. Do not edit.
"""Managing webhooks through the API."""

from __future__ import annotations

import json
from typing import Any

import httpx
import pytest

from sbx import (
    AuthenticationError,
    ConflictError,
    InvalidArgumentError,
    NotFoundError,
    RateLimitError,
    TransportError,
    Webhook,
    Webhooks,
)

from ..fake import API_KEY
from .helpers import BASE_URL

# A published example (the Standard Webhooks specification's test vector),
# not a credential; assembled from prefix and body so that the source
# holds no literal a secret scanner would report.
SECRET = "whsec_" + "MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"


def envelope(status: int, code: str, message: str = "nope") -> httpx.Response:
    return httpx.Response(status, json={"error": {"code": code, "message": message}})


class Hooks:
    """A stand-in for the webhook routes of sbxd."""

    def __init__(self) -> None:
        self.requests: list[httpx.Request] = []
        self.hooks: dict[str, dict[str, Any]] = {}
        self.fail: list[httpx.Response | Exception] = []

    def transport(self) -> httpx.MockTransport:
        return httpx.MockTransport(self.handle)

    def body(self, index: int = -1) -> Any:
        content = self.requests[index].content
        return json.loads(content) if content else None

    def handle(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(request)
        if self.fail:
            failure = self.fail.pop(0)
            if isinstance(failure, Exception):
                raise failure
            return failure
        if request.headers.get("x-api-key") != API_KEY:
            return envelope(401, "unauthorized")
        path, method = request.url.path, request.method
        body = json.loads(request.content) if request.content else {}
        if path == "/v1/webhooks":
            if method == "POST":
                wid = f"wh_{len(self.hooks) + 1:024x}"
                self.hooks[wid] = {
                    "id": wid,
                    "url": body["url"],
                    "event_types": body.get("event_types") or ["*"],
                    "description": body.get("description", ""),
                    "enabled": body.get("enabled", True),
                    "created_at": "2026-10-10T08:00:00.123456789Z",
                    "updated_at": "2026-10-10T08:00:00.123456789Z",
                    "consecutive_failures": 0,
                    "dropped_events": 0,
                    "a_field_from_the_future": 1,
                }
                return httpx.Response(201, json={**self.hooks[wid], "secret": SECRET})
            return httpx.Response(200, json={"webhooks": list(self.hooks.values())})
        wid, _, rest = path.removeprefix("/v1/webhooks/").partition("/")
        hook = self.hooks.get(wid)
        if hook is None:
            return envelope(404, "not_found", f'webhook "{wid}" not found')
        if rest == "" and method == "GET":
            return httpx.Response(200, json=hook)
        if rest == "" and method == "PATCH":
            hook.update(body)
            if body.get("enabled") is False:
                hook["disabled_reason"] = "user"
            return httpx.Response(200, json=hook)
        if rest == "" and method == "DELETE":
            del self.hooks[wid]
            return httpx.Response(204)
        if rest == "rotate-secret" and method == "POST":
            return httpx.Response(
                200,
                json={**hook, "secret": "whsec_bmV3", "previous_secret_expires_at": "2026-10-11T08:00:00Z"},
            )
        if rest == "test" and method == "POST":
            return httpx.Response(
                200,
                json={
                    "event_id": "evt_test_1",
                    "delivered": False,
                    "status": 503,
                    "duration_ms": 12,
                    "error_code": "http_status",
                    "error": "the endpoint answered 503",
                },
            )
        if rest == "deliveries" and method == "GET":
            rows = [
                {
                    "id": 9,
                    "event_id": "evt_7",
                    "event_type": "sandbox.terminated",
                    "attempt": 3,
                    "status": "succeeded",
                    "response_status": 200,
                    "duration_ms": 40,
                    "created_at": "2026-10-10T08:00:02Z",
                },
                {
                    "id": 8,
                    "event_id": "evt_6",
                    "attempt": 1,
                    "status": "dropped",
                    "duration_ms": 0,
                    "error_code": "retention_expired",
                    "error": "4 event(s) older than the retention window of 24h0m0s were not delivered",
                    "dropped_events": 4,
                    "created_at": "2026-10-10T08:00:01Z",
                },
            ]
            limit = int(request.url.params.get("limit", "50"))
            return httpx.Response(200, json={"deliveries": rows[:limit]})
        return envelope(405, "method_not_allowed")


@pytest.fixture
def hooks() -> Hooks:
    return Hooks()


def client(hooks: Hooks, **kwargs: Any) -> Webhooks:
    return Webhooks(api_key=API_KEY, base_url=BASE_URL, transport=hooks.transport(), **kwargs)


def test_create_returns_the_secret_once(hooks: Hooks) -> None:
    with client(hooks) as webhooks:
        hook = webhooks.create(
            "https://hooks.example.com/in",
            event_types=["sandbox.terminated", "command.finished"],
            description="ci",
            enabled=False,
        )
        assert hooks.body() == {
            "url": "https://hooks.example.com/in",
            "event_types": ["sandbox.terminated", "command.finished"],
            "description": "ci",
            "enabled": False,
        }
        request = hooks.requests[-1]
        assert (request.method, request.url.path) == ("POST", "/v1/webhooks")
        assert request.headers["x-api-key"] == API_KEY and "x-access-token" not in request.headers
        assert isinstance(hook, Webhook)
        assert hook.secret == SECRET and hook.id.startswith("wh_") and not hook.enabled
        assert hook.event_types == ("sandbox.terminated", "command.finished")
        assert hook.created_at is not None and hook.created_at.microsecond == 123456
        # The secret is in neither the repr nor the raw JSON kept for debugging.
        assert SECRET not in repr(hook) and "secret" not in hook.raw
        assert hook.raw["a_field_from_the_future"] == 1

        # Defaults are left to the server.
        webhooks.create("https://hooks.example.com/all")
        assert hooks.body() == {"url": "https://hooks.example.com/all"}

        fetched = webhooks.get(hook.id)
        assert fetched.secret is None and fetched.id == hook.id
        listed = webhooks.list()
        assert [w.id for w in listed] == sorted(hooks.hooks) and all(w.secret is None for w in listed)


def test_update_delete_rotate_test_deliveries(hooks: Hooks) -> None:
    with client(hooks) as webhooks:
        hook = webhooks.create("https://hooks.example.com/in")

        updated = webhooks.update(hook.id, enabled=False, event_types=[], description="")
        assert hooks.body() == {"enabled": False, "event_types": [], "description": ""}
        assert (
            hooks.requests[-1].method == "PATCH" and hooks.requests[-1].url.path == f"/v1/webhooks/{hook.id}"
        )
        assert not updated.enabled and updated.disabled_reason == "user"
        webhooks.update(hook.id, url="https://hooks.example.com/new")
        assert hooks.body() == {"url": "https://hooks.example.com/new"}
        with pytest.raises(ValueError, match="nothing to change"):
            webhooks.update(hook.id)

        rotated = webhooks.rotate_secret(hook.id)
        assert (
            hooks.requests[-1].url.path == f"/v1/webhooks/{hook.id}/rotate-secret"
            and not hooks.requests[-1].content
        )
        assert rotated.secret == "whsec_bmV3" and rotated.previous_secret_expires_at is not None
        webhooks.rotate_secret(hook.id, keep_previous=0)
        assert hooks.body() == {"keep_previous_seconds": 0}
        webhooks.rotate_secret(hook.id, keep_previous=3600.9)
        assert hooks.body() == {"keep_previous_seconds": 3600}
        with pytest.raises(ValueError):
            webhooks.rotate_secret(hook.id, keep_previous=-1)

        result = webhooks.test(hook.id)
        assert hooks.requests[-1].url.path == f"/v1/webhooks/{hook.id}/test"
        assert not result.delivered and result.status == 503 and result.error_code == "http_status"
        assert result.event_id == "evt_test_1" and result.duration_ms == 12

        log = webhooks.deliveries(hook.id)
        assert "limit" not in hooks.requests[-1].url.params
        assert [d.status for d in log] == ["succeeded", "dropped"]
        assert (
            log[0].attempt == 3
            and log[0].response_status == 200
            and log[0].event_type == "sandbox.terminated"
        )
        assert (
            log[1].dropped_events == 4
            and log[1].error_code == "retention_expired"
            and log[1].response_status is None
        )
        assert len(webhooks.deliveries(hook.id, limit=1)) == 1
        assert hooks.requests[-1].url.params["limit"] == "1"

        assert webhooks.delete(hook.id) is True
        assert webhooks.delete(hook.id) is False
        with pytest.raises(NotFoundError):
            webhooks.get(hook.id)
        # Ids are path segments, never spliced into the path.
        with pytest.raises(NotFoundError):
            webhooks.get("../sandboxes")
        assert hooks.requests[-1].url.raw_path == b"/v1/webhooks/..%2Fsandboxes"


def test_errors_and_retries(hooks: Hooks, sleeps: list[float]) -> None:
    with client(hooks) as webhooks:
        hooks.fail.append(
            envelope(400, "webhook_url_not_allowed", "the host does not resolve to a public address")
        )
        with pytest.raises(InvalidArgumentError) as bad:
            webhooks.create("https://internal.example.com/")
        assert bad.value.code == "webhook_url_not_allowed"
        hooks.fail.append(envelope(409, "webhook_limit_exceeded"))
        with pytest.raises(ConflictError) as full:
            webhooks.create("https://hooks.example.com/")
        assert full.value.code == "webhook_limit_exceeded"

        # A read is retried after a rate limit and after a connection error.
        hooks.fail.extend([httpx.Response(429, headers={"Retry-After": "2"}, json={"error": {"code": "rate_limited"}}),
                           httpx.ConnectError("down")])  # fmt: skip
        assert webhooks.list() == []
        assert sleeps[0] == 2 and len(sleeps) == 2

        # A create is not retried when it may have happened: a second
        # request would make a second webhook with another secret.
        before = len(hooks.requests)
        hooks.fail.append(httpx.ReadTimeout("slow"))
        with pytest.raises(Exception) as lost:
            webhooks.create("https://hooks.example.com/")
        assert not isinstance(lost.value, RateLimitError) and len(hooks.requests) == before + 1
        hooks.fail.append(httpx.ConnectError("down"))
        with pytest.raises(TransportError):
            webhooks.rotate_secret("wh_1")

    with Webhooks(api_key="sbx_wrong", base_url=BASE_URL, transport=hooks.transport()) as stranger:
        with pytest.raises(AuthenticationError):
            stranger.list()
    assert API_KEY not in repr(webhooks) and "sbx.test" in repr(webhooks)
