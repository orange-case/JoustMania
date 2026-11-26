"""Event-loop watchdog for the long-running Python services (UC-060 A8, NFR-080).

Compose restarts a container only when its process exits, never on `unhealthy`.
A task on the service's event loop beats every second; a daemon thread checks
the beat and, once it is older than the timeout, exits the process with
rust-hid's watchdog code so `restart: unless-stopped` brings the service back.

Usage, in the service's async entry point once start-up is done:

    watchdog = start_loop_watchdog("menu")
    await shutdown_event.wait()
    watchdog.stop()
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
import os
import signal
import threading
import time
from collections.abc import Callable

from opentelemetry import trace

logger = logging.getLogger(__name__)

# EX_SOFTWARE, the same code as services/rust-hid/src/watchdog.rs.
EXIT_CODE = 70
DEFAULT_TIMEOUT_SECONDS = 20.0
BEAT_INTERVAL_SECONDS = 1.0
CHECK_INTERVAL_SECONDS = 1.0
FLUSH_TIMEOUT_SECONDS = 1.0
# The trip path logs and flushes; a wedged logging handler or exporter must not keep the process alive.
HARD_EXIT_AFTER_SECONDS = 2.0
TIMEOUT_ENV = "WATCHDOG_TIMEOUT_S"
STALL_SIGNAL_ENV = "WATCHDOG_STALL_ON_SIGUSR2"
TRIP_SPAN = "watchdog.trip"


def timeout_from_env() -> float:
    """``WATCHDOG_TIMEOUT_S`` in seconds; unset or empty means the default, 0 turns the watchdog off."""
    raw = os.getenv(TIMEOUT_ENV, "").strip()
    if not raw:
        return DEFAULT_TIMEOUT_SECONDS
    try:
        value = float(raw)
    except ValueError:
        raise ValueError(f"{TIMEOUT_ENV}={raw!r} is not a number of seconds") from None
    if not value >= 0 or value == float("inf"):
        raise ValueError(f"{TIMEOUT_ENV}={raw!r} must be a finite number at least 0")
    return value


def _flush_telemetry() -> None:
    for provider in (trace.get_tracer_provider(), _logger_provider()):
        flush = getattr(provider, "force_flush", None)
        if flush is not None:
            with contextlib.suppress(Exception):
                flush(int(FLUSH_TIMEOUT_SECONDS * 1000))


def _logger_provider():
    from lib.otel_logging import get_logger_provider

    return get_logger_provider()


class LoopWatchdog:
    """Exit the process when the event loop has not beaten for ``timeout_s`` seconds."""

    def __init__(
        self,
        service: str,
        timeout_s: float,
        *,
        beat_interval_s: float = BEAT_INTERVAL_SECONDS,
        check_interval_s: float = CHECK_INTERVAL_SECONDS,
        exit_fn: Callable[[int], object] = os._exit,
        flush_fn: Callable[[], None] = _flush_telemetry,
        clock: Callable[[], float] = time.monotonic,
        hard_exit_after_s: float = HARD_EXIT_AFTER_SECONDS,
        tracer: trace.Tracer | None = None,
    ):
        self.service = service
        self.timeout_s = timeout_s
        self._beat_interval_s = beat_interval_s
        self._check_interval_s = check_interval_s
        self._exit_fn = exit_fn
        self._flush_fn = flush_fn
        self._clock = clock
        self._hard_exit_after_s = hard_exit_after_s
        self._last_beat = clock()
        self._stopped = threading.Event()
        self._tripped = threading.Event()
        self._beat_task: asyncio.Task | None = None
        self._thread: threading.Thread | None = None
        self._tracer = tracer

    @property
    def enabled(self) -> bool:
        return self.timeout_s > 0

    @property
    def tripped(self) -> bool:
        return self._tripped.is_set()

    def start(self, loop: asyncio.AbstractEventLoop | None = None) -> LoopWatchdog:
        if not self.enabled:
            logger.info("watchdog: %s loop watchdog off (%s=0)", self.service, TIMEOUT_ENV)
            return self
        loop = loop or asyncio.get_running_loop()
        if self._tracer is None:
            # Resolved here, on the loop: the first get_tracer reads flagd, which the trip path must never wait on.
            from lib.telemetry import get_tracer

            self._tracer = get_tracer(__name__)
        self._last_beat = self._clock()
        self._beat_task = loop.create_task(self._beat(), name="watchdog-beat")
        self._thread = threading.Thread(target=self._watch, name="loop-watchdog", daemon=True)
        self._thread.start()
        logger.info("watchdog: %s loop watchdog on (timeout %.0f s)", self.service, self.timeout_s)
        return self

    def stop(self) -> None:
        """Stop watching (shutdown has started); idempotent."""
        self._stopped.set()
        if self._beat_task is not None:
            self._beat_task.cancel()
            self._beat_task = None

    async def _beat(self) -> None:
        while True:
            self._last_beat = self._clock()
            await asyncio.sleep(self._beat_interval_s)

    def _watch(self) -> None:
        while not self._stopped.wait(self._check_interval_s):
            stalled = self._clock() - self._last_beat
            if stalled >= self.timeout_s:
                self._trip(stalled)
                return

    def _trip(self, stalled: float) -> None:
        if self._stopped.is_set():
            return
        self._tripped.set()
        hard_exit = threading.Timer(self._hard_exit_after_s, self._exit_fn, args=(EXIT_CODE,))
        hard_exit.daemon = True
        hard_exit.start()
        logger.error(
            "watchdog: %s event loop stalled for %.1f s (limit %.0f s), exiting with code %d "
            "so the restart policy restarts it",
            self.service,
            stalled,
            self.timeout_s,
            EXIT_CODE,
        )
        try:
            if self._tracer is not None:
                span = self._tracer.start_span(
                    TRIP_SPAN,
                    attributes={
                        "watchdog.stalled_seconds": round(stalled, 3),
                        "watchdog.timeout_seconds": self.timeout_s,
                    },
                )
                span.end()
            self._flush_fn()
        finally:
            hard_exit.cancel()
            self._exit_fn(EXIT_CODE)


def _install_stall_signal(loop: asyncio.AbstractEventLoop) -> None:
    def _stall() -> None:
        logger.warning("watchdog: %s received, blocking the event loop for good", "SIGUSR2")
        while True:
            time.sleep(3600)

    loop.add_signal_handler(signal.SIGUSR2, _stall)


def start_loop_watchdog(service: str, tracer: trace.Tracer | None = None) -> LoopWatchdog:
    """Start the watchdog on the running loop with the timeout from ``WATCHDOG_TIMEOUT_S``."""
    loop = asyncio.get_running_loop()
    if os.getenv(STALL_SIGNAL_ENV) == "1":
        _install_stall_signal(loop)
    return LoopWatchdog(service, timeout_from_env(), tracer=tracer).start(loop)
