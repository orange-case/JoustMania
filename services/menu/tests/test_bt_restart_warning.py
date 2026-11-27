"""The lobby's warning before a planned Bluetooth restart (UC-002 A9, BR-011; UC-001 BR-011)."""

import asyncio

import pytest
import pytest_asyncio

from lib.colors import Colors
from lib.types import Sound
from proto import controller_manager_pb2, menu_pb2
from services.menu.bt_restart_warning import HOLD_AFTER_CUE_WINDOW_SECONDS, START_HOLD_SECONDS, WARNING_LEAD_MS
from services.menu.handlers.base import ControllerState
from services.menu.tests.servicer_harness import cancel_background_tasks, make_real_servicer

pytestmark = pytest.mark.asyncio


@pytest_asyncio.fixture
async def menu():
    servicer = make_real_servicer()
    servicer.state = menu_pb2.MenuState.RUNNING
    yield servicer
    await cancel_background_tasks(servicer)


def lobby(servicer, states: dict[str, ControllerState]) -> None:
    servicer.state_manager.controller_states = dict(states)


async def warn(servicer, check_only: bool = False) -> menu_pb2.WarnBluetoothRestartResponse:
    return await servicer.WarnBluetoothRestart(menu_pb2.WarnBluetoothRestartRequest(check_only=check_only), None)


def pulsed(servicer) -> list[str]:
    return [
        c.args[0]
        for c in servicer.state_manager.led.send_game_effect.await_args_list
        if c.args[1] == controller_manager_pb2.GAME_EFFECT_PULSE
    ]


def no_cue(servicer) -> None:
    servicer.state_manager.audio.play_sound.assert_not_awaited()
    assert pulsed(servicer) == []


@pytest.mark.uc("UC-002", flow="A9", br=["BR-011"])
async def test_lobby_hears_a_beep_and_sees_a_white_pulse_then_the_daemon_waits_for_it(menu):
    lobby(menu, {"a": ControllerState.CONNECTED, "b": ControllerState.READY, "c": ControllerState.IDLE})

    answer = await warn(menu)

    assert answer == menu_pb2.WarnBluetoothRestartResponse(warned=True, connected_count=3, lead_ms=WARNING_LEAD_MS)
    menu.state_manager.audio.play_sound.assert_awaited_once_with(Sound.SFX_BEEP_LOUD)
    # IDLE keeps its LED off (UC-002 A3)
    assert sorted(pulsed(menu)) == ["a", "b"]
    for call in menu.state_manager.led.send_game_effect.await_args_list:
        assert call.kwargs["color"] == Colors.White.value
        assert call.kwargs["duration_ms"] == WARNING_LEAD_MS


@pytest.mark.uc("UC-002", flow="A9", br=["BR-011"])
async def test_no_cue_when_no_controller_is_connected(menu):
    lobby(menu, {})

    answer = await warn(menu)

    assert answer == menu_pb2.WarnBluetoothRestartResponse(warned=False, game_live=False, connected_count=0)
    no_cue(menu)


@pytest.mark.uc("UC-002", flow="A9", br=["BR-011"])
async def test_no_cue_during_a_game_and_the_daemon_is_told_a_game_is_live(menu):
    lobby(menu, {"a": ControllerState.READY})
    game = asyncio.get_running_loop().create_future()
    menu._game_lifecycle_task = game
    try:
        answer = await warn(menu)
    finally:
        game.cancel()

    assert (answer.warned, answer.game_live) == (False, True)
    no_cue(menu)


@pytest.mark.uc("UC-002", flow="A9", br=["BR-011"])
async def test_a_reset_menu_recovery_counts_as_a_live_game(menu):
    lobby(menu, {"a": ControllerState.CONNECTED})
    recovery = asyncio.get_running_loop().create_future()
    menu._reset_recovery = recovery
    try:
        answer = await warn(menu)
    finally:
        recovery.cancel()

    assert (answer.warned, answer.game_live) == (False, True)
    no_cue(menu)


@pytest.mark.uc("UC-002", flow="A9", br=["BR-011"])
async def test_the_check_after_the_cue_plays_nothing_and_reports_a_game_that_went_live(menu):
    lobby(menu, {"a": ControllerState.READY})

    idle = await warn(menu, check_only=True)
    game = asyncio.get_running_loop().create_future()
    menu._game_lifecycle_task = game
    try:
        live = await warn(menu, check_only=True)
    finally:
        game.cancel()

    assert (idle.warned, idle.game_live, idle.lead_ms) == (False, False, 0)
    assert (live.warned, live.game_live) == (False, True)
    no_cue(menu)


@pytest.mark.uc("UC-002", flow="A9", br=["BR-011"])
async def test_no_cue_while_the_menu_is_stopped(menu):
    menu.state = menu_pb2.MenuState.STOPPED
    lobby(menu, {"a": ControllerState.CONNECTED})

    answer = await warn(menu)

    assert (answer.warned, answer.game_live, answer.lead_ms) == (False, False, 0)
    no_cue(menu)


@pytest.mark.uc("UC-002", flow="A9", br=["BR-011"])
async def test_a_failed_pulse_does_not_stop_the_others(menu):
    lobby(menu, {"a": ControllerState.CONNECTED, "b": ControllerState.CONNECTED})
    menu.state_manager.led.send_game_effect.side_effect = [RuntimeError("cm down"), True]

    answer = await warn(menu)

    assert answer.warned is True
    assert pulsed(menu) == ["a", "b"]


class FakeClock:
    def __init__(self) -> None:
        self.now = 1000.0

    def __call__(self) -> float:
        return self.now


@pytest.fixture
def clock(menu):
    fake = FakeClock()
    menu.bt_restart_warner._clock = fake
    return fake


def rejected(servicer) -> list[dict]:
    return [c.args[1] for c in servicer.event_publisher.publish.await_args_list if c.args[0] == "game_start_rejected"]


@pytest.mark.uc("UC-001", flow="A7", br=["BR-011"])
async def test_the_last_check_after_the_cue_holds_starts_while_bluetoothd_restarts(menu, clock):
    lobby(menu, {"a": ControllerState.READY, "b": ControllerState.READY})
    await warn(menu)
    clock.now += WARNING_LEAD_MS / 1000

    await warn(menu, check_only=True)
    clock.now += START_HOLD_SECONDS - 0.1
    started = await menu.start_game("web", "web")

    assert started is False
    assert menu._game_lifecycle_task is None
    assert rejected(menu) == [
        {"game_name": menu.current_selection.name, "source": "web", "reason": "bluetooth_restart"}
    ]
    clock.now += 0.2
    assert menu.bt_restart_warner.start_held() is False


@pytest.mark.usefixtures("clock")
@pytest.mark.uc("UC-001", flow="A7", br=["BR-011"])
async def test_a_held_ready_start_clears_the_ready_latch(menu):
    lobby(menu, {"a": ControllerState.READY, "b": ControllerState.READY})
    await warn(menu)
    await warn(menu, check_only=True)
    menu.ready_handler._game_start_in_progress = True

    assert await menu._ready_start_game("a") is False
    assert menu.ready_handler._game_start_in_progress is False


@pytest.mark.uc("UC-001", flow="A7", br=["BR-011"])
async def test_a_check_without_a_cue_of_its_own_holds_nothing(menu, clock):
    lobby(menu, {"a": ControllerState.READY})

    await warn(menu, check_only=True)
    assert menu.bt_restart_warner.start_held() is False

    await warn(menu)
    clock.now += HOLD_AFTER_CUE_WINDOW_SECONDS + 1
    await warn(menu, check_only=True)
    assert menu.bt_restart_warner.start_held() is False


@pytest.mark.uc("UC-001", flow="A7", br=["BR-011"])
async def test_each_cue_allows_one_hold(menu, clock):
    lobby(menu, {"a": ControllerState.READY})
    await warn(menu)
    await warn(menu, check_only=True)
    clock.now += START_HOLD_SECONDS

    await warn(menu, check_only=True)

    assert menu.bt_restart_warner.start_held() is False


@pytest.mark.usefixtures("clock")
@pytest.mark.uc("UC-001", flow="A7", br=["BR-011"])
async def test_a_check_that_reports_a_live_game_holds_nothing(menu):
    lobby(menu, {"a": ControllerState.READY})
    await warn(menu)
    game = asyncio.get_running_loop().create_future()
    menu._game_lifecycle_task = game
    try:
        await warn(menu, check_only=True)
    finally:
        game.cancel()

    assert menu.bt_restart_warner.start_held() is False
