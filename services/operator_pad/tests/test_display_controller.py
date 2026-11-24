"""The display on the key path (UC-016 steps 4 to 6, A1, A3, A4, BR-001, BR-003, BR-004, FR-240).

The real controller, gesture machine, actions and display model; only the gRPC stubs and the
hidraw node are fakes. Every wait is bounded.
"""

import asyncio
from unittest.mock import create_autospec

import pytest

from lib.setting_ladders import MODE_LADDER
from proto import menu_pb2
from services.operator_pad.actions import Actions
from services.operator_pad.config import DEFAULT_KEYMAP, Timings, parse_keymap
from services.operator_pad.controller import PadController
from services.operator_pad.cues import CuePlayer
from services.operator_pad.display import DisplayModel
from services.operator_pad.gestures import PRESS, RELEASE
from services.operator_pad.state import MenuStatusReader
from services.operator_pad.tests.display_fakes import pad_config
from services.operator_pad.tests.fakes import CoordinatorStubSpec, MenuStubSpec

KEYMAP = parse_keymap(DEFAULT_KEYMAP)
K1, K2, K3, K4, K5, K6, K7, K8 = 183, 184, 188, 189, 190, 191, 192, 193  # F13..F23 (DEFAULT_KEYMAP)
KNOB_PRESS, DOWN, UP = 185, 186, 187  # F15, F16 (turn A), F17 (turn B)


def lobby(**kw) -> menu_pb2.GetMenuStatusResponse:
    base = {
        "selection": "JoustFFA",
        "ready_count": 1,
        "connected_count": 2,
        "sensitivity": 2,
        "num_teams": 2,
        "audio_volume_master": "normal",
        "operator_settings": {"sensitivity": "medium", "num_teams": "two", "shield.activation": "automatic"},
    }
    base.update(kw)
    return menu_pb2.GetMenuStatusResponse(**base)


class Clock:
    def __init__(self) -> None:
        self.now = 100.0

    def __call__(self) -> float:
        return self.now


class Pad:
    def __init__(self, link_up: bool = True, result_ms: int = 1500) -> None:
        self.config = pad_config(result_ms=result_ms)
        self.config.timings.return_value = Timings(knob_events_per_step=1)
        self.clock = Clock()
        self.model = DisplayModel(self.config, version="0.14.0", clock=self.clock)
        if link_up:
            self.model.acknowledged()
        self.reader = create_autospec(MenuStatusReader, instance=True)
        self.reader.fetch.return_value = lobby()
        self.menu = create_autospec(MenuStubSpec, instance=True)
        self.menu.ProcessInput.return_value = menu_pb2.ProcessInputResponse(success=True, value="fast")
        self.cues = create_autospec(CuePlayer, instance=True)
        self.actions = Actions(
            self.menu,
            create_autospec(CoordinatorStubSpec, instance=True),
            self.cues,
            sleep=_no_wait,
            display=self.model,
        )
        self.controller = PadController(self.config, self.reader, self.actions, self.cues, display=self.model)
        self.controller.attached(KEYMAP)
        self.t = int(self.clock.now * 1000)  # the gesture clock and the screen's share one basis

    async def key(self, code: int, value: int, dt: int = 10) -> None:
        self.t += dt
        await asyncio.wait_for(self.controller.on_code(code, value, self.t), 2.0)

    async def turn(self, code: int, n: int = 1) -> None:
        for _ in range(n):
            await self.key(code, PRESS, 5)
            await self.key(code, RELEASE, 1)

    def screen(self) -> list[str]:
        shown = self.model.current(self.clock.now)
        return [shown.text(i) for i in range(1, 9)]

    def sent(self) -> list[dict]:
        return [dict(c.args[0].data) for c in self.menu.ProcessInput.await_args_list]


async def _no_wait(_seconds: float) -> None:
    return None


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[4])
async def test_layer_step_shows_setting_screen_with_preview_disabled():
    pad = Pad()
    await pad.key(K6, PRESS)
    await pad.turn(UP)
    assert pad.screen()[:5] == ["SENSITIVITY", "", "NOW 2", "", "NEW 3"]
    await pad.turn(UP)
    assert pad.screen()[4] == "NEW 4"


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[5])
async def test_release_shows_result_then_status_after_result_ms():
    pad = Pad(result_ms=1500)
    await pad.key(K6, PRESS)
    await pad.turn(UP)
    await pad.key(K6, RELEASE)
    assert pad.screen()[:3] == ["SENSITIVITY", "", "3"]  # the value the menu reports, not the prediction
    pad.clock.now += 1.4
    assert pad.screen()[0] == "SENSITIVITY"
    pad.clock.now += 0.2
    assert pad.screen()[0] == "JOUSTMANIA LOBBY"


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[5])
async def test_result_with_no_value_shows_ok_never_the_prediction():
    pad = Pad()
    pad.menu.ProcessInput.return_value = menu_pb2.ProcessInputResponse(success=True)  # an older menu
    await pad.key(K6, PRESS)
    await pad.turn(UP)
    await pad.key(K6, RELEASE)
    assert pad.screen()[:3] == ["SENSITIVITY", "", "OK"]


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"])
async def test_a_k8_hold_shows_its_result_like_every_command():
    pad = Pad()
    pad.menu.ProcessInput.return_value = menu_pb2.ProcessInputResponse(success=True)  # force_start names no value
    await pad.key(K8, PRESS)
    await pad.key(K8, RELEASE, dt=1700)
    assert pad.screen()[:3] == ["FORCE START", "", "OK"]


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[4, 5])
async def test_new_layer_step_replaces_result():
    pad = Pad()
    await pad.key(K6, PRESS)
    await pad.turn(UP)
    await pad.key(K6, RELEASE)
    assert pad.screen()[2] == "3"
    await pad.key(K1, PRESS)
    await pad.turn(UP)
    assert pad.screen()[0] == "GAME"


@pytest.mark.uc("UC-016", flow="main", br=["BR-007"], steps=[5])
@pytest.mark.parametrize(("key", "options"), [(183, len(MODE_LADDER))])
async def test_a_full_turn_of_the_mode_or_shield_wheel_sends_nothing_and_shows_no_result(key, options):
    pad = Pad()
    await pad.key(key, PRESS)
    await pad.turn(UP, options)
    await pad.key(key, RELEASE)
    assert pad.sent() == []
    assert pad.screen()[0] == "JOUSTMANIA LOBBY"


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[4])
async def test_a_later_hold_with_only_a_knob_press_never_shows_the_old_values():
    pad = Pad()
    await pad.key(K6, PRESS)
    await pad.turn(UP)
    await pad.key(K6, RELEASE)
    pad.clock.now += 5
    await pad.key(K6, PRESS)
    await pad.key(KNOB_PRESS, PRESS)  # bound to the layer: no step
    assert pad.screen()[0] == "JOUSTMANIA LOBBY"


@pytest.mark.uc("UC-016", flow="A3", br=["BR-001"])
async def test_refusal_shows_not_possible():
    pad = Pad()
    pad.menu.ProcessInput.return_value = menu_pb2.ProcessInputResponse(success=False, error="game_active")
    await pad.key(K6, PRESS)
    await pad.turn(UP)
    await pad.key(K6, RELEASE)
    assert pad.screen()[:3] == ["SENSITIVITY", "", "NOT POSSIBLE"]


@pytest.mark.uc("UC-016", flow="A4", br=["BR-001"])
async def test_lobby_only_layer_while_live_shows_not_possible_then_refuses():
    pad = Pad()
    pad.reader.fetch.return_value = lobby(lifecycle_live=True)
    await pad.key(K6, PRESS)
    await pad.turn(UP)
    assert pad.screen()[7] == "NOT POSSIBLE"
    await pad.key(K6, RELEASE)
    assert pad.screen()[2] == "NOT POSSIBLE"
    assert pad.sent() == []


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[5])
async def test_release_ends_the_setting_screen_before_the_command_returns():
    pad = Pad()
    gate = asyncio.Event()

    async def slow(*_args, **_kwargs):
        await gate.wait()
        return menu_pb2.ProcessInputResponse(success=True, value="fast")

    pad.menu.ProcessInput.side_effect = slow
    await pad.key(K6, PRESS)
    await pad.turn(UP)
    release = asyncio.create_task(pad.controller.on_code(K6, RELEASE, pad.t + 10))
    await asyncio.sleep(0.01)
    assert pad.screen()[:3] == ["SENSITIVITY", "", ""]  # the label while the command runs, no NOW/NEW
    gate.set()
    await asyncio.wait_for(release, 2.0)
    assert pad.screen()[2] == "3"


@pytest.mark.uc("UC-016", flow="main", br=["BR-003"])
@pytest.mark.uc("UC-015", flow="A5", br=["BR-013"])
async def test_commands_use_press_and_release_reads_not_the_poll():
    """BR-003: the polled status is for the screen only; the K4 hold judges its own two reads."""
    pad = Pad()
    pad.model.note_status(lobby(lifecycle_live=True, lifecycle_generation=9), pad.model.read_token())
    pad.reader.fetch.return_value = lobby(lifecycle_generation=4)
    await pad.key(K8, PRESS)
    await pad.key(K8, RELEASE, dt=1700)
    assert pad.sent() == [{"command": "force_start", "expected_generation": "4"}]


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[6])
async def test_volume_turns_show_the_level_and_the_menu_value_rebases():
    pad = Pad()
    await pad.key(K6, PRESS)  # a key read: the screen's volume starts at `normal`
    await pad.key(K6, RELEASE)
    await pad.turn(UP)
    assert pad.screen()[:3] == ["VOLUME", "", "LOUD"]
    pad.menu.ProcessInput.return_value = menu_pb2.ProcessInputResponse(success=True, value="loud")
    await asyncio.wait_for(pad.actions.drain(), 2.0)
    assert pad.screen()[2] == "LOUD"


@pytest.mark.uc("UC-016", flow="main", br=["BR-001", "BR-003"], steps=[6])
async def test_a_status_read_started_before_a_volume_commit_never_rebases_the_volume():
    """A poll that started before the commit ended carries the old level: the screen keeps the commit's."""
    pad = Pad()
    pad.model.note_status(lobby(audio_volume_master="normal"), pad.model.read_token())
    token = pad.model.read_token()  # the poll's read leaves
    ticket = pad.model.volume_commit_start(("steps", 1))
    pad.model.volume_commit_end(ticket, "ok", "loud")
    pad.model.note_status(lobby(audio_volume_master="normal"), token)  # ...and answers after the commit
    pad.model.volume_turn(0)
    assert pad.screen()[:3] == ["VOLUME", "", "LOUD"]
    pad.model.note_status(lobby(audio_volume_master="normal"), pad.model.read_token())  # a later read does
    assert pad.screen()[2] == "NORM"


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[6])
async def test_volume_prediction_clamps_each_committed_batch():
    """At `max`: +2 writes nothing (at_limit), then -1 commits `loud`: the screen says LOUD, not MAX."""
    pad = Pad()
    pad.model.note_status(lobby(audio_volume_master="max"), pad.model.read_token())
    pad.menu.ProcessInput.return_value = menu_pb2.ProcessInputResponse(success=False, error="at_limit")
    await pad.turn(UP, 2)
    assert pad.screen()[2] == "MAX" and pad.screen()[7] == "MAXIMUM"
    await asyncio.wait_for(pad.actions.drain(), 2.0)
    await pad.turn(DOWN, 1)
    assert pad.screen()[2] == "LOUD"


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[6])
async def test_mute_shows_muted_and_its_ok_result_never_replaces_it():
    pad = Pad()
    pad.menu.ProcessInput.return_value = menu_pb2.ProcessInputResponse(success=True, value="mute")
    await pad.key(KNOB_PRESS, PRESS)
    assert pad.screen()[2] == "MUTE" and pad.screen()[7] == "MUTED"
    pad.reader.fetch.return_value = lobby(audio_volume_master="mute")
    await pad.key(KNOB_PRESS, RELEASE)
    assert pad.screen()[0] == "VOLUME" and pad.screen()[7] == "MUTED"


@pytest.mark.uc("UC-016", flow="A3", br=["BR-001"])
async def test_a_failed_volume_commit_shows_failed():
    pad = Pad()
    pad.menu.ProcessInput.return_value = menu_pb2.ProcessInputResponse(success=False, error="flag_service_unavailable")
    await pad.turn(UP)
    await asyncio.wait_for(pad.actions.drain(), 2.0)
    assert pad.screen()[:3] == ["VOLUME", "", "FAILED"]


@pytest.mark.uc("UC-016", flow="A1", br=["BR-004"], steps=[6])
async def test_volume_step_and_mute_silent_while_link_up():
    pad = Pad(link_up=True)
    await pad.turn(UP)
    await asyncio.wait_for(pad.actions.drain(), 2.0)
    await pad.key(KNOB_PRESS, PRESS)
    assert [d.get("cue") for d in pad.sent()] == ["none", "none"]


@pytest.mark.uc("UC-016", flow="A1", br=["BR-004"], steps=[6])
async def test_volume_sound_back_when_link_down():
    pad = Pad(link_up=False)
    await pad.turn(UP)
    await asyncio.wait_for(pad.actions.drain(), 2.0)
    pad.model.acknowledged()
    pad.clock.now += 2.0  # the last acknowledged show is too old: the link counts as down
    await pad.key(KNOB_PRESS, PRESS)
    assert [d.get("cue") for d in pad.sent()] == [None, None]


@pytest.mark.uc("UC-016", flow="A1", br=["BR-002"])
async def test_a_display_hook_that_raises_never_stops_a_key():
    """FR-240: every hook is outside the key's path to the menu."""
    pad = Pad()

    def boom(*_args):
        raise RuntimeError("display bug")

    for hook in ("note_status", "hold_key", "set_locked", "layer_step", "pending", "result", "volume_turn"):
        setattr(pad.model, hook, boom)
    await pad.key(K6, PRESS)
    await pad.turn(UP)
    await pad.key(K6, RELEASE)
    assert pad.sent() == [{"command": "step_setting", "option": "sensitivity", "direction": "up", "steps": "1"}]


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"])
async def test_lock_shows_locked_on_the_status_screen():
    pad = Pad()
    await pad.key(K4, PRESS)
    await pad.key(K4, RELEASE, dt=3200)
    assert pad.screen()[7] == "LOCKED"


@pytest.mark.uc("UC-016", flow="A1", br=["BR-002"])
async def test_detach_drops_the_setting_screen():
    pad = Pad()
    await pad.key(K6, PRESS)
    await pad.turn(UP)
    pad.controller.detached()
    assert pad.screen()[0] == "JOUSTMANIA LOBBY"


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"])
@pytest.mark.parametrize(
    ("code", "fields", "expected"),
    [
        (K6, {}, ["SENSITIVITY", "", "2"]),
        (K6, {"operator_settings": {"sensitivity": "automatic"}}, ["SENSITIVITY", "", "AUTO"]),
        (K5, {"agent_enabled_state": "unknown"}, ["AGENT", "", "?"]),
        (K3, {}, ["VOLUME", "", "NORM"]),
        (K7, {}, ["SENSITIVITY", "", "DEFAULT AUTO"]),
    ],
)
async def test_a_tap_shows_the_label_and_the_value_read(code, fields, expected):
    """BR-001 tap screen: like a result, for `operator_pad.display_result_ms`; nothing is sent."""
    pad = Pad(result_ms=1500)
    pad.reader.fetch.return_value = lobby(**fields)
    await pad.key(code, PRESS)
    await pad.key(code, RELEASE, dt=50)
    assert pad.screen()[:3] == expected
    assert pad.sent() == []
    pad.clock.now += 1.6
    assert pad.screen()[0] == "JOUSTMANIA LOBBY"


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[6])
async def test_volume_prediction_clamps_a_batch_still_in_flight_on_its_own():
    """At `max` with +2 still being committed (nothing will change), a -1 turn shows LOUD, not MAX."""
    pad = Pad()
    pad.model.note_status(lobby(audio_volume_master="max"), pad.model.read_token())
    gate = asyncio.Event()

    async def at_limit(*_args, **_kwargs):
        await gate.wait()
        return menu_pb2.ProcessInputResponse(success=False, error="at_limit")

    pad.menu.ProcessInput.side_effect = at_limit
    await pad.turn(UP, 2)
    await asyncio.sleep(0.01)  # the +2 batch leaves and waits for the menu
    await pad.turn(DOWN, 1)
    assert pad.screen()[2] == "LOUD"
    gate.set()
    await asyncio.wait_for(pad.actions.drain(), 2.0)


# --- UC-016 A5, BR-008: the knob with the pages -------------------------------------------------------


def paged(version: int = 3, link_up: bool = True) -> Pad:
    pad = Pad(link_up=link_up)
    pad.model.set_protocol(version)
    return pad


@pytest.mark.uc("UC-016", flow="A5", br=["BR-008"], steps=[2])
async def test_knob_turn_with_pages_shows_next_page_and_sends_nothing():
    pad = paged()
    assert pad.screen()[0].startswith("HOME")
    await pad.turn(UP)
    assert pad.screen()[0].startswith("GAME")
    await pad.turn(DOWN, 2)
    assert pad.screen()[0].startswith("SYSTEM")
    await asyncio.wait_for(pad.actions.drain(), 2.0)
    assert pad.sent() == [] and pad.cues.mock_calls == []


@pytest.mark.uc("UC-016", flow="A5", br=["BR-008", "BR-004"], steps=[3, 4])
async def test_press_and_turn_sets_volume_and_short_press_mutes_at_release():
    pad = paged()
    await pad.key(KNOB_PRESS, PRESS)
    pad.t += 200  # past the push jitter
    await pad.turn(UP)
    await pad.key(KNOB_PRESS, RELEASE)
    await asyncio.wait_for(pad.actions.drain(), 2.0)
    assert pad.sent() == [
        {"command": "step_setting", "option": "audio_volume.master", "direction": "up", "steps": "1", "cue": "none"}
    ]
    await pad.key(KNOB_PRESS, PRESS)
    assert len(pad.sent()) == 1  # no mute at the press
    await pad.key(KNOB_PRESS, RELEASE)
    assert pad.sent()[-1]["direction"] == "down"  # the mute: `normal` down to `mute`


@pytest.mark.uc("UC-016", flow="main", br=["BR-008"], steps=[6])
@pytest.mark.parametrize(("version", "link_up"), [(1, True), (3, False)])
async def test_without_pages_the_knob_is_volume_and_mutes_at_press(version, link_up):
    """M8 / FR-240: protocol 1 with the link up, or the link down, keeps UC-015's knob."""
    pad = paged(version, link_up)
    await pad.turn(UP)
    await asyncio.wait_for(pad.actions.drain(), 2.0)
    assert [d["option"] for d in pad.sent()] == ["audio_volume.master"]
    await pad.key(KNOB_PRESS, PRESS)
    assert len(pad.sent()) == 2


@pytest.mark.uc("UC-016", flow="A1", br=["BR-008"])
async def test_a_raising_pages_check_falls_back_to_the_volume_knob():
    pad = paged()

    class Broken(DisplayModel):
        @property
        def pages_active(self):
            raise RuntimeError("display bug")

    pad.model.__class__ = Broken
    await pad.turn(UP)
    await asyncio.wait_for(pad.actions.drain(), 2.0)
    assert [d["option"] for d in pad.sent()] == ["audio_volume.master"]


@pytest.mark.uc("UC-016", flow="A6", br=["BR-009"])
async def test_a_held_key_pauses_auto_paging():
    pad = paged()
    await pad.key(K4, PRESS)
    pad.clock.now += 6.0
    assert pad.screen()[0].startswith("PAD")  # the K4 lock stage over the page, which stays
    assert pad.model._pager.page == 0
    await pad.key(K4, RELEASE, dt=6000)  # the lock: no result screen of its own
    pad.clock.now += 4.9
    assert pad.screen()[0].startswith("HOME")  # a full period from the release
    pad.clock.now += 0.1
    assert pad.screen()[0].startswith("GAME")
