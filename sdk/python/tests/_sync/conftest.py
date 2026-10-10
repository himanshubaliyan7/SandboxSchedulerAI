# Generated from tests/_async/conftest.py by scripts/unasync.py. Do not edit.
"""Fixtures of the flavoured tests (this file is converted for the blocking client)."""

from __future__ import annotations

from collections.abc import Iterator

import pytest

import sbx._sync.api
import sbx._sync.commands
import sbx._sync.pty
from sbx import Sandbox

from ..fake import API_KEY, FakeServer, FakeWSServer
from .helpers import BASE_URL


@pytest.fixture
def server() -> FakeServer:
    return FakeServer()


@pytest.fixture
def sleeps(monkeypatch: pytest.MonkeyPatch) -> list[float]:
    """Replace every retry/backoff sleep of the SDK with a recorder."""
    calls: list[float] = []

    def fake_sleep(seconds: float) -> None:
        calls.append(seconds)

    for module in (sbx._sync.api, sbx._sync.commands, sbx._sync.pty):
        monkeypatch.setattr(module, "sleep", fake_sleep)
    return calls


@pytest.fixture
def ws() -> Iterator[FakeWSServer]:
    fake = FakeWSServer()
    yield fake
    fake.close()


@pytest.fixture
def sandbox(server: FakeServer, ws: FakeWSServer, sleeps: list[float]) -> Iterator[Sandbox]:
    sb = Sandbox.create(
        "python", api_key=API_KEY, base_url=BASE_URL, transport=server.transport(), ws_base_url=ws.url
    )
    ws.token = sb.access_token or ""
    yield sb
    sb.close()
