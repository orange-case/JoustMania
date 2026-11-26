"""UC-020 BR-010: the tie cue of every mode that can end in a tie.

A tie gets a white flash on every player's controller and the tie sound, after the
post-win hold and the tied players' death effect, instead of the silent rainbow wait.
Drives the real modes' ``_end_game_impl`` with a recording gameplay stream and sound.
"""

from unittest.mock import AsyncMock

import pytest

from lib.types import GameEvent, Sound
from proto import controller_manager_pb2, game_coordinator_pb2
from services.game_coordinator import game_clock
from services.game_coordinator.games.base import GameState
from services.game_coordinator.games.ffa import FFAGame
from services.game_coordinator.games.random_teams import RandomTeamsGame
from services.game_coordinator.games.teams import SimpleTeamsGame
from services.game_coordinator.games.traitor import TraitorGame
from services.game_coordinator.games.werewolf import WerewolfGame
from services.game_coordinator.tests.conftest import EventCollector, MockControllerManagerService

FLASH = controller_manager_pb2.GAME_EFFECT_FLASH
RAINBOW = controller_manager_pb2.GAME_EFFECT_WINNER_RAINBOW
SERIALS = [f"mock_controller_{i}" for i in range(4)]


class _Stream:
    def __init__(self, log: list):
        self.log = log

    async def write(self, message):
        if message.HasField("game_effect"):
            self.log.append(("effect", message.game_effect))


@pytest.fixture
def log(monkeypatch):
    """One ordered log of the end path's game-clock sleeps and effect writes."""
    entries: list = []
    dilated_sleep = game_clock.sleep

    async def record_sleep(seconds):
        entries.append(("sleep", seconds))
        await dilated_sleep(seconds)

    monkeypatch.setattr(game_clock, "sleep", record_sleep)
    return entries


async def _tied_game(game_cls, log: list):
    manager = MockControllerManagerService(num_controllers=4, death_schedule={}, max_duration=10.0)
    events = EventCollector()
    kwargs = {}
    if game_cls in (SimpleTeamsGame, RandomTeamsGame):
        kwargs = {"num_teams": 2, "initial_players": [game_coordinator_pb2.Player(serial=s) for s in SERIALS]}
    game = game_cls(
        controller_manager_client=manager,
        event_publisher=events.publish,
        audio_client=None,
        game_id="tie_cue",
        **kwargs,
    )
    await game._initialize_players_impl(manager.controllers)
    game.gameplay_stream = _Stream(log)
    game.start_time = game_clock.now()
    game.state = GameState.RUNNING
    game.running = True
    game._play_sound = AsyncMock()
    game._wait_for_rainbow_effect = AsyncMock(return_value=True)
    for player in game.players.values():
        player.alive = False
    assert await game._check_win_condition()
    assert events.get_events_of_type(GameEvent.GAME_TIE) == [{}]
    log.clear()
    return game, events


def _effects(log: list, effect: int) -> list:
    return [e for kind, e in log if kind == "effect" and e.effect == effect]


def _sounds(game) -> list:
    return [c.args[0] for c in game._play_sound.call_args_list]


def _assert_tie_cue(game, log: list) -> None:
    flashes = _effects(log, FLASH)
    assert sorted(f.serial for f in flashes) == SERIALS  # every player, the dead included
    for flash in flashes:
        assert (flash.color.r, flash.color.g, flash.color.b) == (255, 255, 255)
        assert flash.duration_ms == 2000
        assert flash.speed == 4
    assert _effects(log, RAINBOW) == []
    assert Sound.VOX_CONGRATULATIONS not in _sounds(game)

    # 0.3 s after the post-win hold (the death effect has ended), then the 2 s of the flash.
    first_flash = next(i for i, (kind, e) in enumerate(log) if kind == "effect" and e.effect == FLASH)
    before = sum(s for kind, s in log[:first_flash] if kind == "sleep")
    after = sum(s for kind, s in log[first_flash:] if kind == "sleep")
    assert before == pytest.approx(0.3, abs=0.01)
    assert after == pytest.approx(2.0, abs=0.01)
    game._wait_for_rainbow_effect.assert_not_called()


@pytest.mark.asyncio
@pytest.mark.uc("UC-020", flow="A1", br=["BR-010"])
async def test_ffa_tie_flashes_every_player_white_and_plays_the_tie_sound(log):
    game, events = await _tied_game(FFAGame, log)

    await game._end_game_impl()

    _assert_tie_cue(game, log)
    assert _sounds(game) == [Sound.SFX_DEATH]
    (ended,) = events.get_events_of_type(GameEvent.GAME_ENDED)
    assert ended["outcome"] == "tie"


@pytest.mark.asyncio
@pytest.mark.uc("UC-021", flow="A2")
@pytest.mark.uc("UC-020", br=["BR-010"])
@pytest.mark.parametrize("game_cls", [SimpleTeamsGame, RandomTeamsGame], ids=["Teams", "RandomTeams"])
async def test_team_tie_flashes_every_player_white_and_plays_the_tie_sound(log, game_cls):
    game, events = await _tied_game(game_cls, log)

    await game._end_game_impl()

    _assert_tie_cue(game, log)
    assert _sounds(game) == [Sound.SFX_DEATH]
    (ended,) = events.get_events_of_type(GameEvent.GAME_ENDED)
    assert ended["winning_team"] == "none"


@pytest.mark.asyncio
@pytest.mark.uc("UC-022", flow="A2")
@pytest.mark.uc("UC-020", br=["BR-010"])
async def test_traitor_tie_flashes_every_player_white_and_plays_the_tie_sound(log):
    game, events = await _tied_game(TraitorGame, log)

    await game._end_game_impl()

    _assert_tie_cue(game, log)
    assert _sounds(game) == [Sound.SFX_DEATH]
    (ended,) = events.get_events_of_type(GameEvent.GAME_ENDED)
    assert ended["winning_team"] == "none"


@pytest.mark.asyncio
@pytest.mark.uc("UC-022", flow="A2")
@pytest.mark.uc("UC-020", br=["BR-010"])
async def test_werewolf_tie_flashes_every_player_white_and_keeps_wolfdown(log):
    game, events = await _tied_game(WerewolfGame, log)

    await game._end_game_impl()

    _assert_tie_cue(game, log)
    assert _sounds(game) == [Sound.SFX_WOLFDOWN]
    (ended,) = events.get_events_of_type(GameEvent.GAME_ENDED)
    assert ended["winner"] == "none"


@pytest.mark.asyncio
@pytest.mark.uc("UC-020", flow="A1", br=["BR-007", "BR-010"])
async def test_force_end_after_the_tie_cuts_the_waits_but_keeps_the_cue(log):
    game, events = await _tied_game(FFAGame, log)
    game.force_end("operator")

    await game._end_game_impl()

    assert sorted(f.serial for f in _effects(log, FLASH)) == SERIALS
    assert _sounds(game) == [Sound.SFX_DEATH]
    assert [s for kind, s in log if kind == "sleep"] == []
    (ended,) = events.get_events_of_type(GameEvent.GAME_ENDED)
    assert ended["outcome"] == "tie"


@pytest.mark.asyncio
@pytest.mark.uc("UC-020", flow="main", steps=[6], br=["BR-010"])
async def test_a_winner_gets_no_tie_cue(log):
    manager = MockControllerManagerService(num_controllers=2, death_schedule={}, max_duration=10.0)
    game = FFAGame(
        controller_manager_client=manager,
        event_publisher=EventCollector().publish,
        audio_client=None,
        game_id="tie_cue_winner",
    )
    await game._initialize_players_impl(manager.controllers)
    game.gameplay_stream = _Stream(log)
    game.state = GameState.RUNNING
    game.running = True
    game._play_sound = AsyncMock()
    game._wait_for_rainbow_effect = AsyncMock(return_value=True)
    game.players[manager.controllers[0].serial].alive = False
    assert await game._check_win_condition()

    await game._end_game_impl()

    assert _effects(log, FLASH) == []
    assert Sound.SFX_DEATH not in _sounds(game)
    assert [e.serial for e in _effects(log, RAINBOW)] == [manager.controllers[1].serial]
