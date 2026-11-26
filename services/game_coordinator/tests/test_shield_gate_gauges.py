"""The agent's shield gate: the coordinator exports what its grant_shield refusal reads.

UC-048 A4 (a held charge), A6 (the cap count and the cooldown end), A13 (the
game's group shields). The agent filters its grants on these gauges; the
coordinator stays authoritative and refuses a grant the agent's stale view let
through.
"""

import sys
from pathlib import Path
from unittest.mock import patch

import pytest

test_dir = Path(__file__).parent
sys.path.insert(0, str(test_dir.parent.parent.parent))
sys.path.insert(0, str(test_dir))

from test_shield_hold_gauges import GaugeSpy  # noqa: E402
from test_shield_limits import T0, activation, clock, game, limits  # noqa: E402, F401 - fixtures

from services.game_coordinator import metrics as metrics_module  # noqa: E402
from services.game_coordinator.games import base as base_module  # noqa: E402

GAME_ID = "test_shield_limits"


@pytest.fixture
def gate():
    spy = GaugeSpy()
    groups = GaugeSpy()
    with patch.multiple(base_module.metrics, player_shield_gate=spy, shield_group_grants=groups):
        yield spy, groups


def stat(gate, name, serial="p1"):
    return gate[0].get(serial=serial, game_id=GAME_ID, stat=name)


class TestUC040AgentShieldGate:
    @pytest.mark.asyncio
    @pytest.mark.uc("UC-048", flow="A6", br=["BR-007"])
    async def test_an_applied_grant_exports_count_and_cooldown_end(self, game, clock, limits, gate):  # noqa: F811, ARG002
        limits["value"] = (3, 15)
        assert await game.grant_shield("p1", 5.0) is True
        assert stat(gate, "grants") == 1
        assert stat(gate, "cooldown_until") == pytest.approx(T0 + 5.0 + 15.0)

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-048", flow="A6", br=["BR-007"])
    async def test_no_cooldown_exports_zero_and_a_refusal_nothing(self, game, clock, limits, gate):  # noqa: F811, ARG002
        limits["value"] = (1, 0)
        assert await game.grant_shield("p1", 5.0) is True
        assert stat(gate, "cooldown_until") == 0
        clock.t += 10.0
        with pytest.raises(Exception):  # noqa: B017, PT011 - the cap refusal
            await game.grant_shield("p1", 5.0)
        assert stat(gate, "grants") == 1

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-048", flow="A4", br=["BR-005"])
    async def test_a_held_charge_is_exported_until_it_ends(self, game, activation, limits, gate):  # noqa: F811, ARG002
        activation["value"] = "trigger"
        limits["value"] = (0, 0)
        player = game.players["p1"]
        with patch.object(game, "_read_charge_expiry", return_value=0.0):
            assert await game.grant_shield("p1", 5.0) is True
        assert stat(gate, "charged") == 1
        game._clear_shield_charge(player)
        assert stat(gate, "charged") == 0

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-048", flow="A13", br=["BR-014"])
    async def test_an_applied_group_shield_exports_the_game_count(self, game, gate):  # noqa: F811, ARG002
        ints = {"policy.group_grant.max_targets": 3, "policy.group_grant.max_per_game": 2}
        with (
            patch.object(base_module, "read_bool_flag", return_value=True),
            patch.object(
                base_module, "read_int_flag", side_effect=lambda _d, key, default, **_: ints.get(key, default)
            ),
        ):
            game.game_kind = "shadow"
            assert await game.grant_group_shield(["p1", "p2"], 3.0) is True
        assert gate[1].get(game_kind="shadow", game_id=GAME_ID) == 1


def test_clear_player_analytics_removes_the_gate_series():
    spy = GaugeSpy()
    with patch.object(metrics_module, "player_shield_gate", spy):
        metrics_module.clear_player_analytics("p1", "g1")
    assert set(spy.removed) == {("p1", "g1", s) for s in metrics_module.PLAYER_SHIELD_GATE_STATS}
