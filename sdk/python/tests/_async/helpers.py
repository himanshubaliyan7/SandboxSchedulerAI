"""Helpers shared by the flavoured tests."""

from __future__ import annotations

import time
from collections.abc import Callable

from sbx._async._compat import sleep

BASE_URL = "http://sbx.test"


async def eventually(condition: Callable[[], bool], timeout: float = 5.0) -> None:
    """Wait (without blocking the event loop) until ``condition()`` holds."""
    deadline = time.monotonic() + timeout
    while not condition():
        if time.monotonic() > deadline:
            raise AssertionError("condition not met in time")
        await sleep(0.005)
