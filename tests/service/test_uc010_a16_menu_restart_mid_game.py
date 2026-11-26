"""UC-010 A16 on the service tier (T2): the menu restarts while a game it started runs.

Real controller manager, game coordinator and menu from source (LOBBY), on a
stack of its own (the menu process is restarted). Two mock controllers ready up
in the lobby and start an FFA game; the test then restarts the menu process, as
Docker does after a crash or the watchdog's exit 70:

- steps 1 and 4: the restarted menu follows the running game (GAME_STARTING,
  lifecycle live), paints no lobby colour over the players' game colours and
  refuses a start (``game_active``); the game keeps running;
- step 5: the game's end brings the lobby back (dim FFA lobby colour, RUNNING).
"""

import asyncio

import pytest

from lib.colors import Colors
from proto import menu_pb2
from tests.integration.helpers import (
    GameEventCollector,
    async_poll_until,
    force_end_game_by_id,
    get_controller_color,
    get_game_client,
    get_menu_client,
    get_mock_client,
    list_games,
    wait_for_lobby_colors,
)
from tests.service.harness import LOBBY
from tests.service.lobby_flow import ready_up_until_started, start_lobby, wait_running

pytestmark = pytest.mark.tier("service")

SERVICE_COMPOSITION = LOBBY

# The menu lights CONNECTED controllers in the selected mode's colour at 30 % (menu/utils/led.py).
FFA_LOBBY_COLOR = tuple(int(c * 0.3) for c in Colors.Orange.value)
# What the boot logs instead of "Menu auto-started" when it follows a live game.
FOLLOW_MARKER = "Boot follows a live game"
# How long the test watches the LEDs after the restart for a lobby repaint.
NO_REPAINT_WATCH_SECONDS = 4.0
LOBBY_TIMEOUT_SECONDS = 15.0


def _near(color, expected, tolerance=2) -> bool:
    return all(abs(a - b) <= tolerance for a, b in zip(color, expected, strict=True))


@pytest.mark.uc("UC-010", flow="A16", steps=[1, 4, 5, 6])
async def test_menu_restart_mid_game_follows_the_game(fresh_stack, headless_cleanup):
    stack = fresh_stack
    serials = await start_lobby(stack, count=2)
    game_client, game_channel = await get_game_client(stack)
    mock_client, mock_channel = await get_mock_client(stack)
    collector = GameEventCollector(game_client)
    menu_channel = None
    try:
        await collector.start()
        game_id = headless_cleanup.add_game(await ready_up_until_started(stack, serials, collector, set()))
        await wait_running(game_client, game_id)

        menu = stack.services["menu"]
        menu.ready_log_marker = FOLLOW_MARKER
        await asyncio.to_thread(menu.restart)

        menu_client, menu_channel = await get_menu_client(stack)
        status = await menu_client.GetMenuStatus(menu_pb2.GetMenuStatusRequest())
        assert status.menu_state == menu_pb2.MenuState.GAME_STARTING, status
        assert status.lifecycle_live, status

        # Step 4: no lobby colour is painted over the running game.
        loop = asyncio.get_running_loop()
        deadline = loop.time() + NO_REPAINT_WATCH_SECONDS
        while loop.time() < deadline:
            colors = {s: await get_controller_color(mock_client, s) for s in serials}
            repainted = [s for s, c in colors.items() if _near(c, FFA_LOBBY_COLOR)]
            assert not repainted, f"lobby colour painted over the game: {colors}"
            await asyncio.sleep(0.5)

        # Step 1: a start during the followed game is refused; the game is untouched.
        start = await menu_client.ProcessInput(
            menu_pb2.ProcessInputRequest(input_type="web_command", data={"command": "start_game"})
        )
        assert not start.success
        assert start.error == "game_active", start.error
        assert {g.game_id for g in await list_games(game_client)} == {game_id}

        # Step 5: the game's end brings the lobby back.
        assert await force_end_game_by_id(game_client, game_id, reason="uc010_a16")
        await wait_for_lobby_colors(
            mock_client, serials, expected_color=FFA_LOBBY_COLOR, tolerance=2, timeout=LOBBY_TIMEOUT_SECONDS
        )

        async def lobby_running() -> bool:
            now = await menu_client.GetMenuStatus(menu_pb2.GetMenuStatusRequest())
            return now.menu_state == menu_pb2.MenuState.RUNNING and not now.lifecycle_live

        assert await async_poll_until(lobby_running, timeout=LOBBY_TIMEOUT_SECONDS), "menu never back in the lobby"
    finally:
        await collector.stop()
        for channel in (menu_channel, mock_channel, game_channel):
            if channel is not None:
                await channel.close()
