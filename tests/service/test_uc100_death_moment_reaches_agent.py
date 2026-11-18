"""UC-100 BR-007 / UC-041 A7 on the service tier (T2): a death sent as a log event reaches the agent (#2658 slice 3).

AGENT composition with ``telemetry.moment_shape`` served as ``log_event`` (the
baseline's ``span_event`` variant is rewritten, so every game of this stack reads
``log_event``): the real game coordinator and controller manager from source at HEAD,
the Go agent built at HEAD and the OtlpSink, which tees the moment log events to the
agent as the collector's ``logs/agent`` pipeline does.

A NonStop death keeps the player in the game (``death.eliminated`` false) and its
player span stays open until game end, so while the game runs the only way the agent
can have read the death is its log event: the agent's
``agent_moments_received_total{moment=player_death,shape=log_event,outcome=counted}``
proves it. Without the slice the coordinator sends a span event, the sink forwards
no log and the counter does not exist.
"""

import dataclasses

import pytest

from proto import game_coordinator_pb2
from tests.integration.helpers import (
    build_start_config,
    get_game_client,
    get_mock_client,
    kill_player_verified,
    setup_mock_controllers,
    start_game_headless,
    wait_until_running,
)
from tests.service.harness import AGENT

pytestmark = pytest.mark.tier("service")

SERVICE_COMPOSITION = dataclasses.replace(
    AGENT,
    flag_variant_values={("observability", "telemetry.moment_shape", "span_event"): "log_event"},
)

TAG = "death-moment"
COORDINATOR = "game-coordinator-service"
AGENT_SERVICE = "agent"
EVENT_TIMEOUT_SECONDS = 15.0
# SDK log batch (up to 1 s), the agent's metric export (1 s), CI slack.
TELEMETRY_TIMEOUT_SECONDS = 30.0


@pytest.mark.uc("UC-100", flow="main", steps=[3, 4, 5], br=["BR-001", "BR-003", "BR-007"])
@pytest.mark.uc("UC-041", flow="A7", steps=[1, 2], br=["BR-009"])
async def test_a_nonstop_death_as_a_log_event_reaches_the_agent(stack, headless_cleanup):
    sink = stack.otlp_sink
    assert sink is not None

    serials = await setup_mock_controllers(stack, count=3, reserved=True, tag=headless_cleanup.add_tag(TAG))
    game_client, game_channel = await get_game_client(stack)
    mock_client, mock_channel = await get_mock_client(stack)
    collector = None
    try:
        game_id, collector = await start_game_headless(game_client, build_start_config("NonStop", serials))
        headless_cleanup.add_game(game_id)
        assert game_id in await wait_until_running(game_client, {game_id}, timeout=20.0)

        victim = serials[0]
        assert await kill_player_verified(
            mock_client, game_client, victim, lambda p: not p.alive, game_id=game_id
        ), "the game stopped before the kill registered"

        def the_death(record) -> bool:
            return str(record.attributes.get("moment.id", "")).startswith(f"{game_id}/{victim}/")

        try:
            death = await sink.wait_log_async(
                service=COORDINATOR,
                event_name="player_death",
                attrs={"game.id": game_id, "player.serial": victim},
                predicate=the_death,
                timeout=TELEMETRY_TIMEOUT_SECONDS,
            )
        except TimeoutError as exc:
            raise AssertionError(
                f"no player_death log event: the shape log_event was not in effect for {game_id} "
                "(the root span's telemetry.moment_shape says which shape was)"
            ) from exc
        assert death.attributes["death.eliminated"] is False
        assert death.attributes["game.kind"] == "shadow"
        assert death.trace_id, "the log event carries the player span's trace context"
        # The player span of a NonStop game stays open until game end: no span event could
        # have brought this death to the agent yet.
        assert not sink.spans("player_lifecycle", {"game.id": game_id, "player.serial": victim}), (
            "the player span ended before game end"
        )

        counted = await sink.wait_metric_async(
            "agent_moments_received_total",
            {"moment": "player_death", "shape": "log_event", "outcome": "counted"},
            service=AGENT_SERVICE,
            timeout=TELEMETRY_TIMEOUT_SECONDS,
        )
        assert counted.value >= 1

        await game_client.ForceEndGame(
            game_coordinator_pb2.ForceEndGameRequest(reason="death_moment_test", game_id=game_id)
        )
        await collector.wait_for_event("game_force_ended", timeout=EVENT_TIMEOUT_SECONDS)
    finally:
        if collector is not None:
            await collector.stop()
        await mock_channel.close()
        await game_channel.close()

    root = await sink.wait_span_async(
        # The game root: the only span with game.name, game.id and game.kind together.
        None,
        {"game.id": game_id, "game.kind": "shadow", "game.name": "NonStop"},
        service=COORDINATOR,
        root=True,
        timeout=TELEMETRY_TIMEOUT_SECONDS,
    )
    assert root.attributes["telemetry.moment_shape"] == "log_event", "the shape was not in effect"
