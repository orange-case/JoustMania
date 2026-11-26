"""NonStop's own gameplay stream is recovered like the base loop's (UC-026 BR-003, UC-030 BR-006).

NonStop opens its own gameplay stream in its game loop and reads it through the
base loop's reader: a stream that fails or goes silent is reopened within the
reconnect budget, and the game ends with one ``game_error``, no winner and
"Cancelled." once the budget is spent. A server-side OK end is pinned in
``test_nonstop.py`` (outcome ``completed``, no winner).
"""

import asyncio
import sys
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import grpc
import pytest

test_dir = Path(__file__).parent
sys.path.insert(0, str(test_dir.parent.parent.parent))
sys.path.insert(0, str(test_dir))

from conftest import MockControllerManagerService  # noqa: E402
from test_nonstop import (  # noqa: E402
    _assert_no_winner,
    _ClosableQueueStream,
    _lead_after_init,
    _nonstop_run_game,
    _NonstopControllerManager,
    _record_sounds,
    _stop_producers,
    fast_nonstop,  # noqa: F401 - fixture
)
from test_random_teams_formation import RecordingLog, _players  # noqa: E402

from lib.types import GameEvent, Sound  # noqa: E402
from services.game_coordinator import game_clock  # noqa: E402
from services.game_coordinator.games import nonstop_joust as nonstop_mod  # noqa: E402
from services.game_coordinator.games.base import GameState  # noqa: E402
from services.game_coordinator.games.ffa import FFAGame  # noqa: E402
from services.game_coordinator.games.game_loop import _LoopState  # noqa: E402
from services.game_coordinator.games.gameplay_stream import GameplayStreamLostError  # noqa: E402
from services.game_coordinator.games.nonstop_joust import NonstopJoustGame  # noqa: E402

FRAME_GAP_S = 0.1


class _BreakingStream(_ClosableQueueStream):
    """NonStop's game stream: breaks with UNAVAILABLE after a few frames."""

    def __init__(self, log, frames: int = 5):
        super().__init__(log)
        self.frames = frames

    async def __anext__(self):
        if self.read >= self.frames:
            raise grpc.aio.AioRpcError(grpc.StatusCode.UNAVAILABLE, None, None, "stream broke")
        return await super().__anext__()


class _SilentStream(_ClosableQueueStream):
    """Goes silent after a few frames but stays open (a wedged controller manager).

    A cancel ends the pending read with ``CancelledError``, as grpc.aio does for a
    locally cancelled call.
    """

    def __init__(self, log, frames: int = 5):
        super().__init__(log)
        self.frames = frames
        self.cancelled = asyncio.Event()
        self.silent_at: float | None = None

    def cancel(self):
        self.cancelled.set()
        return super().cancel()

    async def __anext__(self):
        if self.read >= self.frames:
            if self.silent_at is None:
                self.silent_at = asyncio.get_running_loop().time()
            await self.cancelled.wait()
            raise asyncio.CancelledError
        return await super().__anext__()


class _ScriptedGameStreams(_NonstopControllerManager):
    """The countdown stream, then the scripted game streams in order, then ``then`` (healthy by default)."""

    def __init__(self, log, *scripted, then=None):
        super().__init__(log)
        self.scripted = list(scripted)
        self.then = then
        self.opened_at: list[float] = []

    def StreamGameplayData(self):  # noqa: N802 - gRPC stub method name
        if self.streams:
            self.opened_at.append(asyncio.get_running_loop().time())
        factory = self.scripted.pop(0) if self.streams and self.scripted else (self.then if self.streams else None)
        if factory is None:
            return super().StreamGameplayData()
        stream = factory(self.log)
        self.streams.append(stream)
        return stream


def _fast_recovery(game):
    game._STREAM_FRAME_GAP_S = FRAME_GAP_S
    game._RECONNECT_BACKOFF_S = 0.01


async def _run_to_error(game, client):
    try:
        with patch.object(nonstop_mod, "WINNER_HOLD_SECONDS", 30.0), pytest.raises(GameplayStreamLostError):
            await asyncio.wait_for(game.run(), timeout=10)
    finally:
        _stop_producers(client)


@pytest.mark.uc("UC-026", br=["BR-003"])
@pytest.mark.uc("UC-030", br=["BR-006"])
@pytest.mark.uc("UC-026", cond=["Post-F-1"])
@pytest.mark.usefixtures("fast_nonstop")
@pytest.mark.asyncio
async def test_a_game_stream_that_keeps_breaking_ends_after_three_reconnects_with_no_winner():
    log = RecordingLog()
    client = _ScriptedGameStreams(log, then=_BreakingStream)
    game = _nonstop_run_game(log, client, time_limit=0)
    _fast_recovery(game)
    sounds = _record_sounds(game)

    await _run_to_error(game, client)

    assert len(client.streams) == 5, "the countdown stream, NonStop's game stream and three reconnects"
    assert len(log.events.get_events_of_type(GameEvent.GAME_ERROR)) == 1
    assert Sound.MENU_VOX_STATUS_CANCELLED in sounds
    _assert_no_winner(log, sounds)


@pytest.mark.uc("UC-026", br=["BR-003"])
@pytest.mark.uc("UC-030", br=["BR-006"])
@pytest.mark.usefixtures("fast_nonstop")
@pytest.mark.asyncio
async def test_a_silent_game_stream_is_detected_and_the_game_aborted_with_no_winner():
    log = RecordingLog()
    client = _ScriptedGameStreams(log, then=_SilentStream)
    game = _nonstop_run_game(log, client, time_limit=0)
    _fast_recovery(game)
    sounds = _record_sounds(game)

    await _run_to_error(game, client)

    assert len(client.streams) == 5, "every silent stream was reopened until the budget ran out"
    first_silent = client.streams[1]
    waited = client.opened_at[1] - first_silent.silent_at
    # Detected between 1 and 1.5 gaps into the wait, plus the 10 ms backoff.
    assert FRAME_GAP_S - 0.02 <= waited <= FRAME_GAP_S * 1.5 + 0.2, f"silence detected after {waited:.3f}s"
    assert len(log.events.get_events_of_type(GameEvent.GAME_ERROR)) == 1
    assert Sound.MENU_VOX_STATUS_CANCELLED in sounds
    _assert_no_winner(log, sounds)


@pytest.mark.uc("UC-026", br=["BR-003"])
@pytest.mark.uc("UC-030", br=["BR-006"])
@pytest.mark.usefixtures("fast_nonstop")
@pytest.mark.asyncio
@pytest.mark.parametrize("fault", [_BreakingStream, _SilentStream], ids=["broken", "silent"])
async def test_a_stream_back_within_the_budget_resumes_the_game(fault):
    log = RecordingLog()
    client = _ScriptedGameStreams(log, fault)
    game = _nonstop_run_game(log, client, time_limit=0.5)
    _lead_after_init(game)
    _fast_recovery(game)
    sounds = _record_sounds(game)
    clock_at_fault, respawned_on = [], []
    dead = list(_players())[2].serial
    real_reopen, real_respawn = game._reopen_gameplay_stream, game._respawn_player

    async def reopen(reason):
        clock_at_fault.append(game.start_time)
        # Dead when the stream broke, with the respawn deadline passed during the outage.
        await game._kill_player_impl(dead, 0.0)
        game.players[dead].respawn_deadline = game._clock()
        await real_reopen(reason)

    async def respawn(serial):
        respawned_on.append((serial, client.streams.index(game.gameplay_stream), game.gameplay_stream.read))
        return await real_respawn(serial)

    game._reopen_gameplay_stream = reopen
    game._respawn_player = respawn
    try:
        await asyncio.wait_for(game.run(), timeout=10)
    finally:
        _stop_producers(client)

    assert len(client.streams) == 3, "the faulty game stream was reopened exactly once"
    assert client.streams[2].read > 0, "the reopened stream delivered no frame"
    assert clock_at_fault[0] is not None
    assert game.start_time == clock_at_fault[0], "the reconnect restarted the time-limit clock"
    assert respawned_on[0] == (dead, 2, 1), "the respawn did not happen on the reopened stream's first frame"
    assert log.events.get_events_of_type(GameEvent.GAME_ERROR) == []
    assert len(log.events.get_events_of_type(GameEvent.GAME_WINNER)) == 1, "the time limit did not end the game"
    assert Sound.MENU_VOX_STATUS_CANCELLED not in sounds


@pytest.mark.uc("UC-026", br=["BR-003"])
@pytest.mark.uc("UC-030", br=["BR-006"])
@pytest.mark.usefixtures("fast_nonstop")
@pytest.mark.asyncio
async def test_frame_processing_longer_than_the_gap_is_no_silence():
    log = RecordingLog()
    client = _NonstopControllerManager(log)
    game = _nonstop_run_game(log, client, time_limit=0.8)
    _lead_after_init(game)
    _fast_recovery(game)
    _record_sounds(game)
    real_update = game._update_respawn_timers
    frames, slow_done = [], []

    async def slow_frame():
        frames.append(1)
        if len(frames) == 3:
            await asyncio.sleep(FRAME_GAP_S * 5)
            slow_done.append(1)
        await real_update()

    game._update_respawn_timers = slow_frame
    try:
        await asyncio.wait_for(game.run(), timeout=10)
    finally:
        _stop_producers(client)

    assert slow_done == [1], "the slow frame was cut short"
    assert len(client.streams) == 2, "a healthy stream was reconnected while a frame was processed"
    assert log.events.get_events_of_type(GameEvent.GAME_ERROR) == []
    assert len(log.events.get_events_of_type(GameEvent.GAME_WINNER)) == 1


def _ffa_game():
    return FFAGame(
        controller_manager_client=MockControllerManagerService(num_controllers=4),
        event_publisher=RecordingLog().publish,
        game_id="test_watch_tick",
        initial_players=_players(),
    )


def _nonstop_game():
    return NonstopJoustGame(
        controller_manager_client=MockControllerManagerService(num_controllers=4),
        event_publisher=RecordingLog().publish,
        audio_client=None,
        game_id="test_watch_tick",
        initial_players=_players(),
        time_limit_seconds=0,
    )


@pytest.mark.uc("UC-030", br=["BR-006"])
@pytest.mark.uc("UC-026", br=["BR-003"])
@pytest.mark.asyncio
@pytest.mark.parametrize("make_game", [_ffa_game, _nonstop_game], ids=["base_loop", "nonstop"])
async def test_a_watch_tick_that_raises_does_not_stop_silence_detection(make_game):
    game = make_game()
    game._STREAM_FRAME_GAP_S = 0.02
    cancels = []
    game._cancel_gameplay_call = lambda: cancels.append(1)
    ticks = []

    def clock():
        ticks.append(1)
        if len(ticks) == 1:
            raise RuntimeError("tick failed")
        return 10.0

    loop = _LoopState(
        loop_start_time=0.0,
        last_iteration_time=0.0,
        target_frame_time_ms=16.0,
        last_alive_serials=set(),
        waiting_since=0.0,
        clock=clock,
    )
    watch = asyncio.create_task(game._watch_frame_gap(loop))
    try:
        for _ in range(100):
            if cancels:
                break
            await asyncio.sleep(0.01)
        assert cancels == [1], "the watch stopped after a tick raised"
        assert loop.silenced
        assert not watch.done(), "the watch task ended"
    finally:
        watch.cancel()


class _DyingCall:
    """A gameplay call that fails every write with UNAVAILABLE, as a call dying under the write."""

    async def write(self, _message):
        raise grpc.aio.AioRpcError(grpc.StatusCode.UNAVAILABLE, None, None, "stream broke")


class _RecordingCall:
    def __init__(self):
        self.messages = []

    async def write(self, message):
        self.messages.append(message)


async def _running_nonstop():
    log = RecordingLog()
    game = _nonstop_game()
    game.event_publisher = log.publish
    await game._initialize_players_impl(game.controller_client.controllers)
    game.state = GameState.RUNNING
    game.running = True
    return game, log


@pytest.mark.uc("UC-030", br=["BR-006"])
@pytest.mark.uc("UC-026", flow="main", steps=[2])
@pytest.mark.asyncio
async def test_a_respawn_whose_led_write_fails_on_a_dying_call_still_counts():
    game, log = await _running_nonstop()
    serial = next(iter(game.players))
    game.players[serial].alive = False
    game.gameplay_stream = _DyingCall()

    assert await game._respawn_player(serial) is True

    assert game.players[serial].alive
    assert [e["serial"] for e in log.events.get_events_of_type("player_respawned")] == [serial]


@pytest.mark.uc("UC-030", br=["BR-006"])
@pytest.mark.uc("UC-101", br=["BR-001"])
@pytest.mark.asyncio
async def test_an_agent_revive_on_a_dying_call_is_recorded_with_its_grace():
    game, _log = await _running_nonstop()
    serial = next(iter(game.players))
    game.players[serial].alive = False
    game.gameplay_stream = _DyingCall()
    recorded = []
    game.record_intervention = lambda s, types=(): recorded.append((s, tuple(types)))

    assert await game.revive_player(serial) is True

    assert game.players[serial].grace_until > game_clock.now()
    assert recorded == [(serial, ("revive_player",))]


@pytest.mark.uc("UC-030", br=["BR-006"])
@pytest.mark.uc("UC-026", flow="main", steps=[2])
@pytest.mark.usefixtures("fast_nonstop")
@pytest.mark.asyncio
async def test_a_reopened_stream_keeps_the_spawn_protection_colour():
    game, _log = await _running_nonstop()
    protected, unprotected = list(game.players)[:2]
    game.players[protected].spawn_protected = True
    for serial in (protected, unprotected):
        game.players[serial].color = (255, 0, 0)  # a unique colour (_set_unique_colors)
    call = _RecordingCall()
    game.controller_client = SimpleNamespace(StreamGameplayData=lambda: call)

    await game._create_gameplay_stream()

    config = next(m.config for m in call.messages if m.HasField("config"))
    colors = {c.serial: (c.color.r, c.color.g, c.color.b) for c in config.colors}
    assert colors[protected] == nonstop_mod.SPAWN_PROTECTION_COLOR
    assert colors[unprotected] == (255, 0, 0)
