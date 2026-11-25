"""Game moments in a chosen shape (#2658, UC-100, FR-216, FR-217, NFR-060).

A *moment* is one happening in a game (a player's warning or death) with one identity,
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

The agent also gets its own log records on the same logger (#2722, #2783, UC-100 BR-012),
each marked ``orangecase.audience`` = ``agent`` so the collector keeps them from the
gateway: a log copy of a moment the shape sends as a span event or a span
(``agent_copy``), and the game's ``game.start`` / ``game.phase.start`` / ``game.end``
(``emit_agent_event``).

A shape that needs the log channel falls back to ``span_event`` in a process without
OTLP logging (UC-100 A1). Every function here only enqueues (span processor, log
processor): no blocking work, no flag read and no lock, so it is safe on the frame path.
A moment never raises into the game (the kill path records the death between its
bookkeeping and the mode's handler, #2658 slice 3): a failing log emit or moment span is
reported once per process on stderr and then dropped silently.
"""

from __future__ import annotations

import contextlib
import sys
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

AUDIENCE_ATTR = "orangecase.audience"
AUDIENCE_AGENT = "agent"
# In the log shapes the moment's own log event already reaches the agent.
_AGENT_COPY_SHAPES = frozenset({MOMENT_SHAPE_SPAN_EVENT, MOMENT_SHAPE_SPAN})
_LOGGER_NAME = "joustmania.moments"

# (provider, logger) for the last provider seen: get_logger once, not per moment.
_logger_cache: tuple[Any, Any] | None = None

# Whether a failed moment record was reported (once per process, NFR-060: no flood).
_failure_reported = False


def _report_failure_once(name: str, error: Exception) -> None:
    """Report the first failed moment record of the process, never raising.

    Written to stderr, never through Python ``logging``: in a service the root logger's
    OTLP handler writes to the same provider that may be what failed (#2658).
    """
    global _failure_reported
    if _failure_reported:
        return
    _failure_reported = True
    with contextlib.suppress(Exception):  # the report must never raise into the game
        sys.stderr.write(
            f"lib.moments: a moment could not be recorded ({name}: {type(error).__name__}: {error}); "
            "later failures are not reported\n"
        )


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
    timestamp_ns: int | None = None,
    agent_copy: bool = False,
) -> str:
    """Record moment ``name`` on ``parent_span`` in ``shape``; returns the shape actually sent.

    ``attributes`` must already carry the identity keys (``moment.id``, the player, the
    game); every record of the moment gets the same attributes. ``duration_s`` is the
    span shape's length (the warning window for ``death_warning``; 0 for a death, a
    point in time). ``timestamp_ns`` (epoch ns) is the record's own timestamp: the span
    event's, the moment span's start, the log record's; ``None`` takes the clock now
    (UC-100 BR-006: the ground truth passes its own read, so both are equal).
    ``agent_copy``: in the ``span_event`` and ``span`` shapes also emit the log event the
    ``log_event`` shape would send, marked for the agent (it does not change the return).

    Never raises from the log emit or the moment span: on a failure the shape actually
    sent is returned (``span_event`` for ``both``, ``""`` when nothing was sent).
    """
    attrs = dict(attributes)
    if shape not in MOMENT_SHAPES:
        shape = MOMENT_SHAPE_SPAN_EVENT
    moment_logger = _moment_logger() if shape in _LOG_SHAPES or agent_copy else None
    if shape in _LOG_SHAPES and moment_logger is None:
        shape = MOMENT_SHAPE_SPAN_EVENT  # no log channel in this process (UC-100 A1)
    if timestamp_ns is None and (shape == MOMENT_SHAPE_SPAN or moment_logger is not None):
        timestamp_ns = time.time_ns()  # one clock read: the copy carries the moment's time

    sent = _record_primary(name, shape, parent_span, attrs, duration_s, tracer, timestamp_ns, moment_logger)
    if agent_copy and shape in _AGENT_COPY_SHAPES and moment_logger is not None:
        _emit(moment_logger, name, parent_span, {**attrs, AUDIENCE_ATTR: AUDIENCE_AGENT}, timestamp_ns)
    return sent


def _record_primary(name, shape, parent_span, attrs, duration_s, tracer, timestamp_ns, moment_logger) -> str:
    if shape == MOMENT_SHAPE_SPAN:
        try:
            span = (tracer or trace.get_tracer(__name__)).start_span(
                f"game.{name}",
                context=trace.set_span_in_context(parent_span),
                attributes=attrs,
                start_time=timestamp_ns,
            )
            span.end(end_time=timestamp_ns + int(duration_s * 1_000_000_000))
        except Exception as error:  # noqa: BLE001 - a moment never raises into the game
            _report_failure_once(name, error)
            return ""
        return shape

    if shape in (MOMENT_SHAPE_SPAN_EVENT, MOMENT_SHAPE_BOTH):
        parent_span.add_event(name, attributes=attrs, timestamp=timestamp_ns)
    if shape in _LOG_SHAPES and not _emit(moment_logger, name, parent_span, attrs, timestamp_ns):
        return MOMENT_SHAPE_SPAN_EVENT if shape == MOMENT_SHAPE_BOTH else ""
    return shape


def _emit(moment_logger, name: str, span: trace.Span, attrs: Mapping[str, Any], timestamp_ns: int | None) -> bool:
    """Emit one INFO log record ``name`` in ``span``'s context; False (reported once) on a failure."""
    try:
        moment_logger.emit(
            timestamp=timestamp_ns if timestamp_ns is not None else time.time_ns(),
            context=trace.set_span_in_context(span),
            severity_number=SeverityNumber.INFO,
            severity_text="INFO",
            body=name,
            attributes=attrs,
            event_name=name,
        )
    except Exception as error:  # noqa: BLE001 - a record never raises into the game
        _report_failure_once(name, error)
        return False
    return True


def emit_agent_event(name: str, *, span: trace.Span, attributes: Mapping[str, Any]) -> bool:
    """Emit log event ``name`` for the agent only, in ``span``'s context; never raises.

    False when nothing was sent (no OTLP logging in this process, or a failed emit).
    """
    try:
        moment_logger = _moment_logger()
    except Exception as error:  # noqa: BLE001 - runs in the session's end path
        _report_failure_once(name, error)
        return False
    if moment_logger is None:
        return False
    return _emit(moment_logger, name, span, {**attributes, AUDIENCE_ATTR: AUDIENCE_AGENT}, None)
