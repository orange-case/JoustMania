"""UC-103 BR-003, FR-246: the delivered span count per trace (lib/span_count.py)."""

from unittest.mock import create_autospec

import pytest
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor, SpanExporter, SpanExportResult
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from lib.span_count import (
    ATTR_DELIVERED,
    SPAN_COUNT,
    CountingSpanExporter,
    DeliveredSpans,
    flush_spans,
    record_span_count,
)

pytestmark = pytest.mark.uc("UC-103", flow="main", steps=[5], br=["BR-003"])


@pytest.fixture
def sdk(monkeypatch):
    monkeypatch.delenv("OTEL_SDK_DISABLED", raising=False)  # the providers here are local


def _provider(exporter):
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    return provider


@pytest.mark.usefixtures("sdk")
def test_only_successful_exports_of_watched_traces_are_counted():
    counts = DeliveredSpans()
    inner = create_autospec(SpanExporter, instance=True)
    inner.export.return_value = SpanExportResult.SUCCESS
    tracer = _provider(CountingSpanExporter(inner, counts)).get_tracer("t")

    with tracer.start_as_current_span("root") as root:
        trace_id = root.get_span_context().trace_id
        counts.watch(trace_id)
        for _ in range(3):
            tracer.start_span("child").end()
        inner.export.return_value = SpanExportResult.FAILURE
        tracer.start_span("lost").end()
        inner.export.return_value = SpanExportResult.SUCCESS
    tracer.start_span("other trace").end()

    assert counts.take(trace_id) == 4  # three children and the root; the failed export is not counted
    assert counts.take(trace_id) is None


def test_a_registry_without_a_counting_exporter_watches_nothing():
    counts = DeliveredSpans()
    counts.watch(1)
    assert not counts.is_watching(1)
    assert counts.take(1) is None


def test_the_oldest_watched_trace_is_dropped_past_the_bound():
    counts = DeliveredSpans(max_watched=2)
    counts.active = True
    for trace_id in (1, 2, 3):
        counts.watch(trace_id)
    assert not counts.is_watching(1)
    assert counts.is_watching(2) and counts.is_watching(3)


@pytest.mark.usefixtures("sdk")
def test_the_count_span_is_a_child_of_the_parent_and_carries_the_count():
    exporter = InMemorySpanExporter()
    tracer = _provider(exporter).get_tracer("t")
    root = tracer.start_span("root")
    record_span_count(tracer, root, "g1", 42)
    root.end()

    (count,) = [s for s in exporter.get_finished_spans() if s.name == SPAN_COUNT]
    assert count.parent.span_id == root.get_span_context().span_id
    assert count.attributes[ATTR_DELIVERED] == 42
    assert count.attributes["game.id"] == "g1"


def test_flush_without_an_sdk_provider_reports_false():
    assert flush_spans(object()) is False


@pytest.mark.usefixtures("sdk")
def test_flush_runs_the_provider_flush():
    assert flush_spans(_provider(InMemorySpanExporter())) is True
