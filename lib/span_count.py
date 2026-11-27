"""Delivered span count per trace (FR-246, UC-103 BR-003; telemetry.md §3.5).

``CountingSpanExporter`` wraps a producer's span exporter and counts, per watched trace, the spans
of every export that returned ``SUCCESS``. A span the SDK dropped before export, or a failed export,
is never counted.
"""

from __future__ import annotations

import threading
from collections import OrderedDict
from collections.abc import Sequence

from opentelemetry import trace
from opentelemetry.sdk.trace import ReadableSpan
from opentelemetry.sdk.trace.export import SpanExporter, SpanExportResult

SPAN_COUNT = "orangecase.span_count"
ATTR_DELIVERED = "orangecase.span_count.delivered"

# A trace whose count is never taken (an aborted game) must not grow the table forever.
MAX_WATCHED = 64
FLUSH_TIMEOUT_MS = 5_000


class DeliveredSpans:
    """Thread-safe per-trace counters; only watched traces are counted.

    Inactive until a ``CountingSpanExporter`` feeds it: a process without one (telemetry off, unit
    tests) watches nothing and so records no count it could not have made.
    """

    def __init__(self, max_watched: int = MAX_WATCHED) -> None:
        self._lock = threading.Lock()
        self._counts: OrderedDict[int, int] = OrderedDict()
        self._max = max_watched
        self.active = False

    def watch(self, trace_id: int) -> None:
        if not self.active:
            return
        with self._lock:
            if trace_id in self._counts:
                return
            self._counts[trace_id] = 0
            while len(self._counts) > self._max:
                self._counts.popitem(last=False)

    def add(self, spans: Sequence[ReadableSpan]) -> None:
        with self._lock:
            if not self._counts:
                return
            for span in spans:
                ctx = span.context
                if ctx is not None and ctx.trace_id in self._counts:
                    self._counts[ctx.trace_id] += 1

    def is_watching(self, trace_id: int) -> bool:
        with self._lock:
            return trace_id in self._counts

    def take(self, trace_id: int) -> int | None:
        """The count of a watched trace, which is then no longer watched; None when not watched."""
        with self._lock:
            return self._counts.pop(trace_id, None)


DELIVERED = DeliveredSpans()


class CountingSpanExporter(SpanExporter):
    def __init__(self, inner: SpanExporter, counts: DeliveredSpans = DELIVERED) -> None:
        self._inner = inner
        self._counts = counts
        counts.active = True

    def export(self, spans: Sequence[ReadableSpan]) -> SpanExportResult:
        result = self._inner.export(spans)
        if result is SpanExportResult.SUCCESS:
            self._counts.add(spans)
        return result

    def shutdown(self) -> None:
        self._inner.shutdown()

    def force_flush(self, timeout_millis: int = 30000) -> bool:
        return self._inner.force_flush(timeout_millis)


def flush_spans(provider: object | None = None, timeout_ms: int = FLUSH_TIMEOUT_MS) -> bool:
    """Flush the tracer provider's span processors; False when the provider cannot flush (no SDK)."""
    target = provider if provider is not None else trace.get_tracer_provider()
    flush = getattr(target, "force_flush", None)
    if flush is None:
        return False
    return bool(flush(timeout_ms))


def record_span_count(
    tracer: trace.Tracer, parent: trace.Span | object, game_id: str, delivered: int, attributes: dict | None = None
) -> None:
    """The count as an ``orangecase.span_count`` span under ``parent`` (a span or a context)."""
    context = trace.set_span_in_context(parent) if isinstance(parent, trace.Span) else parent
    attrs = {"game.id": game_id, ATTR_DELIVERED: delivered, **(attributes or {})}
    tracer.start_span(SPAN_COUNT, context=context, attributes=attrs).end()
