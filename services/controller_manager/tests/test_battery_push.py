"""The 30 s battery-only events that keep the menu's levels fresh without a button press (UC-002 BR-010)."""

from unittest.mock import call

import pytest

from lib.controller_constants import ControllerInfoKey
from services.controller_manager.discovery_loop import DiscoveryLoop
from services.controller_manager.tests.backend_doubles import (
    autospec_backend,
    autospec_button_detector,
    autospec_feedback_manager,
    autospec_monitoring,
    autospec_publisher,
    autospec_rescan_timer,
)


def make_loop(tracked: dict) -> DiscoveryLoop:
    return DiscoveryLoop(
        backend=autospec_backend(),
        tracked_controllers=tracked,
        controller_states={},
        button_detector=autospec_button_detector(),
        feedback_manager=autospec_feedback_manager(),
        monitoring=autospec_monitoring(),
        rescan_timer=autospec_rescan_timer(),
        paired_serials=[],
        base_colors={},
        event_publisher=autospec_publisher(),
    )


def info(battery: int | None, name: str = "", reserved: bool = False) -> dict:
    d: dict = {ControllerInfoKey.NAME: name}
    if battery is not None:
        d[ControllerInfoKey.BATTERY] = battery
    if reserved:
        d[ControllerInfoKey.RESERVED] = True
    return d


@pytest.mark.asyncio
@pytest.mark.uc("UC-002", flow="A8", br=["BR-010"])
async def test_the_battery_step_sends_read_levels_of_visible_controllers():
    loop = make_loop({"a": info(3, "Red"), "empty": info(0), "unread": info(None), "mock": info(1, reserved=True)})

    await loop._battery_step()

    publish = loop.button_detector.publish_battery_event
    assert sorted(publish.call_args_list) == [call("a", 3, "Red"), call("empty", 0, "")]


@pytest.mark.asyncio
@pytest.mark.uc("UC-002", flow="A8", br=["BR-010"])
async def test_the_battery_step_sends_only_a_changed_level_and_again_after_a_drop():
    tracked = {"a": info(3), "b": info(5)}
    loop = make_loop(tracked)
    await loop._battery_step()
    publish = loop.button_detector.publish_battery_event
    publish.reset_mock()

    tracked["a"][ControllerInfoKey.BATTERY] = 1
    await loop._battery_step()
    assert publish.call_args_list == [call("a", 1, "")]

    publish.reset_mock()
    del tracked["b"]
    await loop._battery_step()
    tracked["b"] = info(5)
    await loop._battery_step()
    assert publish.call_args_list == [call("b", 5, "")]
