"""Focused tests for ``intervention_effects`` (#2634).

The ambient effect handlers and the agent cue moved out of
``InterventionManager`` into free functions. These tests pin the two
properties the move could lose: the manager's end_game handler reads
``_end_game_fn`` at call time (it may be wired after construction), and the
cue re-checks its gate itself on the game loop instead of trusting an earlier
read.
"""

from unittest.mock import AsyncMock

import pytest

from services.game_coordinator import intervention_effects
from services.game_coordinator.games.base import Player
from services.game_coordinator.interventions import (
    INTERVENTION_SPECS,
    InterventionContext,
    InterventionManager,
)
from services.game_coordinator.tests.conftest import FakeInterventionGame


class FakeStream:
    def __init__(self):
        self.writes = []

    async def write(self, cmd):
        self.writes.append(cmd)


def _spec(type_id):
    return next(s for s in INTERVENTION_SPECS if s.type_id == type_id)


def _ctx(*, game_id):
    return InterventionContext(
        spec=_spec("end_game"),
        value="",
        payload="",
        target_serial=None,
        game=None,
        objective="balanced",
        game_id=game_id,
    )


@pytest.mark.uc("UC-040", br=["BR-011"])
@pytest.mark.asyncio
async def test_end_game_handler_reads_end_game_fn_at_call_time():
    mgr = InterventionManager(event_publisher=AsyncMock(), get_game=lambda: None, end_game_fn=None)
    mgr.register_ambient_handlers()

    # Wired after construction and registration: the handler must still see it.
    end = AsyncMock(return_value=(True, ""))
    mgr._end_game_fn = end

    await mgr._handlers["end_game"](_ctx(game_id="g7"))

    end.assert_awaited_once_with("g7", "agent_intervention")


@pytest.mark.uc("UC-040", flow="main", steps=[13])
@pytest.mark.asyncio
async def test_emit_agent_cue_rechecks_the_gate_on_the_game_loop():
    def game_with(stream):
        return FakeInterventionGame(
            players={"A": Player("A"), "B": Player("B"), "C": Player("C", alive=False)},
            gameplay_stream=stream,
        )

    # Gate off at emission time: no write at all, whatever the caller read before.
    off = FakeStream()
    await intervention_effects.emit_agent_cue(_spec("end_game"), None, game_with(off), cue_enabled=lambda: False)
    assert off.writes == []

    # Gate on: one pulse per alive player.
    on = FakeStream()
    await intervention_effects.emit_agent_cue(_spec("end_game"), None, game_with(on), cue_enabled=lambda: True)
    assert sorted(c.game_effect.serial for c in on.writes) == ["A", "B"]
