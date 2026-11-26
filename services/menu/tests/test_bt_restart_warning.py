"""The lobby's warning before a planned Bluetooth restart (UC-002 A9, BR-011; UC-001 BR-011)."""

import asyncio

import pytest
import pytest_asyncio

from lib.colors import Colors
from lib.types import Sound
from proto import controller_manager_pb2, menu_pb2
from services.menu.bt_restart_warning import WARNING_LEAD_MS
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


async def warn(servicer) -> menu_pb2.WarnBluetoothRestartResponse:
    answer = await servicer.WarnBluetoothRestart(menu_pb2.WarnBluetoothRestartRequest(), None)
    tasks = list(servicer.bt_restart_warner._tasks)
    if tasks:
        await asyncio.gather(*tasks)
    return answer


def pulsed(servicer) -> list[str]:
    return [
        c.args[0]
        for c in servicer.state_manager.led.send_game_effect.await_args_list
        if c.args[1] == controller_manager_pb2.GAME_EFFECT_PULSE
    ]


@pytest.mark.uc("UC-002", flow="A9", br=["BR-011"])
async def test_lobby_hears_a_beep_and_sees_a_white_pulse_then_the_daemon_waits_for_it(menu):
    lobby(menu, {"a": ControllerState.CONNECTED, "b": ControllerState.READY, "c": ControllerState.IDLE})

    answer = await warn(menu)

    assert answer == menu_pb2.WarnBluetoothRestartResponse(warned=True, connected_count=3, lead_ms=WARNING_LEAD_MS)
    menu.state_manager.audio.play_sound.assert_awaited_once_with(Sound.SFX_BEEP_LOUD)
    # IDLE keeps its LED off (UC-002 A3)
    assert sorted(pulsed(menu)) == ["a", "b"]
    for call in menu.state_manager.led.send_game_effect.await_args_list:
        assert call.kwargs == {"color": Colors.White.value, "duration_ms": WARNING_LEAD_MS}


@pytest.mark.uc("UC-002", flow="A9", br=["BR-011"])
async def test_no_cue_when_no_controller_is_connected(menu):
    lobby(menu, {})

    answer = await warn(menu)

    assert answer == menu_pb2.WarnBluetoothRestartResponse(warned=False, game_live=False, connected_count=0)
    menu.state_manager.audio.play_sound.assert_not_awaited()
    assert pulsed(menu) == []


@pytest.mark.uc("UC-002", flow="A9", br=["BR-011"])
async def test_no_cue_during_a_game_and_the_daemon_is_told_a_game_is_live(menu):
    lobby(menu, {"a": ControllerState.READY})
    game = asyncio.get_running_loop().create_future()
    menu._game_lifecycle_task = game
    try:
        answer = await warn(menu)
    finally:
        game.cancel()

    assert answer.game_live is True
    assert answer.warned is False
    menu.state_manager.audio.play_sound.assert_not_awaited()
    assert pulsed(menu) == []


@pytest.mark.uc("UC-002", flow="A9", br=["BR-011"])
async def test_no_cue_while_the_menu_is_stopped(menu):
    menu.state = menu_pb2.MenuState.STOPPED
    lobby(menu, {"a": ControllerState.CONNECTED})

    answer = await warn(menu)

    assert (answer.warned, answer.game_live, answer.lead_ms) == (False, False, 0)
    menu.state_manager.audio.play_sound.assert_not_awaited()


@pytest.mark.uc("UC-002", flow="A9", br=["BR-011"])
async def test_the_answer_does_not_wait_for_a_hung_audio_service(menu):
    lobby(menu, {"a": ControllerState.CONNECTED})
    hung = asyncio.Event()

    async def hang(*_args, **_kwargs):
        await hung.wait()

    menu.state_manager.audio.play_sound.side_effect = hang

    answer = await asyncio.wait_for(
        menu.WarnBluetoothRestart(menu_pb2.WarnBluetoothRestartRequest(), None), timeout=1.0
    )

    assert answer.warned is True
    hung.set()
    await asyncio.gather(*list(menu.bt_restart_warner._tasks))


@pytest.mark.uc("UC-002", flow="A9", br=["BR-011"])
async def test_a_failed_pulse_does_not_stop_the_others(menu):
    lobby(menu, {"a": ControllerState.CONNECTED, "b": ControllerState.CONNECTED})
    menu.state_manager.led.send_game_effect.side_effect = [RuntimeError("cm down"), True]

    await warn(menu)

    assert pulsed(menu) == ["a", "b"]
