"""Fixtures of the flavoured tests (this file is converted for the blocking client)."""

from __future__ import annotations

from collections.abc import AsyncIterator, Iterator

import pytest

import sbx._async.api
import sbx._async.commands
import sbx._async.pty
from sbx import AsyncSandbox

from ..fake import API_KEY, FakeServer, FakeWSServer
from .helpers import BASE_URL


@pytest.fixture
def server() -> FakeServer:
    return FakeServer()


@pytest.fixture
def sleeps(monkeypatch: pytest.MonkeyPatch) -> list[float]:
    """Replace every retry/backoff sleep of the SDK with a recorder."""
    calls: list[float] = []

    async def fake_sleep(seconds: float) -> None:
        calls.append(seconds)

    for module in (sbx._async.api, sbx._async.commands, sbx._async.pty):
        monkeypatch.setattr(module, "sleep", fake_sleep)
    return calls


@pytest.fixture
def ws() -> Iterator[FakeWSServer]:
    fake = FakeWSServer()
    yield fake
    fake.close()


@pytest.fixture
async def sandbox(server: FakeServer, ws: FakeWSServer, sleeps: list[float]) -> AsyncIterator[AsyncSandbox]:
    sb = await AsyncSandbox.create(
        "python", api_key=API_KEY, base_url=BASE_URL, transport=server.transport(), ws_base_url=ws.url
    )
    ws.token = sb.access_token or ""
    yield sb
    await sb.close()
