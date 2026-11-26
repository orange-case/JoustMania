"""The lobby's low-battery blink and the pad's LOW count (UC-002 A8, BR-010; UC-016 BR-006)."""

import asyncio
from unittest.mock import create_autospec

import grpc
import pytest
import pytest_asyncio

from lib.clock import FakeClock
from proto import controller_manager_pb2
from services.menu import metrics as menu_metrics
from services.menu.controller_events import ControllerEventLoop
from services.menu.handlers.base import ControllerState
from services.menu.idle_monitor import IdleMonitor
from services.menu.low_battery import (
    LOW_BATTERY_BLINK_INTERVAL_S,
    LowBatteryWarner,
    is_low,
    low_battery_serials,
)
from services.menu.state_manager import StateManager
from services.menu.tests.servicer_harness import cancel_background_tasks, make_real_servicer
from services.menu.utils.led import LedController

LOW_BATTERY = controller_manager_pb2.GAME_EFFECT_LOW_BATTERY


def state_manager(states: dict[str, ControllerState], levels: dict[str, int]) -> StateManager:
    sm = create_autospec(StateManager, instance=True)
    sm.controller_states = dict(states)
    sm.battery_levels = dict(levels)
    return sm


def warner(sm, clock=None):
    led = create_autospec(LedController, instance=True)
    w = LowBatteryWarner(state_manager=sm, send_effect=led.send_game_effect, clock=(clock or FakeClock()).time)
    return w, led.send_game_effect


@pytest.mark.uc("UC-002", flow="A8", br=["BR-010"])
@pytest.mark.parametrize(
    ("level", "low"),
    [
        (0, True),
        (1, True),  # 0-5 scale: 20 %
        (2, False),
        (5, False),
        (20, True),  # Rust HID percentage
        (40, False),
        (100, False),
        (0xEE, False),  # charging
        (0xEF, False),  # charged
    ],
)
def test_low_is_twenty_percent_or_less_on_the_battery_pct_scale(level, low):
    assert is_low(level) is low


@pytest.mark.asyncio
@pytest.mark.uc("UC-002", flow="A8", br=["BR-010"])
async def test_tick_blinks_only_low_connected_and_ready_controllers():
    sm = state_manager(
        {
            "conn": ControllerState.CONNECTED,
            "ready": ControllerState.READY,
            "idle": ControllerState.IDLE,
            "full": ControllerState.CONNECTED,
            "unknown": ControllerState.CONNECTED,
        },
        {"conn": 1, "ready": 20, "idle": 1, "full": 5},
    )
    w, send = warner(sm)

    await w.tick()

    assert sorted(call.args for call in send.await_args_list) == [("conn", LOW_BATTERY), ("ready", LOW_BATTERY)]


@pytest.mark.asyncio
@pytest.mark.uc("UC-002", flow="A8", br=["BR-010"])
async def test_tick_rate_limits_per_controller_and_a_recovered_controller_blinks_again_at_once():
    clock = FakeClock(start_time=100.0)
    sm = state_manager({"a": ControllerState.CONNECTED}, {"a": 1})
    w, send = warner(sm, clock)

    await w.tick()
    clock.advance(LOW_BATTERY_BLINK_INTERVAL_S - 1)
    await w.tick()
    assert send.await_count == 1

    clock.advance(1)
    await w.tick()
    assert send.await_count == 2

    sm.battery_levels["a"] = 5  # charged in between
    await w.tick()
    sm.battery_levels["a"] = 1
    await w.tick()
    assert send.await_count == 3


@pytest.mark.uc("UC-002", flow="A8", br=["BR-010"])
@pytest.mark.uc("UC-016", br=["BR-006"])
def test_low_battery_serials_counts_every_connected_state_but_not_unknown_levels():
    sm = state_manager(
        {
            "a": ControllerState.CONNECTED,
            "b": ControllerState.IDLE,
            "c": ControllerState.READY,
            "d": ControllerState.READY,
        },
        {"a": 1, "b": 0, "c": 4, "gone": 1},
    )
    assert low_battery_serials(sm) == {"a", "b"}


@pytest.mark.asyncio
@pytest.mark.uc("UC-002", flow="A8", br=["BR-010"])
async def test_the_blink_runs_only_while_the_idle_monitor_runs_the_lobby():
    clock = FakeClock(start_time=100.0)
    sm = state_manager({"a": ControllerState.CONNECTED}, {"a": 1})
    w, send = warner(sm, clock)
    w._tick_s = 0.01
    monitor = IdleMonitor(
        state_manager=sm,
        get_idle_enabled=lambda: False,
        get_idle_timeout=lambda: 15,
        get_sentinel_count=lambda: 2,
        get_rotation_minutes=lambda: 5,
        low_battery_warner=w,
    )

    monitor.start()
    for _ in range(50):
        if send.await_count:
            break
        await asyncio.sleep(0.01)
    assert send.await_count == 1

    monitor.stop()  # a game starts
    clock.advance(LOW_BATTERY_BLINK_INTERVAL_S * 3)  # well past the rate limit
    await asyncio.sleep(0.1)
    assert send.await_count == 1
    assert not w.running


@pytest.mark.asyncio
@pytest.mark.uc("UC-002", flow="A8", br=["BR-010"])
async def test_stop_forgets_the_rate_limit_so_the_next_lobby_blinks_at_once():
    clock = FakeClock(start_time=100.0)
    sm = state_manager({"a": ControllerState.CONNECTED}, {"a": 1})
    w, send = warner(sm, clock)

    await w.tick()
    w.stop()  # the game
    clock.advance(1)  # far inside the interval
    await w.tick()  # back in the lobby

    assert send.await_count == 2


@pytest.mark.uc("UC-002", flow="A8", br=["BR-010"])
def test_build_menu_hands_its_warner_to_the_idle_monitor():
    menu = make_real_servicer()

    assert isinstance(menu.low_battery_warner, LowBatteryWarner)
    assert menu.low_battery_warner._state_manager is menu.state_manager
    assert menu.idle_monitor_cls.call_args.kwargs["low_battery_warner"] is menu.low_battery_warner


def connect_event(serial: str, battery: int, reported: bool) -> controller_manager_pb2.ButtonEvent:
    return controller_manager_pb2.ButtonEvent(
        serial=serial,
        battery=battery,
        battery_reported=reported,
        event_type=controller_manager_pb2.EVENT_CONNECT,
    )


@pytest_asyncio.fixture
async def lobby():
    menu = make_real_servicer()
    events = ControllerEventLoop(
        controller_channel=create_autospec(grpc.aio.Channel, instance=True),
        led=menu.led,
        callbacks=menu,
        metrics=menu_metrics,
    )
    yield menu, events
    await cancel_background_tasks(menu)


@pytest.mark.asyncio
@pytest.mark.uc("UC-002", flow="A8", br=["BR-010"])
async def test_a_reported_empty_battery_is_low_and_an_unreported_zero_is_not(lobby):
    menu, events = lobby

    await events._dispatch_event(connect_event("empty", 0, reported=True))
    await events._dispatch_event(connect_event("old_cm", 0, reported=False))

    assert menu.state_manager.battery_levels == {"empty": 0}
    assert low_battery_serials(menu.state_manager) == {"empty"}


@pytest.mark.asyncio
@pytest.mark.uc("UC-002", flow="A8", br=["BR-010"])
async def test_an_empty_battery_is_low_again_after_the_stream_reconnects(lobby):
    menu, events = lobby
    await events._dispatch_event(connect_event("a", 1, reported=True))

    # After a game the button stream reconnects: the menu clears its lobby, the controller
    # manager replays a connect with the level the controller ran down to.
    await menu.on_stream_reconnected()
    assert low_battery_serials(menu.state_manager) == set()
    await events._dispatch_event(connect_event("a", 0, reported=True))

    assert low_battery_serials(menu.state_manager) == {"a"}
