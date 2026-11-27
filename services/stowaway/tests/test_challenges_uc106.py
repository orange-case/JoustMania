"""UC-106 BR-006, A1, A3, A5: the generators attach synthetic CONSUMER spans to the match trace."""

import asyncio
import logging
import time

import pytest
from opentelemetry.trace import SpanKind

from services.stowaway.challenges import (
    METRIC_SWING,
    STOP_CHALLENGE_CHANGE,
    STOP_KILL_SWITCH,
    STOP_MATCH_END,
    STOP_TARGET,
    NotImplementedChallenge,
    Run,
    SpanRounds,
    SwingDensity,
    SwingGauge,
    build_generators,
    swing_players,
    swing_value,
)
from services.stowaway.config import CHALLENGES, SettingsSource
from services.stowaway.match import match_from_event
from services.stowaway.tests.fakes import FlagState, event

SYNTH = "orangecase.synthetic"


def _match(spans):
    parent = spans.tracer.start_span("game.phase.gameplay")
    match = match_from_event(event("game_started", parent_span=parent))
    assert match is not None
    return parent, match


def _run(name, match, flags, tick_s=0.01):
    return Run(name, match, SettingsSource(read=flags, max_age=0.0), tick_s=tick_s)


def _named(spans, name):
    return [s for s in spans.get_finished_spans() if s.name == name]


@pytest.mark.uc("UC-106", flow="main", steps=[4, 5], br=["BR-001", "BR-002", "BR-006"])
async def test_span_rounds_pads_the_match_trace_up_to_the_target(spans):
    parent, match = _match(spans)
    flags = FlagState(enabled=True, challenge="span-rounds", intensity=4.0, span_target=300)
    reason = await SpanRounds(spans.tracer).run(_run("span-rounds", match, flags))

    assert reason == STOP_TARGET
    (pad,) = _named(spans, "stowaway.padding")
    children = _named(spans, "stowaway.padding.span")
    assert pad.kind == SpanKind.CONSUMER
    assert pad.parent.span_id == parent.get_span_context().span_id
    assert pad.context.trace_id == parent.get_span_context().trace_id
    assert pad.attributes[SYNTH] is True
    assert pad.attributes["game.id"] == "g1"
    assert pad.attributes["orangecase.challenge.name"] == "span-rounds"
    assert pad.attributes["stowaway.span_target"] == 300
    assert pad.attributes["stowaway.spans_emitted"] == 300
    assert pad.attributes["stowaway.stop_reason"] == STOP_TARGET
    assert len(children) == 300
    assert all(c.parent.span_id == pad.context.span_id and c.attributes[SYNTH] is True for c in children)
    assert sorted(c.attributes["stowaway.padding.index"] for c in children) == list(range(300))


@pytest.mark.uc("UC-106", flow="A1", br=["BR-003"])
async def test_the_kill_switch_stops_the_padding_within_one_second(spans):
    _, match = _match(spans)
    flags = FlagState(enabled=True, challenge="span-rounds", intensity=1.0, span_target=1_000_000)
    run = Run("span-rounds", match, SettingsSource(read=flags))  # production cache age and tick
    task = asyncio.create_task(SpanRounds(spans.tracer).run(run))
    await asyncio.sleep(0.3)
    flags.set(enabled=False)
    off_at = time.monotonic()
    reason = await asyncio.wait_for(task, 1.0)
    assert time.monotonic() - off_at < 1.0
    assert reason == STOP_KILL_SWITCH
    emitted = len(_named(spans, "stowaway.padding.span"))
    assert 0 < emitted < 1_000_000
    await asyncio.sleep(0.2)
    assert len(_named(spans, "stowaway.padding.span")) == emitted


@pytest.mark.uc("UC-106", flow="A5")
async def test_a_challenge_change_ends_the_generator(spans):
    _, match = _match(spans)
    flags = FlagState(enabled=True, challenge="span-rounds", span_target=1_000_000)
    task = asyncio.create_task(SpanRounds(spans.tracer).run(_run("span-rounds", match, flags)))
    await asyncio.sleep(0.05)
    flags.set(challenge="swing-density")
    assert await asyncio.wait_for(task, 1.0) == STOP_CHALLENGE_CHANGE


@pytest.mark.uc("UC-106", flow="main", steps=[5])
async def test_the_match_end_ends_the_padding(spans):
    _, match = _match(spans)
    flags = FlagState(enabled=True, challenge="span-rounds", span_target=1_000_000)
    task = asyncio.create_task(SpanRounds(spans.tracer).run(_run("span-rounds", match, flags)))
    await asyncio.sleep(0.05)
    match.ended.set()
    assert await asyncio.wait_for(task, 1.0) == STOP_MATCH_END
    (pad,) = _named(spans, "stowaway.padding")
    assert pad.attributes["stowaway.stop_reason"] == STOP_MATCH_END


def _points(reader):
    data = reader.get_metrics_data()
    if data is None:
        return []
    return [
        point
        for rm in data.resource_metrics
        for sm in rm.scope_metrics
        for metric in sm.metrics
        if metric.name == METRIC_SWING
        for point in metric.data.data_points
    ]


@pytest.mark.uc("UC-106", flow="main", steps=[4, 5], br=["BR-006"])
async def test_swing_density_reports_per_player_motion_only_during_the_match(spans, metric_reader):
    parent, match = _match(spans)
    gauge = SwingGauge(metric_reader.meter)
    assert _points(metric_reader) == []
    flags = FlagState(enabled=True, challenge="swing-density", intensity=1.0)
    task = asyncio.create_task(SwingDensity(gauge, spans.tracer).run(_run("swing-density", match, flags)))
    await asyncio.sleep(0.05)

    points = _points(metric_reader)
    assert {p.attributes["serial"] for p in points} == {f"stowaway-{i}" for i in range(1, 5)}
    assert {p.attributes["game_id"] for p in points} == {"g1"}
    assert all(1.0 <= p.value <= 3.0 for p in points)

    match.ended.set()
    assert await asyncio.wait_for(task, 1.0) == STOP_MATCH_END
    assert _points(metric_reader) == []
    (swing,) = _named(spans, "stowaway.swing")
    assert swing.kind == SpanKind.CONSUMER
    assert swing.parent.span_id == parent.get_span_context().span_id
    assert swing.attributes[SYNTH] is True
    assert swing.attributes["stowaway.swing.players"] == 4
    assert swing.attributes["stowaway.stop_reason"] == STOP_MATCH_END


@pytest.mark.uc("UC-106", br=["BR-004", "BR-006"])
def test_swing_shape_and_player_count():
    values = [swing_value(t / 10, 0) for t in range(30)]
    assert max(values) > 2.5
    assert min(values) == pytest.approx(1.0, abs=1e-3)
    assert swing_players(0.0) == 1
    assert swing_players(4.0) == 16


@pytest.mark.uc("UC-106", flow="A3", br=["BR-006"])
async def test_a_stub_challenge_logs_once_and_emits_nothing(spans, caplog):
    _, match = _match(spans)
    flags = FlagState(enabled=True, challenge="clock-skew")
    stub = NotImplementedChallenge("clock-skew")
    with caplog.at_level(logging.WARNING):
        task = asyncio.create_task(stub.run(_run("clock-skew", match, flags)))
        await asyncio.sleep(0.03)
        flags.set(enabled=False)
        assert await asyncio.wait_for(task, 1.0) == STOP_KILL_SWITCH
    assert "not implemented" in caplog.text
    assert spans.get_finished_spans() == ()


@pytest.mark.uc("UC-106", br=["BR-006"])
def test_every_challenge_but_none_is_selectable(spans, metric_reader):
    generators = build_generators(spans.tracer, metric_reader.meter)
    assert set(generators) == set(CHALLENGES) - {"none"}
    assert isinstance(generators["cardinality-bomb"], NotImplementedChallenge)
