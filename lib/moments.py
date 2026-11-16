"""Game moments in a chosen shape (#2658, UC-100, FR-216, FR-217, NFR-060).

A *moment* is one happening in a game (today: a player's warning) with one identity,
sent in the shape the ``observability/telemetry.moment_shape`` flag chose for the game
(telemetry.md §3.2 "Moment shapes"):

- ``span_event`` (default): an event on the parent (player) span, as before;
- ``span``: a span ``game.<name>`` under the parent span, started now and ended at once
  with end time = start + ``duration_s`` (no open span, no timer);
- ``log_event``: an SDK log record with ``event_name`` = the moment's name, severity INFO,
  the parent span's trace context, emitted on the process's OTLP ``LoggerProvider``
  (``lib.otel_logging.get_logger_provider``), never through Python ``logging``, so the
  ``log_level`` flag never drops it;
- ``both``: ``span_event`` and ``log_event`` with the same attributes (one ``moment.id``).

A shape that needs the log channel falls back to ``span_event`` in a process without
OTLP logging (UC-100 A1). Every function here only enqueues (span processor, log
processor): no blocking work, no flag read and no lock, so it is safe on the frame path.
"""

from __future__ import annotations

import time
from collections.abc import Mapping
from typing import Any

from opentelemetry import trace
from opentelemetry._logs import SeverityNumber

from lib import otel_logging

MOMENT_SHAPE_FLAG = "telemetry.moment_shape"
MOMENT_SHAPE_SPAN_EVENT = "span_event"
MOMENT_SHAPE_SPAN = "span"
MOMENT_SHAPE_LOG_EVENT = "log_event"
MOMENT_SHAPE_BOTH = "both"
MOMENT_SHAPES = frozenset({MOMENT_SHAPE_SPAN_EVENT, MOMENT_SHAPE_SPAN, MOMENT_SHAPE_LOG_EVENT, MOMENT_SHAPE_BOTH})
_LOG_SHAPES = frozenset({MOMENT_SHAPE_LOG_EVENT, MOMENT_SHAPE_BOTH})

# Root span attribute carrying the shape in effect for the game (UC-100 step 3).
MOMENT_SHAPE_ATTR = "telemetry.moment_shape"
MOMENT_ID_ATTR = "moment.id"
_LOGGER_NAME = "joustmania.moments"

# (provider, logger) for the last provider seen: get_logger once, not per moment.
_logger_cache: tuple[Any, Any] | None = None


def effective_moment_shape(value: object) -> str:
    """The shape a game runs with for a raw flag value.

    Anything but a declared variant is ``span_event`` (UC-100 A1, BR-004), and a shape
    that needs the log channel is ``span_event`` in a process without OTLP logging.
    """
    shape = value if isinstance(value, str) and value in MOMENT_SHAPES else MOMENT_SHAPE_SPAN_EVENT
    if shape in _LOG_SHAPES and otel_logging.get_logger_provider() is None:
        return MOMENT_SHAPE_SPAN_EVENT
    return shape


def read_moment_shape() -> str:
    """Read ``telemetry.moment_shape`` once (per game) and return the shape in effect.

    Fail-open: an absent flag, an unknown value, a provider that is not READY or any
    read error gives ``span_event`` (``read_string_flag`` returns the default).
    """
    from lib.feature_flags import read_string_flag  # lazy: lib.feature_flags pulls in the metrics libs

    return effective_moment_shape(read_string_flag("observability", MOMENT_SHAPE_FLAG, MOMENT_SHAPE_SPAN_EVENT))


def moment_id(game_id: str, serial: str, sequence: int) -> str:
    """``moment.id``: ``<game.id>/<player.serial>/<n>``, ``n`` counting from 1 per serial per game."""
    return f"{game_id}/{serial}/{sequence}"


def _moment_logger():
    global _logger_cache
    provider = otel_logging.get_logger_provider()
    if provider is None:
        return None
    cached = _logger_cache
    if cached is not None and cached[0] is provider:
        return cached[1]
    moment_logger = provider.get_logger(_LOGGER_NAME)
    _logger_cache = (provider, moment_logger)
    return moment_logger


def record_moment(
    name: str,
    *,
    shape: str,
    parent_span: trace.Span,
    attributes: Mapping[str, Any],
    duration_s: float = 0.0,
    tracer: trace.Tracer | None = None,
) -> str:
    """Record moment ``name`` on ``parent_span`` in ``shape``; returns the shape actually sent.

    ``attributes`` must already carry the identity keys (``moment.id``, the player, the
    game); every record of the moment gets the same attributes. ``duration_s`` is the
    span shape's length (the warning window for ``death_warning``).
    """
    attrs = dict(attributes)
    if shape not in MOMENT_SHAPES:
        shape = MOMENT_SHAPE_SPAN_EVENT
    moment_logger = _moment_logger() if shape in _LOG_SHAPES else None
    if shape in _LOG_SHAPES and moment_logger is None:
        shape = MOMENT_SHAPE_SPAN_EVENT  # no log channel in this process (UC-100 A1)

    if shape == MOMENT_SHAPE_SPAN:
        start_ns = time.time_ns()
        span = (tracer or trace.get_tracer(__name__)).start_span(
            f"game.{name}",
            context=trace.set_span_in_context(parent_span),
            attributes=attrs,
            start_time=start_ns,
        )
        span.end(end_time=start_ns + int(duration_s * 1_000_000_000))
        return shape

    if shape in (MOMENT_SHAPE_SPAN_EVENT, MOMENT_SHAPE_BOTH):
        parent_span.add_event(name, attributes=attrs)
    if moment_logger is not None:
        moment_logger.emit(
            timestamp=time.time_ns(),
            context=trace.set_span_in_context(parent_span),
            severity_number=SeverityNumber.INFO,
            severity_text="INFO",
            body=name,
            attributes=attrs,
            event_name=name,
        )
    return shape
