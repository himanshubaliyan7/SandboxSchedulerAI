"""Concurrency primitives of the asyncio flavour.

This module and ``sbx/_sync/_compat.py`` are the only hand-written files
that differ between the two flavours; everything else in ``sbx/_sync`` is
generated from ``sbx/_async`` by ``scripts/unasync.py``.
"""

from __future__ import annotations

import asyncio
import inspect
from collections.abc import Awaitable, Callable, Coroutine
from typing import Any, Union

from websockets.asyncio.client import ClientConnection
from websockets.asyncio.client import connect as _ws_connect

__all__ = ["ClientConnection", "DataHandler", "Event", "Task", "TextHandler", "TruncatedHandler", "invoke"]
__all__ += ["sleep", "spawn", "ws_connect"]

#: Callback for decoded stdout / stderr text. It may be a coroutine function.
TextHandler = Callable[[str], Union[Awaitable[None], None]]
#: Callback for raw PTY output. It may be a coroutine function.
DataHandler = Callable[[bytes], Union[Awaitable[None], None]]
#: Callback ``(offset, dropped_bytes)`` for a gap in the output.
TruncatedHandler = Callable[[int, int], Union[Awaitable[None], None]]


async def sleep(seconds: float) -> None:
    await asyncio.sleep(seconds)


async def invoke(callback: Callable[..., Any], *args: Any) -> None:
    result = callback(*args)
    if inspect.isawaitable(result):
        await result


class Event:
    def __init__(self) -> None:
        self._event = asyncio.Event()

    def set(self) -> None:
        self._event.set()

    def is_set(self) -> bool:
        return self._event.is_set()

    async def wait(self, timeout: float | None = None) -> bool:
        if timeout is None:
            await self._event.wait()
            return True
        try:
            await asyncio.wait_for(self._event.wait(), timeout)
        except asyncio.TimeoutError:
            return False
        return True


class Task:
    def __init__(self, task: asyncio.Task[None]) -> None:
        self._task = task

    def is_current(self) -> bool:
        return asyncio.current_task() is self._task


def spawn(fn: Callable[[], Coroutine[Any, Any, None]], name: str) -> Task:
    return Task(asyncio.get_running_loop().create_task(fn(), name=name))


async def ws_connect(uri: str, **kwargs: Any) -> ClientConnection:
    return await _ws_connect(uri, **kwargs)
