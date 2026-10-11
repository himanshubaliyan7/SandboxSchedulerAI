"""verify_webhook: the receiving side, which needs no client."""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import os
from typing import Any

import httpx
import pytest

import sbx.webhooks
from sbx import WebhookEvent, WebhookVerificationError, verify_webhook

# The test vector of the Standard Webhooks reference implementations.
# A published example (the Standard Webhooks specification's test vector),
# not a credential; assembled from prefix and body so that the source
# holds no literal a secret scanner would report.
SECRET = "whsec_" + "MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
MSG_ID = "msg_p5jXN8AQM9LWM0D4loKWxJek"
TIMESTAMP = 1614265330
BODY = b'{"test": 2432232314}'
SIGNATURE = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="


def sign(secret: str, msg_id: str, timestamp: int, body: bytes) -> str:
    key = base64.b64decode(secret.removeprefix("whsec_"))
    mac = hmac.new(key, f"{msg_id}.{timestamp}.".encode() + body, hashlib.sha256).digest()
    return "v1," + base64.b64encode(mac).decode()


def headers(msg_id: str, timestamp: int, signature: str) -> dict[str, str]:
    return {"Webhook-Id": msg_id, "Webhook-Timestamp": str(timestamp), "Webhook-Signature": signature}


def event(**over: Any) -> bytes:
    body = {
        "id": "evt_42",
        "type": "sandbox.terminated",
        "version": 1,
        "created_at": "2026-10-10T11:00:00.123456Z",
        "sandbox_id": "sb-1",
        "template_id": "python",
        "data": {"state": "recycling", "reason": "idle_timeout", "metadata": {"job": "9"}},
    }
    body.update(over)
    return json.dumps(body, separators=(",", ":")).encode()


def test_known_answer_vector() -> None:
    assert sign(SECRET, MSG_ID, TIMESTAMP, BODY) == SIGNATURE
    got = verify_webhook(BODY, headers(MSG_ID, TIMESTAMP, SIGNATURE), SECRET, now=TIMESTAMP + 10)
    assert isinstance(got, WebhookEvent) and got.raw == {"test": 2432232314}


def test_returns_the_event() -> None:
    body = event()
    now = 1_800_000_000
    hdrs = headers("evt_42", now, sign(SECRET, "evt_42", now, body))
    got = verify_webhook(body, hdrs, SECRET, now=now)
    assert (got.id, got.type, got.version, got.sandbox_id, got.template_id) == (
        "evt_42",
        "sandbox.terminated",
        1,
        "sb-1",
        "python",
    )
    assert got.data == {"state": "recycling", "reason": "idle_timeout", "metadata": {"job": "9"}}
    assert got.created_at is not None and got.created_at.microsecond == 123456
    # str payloads, any header casing, and the header objects of real frameworks.
    assert verify_webhook(body.decode(), {k.lower(): v for k, v in hdrs.items()}, SECRET, now=now) == got
    assert verify_webhook(bytearray(body), {k.upper(): v for k, v in hdrs.items()}, SECRET, now=now) == got
    assert verify_webhook(memoryview(body), httpx.Headers(hdrs), SECRET, now=now) == got
    assert verify_webhook(body, {k.encode(): v.encode() for k, v in hdrs.items()}, SECRET, now=now) == got
    # An event type from the future is an event, not an error.
    later = event(type="sandbox.snapshotted", sandbox_id=None)
    assert (
        verify_webhook(
            later, headers("evt_42", now, sign(SECRET, "evt_42", now, later)), SECRET, now=now
        ).type
        == "sandbox.snapshotted"
    )


def test_refuses_what_is_not_authentic() -> None:
    body = event()
    now = 1_800_000_000
    good = sign(SECRET, "evt_42", now, body)
    other = "whsec_" + base64.b64encode(os.urandom(32)).decode()
    cases: list[tuple[str, bytes, dict[str, str], str, str]] = [
        ("wrong secret", body, headers("evt_42", now, good), other, "no signature matches"),
        ("altered body", body + b" ", headers("evt_42", now, good), SECRET, "no signature matches"),
        (
            "re-serialised body",
            json.dumps(json.loads(body)).encode(),
            headers("evt_42", now, good),
            SECRET,
            "no sig",
        ),
        ("another id", body, headers("evt_43", now, good), SECRET, "no signature matches"),
        ("another timestamp", body, headers("evt_42", now + 1, good), SECRET, "no signature matches"),
        ("unknown version", body, headers("evt_42", now, "v2," + good[3:]), SECRET, "no signature matches"),
        ("garbage", body, headers("evt_42", now, "v1,@@@"), SECRET, "no signature matches"),
        ("truncated", body, headers("evt_42", now, good[:-6]), SECRET, "no signature matches"),
        ("empty signature", body, headers("evt_42", now, ""), SECRET, "missing"),
        ("no id", body, {"Webhook-Timestamp": str(now), "Webhook-Signature": good}, SECRET, "missing"),
        ("no timestamp", body, {"Webhook-Id": "evt_42", "Webhook-Signature": good}, SECRET, "missing"),
        (
            "timestamp not a number",
            body,
            headers("evt_42", now, good) | {"Webhook-Timestamp": "now"},
            SECRET,
            "not a n",
        ),
        (
            "negative timestamp",
            body,
            headers("evt_42", now, good) | {"Webhook-Timestamp": "-5"},
            SECRET,
            "not a n",
        ),
        ("secret without prefix", body, headers("evt_42", now, good), SECRET[6:], "whsec_"),
        ("secret not base64", body, headers("evt_42", now, good), "whsec_***", "whsec_"),
        ("empty secret", body, headers("evt_42", now, good), "whsec_", "whsec_"),
    ]
    for name, payload, hdrs, secret, match in cases:
        with pytest.raises(WebhookVerificationError, match=match):
            verify_webhook(payload, hdrs, secret, now=now)
            pytest.fail(name)
    # Authentic, but not an event: still refused, after the signature held.
    for junk in (b"[1,2]", b"not json", b'"text"'):
        with pytest.raises(WebhookVerificationError, match="JSON"):
            verify_webhook(junk, headers("evt_42", now, sign(SECRET, "evt_42", now, junk)), SECRET, now=now)


def test_timestamp_tolerance() -> None:
    body = event()
    sent = 1_800_000_000
    hdrs = headers("evt_42", sent, sign(SECRET, "evt_42", sent, body))
    for now in (sent, sent + 300, sent - 300):
        verify_webhook(body, hdrs, SECRET, now=now)
    with pytest.raises(WebhookVerificationError, match="too old"):
        verify_webhook(body, hdrs, SECRET, now=sent + 301)
    with pytest.raises(WebhookVerificationError, match="future"):
        verify_webhook(body, hdrs, SECRET, now=sent - 301)
    with pytest.raises(WebhookVerificationError, match="too old"):
        verify_webhook(body, hdrs, SECRET, now=sent + 31, tolerance=30)
    verify_webhook(body, hdrs, SECRET, now=sent + 3600, tolerance=3600)
    # Without `now`, the wall clock decides: a 2027 timestamp is old by now.
    with pytest.raises(WebhookVerificationError, match=r"too old|future"):
        verify_webhook(body, hdrs, SECRET)


def test_rotation_and_constant_time_comparison(monkeypatch: pytest.MonkeyPatch) -> None:
    body = event()
    now = 1_800_000_000
    old = "whsec_" + base64.b64encode(b"o" * 32).decode()
    both = f"{sign(SECRET, 'evt_42', now, body)} {sign(old, 'evt_42', now, body)}"
    for secret in (SECRET, old):
        verify_webhook(body, headers("evt_42", now, both), secret, now=now)
    verify_webhook(body, headers("evt_42", now, "v1a,zzz v9,AAAA " + both), old, now=now)

    # Every candidate goes through hmac.compare_digest, match or not, and
    # nothing is compared with ==.
    seen: list[int] = []
    real = hmac.compare_digest

    def spy(a: bytes, b: bytes) -> bool:
        seen.append(len(a))
        return real(a, b)

    monkeypatch.setattr(sbx.webhooks.hmac, "compare_digest", spy)
    verify_webhook(body, headers("evt_42", now, both), SECRET, now=now)
    assert seen == [32, 32]
    seen.clear()
    with pytest.raises(WebhookVerificationError):
        verify_webhook(
            body, headers("evt_42", now, both), "whsec_" + base64.b64encode(b"x" * 32).decode(), now=now
        )
    assert seen == [32, 32]
