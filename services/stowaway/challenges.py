"""The challenge generators (UC-106 BR-006; telemetry.md §3.5)."""

from __future__ import annotations

import asyncio
import contextlib
import logging
import math
import threading
import time
from collections.abc import Callable

from opentelemetry import metrics, trace
from opentelemetry.trace import SpanKind

from lib.challenge_flags import ATTR_NAME as ATTR_CHALLENGE_NAME
from lib.span_count import DELIVERED, DeliveredSpans, flush_spans, record_span_count
from services.stowaway import metrics as stowaway_metrics
from services.stowaway.config import Settings, SettingsSource
from services.stowaway.match import Match
from services.stowaway.synthetic import SYNTHETIC_ATTR

logger = logging.getLogger(__name__)

TICK_S = 0.05

STOP_TARGET = "target"
STOP_MATCH_END = "match_end"
STOP_KILL_SWITCH = "kill_switch"
STOP_CHALLENGE_CHANGE = "challenge_change"

SPAN_PADDING = "stowaway.padding"
SPAN_PADDING_CHILD = "stowaway.padding.span"
SPAN_SWING = "stowaway.swing"
METRIC_SWING = "stowaway_swing_accel_magnitude"

PADDING_SPANS_PER_S = 500.0
# Every round's target is reached this long into the match at the latest (UC-103 BR-002).
PADDING_WINDOW_S = 100.0
# A stalled loop must not turn into one huge burst.
PADDING_MAX_BACKLOG_S = 0.5
SWING_PLAYERS = 4
SWING_PERIOD_S = 3.0
SWING_REST_G = 1.0
SWING_PEAK_G = 2.0
SWING_WIDTH_S = 0.15


class Run:
    """One generator's view of its match and the live settings."""

    def __init__(
        self,
        name: str,
        match: Match,
        settings: SettingsSource,
        clock: Callable[[], float] = time.monotonic,
        tick_s: float = TICK_S,
    ) -> None:
        self.name = name
        self.match = match
        self.settings = settings
        self.clock = clock
        self.tick_s = tick_s

    def current(self) -> Settings:
        return self.settings.get()

    def stop_reason(self) -> str | None:
        settings = self.current()
        if not settings.enabled:
            return STOP_KILL_SWITCH
        if settings.challenge != self.name:
            return STOP_CHALLENGE_CHANGE
        if self.match.ended.is_set():
            return STOP_MATCH_END
        return None

    async def tick(self) -> None:
        with contextlib.suppress(TimeoutError):
            await asyncio.wait_for(self.match.ended.wait(), self.tick_s)


def _match_attributes(run: Run, settings: Settings) -> dict:
    return {
        SYNTHETIC_ATTR: True,
        "game.id": run.match.game_id,
        ATTR_CHALLENGE_NAME: run.name,
        "stowaway.mode": settings.mode,
        "stowaway.intensity": settings.intensity,
    }


class SpanRounds:
    """Padding of the match trace up to the span target (UC-103 BR-002)."""

    name = "span-rounds"

    def __init__(
        self,
        tracer: trace.Tracer | None = None,
        counts: DeliveredSpans = DELIVERED,
        flush: Callable[[], object] = flush_spans,
        on_count: Callable[[str, int], None] = stowaway_metrics.set_trace_spans_delivered,
    ) -> None:
        self._tracer = tracer or trace.get_tracer("stowaway")
        self._counts = counts
        self._flush = flush
        self._on_count = on_count

    async def run(self, run: Run) -> str:
        trace_id = trace.get_current_span(run.match.context).get_span_context().trace_id
        self._counts.watch(trace_id)
        reason = await self._pad(run)
        await self._record_count(run, trace_id)
        return reason

    async def _record_count(self, run: Run, trace_id: int) -> None:
        """FR-246, UC-103 BR-003: stowaway's delivered count of the match trace, after a flush."""
        if not self._counts.is_watching(trace_id):
            return
        try:
            await asyncio.to_thread(self._flush)
        except Exception as e:
            logger.warning("stowaway: span flush before the delivered count failed: %s", e)
        delivered = self._counts.take(trace_id)
        if delivered is None:
            return
        record_span_count(self._tracer, run.match.context, run.match.game_id, delivered, {SYNTHETIC_ATTR: True})
        self._on_count(run.match.game_id, delivered)
        logger.info("stowaway: game %s, %d padding spans delivered", run.match.game_id, delivered)

    async def _pad(self, run: Run) -> str:
        settings = run.current()
        target = settings.span_target
        attributes = _match_attributes(run, settings)
        attributes["stowaway.span_target"] = target
        padding = self._tracer.start_span(
            SPAN_PADDING, context=run.match.context, kind=SpanKind.CONSUMER, attributes=attributes
        )
        parent = trace.set_span_in_context(padding)
        emitted = 0
        backlog = 0.0
        last = run.clock()
        reason = STOP_TARGET
        try:
            while emitted < target:
                stop = run.stop_reason()
                if stop is not None:
                    reason = stop
                    break
                now = run.clock()
                rate = padding_rate(run.current().intensity, target)
                backlog = min(backlog + (now - last) * rate, rate * PADDING_MAX_BACKLOG_S)
                last = now
                batch = min(int(backlog), target - emitted)
                backlog -= batch
                for _ in range(batch):
                    self._tracer.start_span(
                        SPAN_PADDING_CHILD,
                        context=parent,
                        attributes={SYNTHETIC_ATTR: True, "stowaway.padding.index": emitted},
                    ).end()
                    emitted += 1
                if emitted < target:
                    await run.tick()
        finally:
            padding.set_attribute("stowaway.spans_emitted", emitted)
            padding.set_attribute("stowaway.stop_reason", reason)
            padding.end()
        return reason


def padding_rate(intensity: float, target: int) -> float:
    """Spans per second: the intensity's rate, raised so the target is reached within the window."""
    return max(PADDING_SPANS_PER_S * intensity, target / PADDING_WINDOW_S)


def swing_value(t: float, slot: int) -> float:
    """A 1 g rest with a swing to about 3 g every 3 s; players are offset so they never swing together."""
    phase = (t + slot * 0.37) % SWING_PERIOD_S - SWING_PERIOD_S / 2
    return SWING_REST_G + SWING_PEAK_G * math.exp(-((phase / SWING_WIDTH_S) ** 2))


class SwingGauge:
    """One observable gauge per process; it reports only while a match feeds it."""

    def __init__(self, meter: metrics.Meter | None = None, clock: Callable[[], float] = time.monotonic) -> None:
        self._clock = clock
        self._lock = threading.Lock()
        self._game_id: str | None = None
        self._players = 0
        (meter or metrics.get_meter("stowaway")).create_observable_gauge(
            METRIC_SWING,
            callbacks=[self._observe],
            unit="{g}",
            description="Synthetic per-player acceleration magnitude of the swing-density load (UC-106 BR-006)",
        )

    def show(self, game_id: str, players: int) -> None:
        with self._lock:
            self._game_id, self._players = game_id, players

    def clear(self) -> None:
        with self._lock:
            self._game_id, self._players = None, 0

    def _observe(self, _options):
        with self._lock:
            game_id, players = self._game_id, self._players
        if game_id is None:
            return []
        now = self._clock()
        return [
            metrics.Observation(swing_value(now, slot), {"serial": f"stowaway-{slot + 1}", "game_id": game_id})
            for slot in range(players)
        ]


def swing_players(intensity: float) -> int:
    return max(1, round(SWING_PLAYERS * intensity))


class SwingDensity:
    """Synthetic motion at the density of UC-102 BR-001, as load of its own."""

    name = "swing-density"

    def __init__(self, gauge: SwingGauge, tracer: trace.Tracer | None = None) -> None:
        self._gauge = gauge
        self._tracer = tracer or trace.get_tracer("stowaway")

    async def run(self, run: Run) -> str:
        settings = run.current()
        attributes = _match_attributes(run, settings)
        attributes["stowaway.swing.players"] = swing_players(settings.intensity)
        span = self._tracer.start_span(
            SPAN_SWING, context=run.match.context, kind=SpanKind.CONSUMER, attributes=attributes
        )
        reason = STOP_MATCH_END
        try:
            while True:
                stop = run.stop_reason()
                if stop is not None:
                    reason = stop
                    break
                self._gauge.show(run.match.game_id, swing_players(run.current().intensity))
                await run.tick()
        finally:
            self._gauge.clear()
            span.set_attribute("stowaway.stop_reason", reason)
            span.end()
        return reason


class NotImplementedChallenge:
    """Selectable, emits nothing (UC-106 A3)."""

    def __init__(self, name: str) -> None:
        self.name = name
        self._logged = False

    async def run(self, run: Run) -> str:
        if not self._logged:
            logger.warning("stowaway: challenge %r is not implemented; it emits nothing", self.name)
            self._logged = True
        while True:
            stop = run.stop_reason()
            if stop is not None:
                return stop
            await run.tick()


NOT_IMPLEMENTED = ("cardinality-bomb", "hostile-wifi", "clock-skew")


def build_generators(tracer: trace.Tracer | None = None, meter: metrics.Meter | None = None) -> dict:
    generators: dict = {
        SpanRounds.name: SpanRounds(tracer),
        SwingDensity.name: SwingDensity(SwingGauge(meter), tracer),
    }
    for name in NOT_IMPLEMENTED:
        generators[name] = NotImplementedChallenge(name)
    return generators
