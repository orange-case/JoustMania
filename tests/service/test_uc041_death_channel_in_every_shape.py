"""UC-041 BR-008, BR-009 / UC-100 BR-007 on the service tier (T2): one agent death channel in every shape.

AGENT composition with ``telemetry.moment_shape`` served as ``log_event`` (the
baseline's ``span_event`` variant is rewritten, so every game of this stack reads
``log_event``): the real game coordinator and controller manager from source at HEAD,
the Go agent built at HEAD and the OtlpSink, which tees to the agent only what the
collector's ``logs/agent`` pipeline passes (the agent's own lifecycle log events).

An FFA death in the ``log_event`` shape goes to the backends as a ``player_death`` log
event (UC-100 step 4), and the coordinator still sets
``game_player_elimination_order{serial,game_id}``, the one channel the agent reads deaths
from whatever the shape (UC-041 BR-008, BR-009). The moment log event is not teed to the
agent.
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

TAG = "death-channel"
COORDINATOR = "game-coordinator-service"
EVENT_TIMEOUT_SECONDS = 15.0
# SDK log batch (up to 1 s), the coordinator's metric export (1 s), CI slack.
TELEMETRY_TIMEOUT_SECONDS = 30.0


@pytest.mark.uc("UC-100", flow="main", steps=[3, 4], br=["BR-001", "BR-003", "BR-007"])
@pytest.mark.uc("UC-041", br=["BR-008", "BR-009"])
async def test_a_death_in_the_log_event_shape_reaches_the_agent_through_the_elimination_gauge(stack, headless_cleanup):
    sink = stack.otlp_sink
    assert sink is not None

    serials = await setup_mock_controllers(stack, count=3, reserved=True, tag=headless_cleanup.add_tag(TAG))
    game_client, game_channel = await get_game_client(stack)
    mock_client, mock_channel = await get_mock_client(stack)
    collector = None
    try:
        game_id, collector = await start_game_headless(game_client, build_start_config("JoustFFA", serials))
        headless_cleanup.add_game(game_id)
        assert game_id in await wait_until_running(game_client, {game_id}, timeout=20.0)

        victim = serials[0]
        assert await kill_player_verified(mock_client, game_client, victim, lambda p: not p.alive, game_id=game_id), (
            "the game stopped before the kill registered"
        )

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
        assert "orangecase.audience" not in death.attributes, "a moment is never marked for the agent"

        order = await sink.wait_metric_async(
            "game_player_elimination_order",
            {"game_id": game_id, "serial": victim},
            service=COORDINATOR,
            timeout=TELEMETRY_TIMEOUT_SECONDS,
        )
        assert order.value == 1

        await game_client.ForceEndGame(
            game_coordinator_pb2.ForceEndGameRequest(reason="death_channel_test", game_id=game_id)
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
        {"game.id": game_id, "game.name": "JoustFFA"},
        service=COORDINATOR,
        root=True,
        timeout=TELEMETRY_TIMEOUT_SECONDS,
    )
    assert root.attributes["telemetry.moment_shape"] == "log_event", "the shape was not in effect"
