"""Unit tests for the effect-emission helpers on their own (#2634, intervention_dispatch.py)."""

from unittest.mock import patch

import pytest
from openfeature import api as openfeature_api
from openfeature.evaluation_context import EvaluationContext

from lib.types import GameEvent
from services.game_coordinator.intervention_dispatch import (
    _ids_for,
    _intervention_scope,
    count_dispatch_failure,
    record_outcome,
    run_in_game_flag_context,
)
from services.game_coordinator.intervention_sessions import SessionView
from services.game_coordinator.interventions import INTERVENTION_SPECS
from services.game_coordinator.tests.conftest import FakeInterventionGame

SPECS = {s.flag_key: s for s in INTERVENTION_SPECS}


class Bus:
    def __init__(self):
        self.events = []

    async def publish(self, event_type, data):
        self.events.append((event_type, data))


@pytest.mark.asyncio
@pytest.mark.uc("UC-040", br=["BR-004"])
async def test_record_outcome_counts_and_publishes_on_the_session_bus():
    bus = Bus()
    session = SessionView(game_id="g1", game=None, publish=bus.publish)
    with patch("services.game_coordinator.metrics.interventions_total") as metric:
        await record_outcome(
            SPECS["eliminate_player"], "p1", "balanced", session, blocked=True, block_reason="low_battery"
        )
    metric.labels.assert_called_once_with(
        type="eliminate_player", objective="balanced", blocked="true", block_reason="low_battery"
    )
    assert bus.events == [
        (
            GameEvent.AGENT_INTERVENTION,
            {"type": "eliminate_player", "target": "p1", "blocked": "true", "block_reason": "low_battery"},
        )
    ]


@pytest.mark.asyncio
async def test_a_failing_metric_never_stops_the_outcome_event():
    bus = Bus()
    session = SessionView(game_id="", game=None, publish=bus.publish)
    with patch("services.game_coordinator.metrics.interventions_total") as metric:
        metric.labels.side_effect = RuntimeError("metrics down")
        await record_outcome(SPECS["end_game"], None, "balanced", session, blocked=False, block_reason="")
    assert bus.events[0][1] == {"type": "end_game", "target": "", "blocked": "false", "block_reason": ""}


@pytest.mark.uc("UC-040", flow="A26", br=["BR-018"])
def test_count_dispatch_failure_labels_type_and_reason():
    with patch("services.game_coordinator.metrics.intervention_dispatch_failures_total") as metric:
        count_dispatch_failure(SPECS["volume_override"], "game_loop_unavailable")
    metric.labels.assert_called_once_with(type="adjust_volume", reason="game_loop_unavailable")


@pytest.mark.uc("UC-101", br=["BR-001"])
def test_only_player_levers_get_an_intervention_scope():
    assert _intervention_scope(SPECS["volume_override"], 0.4, None, None, None) is None
    target = _intervention_scope(SPECS["eliminate_player"], "p1", "p1", "id-1", None)
    assert target.take("p2") is None
    assert target.take("p1").id == "id-1"
    level = _intervention_scope(SPECS["player_sensitivity_factor"], {"p1": 1.5, "p2": 0.5}, None, None, {"p2": "id-2"})
    assert level.take("p3") is None
    assert level.take("p1") is not None
    assert level.take("p2").id == "id-2"


@pytest.mark.uc("UC-101", br=["BR-004"])
def test_ids_for_keeps_only_serials_with_an_id():
    assert _ids_for({"p1": "a", "p2": "", "p3": "c"}, ["p1", "p2", "p4"]) == {"p1": "a"}


@pytest.mark.asyncio
@pytest.mark.uc("UC-040", flow="A26", br=["BR-018"])
async def test_run_in_game_flag_context_sets_and_restores_the_game_context():
    game_ctx = EvaluationContext(attributes={"game_mode": "FFA"})
    game = FakeInterventionGame(flag_eval_context=game_ctx)
    before = openfeature_api.get_transaction_context()
    seen = []

    async def effect(value):
        seen.append((openfeature_api.get_transaction_context(), value))
        return "done"

    assert await run_in_game_flag_context(game, effect, 7) == "done"
    assert seen == [(game_ctx, 7)]
    assert openfeature_api.get_transaction_context() == before

    seen.clear()
    assert await run_in_game_flag_context(None, effect, 8) == "done"
    assert seen == [(before, 8)]
