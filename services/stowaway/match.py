"""The live match as stowaway sees it: game events only, never the gameplay stream (UC-106 BR-002)."""

from __future__ import annotations

import asyncio
import contextlib
import logging
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field

from opentelemetry import context as otel_context
from opentelemetry import trace

from lib.telemetry import extract_trace_context

logger = logging.getLogger(__name__)

EVENT_STARTED = "game_started"
TERMINAL_EVENTS = frozenset({"game_ended", "game_force_ended", "game_error", "game_start_error"})


@dataclass
class Match:
    game_id: str
    context: otel_context.Context
    ended: asyncio.Event = field(default_factory=asyncio.Event)


def match_from_event(event) -> Match | None:
    """A match from a ``game_started`` event whose ``_traceparent`` names a valid span, else None.

    ``game_started`` is published inside the game's trace, so its parent lands the synthetic spans
    in the match trace; events the servicer publishes (``game_start``) belong to other traces.
    """
    if event.event_type != EVENT_STARTED or not event.game_id:
        return None
    ctx = extract_trace_context(event.data.get("_traceparent", ""), event.data.get("_tracestate", ""))
    if ctx is None or not trace.get_current_span(ctx).get_span_context().is_valid:
        return None
    return Match(event.game_id, ctx)


Starter = Callable[[Match], Awaitable[None]]


class MatchTracker:
    """Feeds one event stream; starts ``on_match`` per match and ends it with the match.

    ``on_match`` runs as a task and must return once ``match.ended`` is set.
    """

    def __init__(self, on_match: Starter, accept: Callable[[], bool]) -> None:
        self._on_match = on_match
        self._accept = accept
        self._match: Match | None = None
        self._tasks: set[asyncio.Task] = set()

    @property
    def match(self) -> Match | None:
        return self._match

    def feed(self, event) -> None:
        current = self._match
        if current is not None:
            ended = event.event_type in TERMINAL_EVENTS and event.game_id in ("", current.game_id)
            # A new game on the bus means the old one is over even if its end event was missed.
            replaced = event.event_type == EVENT_STARTED and event.game_id != current.game_id
            if not (ended or replaced):
                return
            current.ended.set()
            self._match = None
        match = match_from_event(event)
        if match is None or not self._accept():
            return
        self._match = match
        task = asyncio.create_task(self._on_match(match))
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)

    async def close(self) -> None:
        """End the current match (the stream ended or stowaway stops) and wait for its generator."""
        if self._match is not None:
            self._match.ended.set()
        self._match = None
        for task in list(self._tasks):
            with contextlib.suppress(Exception):
                await task
