"""Lifecycle webhooks: the data types, and verifying what a receiver gets.

:func:`verify_webhook` needs no client and no network: it is what the
HTTP handler that receives webhooks calls. The requests are signed as the
`Standard Webhooks <https://www.standardwebhooks.com>`_ specification
describes, so that project's libraries verify them too.
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
import json
import time
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any

from .errors import SbxError
from .models import parse_time

__all__ = [
    "WEBHOOK_EVENT_TYPES",
    "Webhook",
    "WebhookDelivery",
    "WebhookEvent",
    "WebhookTestResult",
    "WebhookVerificationError",
    "verify_webhook",
]

#: The event types a webhook can subscribe to (``"*"`` subscribes to all).
WEBHOOK_EVENT_TYPES = (
    "sandbox.created",
    "sandbox.running",
    "sandbox.terminated",
    "sandbox.destroyed",
    "command.started",
    "command.finished",
)

_SECRET_PREFIX = "whsec_"
_DEFAULT_TOLERANCE = 300.0


class WebhookVerificationError(SbxError):
    """A received webhook is not authentic, or too old to trust.

    Answer it with ``400`` and do not act on the payload.
    """


def _str(d: Mapping[str, Any], key: str) -> str:
    v = d.get(key)
    return v if isinstance(v, str) else ""


def _opt_str(d: Mapping[str, Any], key: str) -> str | None:
    v = d.get(key)
    return v if isinstance(v, str) and v else None


def _int(d: Mapping[str, Any], key: str) -> int:
    v = d.get(key)
    if isinstance(v, bool):
        return 0
    if isinstance(v, (int, float)):
        return int(v)
    return 0


@dataclass(frozen=True)
class WebhookEvent:
    """A verified webhook payload.

    ``data`` depends on ``type``: ``state``, ``reason`` and ``metadata``
    for sandbox events; the execution record (``pid``, ``cmd``, ``args``,
    ``exit_code``, ``duration_ms``, ...) for command events. Receivers
    must ignore types and fields they do not know.
    """

    id: str
    type: str
    version: int = 1
    created_at: datetime | None = None
    sandbox_id: str | None = None
    template_id: str | None = None
    data: Mapping[str, Any] = field(default_factory=lambda: dict[str, Any]())
    raw: Mapping[str, Any] = field(default_factory=lambda: dict[str, Any](), repr=False, compare=False)

    @classmethod
    def from_json(cls, d: Mapping[str, Any]) -> WebhookEvent:
        data = d.get("data")
        return cls(
            id=_str(d, "id"),
            type=_str(d, "type"),
            version=_int(d, "version") or 1,
            created_at=parse_time(d.get("created_at")),
            sandbox_id=_opt_str(d, "sandbox_id"),
            template_id=_opt_str(d, "template_id"),
            data=dict(data) if isinstance(data, Mapping) else {},
            raw=dict(d),
        )


@dataclass(frozen=True)
class Webhook:
    """A webhook's configuration and delivery state.

    ``secret`` is set only on what :meth:`~sbx.Webhooks.create` and
    :meth:`~sbx.Webhooks.rotate_secret` return; it is left out of ``repr``
    and of ``raw``.
    """

    id: str
    url: str
    event_types: tuple[str, ...] = ()
    description: str = ""
    enabled: bool = True
    disabled_reason: str | None = None
    disabled_at: datetime | None = None
    created_at: datetime | None = None
    updated_at: datetime | None = None
    secret: str | None = field(default=None, repr=False)
    previous_secret_expires_at: datetime | None = None
    consecutive_failures: int = 0
    failing_since: datetime | None = None
    last_success_at: datetime | None = None
    last_failure_at: datetime | None = None
    last_error: str | None = None
    next_attempt_at: datetime | None = None
    dropped_events: int = 0
    raw: Mapping[str, Any] = field(default_factory=lambda: dict[str, Any](), repr=False, compare=False)

    @classmethod
    def from_json(cls, d: Mapping[str, Any]) -> Webhook:
        types = d.get("event_types")
        return cls(
            id=_str(d, "id"),
            url=_str(d, "url"),
            event_types=tuple(t for t in types if isinstance(t, str)) if isinstance(types, Sequence) else (),
            description=_str(d, "description"),
            enabled=d.get("enabled") is not False,
            disabled_reason=_opt_str(d, "disabled_reason"),
            disabled_at=parse_time(d.get("disabled_at")),
            created_at=parse_time(d.get("created_at")),
            updated_at=parse_time(d.get("updated_at")),
            secret=_opt_str(d, "secret"),
            previous_secret_expires_at=parse_time(d.get("previous_secret_expires_at")),
            consecutive_failures=_int(d, "consecutive_failures"),
            failing_since=parse_time(d.get("failing_since")),
            last_success_at=parse_time(d.get("last_success_at")),
            last_failure_at=parse_time(d.get("last_failure_at")),
            last_error=_opt_str(d, "last_error"),
            next_attempt_at=parse_time(d.get("next_attempt_at")),
            dropped_events=_int(d, "dropped_events"),
            raw={k: v for k, v in d.items() if k != "secret"},
        )


@dataclass(frozen=True)
class WebhookTestResult:
    """What the endpoint did with a ``webhook.test`` event."""

    event_id: str
    delivered: bool
    status: int | None = None
    duration_ms: int = 0
    error_code: str | None = None
    error: str | None = None
    raw: Mapping[str, Any] = field(default_factory=lambda: dict[str, Any](), repr=False, compare=False)

    @classmethod
    def from_json(cls, d: Mapping[str, Any]) -> WebhookTestResult:
        return cls(
            event_id=_str(d, "event_id"),
            delivered=d.get("delivered") is True,
            status=_int(d, "status") or None,
            duration_ms=_int(d, "duration_ms"),
            error_code=_opt_str(d, "error_code"),
            error=_opt_str(d, "error"),
            raw=dict(d),
        )


@dataclass(frozen=True)
class WebhookDelivery:
    """One row of a webhook's delivery log.

    ``status`` is ``"succeeded"``, ``"failed"`` (it will be retried) or
    ``"dropped"`` (the event, or ``dropped_events`` events, were given up on).
    """

    id: int
    status: str
    attempt: int = 1
    event_id: str | None = None
    event_type: str | None = None
    response_status: int | None = None
    duration_ms: int = 0
    error_code: str | None = None
    error: str | None = None
    dropped_events: int = 0
    created_at: datetime | None = None
    raw: Mapping[str, Any] = field(default_factory=lambda: dict[str, Any](), repr=False, compare=False)

    @classmethod
    def from_json(cls, d: Mapping[str, Any]) -> WebhookDelivery:
        return cls(
            id=_int(d, "id"),
            status=_str(d, "status"),
            attempt=_int(d, "attempt") or 1,
            event_id=_opt_str(d, "event_id"),
            event_type=_opt_str(d, "event_type"),
            response_status=_int(d, "response_status") or None,
            duration_ms=_int(d, "duration_ms"),
            error_code=_opt_str(d, "error_code"),
            error=_opt_str(d, "error"),
            dropped_events=_int(d, "dropped_events"),
            created_at=parse_time(d.get("created_at")),
            raw=dict(d),
        )


def _header(headers: Mapping[Any, Any], name: str) -> str:
    """A header value, whatever the case of its name and whether it is ``str`` or ``bytes``."""
    for key, value in headers.items():
        if isinstance(key, bytes):
            key = key.decode("latin-1")
        if not isinstance(key, str) or key.lower() != name:
            continue
        if isinstance(value, bytes):
            value = value.decode("latin-1")
        if isinstance(value, str):
            return value.strip()
    return ""


def verify_webhook(
    payload: bytes | bytearray | memoryview | str,
    headers: Mapping[Any, Any],
    secret: str,
    *,
    tolerance: float = _DEFAULT_TOLERANCE,
    now: float | None = None,
) -> WebhookEvent:
    """Check the signature of a received webhook and return its event.

    Args:
        payload: the raw request body, exactly as it arrived. Do not parse
            and re-serialise it: the signature covers the bytes.
        headers: the request headers (any mapping; names are matched
            without regard to case). ``Webhook-Id``, ``Webhook-Timestamp``
            and ``Webhook-Signature`` are used.
        secret: the webhook's signing secret (``whsec_...``).
        tolerance: how many seconds the timestamp may differ from the
            local clock, in either direction, before the request is
            refused as a possible replay. Default 300.
        now: the current Unix time, for tests.

    Raises:
        WebhookVerificationError: a header is missing, the timestamp is
            outside the tolerance, no signature matches, or the body is
            not a JSON object.

    The comparison takes the same time whether or not a signature
    matches. During a secret rotation the header carries two signatures;
    one match is enough.
    """
    body = payload.encode("utf-8") if isinstance(payload, str) else bytes(payload)
    if not secret.startswith(_SECRET_PREFIX):
        raise WebhookVerificationError("the secret is not a whsec_ signing secret")
    try:
        key = base64.b64decode(secret[len(_SECRET_PREFIX) :], validate=True)
    except (binascii.Error, ValueError):
        raise WebhookVerificationError("the secret is not a whsec_ signing secret") from None
    if not key:
        raise WebhookVerificationError("the secret is not a whsec_ signing secret")

    msg_id = _header(headers, "webhook-id")
    timestamp = _header(headers, "webhook-timestamp")
    signatures = _header(headers, "webhook-signature")
    if not msg_id or not timestamp or not signatures:
        raise WebhookVerificationError(
            "a signature header is missing (Webhook-Id, Webhook-Timestamp, Webhook-Signature)"
        )
    if not timestamp.isascii() or not timestamp.isdigit():
        raise WebhookVerificationError("the Webhook-Timestamp header is not a number")
    sent = int(timestamp)
    current = time.time() if now is None else now
    if current - sent > tolerance:
        raise WebhookVerificationError("the webhook's timestamp is too old")
    if sent - current > tolerance:
        raise WebhookVerificationError("the webhook's timestamp is in the future")

    signed = msg_id.encode("utf-8") + b"." + timestamp.encode("ascii") + b"." + body
    expected = hmac.new(key, signed, hashlib.sha256).digest()
    matched = False
    for candidate in signatures.split():
        version, _, encoded = candidate.partition(",")
        if version != "v1":
            continue
        try:
            got = base64.b64decode(encoded, validate=True)
        except (binascii.Error, ValueError):
            continue
        # No early exit: every candidate is compared, in constant time.
        matched = hmac.compare_digest(got, expected) or matched
    if not matched:
        raise WebhookVerificationError("no signature matches: wrong secret, or the body was altered")

    try:
        parsed = json.loads(body)
    except ValueError:
        raise WebhookVerificationError("the signed body is not JSON") from None
    if not isinstance(parsed, dict):
        raise WebhookVerificationError("the signed body is not a JSON object")
    return WebhookEvent.from_json(parsed)  # pyright: ignore[reportUnknownArgumentType]
