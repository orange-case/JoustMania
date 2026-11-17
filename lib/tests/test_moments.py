"""UC-100 moment shapes (#2658): lib/moments.py records a moment in the shape in effect.

Real SDK tracer and logger providers with in-memory exporters (not the global ones),
so each shape's record, parent and trace context can be asserted.
"""

import logging

import pytest
from opentelemetry._logs import SeverityNumber
from opentelemetry.sdk._logs import LoggerProvider
from opentelemetry.sdk._logs.export import InMemoryLogRecordExporter, SimpleLogRecordProcessor
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from lib import moments, otel_logging
from lib.moments import (
    MOMENT_SHAPE_BOTH,
    MOMENT_SHAPE_LOG_EVENT,
    MOMENT_SHAPE_SPAN,
    MOMENT_SHAPE_SPAN_EVENT,
    effective_moment_shape,
    moment_id,
    read_moment_shape,
    record_moment,
)

ATTRS = {
    "accel_magnitude": 2.5,
    "threshold": 2.0,
    "moment.id": "g1/p0/1",
    "player.serial": "p0",
    "game.id": "g1",
    "game.kind": "real",
}


@pytest.fixture
def spans():
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    return provider.get_tracer("test"), exporter


@pytest.fixture
def logs(monkeypatch):
    exporter = InMemoryLogRecordExporter()
    provider = LoggerProvider()
    provider.add_log_record_processor(SimpleLogRecordProcessor(exporter))
    monkeypatch.setattr(otel_logging, "_logger_provider", provider)
    return exporter


def _record(shape, tracer):
    with tracer.start_as_current_span("player: p0") as player_span:
        sent = record_moment(
            "death_warning", shape=shape, parent_span=player_span, attributes=ATTRS, duration_s=0.5, tracer=tracer
        )
    return sent, player_span


def _player_span(exporter):
    return next(s for s in exporter.get_finished_spans() if s.name == "player: p0")


@pytest.mark.uc("UC-100", flow="main", steps=[4], br=["BR-001", "BR-004"])
def test_span_event_shape_is_an_event_on_the_player_span(spans):
    tracer, exporter = spans
    sent, _ = _record(MOMENT_SHAPE_SPAN_EVENT, tracer)

    assert sent == MOMENT_SHAPE_SPAN_EVENT
    finished = exporter.get_finished_spans()
    assert [s.name for s in finished] == ["player: p0"]
    events = finished[0].events
    assert [e.name for e in events] == ["death_warning"]
    assert dict(events[0].attributes) == ATTRS


@pytest.mark.uc("UC-100", flow="main", steps=[4], br=["BR-001", "BR-003"])
def test_span_shape_is_a_child_span_ended_at_once_with_the_warning_window(spans):
    tracer, exporter = spans
    sent, _ = _record(MOMENT_SHAPE_SPAN, tracer)

    assert sent == MOMENT_SHAPE_SPAN
    player = _player_span(exporter)
    assert player.events == ()
    moment = next(s for s in exporter.get_finished_spans() if s.name == "game.death_warning")
    assert moment.parent.span_id == player.context.span_id
    assert moment.context.trace_id == player.context.trace_id
    assert dict(moment.attributes) == ATTRS
    assert moment.end_time - moment.start_time == 500_000_000


@pytest.mark.uc("UC-100", flow="main", steps=[4], br=["BR-001", "BR-004"])
@pytest.mark.parametrize("shape", [MOMENT_SHAPE_SPAN_EVENT, MOMENT_SHAPE_SPAN])
def test_span_shapes_send_no_log_record_with_otlp_logging_on(spans, logs, shape):
    """NFR-060 / BR-004: with OTLP logging on, the default (and the span) shape adds no log record."""
    tracer, _ = spans
    sent, _ = _record(shape, tracer)

    assert sent == shape
    assert logs.get_finished_logs() == ()


@pytest.mark.uc("UC-100", flow="main", steps=[4], br=["BR-001", "BR-003"])
def test_log_event_shape_is_an_sdk_log_record_in_the_player_span_context(spans, logs):
    tracer, exporter = spans
    sent, _ = _record(MOMENT_SHAPE_LOG_EVENT, tracer)

    assert sent == MOMENT_SHAPE_LOG_EVENT
    player = _player_span(exporter)
    assert player.events == ()
    assert [s.name for s in exporter.get_finished_spans()] == ["player: p0"]
    (record,) = [r.log_record for r in logs.get_finished_logs()]
    assert record.event_name == "death_warning"
    assert record.severity_number == SeverityNumber.INFO
    assert dict(record.attributes) == ATTRS
    assert record.trace_id == player.context.trace_id
    assert record.span_id == player.context.span_id


@pytest.mark.uc("UC-100", flow="A6", br=["BR-003", "BR-005"])
def test_both_shape_sends_span_event_and_log_event_with_one_moment_id(spans, logs):
    tracer, exporter = spans
    sent, _ = _record(MOMENT_SHAPE_BOTH, tracer)

    assert sent == MOMENT_SHAPE_BOTH
    event = _player_span(exporter).events[0]
    (record,) = [r.log_record for r in logs.get_finished_logs()]
    assert event.name == record.event_name == "death_warning"
    assert dict(event.attributes) == dict(record.attributes) == ATTRS
    assert event.attributes["moment.id"] == record.attributes["moment.id"]


@pytest.mark.uc("UC-100", flow="main", steps=[4], br=["BR-001"])
def test_log_event_is_sent_whatever_the_log_level(spans, logs):
    """``log_level=error`` (or any Python logging filter) never drops the moment."""
    tracer, _ = spans
    root = logging.getLogger()
    previous = root.level
    root.setLevel(logging.ERROR)
    logging.disable(logging.CRITICAL)
    try:
        _record(MOMENT_SHAPE_LOG_EVENT, tracer)
    finally:
        logging.disable(logging.NOTSET)
        root.setLevel(previous)

    assert [r.log_record.event_name for r in logs.get_finished_logs()] == ["death_warning"]


@pytest.mark.uc("UC-100", flow="A1", br=["BR-001", "BR-004"])
@pytest.mark.parametrize("shape", [MOMENT_SHAPE_LOG_EVENT, MOMENT_SHAPE_BOTH])
def test_without_otlp_logging_a_log_shape_falls_back_to_span_event(spans, monkeypatch, shape):
    monkeypatch.setattr(otel_logging, "_logger_provider", None)
    tracer, exporter = spans

    assert effective_moment_shape(shape) == MOMENT_SHAPE_SPAN_EVENT
    sent, _ = _record(shape, tracer)

    assert sent == MOMENT_SHAPE_SPAN_EVENT
    assert [e.name for e in _player_span(exporter).events] == ["death_warning"]


@pytest.mark.uc("UC-100", flow="A1", br=["BR-004"])
@pytest.mark.parametrize("value", [None, "", "spans", "SPAN", 3])
def test_unknown_flag_value_is_span_event(value):
    assert effective_moment_shape(value) == MOMENT_SHAPE_SPAN_EVENT


@pytest.mark.uc("UC-100", flow="main", steps=[3], br=["BR-002"])
@pytest.mark.parametrize(
    "value", [MOMENT_SHAPE_SPAN_EVENT, MOMENT_SHAPE_SPAN, MOMENT_SHAPE_LOG_EVENT, MOMENT_SHAPE_BOTH]
)
@pytest.mark.usefixtures("logs")
def test_declared_shapes_are_kept_with_otlp_logging(value):
    assert effective_moment_shape(value) == value


@pytest.mark.uc("UC-100", flow="A1", br=["BR-004"])
@pytest.mark.usefixtures("logs")
def test_not_ready_observability_provider_reads_span_event(monkeypatch):
    """The coordinator does not wait for ``observability``: a not-READY provider gives the default."""
    from openfeature import api
    from openfeature.exception import ProviderNotReadyError
    from openfeature.provider.in_memory_provider import InMemoryProvider

    class _NotReadyProvider(InMemoryProvider):
        def resolve_string_details(self, flag_key, default_value, evaluation_context=None):
            raise ProviderNotReadyError("no first sync yet")

    domain = "observability_not_ready_2658"
    api.set_provider(_NotReadyProvider({}), domain=domain)
    monkeypatch.setattr("lib.feature_flags.init_flag_domain", lambda _domain: None)
    monkeypatch.setattr("lib.feature_flags.get_flag_client", lambda _domain: api.get_client(domain=domain))

    assert read_moment_shape() == MOMENT_SHAPE_SPAN_EVENT


@pytest.mark.uc("UC-100", flow="main", steps=[3], br=["BR-002"])
@pytest.mark.usefixtures("logs")
def test_read_moment_shape_reads_the_observability_flag(monkeypatch):
    calls = []

    def fake_read(domain, key, default):
        calls.append((domain, key, default))
        return "span"

    monkeypatch.setattr("lib.feature_flags.read_string_flag", fake_read)
    assert read_moment_shape() == MOMENT_SHAPE_SPAN
    assert calls == [("observability", "telemetry.moment_shape", "span_event")]


@pytest.mark.uc("UC-100", br=["BR-003"])
def test_moment_id_format():
    assert moment_id("g-1", "00:06:f7:aa", 3) == "g-1/00:06:f7:aa/3"


@pytest.mark.usefixtures("logs")
def test_moment_logger_is_cached_per_provider(monkeypatch):
    """get_logger runs once per provider, not per moment (NFR-060: no per-warning setup)."""
    first = moments._moment_logger()
    assert moments._moment_logger() is first
    monkeypatch.setattr(otel_logging, "_logger_provider", None)
    assert moments._moment_logger() is None
