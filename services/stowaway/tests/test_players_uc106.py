"""UC-106 A2 / BR-005: players mode runs a pattern game only while no real match runs."""

import asyncio
import json
import time

import pytest

from proto import game_coordinator_pb2 as gc_pb2
from services.stowaway.config import SettingsSource
from services.stowaway.players import (
    ARCHETYPE,
    ARCHETYPE_PARAMS,
    STOP_GAME_END,
    STOP_KILL_SWITCH,
    STOP_REAL_MATCH,
    TAG,
    PatternPlayers,
)
from services.stowaway.tests.fakes import FakeCoordinator, FakeMock, FlagState, event


def _players(gc, mock, flags, **kw):
    return PatternPlayers(gc, mock, SettingsSource(read=flags, max_age=0.0), watch_s=0.02, primary_check_s=0.02, **kw)


async def _wait_for_call(gc):
    for _ in range(100):
        if gc.calls:
            return gc.calls[-1]
        await asyncio.sleep(0.01)
    raise AssertionError("no StreamGameEvents call")


@pytest.mark.uc("UC-106", flow="A2", br=["BR-005"])
async def test_a_pattern_game_on_reserved_mock_controllers(spans):
    gc, mock = FakeCoordinator(), FakeMock()
    flags = FlagState(enabled=True, mode="players")
    seen = []
    players = _players(gc, mock, flags, on_event=seen.append, tracer=spans.tracer)
    task = asyncio.create_task(players.play_one())
    call = await _wait_for_call(gc)
    call.push(event("game_start", "p1"), event("game_ended", "p1"))
    assert await asyncio.wait_for(task, 1.0) == STOP_GAME_END

    (added,) = mock.added
    assert added.reserved and added.tag == TAG and added.archetype == ARCHETYPE
    assert json.loads(added.archetype_params) == ARCHETYPE_PARAMS
    start = gc.requests[0].start_config
    assert start.game_name == "JoustFFA"
    assert start.origin == gc_pb2.GAME_ORIGIN_UNSPECIFIED  # always a shadow game
    assert [p.serial for p in start.players] == [f"mock-{i}" for i in range(ARCHETYPE_PARAMS["players"])]
    assert sorted(mock.removed) == sorted(p.serial for p in start.players)
    assert [e.event_type for e in seen] == ["game_start", "game_ended"]
    (span,) = [s for s in spans.get_finished_spans() if s.name == "stowaway.players.game"]
    assert span.attributes["orangecase.synthetic"] is True
    assert span.attributes["game.id"] == "p1"
    assert span.attributes["stowaway.stop_reason"] == STOP_GAME_END


@pytest.mark.uc("UC-106", flow="A2", steps=[3], br=["BR-005"])
async def test_a_real_match_force_ends_the_pattern_game(spans):
    gc, mock = FakeCoordinator(), FakeMock()
    players = _players(gc, mock, FlagState(enabled=True, mode="both"), tracer=spans.tracer)
    task = asyncio.create_task(players.play_one())
    call = await _wait_for_call(gc)
    call.push(event("game_start", "p1"))
    await asyncio.sleep(0.05)
    gc.state = gc_pb2.RUNNING
    assert await asyncio.wait_for(task, 1.0) == STOP_REAL_MATCH
    assert [r.game_id for r in gc.force_ended] == ["p1"]
    assert len(mock.removed) == ARCHETYPE_PARAMS["players"]


@pytest.mark.uc("UC-106", flow="A1", steps=[2], br=["BR-003"])
async def test_the_kill_switch_ends_the_pattern_game_within_one_second(spans):
    gc, mock = FakeCoordinator(), FakeMock()
    flags = FlagState(enabled=True, mode="players")
    players = _players(gc, mock, flags, tracer=spans.tracer)
    task = asyncio.create_task(players.play_one())
    call = await _wait_for_call(gc)
    call.push(event("game_start", "p1"))
    await asyncio.sleep(0.05)
    flags.set(enabled=False)
    off_at = time.monotonic()
    assert await asyncio.wait_for(task, 1.0) == STOP_KILL_SWITCH
    assert time.monotonic() - off_at < 1.0
    assert [r.game_id for r in gc.force_ended] == ["p1"]
    assert len(mock.removed) == ARCHETYPE_PARAMS["players"]


@pytest.mark.uc("UC-106", br=["BR-005"])
@pytest.mark.parametrize("state", [gc_pb2.STARTING, gc_pb2.RUNNING])
async def test_no_pattern_game_while_a_real_match_runs(state):
    gc, mock = FakeCoordinator(state=state), FakeMock()
    players = _players(gc, mock, FlagState(enabled=True, mode="players"))
    stop = asyncio.Event()
    task = asyncio.create_task(players.run(stop))
    await asyncio.sleep(0.1)
    stop.set()
    await asyncio.wait_for(task, 1.0)
    assert mock.added == []


@pytest.mark.uc("UC-106", br=["BR-005"])
async def test_an_unreachable_coordinator_counts_as_a_real_match():
    gc = FakeCoordinator()
    gc.fail_state = True
    assert await _players(gc, FakeMock(), FlagState()).real_match_running() is True


@pytest.mark.uc("UC-106", flow="A2", br=["BR-005"])
async def test_a_refused_reservation_starts_no_game(spans):
    gc, mock = FakeCoordinator(), FakeMock(success=False)
    players = _players(gc, mock, FlagState(enabled=True, mode="players"), tracer=spans.tracer)
    assert await players.play_one() == "error"
    assert gc.requests == []
    assert len(mock.removed) == ARCHETYPE_PARAMS["players"]


@pytest.mark.uc("UC-106", flow="A2", br=["BR-005"])
async def test_players_mode_off_starts_nothing():
    gc, mock = FakeCoordinator(), FakeMock()
    players = _players(gc, mock, FlagState(enabled=True, mode="telemetry"))
    stop = asyncio.Event()
    task = asyncio.create_task(players.run(stop))
    await asyncio.sleep(0.1)
    stop.set()
    await asyncio.wait_for(task, 1.0)
    assert mock.added == []
