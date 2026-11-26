"""A mass disconnect aborts the game with no winner (UC-002 BR-009); a single disconnect is unchanged (UC-020 A2)."""

import sys
import time
from pathlib import Path
from unittest.mock import AsyncMock, patch

import pytest

test_dir = Path(__file__).parent
project_root = test_dir.parent.parent.parent
sys.path.insert(0, str(project_root))
sys.path.insert(0, str(test_dir))

from conftest import EventCollector, MockControllerManagerService, MockGameplayStream
from test_disconnect import ScriptedGameplayStream

from lib.types import GameEvent, Sound
from proto import controller_manager_pb2
from services.game_coordinator import game_clock
from services.game_coordinator.games.base import Player
from services.game_coordinator.games.ffa import FFAGame
from services.game_coordinator.games.game_loop import MassDisconnectError

STILL = controller_manager_pb2.Vector3(x=0.0, y=0.0, z=1.0)


class _Clock:
    """Game time a test moves by hand."""

    def __init__(self):
        self.t = 1000.0

    def __call__(self) -> float:
        return self.t


@pytest.fixture
def clock(monkeypatch):
    c = _Clock()
    monkeypatch.setattr(game_clock, "now", c)
    return c


def _game(serials):
    events = EventCollector()
    game = FFAGame(
        controller_manager_client=MockControllerManagerService(num_controllers=len(serials)),
        event_publisher=events.publish,
        audio_client=None,
        game_id="test_mass_disconnect",
    )
    game.gameplay_stream = MockGameplayStream()
    game.running = True
    game.start_time = game_clock.now()
    for serial in serials:
        game.players[serial] = Player(serial=serial, team=0, alive=True, color=(255, 0, 0))
    return game, events


def _disconnect_frame(*serials):
    return controller_manager_pb2.GameplayDataUpdate(
        controllers=[],
        disconnects=[controller_manager_pb2.DisconnectEvent(serial=s) for s in serials],
        timestamp=int(time.time() * 1000),
    )


def _assert_no_result(events):
    assert events.get_events_of_type(GameEvent.PLAYER_DEATH) == []
    assert events.get_events_of_type(GameEvent.GAME_WINNER) == []
    assert events.get_events_of_type(GameEvent.GAME_TIE) == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-002", br=["BR-009"])
@pytest.mark.uc("UC-020", flow="A2")
@pytest.mark.usefixtures("clock")
async def test_every_player_disconnecting_in_one_frame_aborts_with_no_winner():
    """A rust-hid restart: all 3 players drop in one frame. Nobody is eliminated, nobody wins, "Cancelled." plays."""
    game, events = _game(["p0", "p1", "p2"])

    with (
        patch.object(game, "_play_sound", AsyncMock()) as play_sound,
        pytest.raises(MassDisconnectError, match=r"mass_disconnect: 3 of 3 players disconnected"),
    ):
        await game._process_gameplay_update(_disconnect_frame("p0", "p1", "p2"))

    assert all(p.alive for p in game.players.values())
    _assert_no_result(events)
    play_sound.assert_awaited_once_with(Sound.MENU_VOX_STATUS_CANCELLED, priority=3)


@pytest.mark.asyncio
@pytest.mark.uc("UC-002", br=["BR-009"])
@pytest.mark.parametrize("gap_s", [0.2, 1.0, 1.5], ids=["same-pass", "next-discovery-pass", "window-edge"])
async def test_majority_across_frames_within_the_window_aborts(clock, gap_s):
    """3 players: p0 drops (eliminated as before), p1 drops up to 1.5 s later: 2 of 3 in the window, abort."""
    game, events = _game(["p0", "p1", "p2"])

    with patch.object(game, "_play_sound", AsyncMock()):
        assert await game._process_gameplay_update(_disconnect_frame("p0")) is False
        assert game.players["p0"].alive is False
        clock.t += gap_s
        with pytest.raises(MassDisconnectError, match=r"2 of 3"):
            await game._process_gameplay_update(_disconnect_frame("p1"))

    assert game.players["p1"].alive is True
    assert game.players["p2"].alive is True
    assert events.get_events_of_type(GameEvent.GAME_WINNER) == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-002", br=["BR-009"])
@pytest.mark.uc("UC-020", flow="A2", br=["BR-001"])
@pytest.mark.parametrize("gap_s", [1.51, 3.0])
async def test_disconnects_further_apart_than_the_window_are_single_disconnects(clock, gap_s):
    """Two drops more than 1.5 s apart are two single disconnects: both eliminated, the last player wins."""
    game, events = _game(["p0", "p1", "p2"])

    assert await game._process_gameplay_update(_disconnect_frame("p0")) is False
    clock.t += gap_s
    assert await game._process_gameplay_update(_disconnect_frame("p1")) is True

    assert events.get_events_of_type(GameEvent.GAME_WINNER) == [{"serial": "p2"}]


@pytest.mark.asyncio
@pytest.mark.uc("UC-002", br=["BR-009"])
@pytest.mark.uc("UC-020", flow="A2", br=["BR-001"])
async def test_a_player_dead_by_motion_who_then_disconnects_is_not_a_drop(clock):
    """4 players: p0 drops, p1 dies by motion and switches off at +1.0 s, p2 drops at +1.2 s.

    Only 2 live players dropped (p0, p2): not more than half, so no abort; p2 is eliminated and p3 wins.
    """
    game, events = _game(["p0", "p1", "p2", "p3"])

    with patch.object(game, "_play_sound", AsyncMock()) as play_sound:
        assert await game._process_gameplay_update(_disconnect_frame("p0")) is False
        clock.t += 0.5
        await game._kill_player("p1", accel_mag=5.0)
        assert game.players["p1"].alive is False
        clock.t += 0.5
        assert await game._process_gameplay_update(_disconnect_frame("p1")) is False
        clock.t += 0.2
        assert await game._process_gameplay_update(_disconnect_frame("p2")) is True

    assert events.get_events_of_type(GameEvent.GAME_WINNER) == [{"serial": "p3"}]
    assert Sound.MENU_VOX_STATUS_CANCELLED not in [c.args[0] for c in play_sound.await_args_list]


@pytest.mark.asyncio
@pytest.mark.uc("UC-020", flow="A2")
@pytest.mark.uc("UC-002", br=["BR-009"])
@pytest.mark.usefixtures("clock")
async def test_single_disconnect_is_unchanged():
    """One drop in a 3-player game eliminates that player through the death path; the game goes on."""
    game, events = _game(["p0", "p1", "p2"])

    with patch.object(game, "_play_sound", AsyncMock()) as play_sound:
        assert await game._process_gameplay_update(_disconnect_frame("p1")) is False

    assert game.players["p1"].alive is False
    assert [e["serial"] for e in events.get_events_of_type(GameEvent.PLAYER_DEATH)] == ["p1"]
    assert Sound.MENU_VOX_STATUS_CANCELLED not in [c.args[0] for c in play_sound.await_args_list]


@pytest.mark.asyncio
@pytest.mark.uc("UC-002", br=["BR-009"])
@pytest.mark.uc("UC-020", flow="A2", br=["BR-001"])
@pytest.mark.usefixtures("clock")
async def test_two_player_single_disconnect_still_has_a_winner():
    """A 2-player game: one drop is a single disconnect, the other player wins as before."""
    game, events = _game(["p0", "p1"])

    assert await game._process_gameplay_update(_disconnect_frame("p0")) is True

    assert events.get_events_of_type(GameEvent.GAME_WINNER) == [{"serial": "p1"}]


class TestOneAdapter:
    """Adapters are learnt from ``GameplayData.bt_adapter`` during the EMA warm-up."""

    ADAPTERS = {"p0": "hci0", "p1": "hci0", "p2": "hci1", "p3": "hci1", "p4": "hci1"}

    def _warmup_frames(self, adapters, count=20):
        return [
            controller_manager_pb2.GameplayDataUpdate(
                controllers=[
                    controller_manager_pb2.GameplayData(serial=s, accel=STILL, bt_adapter=a)
                    for s, a in adapters.items()
                ],
                timestamp=int(time.time() * 1000),
            )
            for _ in range(count)
        ]

    async def _warmed_game(self, adapters):
        game, events = _game(list(adapters))
        game.gameplay_stream = ScriptedGameplayStream(self._warmup_frames(adapters))
        await game._warmup_ema()
        return game, events

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-002", br=["BR-009"])
    async def test_every_player_on_one_adapter_aborts(self):
        """A lost dongle: both hci0 players drop (2 of 5, not a majority). Abort; hci1's players do not win."""
        game, events = await self._warmed_game(self.ADAPTERS)

        with (
            patch.object(game, "_play_sound", AsyncMock()),
            pytest.raises(MassDisconnectError, match=r"2 of 5 players disconnected on adapter hci0"),
        ):
            await game._process_gameplay_update(_disconnect_frame("p0", "p1"))

        assert all(p.alive for p in game.players.values())
        _assert_no_result(events)

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-002", br=["BR-009"])
    @pytest.mark.uc("UC-020", flow="A2")
    async def test_part_of_an_adapter_is_not_an_adapter_loss(self):
        """Two of hci1's three players drop: neither a majority (2 of 5) nor a whole adapter. Both are eliminated."""
        game, events = await self._warmed_game(self.ADAPTERS)

        assert await game._process_gameplay_update(_disconnect_frame("p2", "p3")) is False

        assert [e["serial"] for e in events.get_events_of_type(GameEvent.PLAYER_DEATH)] == ["p2", "p3"]

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-002", br=["BR-009"])
    @pytest.mark.uc("UC-020", flow="A2")
    async def test_exactly_half_on_different_adapters_is_not_a_mass_disconnect(self):
        """2 of 4 drop, one per adapter: half is not more than half, and no adapter is emptied. Both are eliminated."""
        game, events = await self._warmed_game({"p0": "hci0", "p1": "hci0", "p2": "hci1", "p3": "hci1"})

        assert await game._process_gameplay_update(_disconnect_frame("p0", "p2")) is False

        assert [e["serial"] for e in events.get_events_of_type(GameEvent.PLAYER_DEATH)] == ["p0", "p2"]
        assert game.players["p1"].alive is True
        assert game.players["p3"].alive is True

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-002", br=["BR-009"])
    @pytest.mark.uc("UC-020", flow="A2")
    async def test_lone_player_on_an_adapter_is_a_single_disconnect(self):
        """An adapter with one player: its drop is a single disconnect, never an adapter loss."""
        game, events = await self._warmed_game({"p0": "hci0", "p1": "hci1", "p2": "hci1"})

        assert await game._process_gameplay_update(_disconnect_frame("p0")) is False

        assert game.players["p0"].alive is False


@pytest.mark.asyncio
@pytest.mark.uc("UC-002", br=["BR-009"])
@pytest.mark.uc("UC-020", flow="A2")
async def test_mass_disconnect_buffered_in_the_warmup_aborts_before_the_first_frame():
    """Disconnects buffered in the countdown and warm-up count as one batch: 2 of 3 abort the loop at its start."""
    serials = ["p0", "p1", "p2"]
    game, events = _game(serials)
    frames = [
        controller_manager_pb2.GameplayDataUpdate(
            controllers=[controller_manager_pb2.GameplayData(serial="p2", accel=STILL)],
            disconnects=[controller_manager_pb2.DisconnectEvent(serial=s) for s in ("p0", "p1")] if i == 0 else [],
            timestamp=int(time.time() * 1000),
        )
        for i in range(20)
    ]
    game.gameplay_stream = ScriptedGameplayStream(frames)
    game.start_time = None

    with patch.object(game, "_play_sound", AsyncMock()):
        await game._warmup_ema()
        with pytest.raises(MassDisconnectError, match=r"2 of 3"):
            await game._game_loop()

    assert all(p.alive for p in game.players.values())
    _assert_no_result(events)
    assert game.start_time is None
