"""The page keys under lost events, slow reads, two pads and races (UC-016 A10, BR-011, BR-014; review of #2754)."""

import asyncio
from unittest.mock import MagicMock, create_autospec

import pytest

from proto import menu_pb2
from services.operator_pad import page_keys
from services.operator_pad.actions import Actions
from services.operator_pad.config import PadConfig, Timings
from services.operator_pad.controller import REFUSED_BINDING, PadController
from services.operator_pad.cues import CuePlayer
from services.operator_pad.gestures import (
    AGENT_HOLD,
    CANCEL,
    CONFIRM,
    HOLD,
    LAYER,
    LOCK,
    PRESS,
    RELEASE,
    RESET,
    TAP,
    TRANSITION,
    Gesture,
    GestureMachine,
    KeyEvent,
)
from services.operator_pad.pages import PAGES
from services.operator_pad.state import MenuStatusReader
from services.operator_pad.tests.fakes import MenuStubSpec
from services.operator_pad.tests.test_page_keys import CODE, KEYMAP, model, on, st


class Rig:
    """The real controller and gesture machine; the reads and the actions are fakes the test steers."""

    def __init__(self, display=None) -> None:
        self.config = create_autospec(PadConfig, instance=True)
        self.config.enabled.return_value = True
        self.config.timings.return_value = Timings()
        self.reader = create_autospec(MenuStatusReader, instance=True)
        self.reader.fetch.return_value = st()
        self.actions = create_autospec(Actions, instance=True)
        self.actions.dispatch.return_value = ("none", "noop")
        self.cues = create_autospec(CuePlayer, instance=True)
        self.pad = PadController(self.config, self.reader, self.actions, self.cues, display=display, sleep=self._never)
        self.pad.attached(KEYMAP)

    @staticmethod
    async def _never(_seconds):
        await asyncio.Event().wait()  # the hold timer: the test calls `agent_due` itself

    async def key(self, name: str, value: int, t: int, source=None) -> None:
        await self.pad.on_code(CODE[name], value, t, source=source)

    def gestures(self) -> list[Gesture]:
        return [c.args[0] for c in self.actions.dispatch.await_args_list]

    def kinds(self) -> list[str]:
        return [g.kind for g in self.gestures()]


async def settle():
    for _ in range(10):
        await asyncio.sleep(0)


# --- BLOCKER 1: SYN_DROPPED mid K5 hold ----------------------------------------------------------


@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
async def test_events_dropped_mid_k5_hold_keeps_the_stop_at_the_hold_time():
    rig = Rig()
    await rig.key("K5", PRESS, 0)
    rig.pad.events_dropped()
    assert rig.pad._agent_timer is not None and not rig.pad._agent_timer.cancelled()
    await rig.pad.agent_due(1000)
    await rig.pad.drain()
    assert rig.gestures() == [Gesture(AGENT_HOLD, "K5", held_ms=1000, clean=False)]  # stop-only
    await rig.key("K5", RELEASE, 1500)
    assert rig.kinds() == [AGENT_HOLD, HOLD]


@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
def test_a_dropped_machine_keeps_only_k5_and_makes_it_unclean():
    machine = GestureMachine()
    machine.feed(KeyEvent("K1", PRESS, 0), Timings())
    machine.feed(KeyEvent("K5", PRESS, 10), Timings())
    machine.dropped()
    assert machine.down("K5") and not machine.down("K1")
    assert machine.due(1010)[0].clean is False


# --- MAJOR 2 and 3: the release is settled before any read -----------------------------------------


@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
async def test_a_k5_release_before_the_hold_time_wins_over_a_slow_release_read():
    """Released at 900 ms; its status read is still out at 1000 ms: a cancel, never a stop or a resume."""
    rig = Rig()
    gate = asyncio.Event()
    reads = iter([None, gate])

    async def fetch():
        wait = next(reads)
        if wait is not None:
            await wait.wait()
        return st(agent_enabled=False, agent_level="none")

    rig.reader.fetch.side_effect = fetch
    await rig.key("K5", PRESS, 0)
    release = asyncio.create_task(rig.key("K5", RELEASE, 900))
    await settle()
    await rig.pad.agent_due(1000)
    gate.set()
    await release
    await rig.pad.drain()
    assert rig.kinds() == [CANCEL]


@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
async def test_a_k5_tap_whose_press_read_is_slow_is_still_a_tap():
    rig = Rig()
    gate = asyncio.Event()
    reads = iter([gate, None])

    async def fetch():
        wait = next(reads)
        if wait is not None:
            await wait.wait()
        return st()

    rig.reader.fetch.side_effect = fetch
    press = asyncio.create_task(rig.key("K5", PRESS, 0))
    await settle()
    release = asyncio.create_task(rig.key("K5", RELEASE, 100))
    await settle()
    gate.set()
    await asyncio.gather(press, release)
    await rig.pad.agent_due(1100)
    assert rig.kinds() == [TAP]
    assert not rig.pad.machine.down("K5")


# --- MINOR 9: a second hold while the first stop is in flight -------------------------------------


@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
async def test_a_k5_hold_while_a_stop_is_in_flight_is_stop_only():
    rig = Rig()
    gate = asyncio.Event()
    holds = []

    async def dispatch(gesture, *_args):
        if gesture.kind == AGENT_HOLD:
            holds.append(gesture)
            if len(holds) == 1:
                await gate.wait()  # the first stop, waiting for the menu
        return ("agent_switch", "ok")

    rig.actions.dispatch.side_effect = dispatch
    await rig.key("K5", PRESS, 0)
    first = asyncio.create_task(rig.pad.agent_due(1000))
    await settle()
    await rig.key("K5", RELEASE, 1100)
    rig.reader.fetch.return_value = st(agent_enabled=False)
    await rig.key("K5", PRESS, 2000)
    await rig.key("K5", RELEASE, 3100)
    gate.set()
    await first
    second = [g for g in rig.gestures() if g.key == "K5"][2:]
    assert second and all(g.clean is False for g in second)


# --- MAJOR 4, MINOR 5, MINOR 6: the agent commands ---------------------------------------------------


@pytest.fixture
def menu():
    stub = create_autospec(MenuStubSpec, instance=True)
    stub.ProcessInput.return_value = menu_pb2.ProcessInputResponse(success=True, value="x")
    return stub


@pytest.fixture
def cues():
    return create_autospec(CuePlayer, instance=True)


@pytest.fixture
def actions(menu, cues):
    return Actions(menu, None, cues)


@pytest.mark.uc("UC-016", flow="A8", br=["BR-013", "BR-014"])
async def test_a_level_reset_needs_a_menu_that_checks_from(actions, menu, cues):
    """MAJOR 4: without `reset_setting.from` a level that dropped under the hold could commit a raise."""
    old = st(agent_level="full", pad_commands=["reset_setting", "interventions_switch", "step_setting.from"])
    result = await actions.dispatch(Gesture(RESET, "K4", held_ms=1200), old, old, on("AGENT", "K4"))
    assert result == ("reset_setting", "refused")
    menu.ProcessInput.assert_not_awaited()
    cues.refusal.assert_awaited_once()


@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
@pytest.mark.parametrize(
    ("gesture", "binding"),
    [
        (Gesture(LAYER, "K3", steps=-1), on("AGENT", "K3")),
        (Gesture(LAYER, "K1", steps=-1), on("AGENT", "K1")),
        (Gesture(RESET, "K4", held_ms=1200, confirmed=True), on("AGENT", "K4")),
    ],
)
async def test_agent_commands_without_the_press_read_are_refused(actions, menu, gesture, binding):
    """MINOR 5: never judged from the release read alone."""
    _, result = await actions.dispatch(gesture, st(), None, binding)
    assert result == "refused"
    menu.ProcessInput.assert_not_awaited()


@pytest.mark.uc("UC-016", flow="A7", br=["BR-014"])
async def test_a_level_step_past_full_sends_nothing_and_says_maximum(actions, menu, cues):
    full = st(agent_level="full")
    result = await actions.dispatch(Gesture(LAYER, "K3", steps=1), full, full, on("AGENT", "K3"))
    assert result == ("step_setting", "at_limit")
    menu.ProcessInput.assert_not_awaited()
    cues.limit.assert_awaited_once_with(True)


@pytest.mark.uc("UC-016", flow="A7", br=["BR-011", "BR-014"])
async def test_a_level_step_needs_a_menu_that_checks_from(actions, menu):
    old = st(agent_level="full", pad_commands=["reset_setting", "reset_setting.from"])
    result = await actions.dispatch(Gesture(LAYER, "K3", steps=-1), old, old, on("AGENT", "K3"))
    assert result == ("step_setting", "refused")
    menu.ProcessInput.assert_not_awaited()


# --- MINOR 7: a lost knob release -------------------------------------------------------------------


@pytest.mark.uc("UC-016", br=["BR-011"])
def test_a_lost_knob_release_counts_as_up_after_30_s():
    machine = GestureMachine()
    machine.feed(KeyEvent("KNOB_PRESS", PRESS, 0), Timings())  # its release never comes
    machine.feed(KeyEvent("K2", PRESS, 30_000), Timings())
    assert machine.feed(KeyEvent("K2", RELEASE, 30_100), Timings()) == [Gesture(TAP, "K2", held_ms=100)]


# --- MINOR 8 and the deviations: the uncertain link and K4's lock stage ---------------------------


def refusing_display():
    display = MagicMock(display_usb=None, pages_active=True)
    display.bind_page.return_value = REFUSED_BINDING
    return display


@pytest.mark.uc("UC-016", br=["BR-011", "BR-015"])
async def test_the_k4_lock_works_while_the_row_keys_are_refused():
    rig = Rig(refusing_display())
    await rig.key("K4", PRESS, 0)
    await rig.key("K4", RELEASE, 3200)
    assert rig.kinds() == [LOCK]
    assert rig.pad.machine.locked


@pytest.mark.uc("UC-016", br=["BR-011"])
async def test_the_transition_cue_plays_while_the_row_keys_are_refused():
    rig = Rig(refusing_display())
    await rig.key("K3", PRESS, 0)
    await rig.key("K3", RELEASE, 1200)
    assert rig.kinds() == [TRANSITION]


@pytest.mark.uc("UC-016", flow="A8", br=["BR-011", "BR-015"])
async def test_k4_released_in_the_lock_guard_band_cues_on_a_row_without_a_reset(actions, cues):
    gesture = Gesture(CANCEL, "K4", held_ms=3050, stage="lock")
    assert await actions.dispatch(gesture, st(), st(), on("HOME", "K4")) == ("cancel", "cancelled")
    cues.cancel.assert_awaited_once()


@pytest.mark.uc("UC-016", flow="A8", br=["BR-015"])
def test_the_lock_stage_tag_marks_only_the_lock_guard_band():
    machine = GestureMachine()
    machine.feed(KeyEvent("K4", PRESS, 0), Timings())
    assert machine.feed(KeyEvent("K4", RELEASE, 3050), Timings()) == [Gesture(CANCEL, "K4", held_ms=3050, stage="lock")]
    machine.feed(KeyEvent("K4", PRESS, 5000), Timings())
    assert machine.feed(KeyEvent("K4", RELEASE, 6050), Timings())[0].stage == ""


# --- MINOR 10: two pads ------------------------------------------------------------------------------


@pytest.mark.uc("UC-016", br=["BR-011", "BR-016"])
async def test_two_pads_with_no_identity_bind_the_fallback_page():
    display = MagicMock(display_usb=None, pages_active=True)
    display.bind_page.return_value = (PAGES.index("AGENT"), False)
    rig = Rig(display)
    rig.pad.pad_devices = 2
    await rig.key("K3", PRESS, 0)
    await rig.key("K3", RELEASE, 100)
    assert [c.args[3] for c in rig.actions.dispatch.await_args_list] == [
        page_keys.binding("K3", page_keys.FALLBACK_PAGE, fallback=True)
    ]


@pytest.mark.uc("UC-016", flow="A9", br=["BR-011", "BR-014"])
async def test_a_knob_press_on_another_pad_never_confirms_this_pads_key():
    rig = Rig()
    await rig.key("K3", PRESS, 0, source="pad-a")
    await rig.key("KNOB_TURN_B", PRESS, 10, source="pad-a")
    await rig.key("KNOB_PRESS", PRESS, 500, source="pad-b")
    await rig.key("KNOB_PRESS", RELEASE, 550, source="pad-b")
    assert CONFIRM not in rig.kinds()
    assert not rig.pad.machine.keys_down() or rig.pad.machine.down("K3")
    await rig.key("K3", RELEASE, 600, source="pad-a")
    assert rig.gestures()[-1] == Gesture(LAYER, "K3", steps=1, held_ms=600)


# --- MINOR 11: the binding at the event's time ---------------------------------------------------------


@pytest.mark.uc("UC-016", br=["BR-011"])
def test_the_page_binds_at_the_key_events_time_not_when_it_is_processed():
    m, clock = model()
    m.page_step(1)  # GAME, acknowledged now
    m.current(clock.now)
    m.acknowledged()
    pressed = clock.now + 0.2  # the key went down 200 ms after the page showed ...
    clock.now += 0.5  # ... and is processed 500 ms after
    m.acknowledged()
    assert m.bind_page(pressed) == (PAGES.index("HOME"), False)
    assert m.bind_page() == (PAGES.index("GAME"), False)


@pytest.mark.uc("UC-016", br=["BR-011"])
async def test_the_controller_binds_with_the_events_time():
    display = MagicMock(display_usb=None, pages_active=True)
    display.bind_page.return_value = (PAGES.index("GAME"), False)
    rig = Rig(display)
    await rig.key("K1", PRESS, 12_345)
    display.bind_page.assert_called_once_with(12.345)


@pytest.mark.uc("UC-016", flow="A8", br=["BR-007"])
def test_the_reset_screen_takes_the_press_read_when_it_arrives():
    m, clock = model()
    m.key_down("K4", int(clock.now * 1000) - 1100, Timings(), on("AGENT", "K4"), None)
    assert "?" in m.current(clock.now).lines[1][0]
    m.press_status("K4", st(agent_level="full"))
    assert "\x7f" * 4 in m.current(clock.now).lines[1][0]  # NOW: the full bar of FULL
