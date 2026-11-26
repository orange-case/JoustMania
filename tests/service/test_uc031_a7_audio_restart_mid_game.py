"""UC-031 A7 on the service tier (T2): the audio service restarts during an FFA game.

Real controller manager, game coordinator and menu from source, three mock
controllers readied through the menu. ``FakeAudio.go_down()`` stops the audio
server mid-game (open connections drop, as at a container stop); a kill while it
is down still registers, so the game goes on. ``come_back()`` serves again on the
same port with no track playing, as the restarted container does (A6, the loop
watchdog's exit 70). The coordinator notices the reconnect and sends ``PlayMusic``
again at the tempo of the intensity now (BR-009).
"""

import asyncio

import pytest

from tests.integration.helpers import (
    GameEventCollector,
    async_poll_until,
    get_game_client,
    get_mock_client,
    kill_player_verified,
)
from tests.service.harness import LOBBY
from tests.service.lobby_flow import ready_up_until_started, start_lobby, wait_running

pytestmark = pytest.mark.tier("service")

SERVICE_COMPOSITION = LOBBY

GAME_MUSIC = "Joust/music/*.ogg"
# Channel reconnect back-off (5 s at most) + probe interval (1 s) + PlayMusic, with CI slack.
MUSIC_BACK_SECONDS = 20.0
DOWN_SECONDS = 3.0


def _game_music_starts(stack) -> list[dict]:
    return [c.request for c in stack.fake_audio.calls_of("PlayMusic") if c.request.get("file_pattern") == GAME_MUSIC]


@pytest.mark.uc("UC-031", flow="A7", steps=[1, 2, 3], br=["BR-009"])
@pytest.mark.uc("UC-031", flow="A6", steps=[3])
async def test_uc031_a7_game_goes_on_and_music_comes_back_after_an_audio_restart(service_stack, headless_cleanup):
    stack = service_stack
    audio = stack.fake_audio
    serials = await start_lobby(stack, count=3)
    audio.clear()
    game_client, game_channel = await get_game_client(stack)
    mock_client, mock_channel = await get_mock_client(stack)
    collector = GameEventCollector(game_client)
    try:
        await collector.start()
        game_id = headless_cleanup.add_game(await ready_up_until_started(stack, serials, collector, set()))
        await wait_running(game_client, game_id)
        assert await async_poll_until(lambda: len(_game_music_starts(stack)) == 1, timeout=10.0), (
            f"no game music: {[c.request for c in audio.calls_of('PlayMusic')]}"
        )

        # The audio container stops mid-game: a kill still registers (A7 step 1).
        audio.go_down()
        assert await kill_player_verified(mock_client, game_client, serials[0], lambda p: not p.alive, game_id=game_id)
        await asyncio.sleep(DOWN_SECONDS)
        await wait_running(game_client, game_id)
        assert len(_game_music_starts(stack)) == 1

        # The restarted service plays no track: the coordinator starts the music again (A7 step 2).
        audio.come_back()
        assert audio.track_id is None
        assert await async_poll_until(
            lambda: len(_game_music_starts(stack)) >= 2, timeout=MUSIC_BACK_SECONDS, interval=0.5
        ), f"the game music never came back: {[c.method for c in audio.calls]}"
        restart = _game_music_starts(stack)[1]
        assert 1.0 <= restart.get("tempo", 1.0) <= 1.3 + 1e-6
        assert audio.track_id is not None
        await wait_running(game_client, game_id)

        # The game ends as usual: a second death leaves the winner.
        assert await kill_player_verified(mock_client, game_client, serials[1], lambda p: not p.alive, game_id=game_id)
        await collector.wait_for_event("game_ended", timeout=15.0)
    finally:
        if not audio.is_up:
            audio.come_back()  # later tests share the stack
        await collector.stop()
        await mock_channel.close()
        await game_channel.close()
