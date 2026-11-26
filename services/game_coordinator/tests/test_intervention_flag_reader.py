"""Unit tests for the intervention flag reader on its own (#2634, InterventionFlagReader).

Target resolution (per session, per player) and the policy values, against an
in-memory flag client; plus the manager attributes that forward to the reader.
"""

from unittest.mock import AsyncMock

import pytest
from openfeature.exception import ErrorCode
from openfeature.flag_evaluation import FlagEvaluationDetails, Reason

from services.game_coordinator.intervention_evaluation import _UNKNOWN
from services.game_coordinator.interventions import (
    DEFAULT_COORDINATOR_BACKSTOP_BUDGET,
    INTERVENTION_SPECS,
    InterventionFlagReader,
    InterventionManager,
)
from services.game_coordinator.tests.conftest import FakeInterventionGame

SPECS = {s.flag_key: s for s in INTERVENTION_SPECS}


class Client:
    """In-memory flag client: ``values[(key, targeting_key)]`` or ``values[key]``; records each context."""

    def __init__(self, values=None, *, raising=False, error=False):
        self.values = values or {}
        self.raising = raising
        self.error = error
        self.contexts = []

    def _get(self, key, default, ctx):
        self.contexts.append(ctx)
        if self.raising:
            raise RuntimeError("flagd down")
        return self.values.get((key, ctx.targeting_key), self.values.get(key, default))

    def get_boolean_value(self, key, default, ctx):
        return self._get(key, default, ctx)

    get_string_value = get_integer_value = get_float_value = get_object_value = get_boolean_value

    def get_float_details(self, key, default, ctx):
        value = self._get(key, default, ctx)
        if self.error:
            return FlagEvaluationDetails(key, default, reason=Reason.ERROR, error_code=ErrorCode.GENERAL)
        return FlagEvaluationDetails(key, value, variant="v1", flag_metadata={"decisionId.v1": "id-1"})


@pytest.mark.uc("UC-040", br=["BR-004"])
def test_allowed_types_is_empty_while_the_agent_is_disabled_or_unreachable():
    reader = InterventionFlagReader()
    assert reader.allowed_types() == set()
    reader._agent_client = Client({"enabled": False, "interventions_allowed": "a,b"})
    assert reader.allowed_types() == set()
    reader._agent_client = Client({"enabled": True, "interventions_allowed": " a, ,b "})
    assert reader.allowed_types() == {"a", "b"}
    reader._agent_client = Client(raising=True)
    assert reader.allowed_types() == set()


def test_policy_values_fall_back_to_their_defaults():
    reader = InterventionFlagReader()
    assert reader.backstop_budget() == DEFAULT_COORDINATOR_BACKSTOP_BUDGET
    assert reader.battery_threshold() == 20.0
    assert reader.dominant_objective() == "balanced"
    assert reader.cue_enabled() is True
    reader._agent_client = Client(
        {"policy.coordinator_backstop_per_minute": 7, "objectives": {"fun": 0.2, "balance": 0.8}}
    )
    assert reader.backstop_budget() == 7.0
    assert reader.dominant_objective() == "balance"
    reader._agent_client = Client(raising=True)
    assert reader.backstop_budget() == DEFAULT_COORDINATOR_BACKSTOP_BUDGET


def test_a_raising_battery_provider_reads_as_unknown():
    def provider(_serial):
        raise RuntimeError("no metric")

    assert InterventionFlagReader(provider).battery_pct("p1") is None
    assert InterventionFlagReader({"p1": 42.0}.get).battery_pct("p1") == 42.0


@pytest.mark.uc("UC-040", flow="A19", br=["BR-012"])
def test_a_grant_read_carries_the_variant_and_id_and_an_error_reads_as_unknown():
    reader = InterventionFlagReader()
    reader._interventions_client = Client({("shield_seconds", "p1"): 5})
    assert reader._read_grant(SPECS["shield_seconds"], "g1", "p1") == ("v1", 5.0, "id-1")
    ctx = reader._interventions_client.contexts[-1]
    assert (ctx.targeting_key, ctx.attributes) == ("p1", {"gameId": "g1"})
    reader._interventions_client = Client(error=True)
    assert reader._read_grant(SPECS["shield_seconds"], "g1", "p1") is _UNKNOWN


@pytest.mark.uc("UC-040", flow="A24", br=["BR-017"])
def test_read_levels_skips_low_battery_players_and_collects_ids():
    reader = InterventionFlagReader({"low": 5.0}.get)
    reader._interventions_client = Client({("player_sensitivity_factor", "p1"): 1.5})
    ids = {}
    reads = reader.read_levels(SPECS["player_sensitivity_factor"], "g1", ["p1", "low"], ids)
    assert reads == {"p1": 1.5}
    assert ids == {"p1": "id-1"}


@pytest.mark.uc("UC-040", flow="A24", br=["BR-017"])
def test_resolve_player_targets_reads_per_alive_serial_with_the_game_scope():
    game = FakeInterventionGame(serials=("p1", "p2", "low"))
    game.players["p2"].alive = False
    reader = InterventionFlagReader({"low": 5.0}.get)
    reader._interventions_client = Client({("shield_seconds", "p1"): 4})
    assert reader.resolve_player_targets("shield_seconds", 0, game, game_id="g1") == {"p1": 4.0}
    assert reader._interventions_client.contexts[0].attributes == {"gameId": "g1"}
    assert reader.resolve_player_targets("shield_seconds", 0, game, battery_gate=False) == {"p1": 4.0, "low": 0.0}
    reader._interventions_client = Client(raising=True)
    assert reader.resolve_player_targets("shield_seconds", 0, game) == {"p1": 0}  # one bad read keeps the default


def test_session_context_adds_game_id_only_for_a_real_id():
    assert InterventionFlagReader.session_context("").attributes == {}
    assert InterventionFlagReader.session_context("g1", targeting_key="p1").attributes == {"gameId": "g1"}


def test_the_manager_flag_clients_and_deferral_state_forward_to_their_units():
    mgr = InterventionManager(event_publisher=AsyncMock(), get_game=lambda: None)
    client = Client({"enabled": True, "interventions_allowed": "x"})
    mgr._agent_client = client
    assert mgr._reads._agent_client is client
    assert mgr._allowed_types() == {"x"}
    assert mgr._deferral_since is None and mgr._deferral_recheck is None
    mgr._gate.since = 1.0
    assert mgr._deferral_since == 1.0
