"""Concurrency primitives of the blocking flavour (threads).

Hand-written counterpart of ``sbx/_async/_compat.py``.
"""

from __future__ import annotations

import threading
import time
from collections.abc import Callable
from typing import Any

from websockets.sync.client import ClientConnection
from websockets.sync.client import connect as _ws_connect

__all__ = ["ClientConnection", "DataHandler", "Event", "Task", "TextHandler", "TruncatedHandler", "invoke"]
__all__ += ["sleep", "spawn", "ws_connect"]

#: Callback for decoded stdout / stderr text.
TextHandler = Callable[[str], None]
#: Callback for raw PTY output. It runs on the handle's reader thread.
DataHandler = Callable[[bytes], None]
#: Callback ``(offset, dropped_bytes)`` for a gap in the output.
TruncatedHandler = Callable[[int, int], None]


def sleep(seconds: float) -> None:
    time.sleep(seconds)


def invoke(callback: Callable[..., Any], *args: Any) -> None:
    callback(*args)


class Event:
    def __init__(self) -> None:
        self._event = threading.Event()

    def set(self) -> None:
        self._event.set()

    def is_set(self) -> bool:
        return self._event.is_set()

    def wait(self, timeout: float | None = None) -> bool:
        return self._event.wait(timeout)


class Task:
    def __init__(self, thread: threading.Thread) -> None:
        self._thread = thread

    def is_current(self) -> bool:
        return threading.current_thread() is self._thread


def spawn(fn: Callable[[], None], name: str) -> Task:
    thread = threading.Thread(target=fn, name=name, daemon=True)
    thread.start()
    return Task(thread)


def ws_connect(uri: str, **kwargs: Any) -> ClientConnection:
    return _ws_connect(uri, **kwargs)
