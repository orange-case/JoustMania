"""UC-103 BR-002, BR-003: the padding rate per round and stowaway's delivered count of the match trace."""

from unittest.mock import MagicMock

import pytest
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from lib.span_count import ATTR_DELIVERED, SPAN_COUNT, CountingSpanExporter, DeliveredSpans
from services.stowaway import metrics as stowaway_metrics
from services.stowaway.challenges import STOP_MATCH_END, STOP_TARGET, Run, SpanRounds, padding_rate
from services.stowaway.config import SettingsSource
from services.stowaway.match import match_from_event
from services.stowaway.tests.fakes import FlagState, event

SYNTH = "orangecase.synthetic"


@pytest.mark.uc("UC-103", br=["BR-002", "BR-007"])
@pytest.mark.parametrize(
    ("intensity", "target", "rate"),
    [(1.0, 10_000, 500.0), (1.0, 100_000, 1000.0), (1.0, 1_000_000, 10_000.0), (4.0, 100_000, 2000.0)],
)
def test_every_round_reaches_its_target_within_100_s(intensity, target, rate):
    assert padding_rate(intensity, target) == rate
    assert target / padding_rate(intensity, target) <= 100.0


def _counted():
    counts = DeliveredSpans()
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(CountingSpanExporter(exporter, counts)))
    return counts, exporter, provider


async def _pad(target, end_match=False):
    counts, exporter, provider = _counted()
    tracer = provider.get_tracer("t")
    parent = tracer.start_span("game.phase.gameplay")
    match = match_from_event(event("game_started", parent_span=parent))
    if end_match:
        match.ended.set()
    on_count = MagicMock()
    flush = MagicMock(side_effect=provider.force_flush)
    flags = FlagState(enabled=True, challenge="span-rounds", intensity=4.0, span_target=target)
    run = Run("span-rounds", match, SettingsSource(read=flags, max_age=0.0), tick_s=0.01)
    reason = await SpanRounds(tracer, counts=counts, flush=flush, on_count=on_count).run(run)
    return reason, parent, exporter, on_count, flush, counts


@pytest.mark.uc("UC-103", flow="main", steps=[5], br=["BR-003"])
async def test_the_count_follows_the_padding_and_is_not_its_own_span(monkeypatch):
    monkeypatch.delenv("OTEL_SDK_DISABLED", raising=False)
    reason, parent, exporter, on_count, flush, counts = await _pad(120)

    assert reason == STOP_TARGET
    flush.assert_called_once()
    (count,) = [s for s in exporter.get_finished_spans() if s.name == SPAN_COUNT]
    assert count.attributes[ATTR_DELIVERED] == 121  # the padding span and its 120 children
    assert count.attributes[SYNTH] is True
    assert count.attributes["game.id"] == "g1"
    assert count.parent.span_id == parent.get_span_context().span_id
    on_count.assert_called_once_with("g1", 121)
    assert not counts.is_watching(parent.get_span_context().trace_id)


@pytest.mark.uc("UC-103", flow="A2", br=["BR-003"])
async def test_a_short_game_still_reports_what_was_delivered(monkeypatch):
    monkeypatch.delenv("OTEL_SDK_DISABLED", raising=False)
    reason, _parent, _exporter, on_count, _flush, _counts = await _pad(1_000_000, end_match=True)

    assert reason == STOP_MATCH_END
    on_count.assert_called_once_with("g1", 1)  # only the padding span: the game ended at once


@pytest.mark.uc("UC-103", flow="main", steps=[5], br=["BR-003"])
def test_the_gauge_keeps_only_the_recent_games(monkeypatch):
    gauge = MagicMock()
    monkeypatch.setattr(stowaway_metrics, "trace_spans_delivered", gauge)
    monkeypatch.setattr(stowaway_metrics, "_games", [])
    for i in range(stowaway_metrics.TRACE_SPANS_DELIVERED_KEEP + 1):
        stowaway_metrics.set_trace_spans_delivered(f"g{i}", i)
    gauge.labels.assert_any_call(game_id="g0")
    gauge.remove.assert_called_once_with("g0")
