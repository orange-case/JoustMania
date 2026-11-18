"""The per-player sensitivity factor floor of 0.2 (#2696, UC-040 BR-045, UC-030 step 5).

An operator may widen decision.magnitude_bounds for adjust_player_sensitivity down
to 0.2 (death threshold x5). The coordinator applies a 0.2 factor, clamps 0.1 up to
0.2 (the factor divides the thresholds, so it never reaches 0), and lowers the floor
of the combined sensitivity x difficulty factor only by the per-player factor:
global difficulty alone keeps its 0.5 floor.
"""

import sys
from pathlib import Path

import pytest

test_dir = Path(__file__).parent
sys.path.insert(0, str(test_dir.parent.parent.parent))
sys.path.insert(0, str(test_dir))

from test_difficulty_interventions import TargetingFlagClient, make_ctx, make_game, make_manager

from services.game_coordinator.difficulty_handlers import SENSITIVITY_FACTOR_MIN, handle_player_sensitivity_factor
from services.game_coordinator.games.base import (
    COMBINED_FACTOR_MIN,
    SENSITIVITY_FACTOR_FLOOR,
    Player,
)


def _death(game, factor, difficulty=1.0):
    game.global_difficulty_factor = difficulty
    return game._compute_effective_thresholds(Player(serial="t", sensitivity_factor=factor))[1]


@pytest.mark.uc("UC-040", br=["BR-045"])
def test_floor_constants():
    assert pytest.approx(0.2) == SENSITIVITY_FACTOR_FLOOR
    assert SENSITIVITY_FACTOR_MIN == SENSITIVITY_FACTOR_FLOOR
    assert pytest.approx(0.5) == COMBINED_FACTOR_MIN


@pytest.mark.uc("UC-040", br=["BR-045"])
@pytest.mark.asyncio
async def test_a_0_2_factor_reaches_the_player_and_0_1_is_clamped_to_0_2():
    game = make_game(num_players=2)
    mgr, _ = make_manager(game=game)
    mgr._interventions_client = TargetingFlagClient(default=1.0, per_serial={"p1": 0.2, "p2": 0.1})

    await handle_player_sensitivity_factor(make_ctx("player_sensitivity_factor", 0, game), mgr)

    assert game.players["p1"].sensitivity_factor == pytest.approx(0.2)
    assert game.players["p2"].sensitivity_factor == pytest.approx(0.2)


@pytest.mark.uc("UC-030", flow="main", steps=[5])
@pytest.mark.uc("UC-040", br=["BR-045"])
def test_a_0_2_factor_multiplies_the_death_threshold_by_5():
    game = make_game()
    neutral = _death(game, 1.0)
    assert _death(game, 0.2) == pytest.approx(neutral * 5)
    # Below the floor the threshold stays finite: 0.1 acts as 0.2.
    assert _death(game, 0.1) == pytest.approx(neutral * 5)


@pytest.mark.uc("UC-030", flow="main", steps=[5])
@pytest.mark.uc("UC-040", br=["BR-045"])
def test_global_difficulty_alone_keeps_the_0_5_floor():
    game = make_game()
    neutral = _death(game, 1.0)
    # difficulty 0.5 with a neutral player: combined 0.5, unchanged.
    assert _death(game, 1.0, difficulty=0.5) == pytest.approx(neutral * 2)
    # difficulty 0.5 x player 0.7 = 0.35: the floor stays 0.5 (the player factor is above it).
    assert _death(game, 0.7, difficulty=0.5) == pytest.approx(neutral * 2)
    # difficulty 0.5 x player 0.4 = 0.2: the floor is lowered only to the player's own 0.4.
    assert _death(game, 0.4, difficulty=0.5) == pytest.approx(neutral / 0.4)
    # difficulty 0.5 x player 0.2 = 0.1: floor 0.2.
    assert _death(game, 0.2, difficulty=0.5) == pytest.approx(neutral * 5)
