"""The blocking and the asyncio client expose the same surface; asyncio-only behaviour."""

from __future__ import annotations

import inspect
from typing import Any

import pytest

import sbx
from sbx import AsyncSandbox

from .fake import API_KEY, FakeServer, FakeWSServer, WSSession, program, ws_exit, ws_offset, ws_output

PAIRS = [
    (sbx.Sandbox, sbx.AsyncSandbox),
    (sbx.Commands, sbx.AsyncCommands),
    (sbx.CommandHandle, sbx.AsyncCommandHandle),
    (sbx.Files, sbx.AsyncFiles),
    (sbx.FileStream, sbx.AsyncFileStream),
    (sbx.Pty, sbx.AsyncPty),
    (sbx.PtyHandle, sbx.AsyncPtyHandle),
    (sbx.Webhooks, sbx.AsyncWebhooks),
]
RENAMED = {"__aenter__": "__enter__", "__aexit__": "__exit__", "__aiter__": "__iter__", "__anext__": "__next__",
           "aclose": "close"}  # fmt: skip


def public(cls: type) -> dict[str, Any]:
    keep = set(RENAMED) | set(RENAMED.values())
    return {RENAMED.get(n, n): v for n, v in vars(cls).items() if not n.startswith("_") or n in keep}


@pytest.mark.parametrize(("sync_cls", "async_cls"), PAIRS, ids=[p[0].__name__ for p in PAIRS])
def test_same_methods_and_parameters(sync_cls: type, async_cls: type) -> None:
    sync_api, async_api = public(sync_cls), public(async_cls)
    assert sync_api.keys() == async_api.keys()
    for name, sync_member in sync_api.items():
        async_member = async_api[name]
        sync_fn = getattr(sync_member, "__func__", sync_member)
        async_fn = getattr(async_member, "__func__", async_member)
        if not inspect.isfunction(sync_fn):
            continue
        assert list(inspect.signature(sync_fn).parameters) == list(inspect.signature(async_fn).parameters), (
            name
        )
        assert not inspect.iscoroutinefunction(sync_fn), name


def test_the_async_client_is_really_async() -> None:
    for name in (
        "create",
        "connect",
        "list",
        "kill",
        "get_info",
        "set_timeout",
        "is_running",
        "update_network",
        "events",
        "stats",
    ):
        assert inspect.iscoroutinefunction(getattr(sbx.AsyncSandbox, name)), name
    for name in ("run", "list", "connect", "kill", "send_stdin"):
        assert inspect.iscoroutinefunction(getattr(sbx.AsyncCommands, name)), name
    for name in ("read", "write", "write_files", "list", "exists", "make_dir", "remove"):
        assert inspect.iscoroutinefunction(getattr(sbx.AsyncFiles, name)), name
    for name in ("create", "connect", "send_input", "resize", "kill"):
        assert inspect.iscoroutinefunction(getattr(sbx.AsyncPty, name)), name
    for name in ("wait", "send_input", "resize", "kill", "disconnect"):
        assert inspect.iscoroutinefunction(getattr(sbx.AsyncPtyHandle, name)), name
    for name in ("create", "list", "get", "update", "delete", "rotate_secret", "test", "deliveries", "close"):
        assert inspect.iscoroutinefunction(getattr(sbx.AsyncWebhooks, name)), name


async def test_coroutine_callbacks() -> None:
    server = FakeServer()
    ws = FakeWSServer()
    try:
        sandbox = await AsyncSandbox.create(
            "python", api_key=API_KEY, transport=server.transport(), ws_base_url=ws.url
        )
        ws.token = sandbox.access_token or ""
        out: list[str] = []
        err: list[str] = []

        async def on_stdout(text: str) -> None:
            out.append(text)

        async def on_stderr(text: str) -> None:
            err.append(text)

        server.programs.append(program(stdout=b"o", stderr=b"e"))
        await sandbox.commands.run("x", on_stdout=on_stdout, on_stderr=on_stderr)
        assert (out, err) == (["o"], ["e"])

        def script(conn: object, session: WSSession) -> None:
            ws_offset(conn, 0)  # type: ignore[arg-type]
            ws_output(conn, session, b"tty")  # type: ignore[arg-type]
            ws_exit(conn)  # type: ignore[arg-type]

        ws.scripts.append(script)  # type: ignore[arg-type]
        data: list[bytes] = []

        async def on_data(chunk: bytes) -> None:
            data.append(chunk)

        handle = await sandbox.pty.create(80, 24, on_data)
        assert (await handle.wait()).exit_code == 0 and data == [b"tty"]
        await sandbox.close()
    finally:
        ws.close()
