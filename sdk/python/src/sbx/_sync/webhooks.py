# Generated from src/sbx/_async/webhooks.py by scripts/unasync.py. Do not edit.
"""Managing lifecycle webhooks."""

from __future__ import annotations

from builtins import list as List
from collections.abc import Mapping, Sequence
from typing import Any

import httpx

from .._config import ConnectionConfig, RetryPolicy, seg
from ..errors import NotFoundError
from ..webhooks import Webhook, WebhookDelivery, WebhookTestResult
from .api import ApiClient


class Webhooks:
    """The webhooks of a tenant.

    Webhooks are control plane: every call needs the tenant API key
    (``api_key=`` or ``SBX_API_KEY``); a sandbox access token is not
    accepted. Used as a context manager, the HTTP connections are released
    on exit::

        with Webhooks() as webhooks:
            hook = webhooks.create("https://hooks.example.com/warmbox",
                                         event_types=["sandbox.terminated"])
            store_somewhere_safe(hook.secret)   # shown this once

    To check what your endpoint receives, use :func:`sbx.verify_webhook`,
    which needs no client.
    """

    def __init__(
        self,
        *,
        api_key: str | None = None,
        base_url: str | None = None,
        request_timeout: float | None = None,
        retry: RetryPolicy | None = None,
        headers: Mapping[str, str] | None = None,
        http_client: httpx.Client | None = None,
        transport: httpx.BaseTransport | None = None,
    ) -> None:
        config = ConnectionConfig.resolve(
            api_key=api_key,
            base_url=base_url,
            access_token=None,
            request_timeout=request_timeout,
            retry=retry,
            headers=headers,
            ws_base_url=None,
        )
        self._api = ApiClient(config, http_client=http_client, transport=transport)

    def __repr__(self) -> str:
        return f"<{type(self).__name__} at {self._api.config.base_url}>"

    @property
    def config(self) -> ConnectionConfig:
        return self._api.config

    @staticmethod
    def _path(webhook_id: str = "", suffix: str = "") -> str:
        if not webhook_id:
            return "/v1/webhooks"
        return f"/v1/webhooks/{seg(webhook_id)}{suffix}"

    def create(
        self,
        url: str,
        *,
        event_types: Sequence[str] | None = None,
        description: str | None = None,
        enabled: bool = True,
    ) -> Webhook:
        """Create a webhook. The result carries ``secret``, this once.

        Args:
            url: where events are POSTed. ``https`` with a host that
                resolves to a public address, unless the operator has
                relaxed that.
            event_types: the events to receive (see
                :data:`sbx.WEBHOOK_EVENT_TYPES`); ``None`` or empty for
                all of them.
            description: free-form note.
            enabled: ``False`` creates it switched off.

        Not retried on a timeout or a connection error: the request may
        have been carried out, and a second one would create a second
        webhook with another secret.
        """
        body: dict[str, Any] = {"url": url}
        if event_types:
            body["event_types"] = List(event_types)
        if description:
            body["description"] = description
        if not enabled:
            body["enabled"] = False
        data = self._api.json("POST", self._path(), plane="control", json=body)
        return Webhook.from_json(data)

    def list(self) -> List[Webhook]:
        """The tenant's webhooks, oldest first. Secrets are never listed."""
        data = self._api.json("GET", self._path(), plane="control", idempotent=True)
        items = data.get("webhooks")
        return [Webhook.from_json(w) for w in items] if isinstance(items, List) else []

    def get(self, webhook_id: str) -> Webhook:
        """One webhook: its configuration and delivery state."""
        data = self._api.json("GET", self._path(webhook_id), plane="control", idempotent=True)
        return Webhook.from_json(data)

    def update(
        self,
        webhook_id: str,
        *,
        url: str | None = None,
        event_types: Sequence[str] | None = None,
        description: str | None = None,
        enabled: bool | None = None,
    ) -> Webhook:
        """Change a webhook; arguments left as ``None`` keep their value.

        ``event_types=[]`` subscribes to every event. ``enabled=True`` on
        a disabled webhook starts it again at the present: events that
        occurred while it was disabled are not delivered.
        """
        body: dict[str, Any] = {}
        if url is not None:
            body["url"] = url
        if event_types is not None:
            body["event_types"] = List(event_types)
        if description is not None:
            body["description"] = description
        if enabled is not None:
            body["enabled"] = enabled
        if not body:
            raise ValueError("nothing to change: give url, event_types, description or enabled")
        data = self._api.json("PATCH", self._path(webhook_id), plane="control", json=body, idempotent=True)
        return Webhook.from_json(data)

    def delete(self, webhook_id: str) -> bool:
        """Delete a webhook and its delivery log. ``False`` if there is no such webhook."""
        try:
            self._api.request("DELETE", self._path(webhook_id), plane="control", idempotent=True)
        except NotFoundError:
            return False
        return True

    def rotate_secret(self, webhook_id: str, *, keep_previous: float | None = None) -> Webhook:
        """Replace the signing secret; the result carries the new one, this once.

        For ``keep_previous`` seconds (24 hours when ``None``; ``0`` for
        not at all; at most 7 days) requests are signed with both secrets,
        so the receiver can be switched over without rejecting anything.

        Not retried on a timeout or a connection error, for the reason
        given at :meth:`create`.
        """
        body: dict[str, Any] | None = None
        if keep_previous is not None:
            if keep_previous < 0:
                raise ValueError("keep_previous must not be negative")
            body = {"keep_previous_seconds": int(keep_previous)}
        data = self._api.json("POST", self._path(webhook_id, "/rotate-secret"), plane="control", json=body)
        return Webhook.from_json(data)

    def test(self, webhook_id: str) -> WebhookTestResult:
        """Send a signed ``webhook.test`` event now and report the endpoint's answer.

        A failing endpoint is a result (``delivered`` is ``False``), not
        an exception.
        """
        data = self._api.json("POST", self._path(webhook_id, "/test"), plane="control")
        return WebhookTestResult.from_json(data)

    def deliveries(self, webhook_id: str, *, limit: int | None = None) -> List[WebhookDelivery]:
        """Recent delivery attempts, newest first (50 unless ``limit`` says otherwise; at most 500)."""
        params = {"limit": limit} if limit is not None else None
        data = self._api.json(
            "GET", self._path(webhook_id, "/deliveries"), plane="control", params=params, idempotent=True
        )
        items = data.get("deliveries")
        return [WebhookDelivery.from_json(d) for d in items] if isinstance(items, List) else []

    def close(self) -> None:
        """Release the HTTP connections."""
        self._api.close()

    def __enter__(self) -> Webhooks:
        return self

    def __exit__(self, exc_type: object, exc: object, tb: object) -> None:
        self.close()
