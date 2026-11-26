"""UC-030 BR-006 on the service tier (T2): the controller manager restarts during an FFA game.

Real controller manager, game coordinator and menu from source (LOBBY, a fresh
stack: the test stops the controller manager). Three mock controllers ready up
and the game runs; the controller manager is then stopped (SIGTERM, as
``docker restart`` sends) and kept down past the game's reconnect budget. The
game ends with ``game_error`` (``gameplay_stream_lost``) and no winner, and plays
"Cancelled.". The controller manager is started again (the restart policy's part)
and serves.

Its shutdown ends an open gameplay stream with ``UNAVAILABLE`` at once, not after
the 5 s gRPC grace, and sends no frame after the SIGTERM.
"""

import asyncio
import dataclasses
import time

import grpc
import grpc.aio
import pytest

from proto import controller_manager_pb2, controller_manager_pb2_grpc
from tests.integration.helpers import GameEventCollector, get_game_client
from tests.service.harness import LOBBY
from tests.service.harness.processes import grpc_health_ready
from tests.service.lobby_flow import ready_up_until_started, start_lobby, wait_running

pytestmark = pytest.mark.tier("service")

SERVICE_COMPOSITION = dataclasses.replace(LOBBY, otlp=True)

# The controller manager's shutdown ends the stream at once, then the game's three
# reconnects take about 6 s; room for a slow runner.
GAME_ERROR_WITHIN_SECONDS = 40.0
# Shutdown start to the stream's UNAVAILABLE: well under the 5 s grace it used to wait out.
STREAM_END_WITHIN_SECONDS = 2.0


def _events(collector: GameEventCollector, game_id: str, event_type: str) -> list:
    return [e for e in collector.get_events(event_type) if e.game_id == game_id]


@pytest.mark.uc("UC-030", br=["BR-006"])
@pytest.mark.uc("UC-002", br=["BR-009"])
async def test_controller_manager_down_mid_game_ends_it_with_no_winner(fresh_stack):
    stack = fresh_stack
    controller_manager = stack.services["controller-manager"]
    serials = await start_lobby(stack, count=3)
    game_client, game_channel = await get_game_client(stack)
    collector = GameEventCollector(game_client)
    try:
        await collector.start()
        game_id = await ready_up_until_started(stack, serials, collector, set())
        await wait_running(game_client, game_id)
        stack.fake_audio.clear()

        await asyncio.to_thread(controller_manager.stop)

        await collector.wait_for_event("game_error", timeout=GAME_ERROR_WITHIN_SECONDS)
        errors = _events(collector, game_id, "game_error")
        assert errors, f"no game_error for {game_id}"
        assert "gameplay_stream_lost" in errors[0].data.get("error", ""), errors[0].data
        assert not _events(collector, game_id, "game_winner"), "a lost stream declared a winner"
        assert any("status_cancelled" in str(c.request) for c in stack.fake_audio.calls_of("PlaySound")), (
            f"no 'Cancelled.': {[c.request for c in stack.fake_audio.calls_of('PlaySound')]}"
        )

        await asyncio.to_thread(controller_manager.start)
        assert grpc_health_ready(controller_manager.grpc_port)()
    finally:
        await collector.stop()
        await game_channel.close()


async def _observer_requests():
    yield controller_manager_pb2.GameplayStreamControl(
        config=controller_manager_pb2.GameplayStreamConfig(update_frequency_hz=60)
    )
    await asyncio.Event().wait()


@pytest.mark.uc("UC-030", br=["BR-006"])
async def test_controller_manager_shutdown_ends_gameplay_stream_unavailable_at_once(fresh_stack):
    controller_manager = fresh_stack.services["controller-manager"]
    channel = grpc.aio.insecure_channel(f"localhost:{controller_manager.grpc_port}")
    try:
        call = controller_manager_pb2_grpc.ControllerManagerServiceStub(channel).StreamGameplayData(
            _observer_requests()
        )
        await call.read()

        stop = asyncio.create_task(asyncio.to_thread(controller_manager.stop))
        sigterm_at = time.monotonic()
        last_frame_at = None
        with pytest.raises(grpc.aio.AioRpcError) as excinfo:
            while True:
                await call.read()
                last_frame_at = time.monotonic()
        ended_after = time.monotonic() - sigterm_at
        await stop

        assert excinfo.value.code() == grpc.StatusCode.UNAVAILABLE, excinfo.value
        assert ended_after < STREAM_END_WITHIN_SECONDS, f"stream ended {ended_after:.2f} s after SIGTERM"
        # A frame or two may be in flight when the signal lands; none after the shutdown has run a while.
        assert last_frame_at is None or last_frame_at - sigterm_at < 0.5, "frames kept coming after SIGTERM"
    finally:
        await channel.close()
