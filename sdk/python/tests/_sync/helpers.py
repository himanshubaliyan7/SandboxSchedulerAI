# Generated from tests/_async/helpers.py by scripts/unasync.py. Do not edit.
"""Helpers shared by the flavoured tests."""

from __future__ import annotations

import time
from collections.abc import Callable

from sbx._async._compat import sleep

BASE_URL = "http://sbx.test"


def eventually(condition: Callable[[], bool], timeout: float = 5.0) -> None:
    """Wait (without blocking the event loop) until ``condition()`` holds."""
    deadline = time.monotonic() + timeout
    while not condition():
        if time.monotonic() > deadline:
            raise AssertionError("condition not met in time")
        sleep(0.005)
