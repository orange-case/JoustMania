"""UC-101 main 3-5 on the service tier (T2): the written intervention id reaches the intervention moment (#2665).

``GAME`` composition with telemetry exported to the OtlpSink and
``telemetry.moment_shape`` served as ``span`` (the baseline's ``span_event`` variant is
rewritten, so every game of this stack records its moments as spans, exported at once):
the real flag service, flagd, controller manager (mock) and game coordinator from source
at HEAD. No agent runs: the test sends the agent's flag-service write itself (as TC-018),
``shield_seconds`` for one serial with a ``decision_id``.

This crosses the hop the unit tests cannot: the flag service serves the id as
``decisionId.<variant>`` metadata of the player's rung, flagd's in-process resolver hands
it to the coordinator with the resolved variant, and the coordinator puts it on the
``game.intervention`` span of that player (BR-003, BR-004). Without the coordinator
slice no ``game.intervention`` span exists.
"""

import asyncio
import dataclasses

import pytest
from openfeature.evaluation_context import EvaluationContext

from tests.integration.flag_service import set_player_value, variant_value
from tests.integration.helpers import (
    build_start_config,
    flagd_probe_client,
    force_end_game_by_id,
    get_game_client,
    get_mock_client,
    poll_until,
    setup_mock_controllers,
    start_game_headless,
    wait_until_running,
)
from tests.service.harness import GAME

pytestmark = pytest.mark.tier("service")

SERVICE_COMPOSITION = dataclasses.replace(
    GAME,
    otlp=True,
    flag_variant_values={("observability", "telemetry.moment_shape", "span_event"): "span"},
)

TAG = "intervention-moment"
COORDINATOR = "game-coordinator-service"
DECISION_ID = "0f1e2d3c4b5a6978"  # the agent's ids are 16 hex characters
TELEMETRY_TIMEOUT_SECONDS = 30.0


def _allow_grant_shield(stack, flag_service) -> None:
    flag_service.put("agent", "interventions_allowed", variant="standard")
    allowed = variant_value("agent", "interventions_allowed", "standard")
    with flagd_probe_client(stack, "agent") as client:
        assert poll_until(
            lambda: client.get_string_value("interventions_allowed", "", EvaluationContext()) == allowed
        ), "flagd did not serve the standard allow-list"


@pytest.mark.asyncio
@pytest.mark.uc("UC-101", flow="main", steps=[3, 4, 5], br=["BR-002", "BR-003", "BR-004"])
async def test_the_written_intervention_id_lands_on_the_players_intervention_moment(
    stack, flag_service, headless_cleanup
):
    sink = stack.otlp_sink
    assert sink is not None
    _allow_grant_shield(stack, flag_service)
    mock_client, mock_channel = await get_mock_client(stack)
    game_client, game_channel = await get_game_client(stack)
    headless_cleanup.add_tag(TAG)
    collector = None
    game_id = ""
    try:
        serials = await setup_mock_controllers(stack, count=3, reserved=True, tag=TAG)
        target = serials[0]
        game_id, collector = await start_game_headless(
            game_client, build_start_config("JoustFFA", serials), timeout=25.0
        )
        headless_cleanup.add_game(game_id)
        await wait_until_running(game_client, {game_id}, timeout=15.0)
        flag_service.wait_game_registered(game_id)

        set_player_value("shield_seconds", game_id, target, 5, decision_id=DECISION_ID)

        moment = await sink.wait_span_async(
            "game.intervention",
            {"intervention.id": DECISION_ID},
            service=COORDINATOR,
            timeout=TELEMETRY_TIMEOUT_SECONDS,
        )
        assert moment.attributes["intervention.type"] == "grant_shield"
        assert moment.attributes["player.serial"] == target
        assert moment.attributes["game.id"] == game_id
        assert moment.attributes["moment.id"].startswith(f"{game_id}/{target}/")
        # The other players got no intervention.
        others = [
            s for s in sink.spans("game.intervention", service=COORDINATOR) if s.attributes.get("game.id") == game_id
        ]
        assert [s.attributes["player.serial"] for s in others] == [target]
    finally:
        if collector is not None:
            await collector.stop()
        if game_id:
            await force_end_game_by_id(game_client, game_id, reason="uc101_cleanup")
        await mock_channel.close()
        await game_channel.close()
        await asyncio.sleep(0)
