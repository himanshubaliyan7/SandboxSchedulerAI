"""The I/O-free building blocks: NDJSON decoding, offsets, framing, models, retry policy."""

from __future__ import annotations

import base64
import json
import random
import subprocess
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

import pytest

from sbx import NetworkPolicy, RetryPolicy, errors
from sbx import _wire as wire
from sbx._config import ConnectionConfig, ms
from sbx._ndjson import NDJSONDecoder
from sbx._stream import StreamTracker
from sbx.models import (
    CommandInfo,
    EntryInfo,
    ExitEvent,
    SandboxInfo,
    StderrEvent,
    StdoutEvent,
    TruncatedEvent,
    parse_time,
)


def out(kind: str, offset: int, data: bytes) -> dict[str, Any]:
    return {"type": kind, "offset": offset, "data": base64.b64encode(data).decode()}


# -- NDJSON -------------------------------------------------------------------


def test_ndjson_split_anywhere() -> None:
    events = [out("stdout", 0, bytes(range(256)) * 50), {"type": "keepalive", "offset": 12800},
              out("stderr", 12800, "hé 世".encode()), {"type": "exit", "offset": 12807, "exit": {"exit_code": 0}}]  # fmt: skip
    body = b"".join(json.dumps(e).encode() + b"\n" for e in events)
    for size in (1, 2, 5, 13, 100, 4096, len(body)):
        decoder = NDJSONDecoder()
        got: list[dict[str, Any]] = []
        for i in range(0, len(body), size):
            got += decoder.feed(body[i : i + size])
        assert got == events and decoder.pending == 0
    rng = random.Random(7)
    for _ in range(50):
        decoder, got, i = NDJSONDecoder(), [], 0
        while i < len(body):
            n = rng.randint(1, 3000)
            got += decoder.feed(body[i : i + n])
            i += n
        assert got == events


def test_ndjson_partial_line_blank_lines_and_errors() -> None:
    decoder = NDJSONDecoder()
    assert decoder.feed(b'{"type":"keepalive","off') == []
    assert decoder.pending == 24
    assert decoder.feed(b'set":1}\r\n\n  \n{"a":') == [{"type": "keepalive", "offset": 1}]
    assert decoder.feed(b"") == [] and decoder.pending == 5
    with pytest.raises(errors.ProtocolError):
        NDJSONDecoder().feed(b"not json\n")
    with pytest.raises(errors.ProtocolError):
        NDJSONDecoder().feed(b"[1,2]\n")
    with pytest.raises(errors.ProtocolError, match="exceeds"):
        NDJSONDecoder(max_line_bytes=10).feed(b"x" * 11)


# -- offsets --------------------------------------------------------------------


def test_tracker_plain_stream() -> None:
    t = StreamTracker()
    assert t.accept({"type": "start", "offset": 0, "command": {"pid": "p1", "cmd": "sh", "pty": False}}) == []
    assert t.command is not None and t.command.pid == "p1"
    assert t.accept(out("stdout", 0, b"abc")) == [StdoutEvent(b"abc", 0)]
    assert t.accept(out("stderr", 3, b"de")) == [StderrEvent(b"de", 3)]
    assert t.accept({"type": "keepalive", "offset": 5}) == [] and t.keepalives == 1
    assert t.accept({"type": "something_new", "offset": 5}) == []
    events = t.accept({"type": "exit", "offset": 5, "exit": {"exit_code": 2, "reason": "exited"}})
    assert isinstance(events[0], ExitEvent) and events[0].exit.exit_code == 2
    assert (t.offset, t.dropped) == (5, 0) and t.exit is not None


def test_tracker_replay_overlap_and_gaps() -> None:
    t = StreamTracker(offset=10)
    assert t.accept(out("stdout", 0, b"0123456789")) == []  # entirely before the position
    assert t.accept(out("stdout", 8, b"89abc")) == [StdoutEvent(b"abc", 10)]  # overlapping: only the new part
    assert t.offset == 13
    # The server says bytes are gone.
    assert t.accept({"type": "truncated", "offset": 20, "dropped": 7}) == [
        TruncatedEvent(offset=20, dropped=7)
    ]
    # A truncation that does not reach the position is not a loss for this client.
    assert t.accept({"type": "truncated", "offset": 15, "dropped": 15}) == []
    # A hole that the server did not announce is still reported.
    assert t.accept(out("stdout", 25, b"xy")) == [
        TruncatedEvent(offset=25, dropped=5),
        StdoutEvent(b"xy", 25),
    ]
    events = t.accept({"type": "exit", "offset": 30, "exit": {"exit_code": 0, "reason": "exited"}})
    assert events[0] == TruncatedEvent(offset=30, dropped=3) and isinstance(events[1], ExitEvent)
    assert (t.offset, t.dropped) == (30, 15)


def test_tracker_errors() -> None:
    t = StreamTracker()
    with pytest.raises(errors.ServiceUnavailableError, match="reconnect"):
        t.accept(
            {"type": "error", "offset": 0, "error": {"code": "shutting_down", "message": "reconnect later"}}
        )
    with pytest.raises(errors.ApiError):
        t.accept({"type": "error", "offset": 0})
    with pytest.raises(errors.ProtocolError):
        t.accept({"type": "stdout", "offset": 0, "data": "!!! not base64"})
    assert t.accept({"type": "stdout", "offset": 0}) == []  # no data: nothing to deliver


# -- WebSocket framing ----------------------------------------------------------


def test_wire_frames() -> None:
    assert wire.frame(wire.CH_STDIN, b"ls\n") == b"\x00ls\n"
    assert wire.json_frame(wire.CH_RESIZE, {"cols": 80, "rows": 24}) == b'\x04{"cols":80,"rows":24}'
    assert wire.json_frame(wire.CH_ACK, {"offset": 5}) == b'\x06{"offset":5}'
    assert wire.split(b"\x01hello") == (1, b"hello") and wire.split(b"\x03") == (3, b"")
    assert wire.json_payload(b'{"offset":3,"dropped":1}') == {"offset": 3, "dropped": 1}
    for bad in (b"",):
        with pytest.raises(errors.ProtocolError):
            wire.split(bad)
    for bad in (b"nope", b"[1]"):
        with pytest.raises(errors.ProtocolError):
            wire.json_payload(bad)
    frames = wire.stdin_frames(b"x" * 70_000)
    assert [len(f) for f in frames] == [32769, 32769, 4465] and all(f[0] == 0 for f in frames)
    assert wire.stdin_frames(b"") == []
    assert wire.MAX_SERVER_FRAME >= 32769 and wire.ACK_STEP < wire.FLOW_LOW_WATERMARK


def test_flow_control_never_leaves_a_watermark_unacknowledged() -> None:
    rng = random.Random(3)
    for _ in range(200):
        flow = wire.FlowControl()
        start = rng.randint(0, 10**6)
        flow.reset(start)
        position, acked = start, start
        for _ in range(300):
            n = rng.randint(1, 32768)
            position += n
            ack = flow.consumed(n)
            if ack is not None:
                assert ack == position
                acked = ack
            # After every frame the outstanding bytes are below the low
            # watermark, so the agent always resumes a paused command.
            assert position - acked < wire.FLOW_LOW_WATERMARK
    with pytest.raises(ValueError):
        wire.FlowControl(step=10_000)
    with pytest.raises(ValueError):
        wire.FlowControl(step=0)


# -- models ---------------------------------------------------------------------


def test_parse_time() -> None:
    utc = timezone.utc
    assert parse_time("2026-10-10T08:00:00Z") == datetime(2026, 10, 10, 8, tzinfo=utc)
    assert parse_time("2026-10-10T08:00:00.5Z") == datetime(2026, 10, 10, 8, 0, 0, 500000, tzinfo=utc)
    assert parse_time("2026-10-10T08:00:00.123456789Z") == datetime(2026, 10, 10, 8, 0, 0, 123456, tzinfo=utc)
    assert parse_time("2026-10-10T08:00:00.123456789+05:30") == datetime(
        2026, 10, 10, 8, 0, 0, 123456, tzinfo=timezone(timedelta(hours=5, minutes=30))
    )
    for bad in (None, 5, "", "yesterday", "2026-13-45T00:00:00Z"):
        assert parse_time(bad) is None


def test_models_tolerate_missing_and_unknown_fields() -> None:
    info = SandboxInfo.from_json({"id": "sb-1", "state": "quantum", "access_token": "sbt_secret", "new": [1]})
    assert (info.sandbox_id, info.state, info.template_id, info.created_at, info.metadata) == (
        "sb-1",
        "quantum",
        "",
        None,
        {},
    )
    assert "sbt_secret" not in repr(info) and "access_token" not in info.raw
    assert SandboxInfo.from_json({}).state == "unknown"
    cmd = CommandInfo.from_json({"pid": "p", "exit": {"exit_code": 1, "reason": "vaporised"}, "args": "oops"})
    assert cmd.exit is not None and cmd.exit.reason == "vaporised" and cmd.args == () and not cmd.running
    assert EntryInfo.from_json({"name": "a", "path": "/a", "mode": 0o644}).mode == "0644"
    assert NetworkPolicy.coerce("none").to_json() == {"mode": "none"}
    assert NetworkPolicy().to_json() == {}


# -- configuration ----------------------------------------------------------------


def test_config_and_units(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("SBX_API_KEY", raising=False)
    monkeypatch.delenv("SBX_BASE_URL", raising=False)
    config = ConnectionConfig.resolve()
    assert config.base_url == "http://127.0.0.1:8080" and config.api_key is None
    with pytest.raises(errors.ConfigurationError):
        config.auth_headers("control")
    with pytest.raises(errors.ConfigurationError):
        config.auth_headers("data")
    both = ConnectionConfig.resolve(api_key="sbx_k", access_token="sbt_t")
    assert both.auth_headers("control") == {"X-API-Key": "sbx_k"}
    assert both.auth_headers("data") == {"X-Access-Token": "sbt_t"}
    assert "sbx_k" not in repr(both) and "sbt_t" not in repr(both)
    with pytest.raises(errors.ConfigurationError):
        ConnectionConfig.resolve(request_timeout=0)
    assert (ms(None), ms(0), ms(1.5), ms(0.0001)) == (None, 0, 1500, 1)
    with pytest.raises(ValueError):
        ms(-1)


def test_retry_policy_decisions() -> None:
    p = RetryPolicy()
    rate = errors.RateLimitError("x", code="rate_limited", retry_after=2)
    assert p.delay_for(rate, 0, idempotent=False) == 2.0
    assert p.delay_for(rate, 3, idempotent=False) is None  # the budget is spent
    pool = errors.PoolEmptyError("x", retry_after=1)
    assert p.delay_for(pool, 0, idempotent=False) is None
    assert p.delay_for(pool, 0, idempotent=False, is_create=True) == 1.0
    agent = errors.AgentUnavailableError("x", retry_after=1)
    assert p.delay_for(agent, 0, idempotent=True) == 1.0
    assert p.delay_for(agent, 0, idempotent=False, is_create=True) is None
    assert p.delay_for(errors.SandboxNotFoundError("x"), 0, idempotent=True) is None
    assert p.delay_for(errors.TransportError("x"), 0, idempotent=False, transport_failure=True) is None
    delay = p.delay_for(errors.TransportError("x"), 2, idempotent=True, transport_failure=True)
    assert delay is not None and 0.75 <= delay <= 1.25
    assert RetryPolicy(backoff_max=1).backoff(20) <= 1.25
    assert RetryPolicy.none().delay_for(rate, 0, idempotent=True) is None


def test_generated_code_is_up_to_date() -> None:
    root = Path(__file__).resolve().parent.parent
    done = subprocess.run(
        [sys.executable, str(root / "scripts" / "unasync.py"), "--check"], capture_output=True, text=True
    )
    assert done.returncode == 0, done.stderr
