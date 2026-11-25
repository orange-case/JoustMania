"""The internet check of the pad's status bar (UC-016 BR-010, FR-256).

A TCP connect to `operator_pad.internet_check_target` every `operator_pad.internet_check_ms` while
the display link is up with protocol 2 or higher. No ICMP (no raw-socket privilege) and nothing is
sent: a connect that succeeds is closed at once. The lookup and the connect run on a thread of their
own, so a hung name lookup never blocks the loop, a key event, a frame or a status read; while it
hangs, the next checks are skipped (one check at a time).
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
import socket
import time
from collections.abc import Callable
from concurrent.futures import Future, ThreadPoolExecutor

from services.operator_pad.config import PadConfig
from services.operator_pad.display import DisplayModel
from services.operator_pad.pages import NET_DOWN, NET_OFF, NET_OK

logger = logging.getLogger(__name__)

CONNECT_TIMEOUT_SECONDS = 3.0  # BR-010
WAIT_MARGIN_SECONDS = 1.0  # a check not done by then counts as down (a slow lookup keeps its thread)


def tcp_check(host: str, port: int, timeout: float = CONNECT_TIMEOUT_SECONDS) -> bool:
    """True when a TCP connect to host:port succeeds within ``timeout`` in all; IPv4 first.

    The addresses share what is left of the budget, so with several A records a dead first one never
    uses it all: each later address still gets its turn before the caller's wait gives up.
    """
    deadline = time.monotonic() + timeout
    try:
        infos = socket.getaddrinfo(host, port, socket.AF_INET, socket.SOCK_STREAM)
    except OSError:
        try:
            infos = socket.getaddrinfo(host, port, 0, socket.SOCK_STREAM)
        except OSError:
            return False
    for k, (*_, address) in enumerate(infos):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            return False
        try:
            with socket.create_connection(address[:2], timeout=remaining / (len(infos) - k)):
                return True
        except OSError:
            continue
    return False


class InternetCheck:
    def __init__(
        self,
        config: PadConfig,
        model: DisplayModel,
        probe: Callable[[str, int, float], bool] = tcp_check,
    ) -> None:
        self._config = config
        self._model = model
        self._probe = probe
        self._executor = ThreadPoolExecutor(max_workers=1, thread_name_prefix="pad-internet")
        self._future: Future | None = None
        self._last: str | None = None

    def _active(self) -> bool:
        return (self._model.protocol or 0) >= 2

    async def check(self) -> None:
        """One check; skipped while the last one still runs (a hung lookup)."""
        target = self._config.internet_check_target()
        if target is None:
            self._note(NET_OFF)
            return
        if self._future is not None and not self._future.done():
            logger.debug("Internet check still running: this one is skipped")
            return
        host, port = target
        self._future = self._executor.submit(self._probe, host, port, CONNECT_TIMEOUT_SECONDS)
        try:
            ok = await asyncio.wait_for(
                asyncio.shield(asyncio.wrap_future(self._future)), CONNECT_TIMEOUT_SECONDS + WAIT_MARGIN_SECONDS
            )
        except Exception:  # the timeout, or a probe that raised
            ok = False
        self._note(NET_OK if ok else NET_DOWN)

    def _note(self, state: str) -> None:
        if state != self._last:
            self._last = state
            logger.info(f"Internet check: {state}")
        self._model.note_internet(state)

    async def run(self, stop: asyncio.Event) -> None:
        wake = self._model.internet_wake
        try:
            while not stop.is_set():
                wake.clear()
                if self._active():
                    try:
                        await self.check()
                    except Exception as e:
                        logger.warning(f"Internet check failed: {e!r}")
                # BR-010: every interval; a link-up wakes it at once.
                with contextlib.suppress(TimeoutError):
                    await asyncio.wait_for(wake.wait(), self._config.internet_check_ms() / 1000)
        finally:
            self._executor.shutdown(wait=False, cancel_futures=True)
