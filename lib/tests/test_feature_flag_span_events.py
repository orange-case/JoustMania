"""One feature_flag.evaluation span event per (span, flag key, variant) (#2649, telemetry.md §6)."""

from __future__ import annotations

import gc
import threading

import pytest
from openfeature.evaluation_context import EvaluationContext
from openfeature.exception import ErrorCode
from openfeature.flag_evaluation import FlagEvaluationDetails, FlagType, Reason
from openfeature.hook import HookContext
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from lib.openfeature_otel_hooks import MAX_SEEN_PER_SPAN, SPAN_EVENT_SKIP_FLAGS, DedupingTracingHook

pytestmark = pytest.mark.uc("UC-035", flow="main", steps=[6])


@pytest.fixture
def tracer():
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    t = provider.get_tracer("t")
    t.exporter = exporter
    return t


def _evaluate(hook, key, variant="on", reason=Reason.STATIC):
    ctx = HookContext(key, FlagType.STRING, "d", EvaluationContext())
    hook.finally_after(ctx, FlagEvaluationDetails(key, "v", variant=variant, reason=reason), {})


def _events(tracer, name):
    (span,) = [s for s in tracer.exporter.get_finished_spans() if s.name == name]
    return [(e.attributes["feature_flag.key"], e.attributes.get("feature_flag.result.variant")) for e in span.events]


def test_repeated_evaluation_records_one_event(tracer):
    hook = DedupingTracingHook()
    with tracer.start_as_current_span("rpc"):
        for _ in range(15):
            _evaluate(hook, "audio_volume.master", "max")
    assert _events(tracer, "rpc") == [("audio_volume.master", "max")]


def test_changed_variant_in_same_span_is_recorded(tracer):
    hook = DedupingTracingHook()
    with tracer.start_as_current_span("game"):
        _evaluate(hook, "sensitivity", "low")
        _evaluate(hook, "sensitivity", "low")
        _evaluate(hook, "sensitivity", "high")
        _evaluate(hook, "sensitivity", "high")
    assert _events(tracer, "game") == [("sensitivity", "low"), ("sensitivity", "high")]


def test_each_span_gets_its_own_event(tracer):
    hook = DedupingTracingHook()
    for name in ("a", "b"):
        with tracer.start_as_current_span(name):
            _evaluate(hook, "k", "on")
            _evaluate(hook, "k", "on")
    assert _events(tracer, "a") == [("k", "on")]
    assert _events(tracer, "b") == [("k", "on")]


def test_error_after_success_is_recorded(tracer):
    hook = DedupingTracingHook()
    with tracer.start_as_current_span("s"):
        _evaluate(hook, "k", "on")
        _evaluate(hook, "k", None, Reason.ERROR)
        _evaluate(hook, "k", None, Reason.ERROR)
    assert _events(tracer, "s") == [("k", "on"), ("k", None)]


def test_infrastructure_flag_is_never_recorded(tracer):
    assert {"grpc_rpc_spans"} == SPAN_EVENT_SKIP_FLAGS
    hook = DedupingTracingHook()
    with tracer.start_as_current_span("s"):
        _evaluate(hook, "grpc_rpc_spans", "enabled")
        _evaluate(hook, "other", "x")
    assert _events(tracer, "s") == [("other", "x")]


def test_no_recording_span_is_ignored():
    hook = DedupingTracingHook()
    _evaluate(hook, "k", "on")  # INVALID_SPAN: nothing raised, nothing remembered
    assert len(hook._seen) == 0


def test_seen_set_is_bounded_per_span(tracer, monkeypatch):
    from openfeature.contrib.hook.opentelemetry import TracingHook

    calls = []
    monkeypatch.setattr(TracingHook, "finally_after", lambda *_a: calls.append(1))
    hook = DedupingTracingHook()
    with tracer.start_as_current_span("long"):
        for i in range(MAX_SEEN_PER_SPAN * 3):
            _evaluate(hook, "k", f"r{i:x}_1")
        (seen,) = hook._seen.values()
        assert len(seen) == MAX_SEEN_PER_SPAN
    assert len(calls) == MAX_SEEN_PER_SPAN


def test_seen_set_is_freed_with_the_span(tracer):
    hook = DedupingTracingHook()
    for i in range(20):
        with tracer.start_as_current_span(f"s{i}"):
            _evaluate(hook, "k", "on")
    tracer.exporter.clear()
    gc.collect()
    assert len(hook._seen) == 0


def test_concurrent_evaluations_record_once_per_variant(tracer):
    hook = DedupingTracingHook()
    with tracer.start_as_current_span("c") as span:
        from opentelemetry import context, trace

        token_ctx = trace.set_span_in_context(span)

        def work():
            tok = context.attach(token_ctx)
            try:
                for _ in range(200):
                    _evaluate(hook, "k", "on")
            finally:
                context.detach(tok)

        threads = [threading.Thread(target=work) for _ in range(8)]
        [t.start() for t in threads]
        [t.join() for t in threads]
    assert 1 <= len(_events(tracer, "c")) <= 8


def test_error_event_keeps_error_attributes(tracer):
    hook = DedupingTracingHook()
    ctx = HookContext("k", FlagType.STRING, "d", EvaluationContext())
    with tracer.start_as_current_span("s"):
        hook.finally_after(
            ctx, FlagEvaluationDetails("k", "d", reason=Reason.ERROR, error_code=ErrorCode.FLAG_NOT_FOUND), {}
        )
    (span,) = tracer.exporter.get_finished_spans()
    assert span.events[0].attributes["error.type"] == "flag_not_found"


def test_error_with_same_variant_as_a_default_is_recorded(tracer):
    hook = DedupingTracingHook()
    with tracer.start_as_current_span("s"):
        _evaluate(hook, "k", None, Reason.DEFAULT)
        _evaluate(hook, "k", None, Reason.ERROR)
    assert len(_events(tracer, "s")) == 2
