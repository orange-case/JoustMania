"""UC-106 BR-002: a match is the primary game's game_started event with a valid _traceparent."""

import asyncio

import pytest

from services.stowaway.match import MatchTracker, match_from_event
from services.stowaway.tests.fakes import event


@pytest.mark.uc("UC-106", flow="main", steps=[3], br=["BR-002"])
def test_game_started_with_a_traceparent_is_a_match(spans):
    parent = spans.tracer.start_span("root")
    match = match_from_event(event("game_started", "g7", parent_span=parent))
    assert match is not None
    assert match.game_id == "g7"


@pytest.mark.uc("UC-106", br=["BR-002"])
@pytest.mark.parametrize(
    "build",
    [
        lambda p: event("game_start", parent_span=p),  # published outside the game's trace
        lambda p: event("game_started", game_id="", parent_span=p),
        lambda _p: event("game_started"),
        lambda _p: event("game_started", _traceparent="garbage"),
    ],
)
def test_other_events_are_no_match(spans, build):
    assert match_from_event(build(spans.tracer.start_span("root"))) is None


@pytest.mark.uc("UC-106", flow="main", steps=[3, 5], br=["BR-002"])
async def test_the_tracker_runs_one_generator_per_match_and_ends_it(spans):
    started, finished = [], []

    async def on_match(match):
        started.append(match.game_id)
        await match.ended.wait()
        finished.append(match.game_id)

    tracker = MatchTracker(on_match, accept=lambda: True)
    parent = spans.tracer.start_span("root")
    tracker.feed(event("game_started", "g1", parent_span=parent))
    tracker.feed(event("player_death", "g1", parent_span=parent))
    await asyncio.sleep(0)
    assert started == ["g1"]
    tracker.feed(event("game_ended", "g1"))
    await asyncio.sleep(0)
    assert finished == ["g1"]
    assert tracker.match is None

    tracker.feed(event("game_started", "g2", parent_span=parent))
    tracker.feed(event("game_started", "g3", parent_span=parent))  # g2's end was missed
    await tracker.close()
    assert started == ["g1", "g2", "g3"]
    assert finished == ["g1", "g2", "g3"]


@pytest.mark.uc("UC-106", flow="A4")
async def test_a_match_is_skipped_when_not_accepted(spans):
    started = []

    async def on_match(match):
        started.append(match.game_id)

    tracker = MatchTracker(on_match, accept=lambda: False)
    tracker.feed(event("game_started", "g1", parent_span=spans.tracer.start_span("root")))
    await tracker.close()
    assert started == []
