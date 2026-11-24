"""The page keys (UC-016 A7 to A10, BR-011 to BR-016): rows, gestures, raises, the K5 stop, the screens."""

import asyncio
import itertools
from unittest.mock import MagicMock, create_autospec, patch

import pytest

from proto import menu_pb2
from services.operator_pad import config as config_module
from services.operator_pad import page_keys, render, screens
from services.operator_pad.actions import Actions
from services.operator_pad.config import DEFAULT_KEYMAP, PadConfig, Timings, parse_keymap
from services.operator_pad.controller import REFUSED_BINDING, PadController
from services.operator_pad.cues import CuePlayer
from services.operator_pad.display import DisplayModel
from services.operator_pad.gestures import (
    AGENT_HOLD,
    CANCEL,
    CONFIRM,
    HOLD,
    KNOB_PRESS_GESTURE,
    LAYER,
    LAYER_STEP,
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
from services.operator_pad.pages import PAGES, PageContext, page_rows
from services.operator_pad.state import MenuStatusReader
from services.operator_pad.tests.display_fakes import pad_config
from services.operator_pad.tests.fakes import CoordinatorStubSpec, MenuStubSpec

ALL_COMMANDS = [
    "reset_setting",
    "reset_setting.from",
    "interventions_switch",
    "agent_switch.only_if_stopped",
    "step_setting.from",
]
DEFAULTS = {
    "agent_level": "standard",
    "game_instructions": "off",
    "force_all_start": "off",
    "decision.backend": "rules",
}


def st(**kw) -> menu_pb2.GetMenuStatusResponse:
    base = {
        "lifecycle_live": False,
        "agent_enabled": True,
        "agent_level": "ambient",
        "agent_backend": "claude",
        "sensitivity": 2,
        "num_teams": 2,
        "audio_volume_master": "normal",
        "pad_commands": ALL_COMMANDS,
        "setting_defaults": DEFAULTS,
    }
    base.update(kw)
    base.setdefault("agent_enabled_state", "on" if base["agent_enabled"] else "off")
    base.setdefault("agent_interventions_enabled_state", "on")
    return menu_pb2.GetMenuStatusResponse(**base)


def on(page: str, key: str) -> page_keys.Binding:
    return page_keys.binding(key, PAGES.index(page))


# --- rows and their keys (BR-012) ----------------------------------------------------------------

# The page row label (BR-006, protocol 3) of each editable option.
ROW_LABELS = {
    "current_game": "MODE",
    "audio_volume.master": "VOLUME",
    "sensitivity": "SENS",
    "num_teams": "TEAMS",
    "shield.activation": "SHIELD",
    "game_instructions": "INSTRUCTIONS",
    "force_all_start": "FORCE ALL",
    page_keys.INTERVENTIONS: "AGENT",
    "agent_level": "LEVEL",
    "decision.backend": "BACKEND",
    "goal.profile": "GOAL",
    "policy.max_interventions_per_minute": "RATE",
    "policy.group_grant.enabled": "GROUP",
    "inference.backend": "RETRO AI",
    "inference.effort": "EFFORT",
    "decision.operator_brief": "BRIEF",
}


@pytest.mark.uc("UC-016", flow="A7", br=["BR-011", "BR-012"])
@pytest.mark.parametrize("page", PAGES)
def test_each_row_key_binds_the_setting_its_row_shows(page):
    """Drift guard: the key table and the page rows of BR-006 name the same settings, row by row."""
    rows = page_rows(PAGES.index(page), st(), PageContext(), 3)
    for key, (row, _role) in page_keys.ROW_KEYS.items():
        spec = page_keys.binding(key, PAGES.index(page)).spec
        if spec is not None:
            assert rows[row][0] == ROW_LABELS[spec.option], (page, key)


@pytest.mark.uc("UC-016", br=["BR-011", "BR-012", "BR-013"])
@pytest.mark.parametrize(
    ("page", "key", "active"),
    [
        ("HOME", "K1", True),
        ("HOME", "K2", False),  # the mode has no reset
        ("HOME", "K4", False),  # nor the volume
        ("HOME", "K7", True),
        ("START", "K3", False),  # empty row
        ("AGENT", "K2", False),  # the interventions switch has no reset
        ("AGENT", "K4", True),
        ("AGENT", "K6", True),  # GOAL, settable from iteration 4
        ("AGENT", "K7", True),
        ("INTERV", "K3", True),  # RATE
        ("INTERV", "K7", True),  # GROUP
        ("RETRO", "K1", True),
        ("RETRO", "K4", True),
        ("RETRO", "K7", True),
        ("SYSTEM", "K6", False),
        ("SYSTEM", "K2", False),
    ],
)
def test_read_only_empty_and_resetless_rows_have_no_key(page, key, active):
    assert on(page, key).active is active


@pytest.mark.uc("UC-016", br=["BR-016"])
def test_without_a_screen_the_row_keys_act_on_home():
    binding = page_keys.binding("K3", page_keys.FALLBACK_PAGE, fallback=True)
    assert (PAGES[binding.page], binding.spec.option, binding.fallback) == ("HOME", "audio_volume.master", True)


# --- raises (BR-014) -----------------------------------------------------------------------------

LEVEL = page_keys.ROWS["AGENT"][1]
SWITCH = page_keys.ROWS["AGENT"][0]


@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
@pytest.mark.parametrize(
    ("status", "steps", "raises"),
    [
        (st(agent_level="ambient"), 1, True),
        (st(agent_level="standard"), -1, False),
        (st(agent_level="full"), 1, False),  # clamped at the top: no raise
        (st(agent_level="standard", agent_interventions_enabled_state="off"), 1, True),  # WATCH: its live level
        (st(agent_enabled=False, agent_saved_level="ambient"), 1, True),  # OFF: the saved level is what K5 resumes
    ],
)
def test_a_higher_rung_of_the_level_is_a_raise(status, steps, raises):
    assert page_keys.edit_plan(LEVEL, status, steps).raises is raises


@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
@pytest.mark.parametrize(
    "status",
    [st(agent_level=""), st(agent_level="probe"), st(agent_enabled_state="unknown"), None],
    ids=["empty", "probe", "enabled-unknown", "no-status"],
)
def test_an_unknown_level_is_refused_never_confirmed(status):
    assert page_keys.edit_plan(LEVEL, status, 1).refused


@pytest.mark.uc("UC-016", flow="A9", br=["BR-012", "BR-014"])
def test_interventions_on_is_a_raise_off_is_not_and_an_off_agent_is_refused():
    gate_off = st(agent_interventions_enabled_state="off")
    assert page_keys.edit_plan(SWITCH, gate_off, 1) == page_keys.Plan("off", "on", raises=True)
    assert page_keys.edit_plan(SWITCH, st(), -1) == page_keys.Plan("on", "off")
    assert page_keys.edit_plan(SWITCH, st(agent_enabled=False), 1).refused
    unknown_gate = st(agent_interventions_enabled_state="unknown")
    assert page_keys.edit_plan(SWITCH, unknown_gate, 1).raises  # ON is known: still a confirmation


@pytest.mark.uc("UC-016", flow="A8", br=["BR-013", "BR-014"])
@pytest.mark.parametrize(
    ("status", "plan"),
    [
        (st(agent_level="ambient"), page_keys.Plan("ambient", "standard", raises=True)),
        (st(agent_level="standard"), page_keys.Plan("standard", "standard")),  # equal: `>` not `>=`
        (st(agent_level="full"), page_keys.Plan("full", "standard")),
        (st(agent_enabled=False, agent_saved_level="full"), page_keys.Plan("full", "standard")),
        (st(agent_enabled=False), page_keys.Plan("ambient", "standard", raises=True)),  # no saved level: ambient
    ],
)
def test_a_level_reset_raises_only_above_the_level_it_replaces(status, plan):
    assert page_keys.reset_plan(LEVEL, status) == plan


@pytest.mark.uc("UC-016", flow="A8", br=["BR-013", "BR-014"])
@pytest.mark.parametrize(
    "status",
    [
        st(setting_defaults={"agent_level": "probe"}),
        st(setting_defaults={"agent_level": "shadow_experimental"}),
        st(setting_defaults={}),
        st(agent_level="bogus"),
        st(agent_interventions_enabled_state="unknown"),
    ],
    ids=["probe", "shadow-experimental", "default-unknown", "level-unknown", "state-unknown"],
)
def test_an_off_ladder_or_unknown_level_default_is_refused(status):
    assert page_keys.reset_plan(LEVEL, status).refused


# --- the commit gate (BR-014) ------------------------------------------------------------------


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
    return Actions(menu, create_autospec(CoordinatorStubSpec, instance=True), cues)


def sent(menu) -> list[dict]:
    return [dict(c.args[0].data) for c in menu.ProcessInput.await_args_list]


@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
async def test_an_unconfirmed_level_raise_sends_nothing_and_cancels(actions, menu, cues):
    status = st(agent_level="ambient")
    result = await actions.dispatch(Gesture(LAYER, "K3", steps=1), status, status, on("AGENT", "K3"))
    assert result == ("step_setting", "cancelled")
    menu.ProcessInput.assert_not_awaited()
    cues.cancel.assert_awaited_once()


@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
async def test_a_confirmed_level_raise_commits_with_from(actions, menu):
    status = st(agent_level="ambient")
    await actions.dispatch(Gesture(LAYER, "K3", steps=2, confirmed=True), status, status, on("AGENT", "K3"))
    assert sent(menu) == [
        {"command": "step_setting", "option": "agent_level", "direction": "up", "steps": "2", "from": "ambient"}
    ]


@pytest.mark.uc("UC-016", flow="A7", br=["BR-014"])
async def test_a_lowering_commits_without_a_confirmation(actions, menu):
    status = st(agent_level="full")
    await actions.dispatch(Gesture(LAYER, "K3", steps=-1), status, status, on("AGENT", "K3"))
    assert sent(menu)[0]["direction"] == "down"


@pytest.mark.uc("UC-016", flow="A7", br=["BR-014"])
async def test_an_unknown_level_edit_is_refused(actions, menu, cues):
    status = st(agent_level="probe")
    result = await actions.dispatch(Gesture(LAYER, "K3", steps=-1, confirmed=True), status, status, on("AGENT", "K3"))
    assert result == ("step_setting", "refused")
    menu.ProcessInput.assert_not_awaited()
    cues.refusal.assert_awaited_once()


@pytest.mark.uc("UC-016", flow="A9", br=["BR-012", "BR-014"])
async def test_interventions_on_needs_the_confirmation_off_is_immediate(actions, menu):
    watch = st(agent_interventions_enabled_state="off")
    assert await actions.dispatch(Gesture(LAYER, "K1", steps=1), watch, watch, on("AGENT", "K1")) == (
        "interventions_switch",
        "cancelled",
    )
    await actions.dispatch(Gesture(LAYER, "K1", steps=1, confirmed=True), watch, watch, on("AGENT", "K1"))
    await actions.dispatch(Gesture(LAYER, "K1", steps=-1), st(), st(), on("AGENT", "K1"))
    assert sent(menu) == [
        {"command": "interventions_switch", "state": "on"},
        {"command": "interventions_switch", "state": "off"},
    ]


@pytest.mark.uc("UC-016", br=["BR-012"])
async def test_interventions_switch_of_an_off_agent_is_refused(actions, menu, cues):
    """BR-012: the resume restores the gate of the stop, so a switch now would be undone."""
    off = st(agent_enabled=False)
    result = await actions.dispatch(Gesture(LAYER, "K1", steps=1, confirmed=True), off, off, on("AGENT", "K1"))
    assert result == ("interventions_switch", "refused")
    menu.ProcessInput.assert_not_awaited()
    cues.refusal.assert_awaited_once()


@pytest.mark.uc("UC-016", br=["BR-011"])
@pytest.mark.parametrize(
    ("gesture", "binding"),
    [
        (Gesture(LAYER, "K1", steps=-1), on("AGENT", "K1")),
        (Gesture(RESET, "K7", held_ms=1200), on("HOME", "K7")),
    ],
)
async def test_an_old_menu_without_the_command_is_refused_with_a_cue(actions, menu, cues, gesture, binding):
    """BR-011 "Menu too old": an unknown web_command would answer success with no value."""
    old = st(pad_commands=[])
    _, result = await actions.dispatch(gesture, old, old, binding)
    assert result == "refused"
    menu.ProcessInput.assert_not_awaited()
    cues.refusal.assert_awaited_once()


@pytest.mark.uc("UC-016", br=["BR-011", "BR-013"])
async def test_a_reset_answered_without_a_value_is_a_failure_never_ok(actions, menu, cues):
    menu.ProcessInput.return_value = menu_pb2.ProcessInputResponse(success=True)
    result = await actions.dispatch(Gesture(RESET, "K7", held_ms=1200), st(), st(), on("HOME", "K7"))
    assert result == ("reset_setting", "failed")
    cues.refusal.assert_awaited_once()


@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
def test_only_the_gated_builders_send_a_raise():
    """Sweep: every edit and reset of the AGENT page, unconfirmed, from every agent state: no raise is sent."""
    levels = ["none", "ambient", "standard", "full", "probe", ""]
    statuses = [
        st(agent_level=level, agent_enabled=enabled, agent_interventions_enabled_state=gate, agent_saved_level=saved)
        for level, enabled, gate, saved in itertools.product(levels, (True, False), ("on", "off"), ("", "ambient"))
    ]

    async def sweep():
        for status, steps, key in itertools.product(statuses, (-2, -1, 1, 2, 3), ("K1", "K3")):
            menu = create_autospec(MenuStubSpec, instance=True)
            menu.ProcessInput.return_value = menu_pb2.ProcessInputResponse(success=True, value="x")
            actions = Actions(menu, None, create_autospec(CuePlayer, instance=True))
            await actions.dispatch(Gesture(LAYER, key, steps=steps), status, status, on("AGENT", key))
            await actions.dispatch(Gesture(RESET, "K4", held_ms=1200), status, status, on("AGENT", "K4"))
            for data in sent(menu):
                assert data.get("state") != "on", (status, steps)
                if data.get("option") == "agent_level" and data["command"] == "step_setting":
                    now = page_keys.row_preview("agent_level", status, 0).value
                    new = page_keys.row_preview("agent_level", status, steps).value
                    assert page_keys.rank(new) <= page_keys.rank(now), (status, steps)
                if data["command"] == "reset_setting":
                    plan = page_keys.reset_plan(LEVEL, status)
                    assert not plan.raises and not plan.refused, status

    asyncio.run(sweep())


@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
@pytest.mark.parametrize(
    ("binding", "status", "expected"),
    [
        (on("HOME", "K7"), st(), {"option": "sensitivity", "expect": "automatic"}),
        (on("GAME", "K7"), st(), {"option": "game_instructions", "expect": "off"}),
        (on("INTERV", "K2"), st(lifecycle_live=True), {"option": "decision.backend", "expect": "rules"}),
        (on("AGENT", "K4"), st(agent_level="full"), {"option": "agent_level", "expect": "standard", "from": "full"}),
    ],
)
async def test_a_reset_sends_reset_setting_with_the_value_it_showed(actions, menu, binding, status, expected):
    await actions.dispatch(Gesture(RESET, "K7", held_ms=1200), status, status, binding)
    assert sent(menu) == [{"command": "reset_setting", **expected}]


@pytest.mark.uc("UC-016", flow="A8", br=["BR-013", "BR-014"])
async def test_a_level_reset_above_the_level_needs_the_confirmation(actions, menu):
    status = st(agent_level="none", agent_enabled=True, agent_saved_level="ambient")
    assert await actions.dispatch(Gesture(RESET, "K4"), status, status, on("AGENT", "K4")) == (
        "reset_setting",
        "cancelled",
    )
    await actions.dispatch(Gesture(RESET, "K4", confirmed=True), status, status, on("AGENT", "K4"))
    assert sent(menu) == [
        {"command": "reset_setting", "option": "agent_level", "expect": "standard", "from": "ambient"}
    ]


@pytest.mark.uc("UC-016", flow="A8", br=["BR-013", "BR-014"])
@pytest.mark.parametrize("default", ["probe", "shadow_experimental"])
async def test_an_off_ladder_level_default_is_refused_not_possible(actions, menu, cues, default):
    status = st(setting_defaults={"agent_level": default})
    result = await actions.dispatch(Gesture(RESET, "K4", confirmed=True), status, status, on("AGENT", "K4"))
    assert result == ("reset_setting", "refused")
    menu.ProcessInput.assert_not_awaited()
    cues.refusal.assert_awaited_once()


@pytest.mark.uc("UC-016", flow="A8", br=["BR-011", "BR-013"])
async def test_a_lobby_only_reset_is_refused_while_live(actions, menu):
    live = st(lifecycle_live=True)
    assert await actions.dispatch(Gesture(RESET, "K1"), live, live, on("GAME", "K2")) == ("reset_setting", "refused")
    menu.ProcessInput.assert_not_awaited()


@pytest.mark.uc("UC-016", br=["BR-011"])
@pytest.mark.parametrize(
    "gesture", [Gesture(RESET, "K2", held_ms=1200), Gesture(CANCEL, "K2", held_ms=600), Gesture(TAP, "K2")]
)
async def test_keys_of_a_row_without_that_key_are_silent(actions, menu, cues, gesture):
    """The mode has no reset: K2 on HOME sends nothing, plays nothing."""
    assert await actions.dispatch(gesture, st(), st(), on("HOME", "K2")) == ("none", "noop")
    menu.ProcessInput.assert_not_awaited()
    assert cues.mock_calls == []


@pytest.mark.uc("UC-016", flow="A8", br=["BR-011"])
async def test_a_reset_key_released_before_its_reset_plays_the_cancel_cue(actions, cues):
    """Maintainer decision D5: on a row with a reset."""
    assert await actions.dispatch(Gesture(CANCEL, "K7", held_ms=600), st(), st(), on("HOME", "K7")) == (
        "cancel",
        "cancelled",
    )
    cues.cancel.assert_awaited_once()


@pytest.mark.uc("UC-016", br=["BR-011"])
async def test_an_edit_key_held_like_the_old_kill_plays_the_transition_cue(actions, menu, cues):
    display = MagicMock()
    actions = Actions(menu, None, cues, display=display)
    await actions.dispatch(Gesture(TRANSITION, "K3", held_ms=1200), st(), st(), on("AGENT", "K3"))
    cues.unknown_key.assert_awaited_once()
    cues.cancel.assert_not_awaited()
    display.result.assert_called_with("", "AGENT IS K5")
    menu.ProcessInput.assert_not_awaited()


@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
async def test_a_reset_tap_shows_the_default(menu, cues):
    display = MagicMock()
    actions = Actions(menu, None, cues, display=display)
    await actions.dispatch(Gesture(TAP, "K4"), st(), st(), on("AGENT", "K4"))
    display.result.assert_called_with("AGENT LEVEL", "DEFAULT STANDARD")
    menu.ProcessInput.assert_not_awaited()


@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
@pytest.mark.parametrize(("locked", "clean"), [(False, False), (True, False)], ids=["unclean", "locked"])
async def test_an_unclean_or_locked_k5_hold_of_a_stopped_agent_stops_at_the_hold_time(actions, menu, locked, clean):
    off = st(agent_enabled=False)
    gesture = Gesture(AGENT_HOLD, "K5", held_ms=1000, locked=locked, clean=clean)
    assert await actions.dispatch(gesture, None, off) == ("agent_switch", "ok")
    assert sent(menu) == [{"command": "agent_switch", "direction": "stop"}]
    await actions.dispatch(Gesture(HOLD, "K5", held_ms=1500, locked=locked, clean=clean), off, off)
    assert len(sent(menu)) == 1  # the release does nothing more


# --- gestures (BR-011, BR-014, BR-015) ---------------------------------------------------------------

T = Timings()  # tap 400, reset 1000, K5 1000, K8 1500, lock 3000


class Pad:
    def __init__(self) -> None:
        self.machine = GestureMachine()

    def ev(self, name: str, value: int, t: int) -> list[Gesture]:
        return self.machine.feed(KeyEvent(name, value, t), T)

    def hold(self, key: str, t0: int, t1: int) -> list[Gesture]:
        self.ev(key, PRESS, t0)
        return self.ev(key, RELEASE, t1)


@pytest.mark.uc("UC-016", flow="A8", br=["BR-011", "BR-015"])
@pytest.mark.parametrize(
    ("released", "kind"),
    [
        (400, TAP),
        (401, CANCEL),
        (1149, CANCEL),  # the guard band after the reset boundary
        (1150, RESET),
        (2999, RESET),
        (3000, CANCEL),  # the guard band after the lock boundary
        (3149, CANCEL),
        (3150, LOCK),
    ],
)
def test_k4_stages_and_their_guard_bands(released, kind):
    assert [g.kind for g in Pad().hold("K4", 0, released)] == [kind]


@pytest.mark.uc("UC-016", flow="A8", br=["BR-011"])
@pytest.mark.parametrize("key", ["K2", "K7"])
def test_k2_and_k7_reset_with_no_lock_stage(key):
    assert [g.kind for g in Pad().hold(key, 0, 5000)] == [RESET]


@pytest.mark.uc("UC-016", flow="A9", br=["BR-015"])
def test_a_confirmed_k4_reset_has_no_lock_stage():
    pad = Pad()
    pad.ev("K4", PRESS, 0)
    assert pad.ev("KNOB_PRESS", PRESS, 1200) == [Gesture(CONFIRM, "K4")]
    pad.ev("KNOB_PRESS", RELEASE, 1300)
    assert pad.ev("K4", RELEASE, 3500) == [Gesture(RESET, "K4", held_ms=3500, confirmed=True)]
    assert not pad.machine.locked


@pytest.mark.uc("UC-016", flow="A9", br=["BR-015"])
def test_an_unconfirmed_k4_reaching_the_lock_never_resets():
    out = Pad().hold("K4", 0, 4000)
    assert [g.kind for g in out] == [LOCK]


@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
def test_a_knob_press_before_the_reset_stage_is_ignored_never_a_mute():
    pad = Pad()
    pad.ev("K7", PRESS, 0)
    assert pad.ev("KNOB_PRESS", PRESS, 500) == []
    pad.ev("KNOB_PRESS", RELEASE, 550)
    assert pad.ev("K7", RELEASE, 1500) == [Gesture(RESET, "K7", held_ms=1500)]  # not confirmed
    pad.ev("K2", PRESS, 2000)
    pad.ev("KNOB_PRESS", PRESS, 2100)
    pad.ev("KNOB_PRESS", RELEASE, 2150)
    assert pad.ev("K2", RELEASE, 2200) == []  # used: no tap


@pytest.mark.uc("UC-016", br=["BR-011"])
def test_a_turn_with_a_reset_key_down_does_nothing():
    pad = Pad()
    pad.ev("K2", PRESS, 0)
    assert pad.ev("KNOB_TURN_B", PRESS, 100) == []
    assert pad.ev("K2", RELEASE, 200) == [Gesture(TAP, "K2", held_ms=200)]


@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
def test_a_knob_press_with_an_edit_key_is_the_confirmation_not_the_mute():
    pad = Pad()
    pad.ev("K3", PRESS, 0)
    pad.ev("KNOB_TURN_B", PRESS, 100)
    got = pad.ev("KNOB_PRESS", PRESS, 400)
    assert got == [Gesture(CONFIRM, "K3", steps=1)]
    assert KNOB_PRESS_GESTURE not in [g.kind for g in got]
    pad.ev("KNOB_PRESS", RELEASE, 450)
    assert pad.ev("K3", RELEASE, 500) == [Gesture(LAYER, "K3", steps=1, held_ms=500, confirmed=True)]


@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
def test_a_step_just_before_the_press_is_jitter_no_confirmation():
    pad = Pad()
    pad.ev("K3", PRESS, 0)
    pad.ev("KNOB_TURN_B", PRESS, 300)
    assert pad.ev("KNOB_PRESS", PRESS, 549) == []
    assert pad.ev("K3", RELEASE, 700)[0].confirmed is False


@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
def test_turns_while_the_knob_is_down_are_ignored_and_a_later_step_clears_the_confirmation():
    pad = Pad()
    pad.ev("K3", PRESS, 0)
    pad.ev("KNOB_TURN_B", PRESS, 10)
    pad.ev("KNOB_PRESS", PRESS, 400)
    assert pad.ev("KNOB_TURN_B", PRESS, 410) == []  # the push jitter never moves the value
    pad.ev("KNOB_PRESS", RELEASE, 450)
    assert pad.machine.confirmed("K3")
    assert pad.ev("KNOB_TURN_B", PRESS, 500) == [Gesture(LAYER_STEP, "K3", steps=2)]
    assert not pad.machine.confirmed("K3")
    assert pad.ev("K3", RELEASE, 600)[0].confirmed is False


@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
def test_reset_drops_the_confirmation_and_keeps_the_lock():
    pad = Pad()
    pad.machine.locked = True
    pad.machine.locked = False
    pad.ev("K3", PRESS, 0)
    pad.ev("KNOB_TURN_B", PRESS, 10)
    pad.ev("KNOB_PRESS", PRESS, 400)
    pad.machine.locked = True
    pad.machine.reset()
    assert not pad.machine.confirmed("K3") and pad.machine.locked


@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
@pytest.mark.parametrize(
    "before",
    [
        [("KNOB_PRESS", PRESS, 0)],  # the knob already down
        [("K3", PRESS, 0)],  # another key down
        [("K1", PRESS, -40_000 + 31_000)],  # a stuck K1 (its release lost), still in its 30 s
    ],
    ids=["knob-down", "k3-down", "stuck-k1"],
)
@pytest.mark.parametrize("locked", [False, True], ids=["unlocked", "locked"])
def test_the_k5_stop_fires_at_the_hold_time_whatever_else_is_down(before, locked):
    """A chord, the knob, a stuck key or the lock never block the stop (mutation: a chord blocks K5)."""
    pad = Pad()
    pad.machine.locked = locked
    for name, value, t in before:
        pad.ev(name, value, t)
    pad.ev("K5", PRESS, 100)
    assert pad.machine.due(1099) == []
    assert pad.machine.due(1100) == [Gesture(AGENT_HOLD, "K5", held_ms=1000, locked=locked, clean=False)]
    assert pad.machine.due(1200) == []  # once


@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
def test_a_clean_k5_hold_fires_clean_and_its_release_carries_it():
    pad = Pad()
    pad.ev("K5", PRESS, 0)
    assert pad.machine.due(1000) == [Gesture(AGENT_HOLD, "K5", held_ms=1000)]
    assert pad.ev("K5", RELEASE, 1500) == [Gesture(HOLD, "K5", held_ms=1500)]


@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
def test_a_k5_release_past_the_hold_time_before_the_timer_still_fires_the_hold():
    assert [g.kind for g in Pad().hold("K5", 0, 1200)] == [AGENT_HOLD, HOLD]


@pytest.mark.uc("UC-016", br=["BR-011"])
def test_a_key_whose_release_was_lost_counts_as_up_after_30_s():
    pad = Pad()
    pad.ev("K1", PRESS, 0)  # its release never comes
    pad.ev("K2", PRESS, 29_000)
    assert pad.ev("K2", RELEASE, 29_100) == []  # a chord with the stuck key
    pad.ev("K2", PRESS, 30_000)
    assert pad.ev("K2", RELEASE, 30_100) == [Gesture(TAP, "K2", held_ms=100)]


@pytest.mark.uc("UC-016", br=["BR-011"])
def test_an_edit_key_held_past_the_agent_hold_without_a_turn_is_the_transition():
    assert Pad().hold("K3", 0, 1000) == [Gesture(TRANSITION, "K3", held_ms=1000)]


@pytest.mark.uc("UC-016", br=["BR-011", "BR-015"])
def test_locked_only_the_k5_stop_and_the_long_k4_act():
    pad = Pad()
    pad.machine.locked = True
    assert pad.hold("K4", 0, 2000) == []  # no reset stage while locked
    assert pad.hold("K2", 3000, 5000) == []
    assert pad.hold("K8", 6000, 6100) == []
    assert pad.hold("K4", 7000, 10_150) == [Gesture(LOCK, "K4", held_ms=3150, locked=False)]


# --- timings (flags.md §10a) -----------------------------------------------------------------


def _config(values: dict) -> PadConfig:
    client = MagicMock()
    client.get_integer_value.side_effect = lambda key, default: values.get(key, default)
    return PadConfig(client)


@pytest.mark.uc("UC-016", br=["BR-011", "BR-015"])
@pytest.mark.parametrize(
    "values",
    [
        {"operator_pad.hold_reset_ms": 300},  # not above the tap limit
        {"operator_pad.hold_reset_ms": 1600},  # leaves less than the confirmation window to the lock
        {"operator_pad.hold_end_ms": 2900},  # the K8 window shorter than the guard band
    ],
    ids=["reset-below-tap", "confirmation-window", "k8-window"],
)
def test_timings_that_break_a_rule_keep_all_defaults_with_one_warning(values):
    config = _config(values)
    with patch.object(config_module, "logger") as log:
        assert config.timings() == Timings()
        assert config.timings() == Timings()
    log.warning.assert_called_once()


@pytest.mark.uc("UC-016", br=["BR-015"])
def test_valid_timings_are_used():
    timings = _config({"operator_pad.hold_reset_ms": 1500}).timings()
    assert timings.hold_reset_ms == 1500


# --- the controller: binding, paging, the K5 stop (BR-011, A10) ---------------------------------

KEYMAP = parse_keymap(DEFAULT_KEYMAP)
CODE = {name: code for code, name in KEYMAP.items()}


class Rig:
    def __init__(self, display=None, status=None) -> None:
        self.config = create_autospec(PadConfig, instance=True)
        self.config.enabled.return_value = True
        self.config.timings.return_value = Timings()
        self.reader = create_autospec(MenuStatusReader, instance=True)
        self.reader.fetch.return_value = status or st()
        self.actions = create_autospec(Actions, instance=True)
        self.actions.dispatch.return_value = ("none", "noop")
        self.cues = create_autospec(CuePlayer, instance=True)
        self.display = display
        self.pad = PadController(self.config, self.reader, self.actions, self.cues, display=display)
        self.pad.attached(KEYMAP)

    async def key(self, name: str, value: int, t: int, source=None) -> None:
        await self.pad.on_code(CODE[name], value, t, source=source)

    def bindings(self) -> list:
        return [c.args[3] for c in self.actions.dispatch.await_args_list]


def fake_display(*pages):
    display = MagicMock(display_usb=None, pages_active=True)
    display.bind_page.side_effect = list(pages)
    return display


@pytest.mark.uc("UC-016", flow="A7", br=["BR-011"])
async def test_the_binding_at_the_press_holds_for_the_whole_hold():
    """The page at the press; a page or link change mid-hold changes nothing (mutation: bind at release)."""
    rig = Rig(fake_display((PAGES.index("GAME"), False), (page_keys.FALLBACK_PAGE, True)))
    await rig.key("K1", PRESS, 0)
    await rig.key("K1", RELEASE, 100)
    assert rig.bindings() == [on("GAME", "K1")]
    rig.display.bind_page.assert_called_once()


@pytest.mark.uc("UC-016", br=["BR-009", "BR-011"])
async def test_the_paging_stops_before_the_press_status_read():
    """Mutation guard: `note_input` after the fetch would let the auto-pager turn the page under the key."""
    order = []
    display = fake_display((0, False))
    display.note_input.side_effect = lambda down: order.append(("note_input", down))
    rig = Rig(display)

    async def fetch():
        order.append(("fetch",))
        return st()

    rig.reader.fetch.side_effect = fetch
    await rig.key("K6", PRESS, 0)
    assert ("note_input", True) in order[: order.index(("fetch",))]


@pytest.mark.uc("UC-016", br=["BR-011"])
async def test_a_row_key_while_the_link_is_uncertain_is_refused_with_a_cue():
    rig = Rig(fake_display(REFUSED_BINDING))
    await rig.key("K6", PRESS, 0)
    await rig.key("K6", RELEASE, 100)
    rig.cues.refusal.assert_awaited_once()
    rig.actions.dispatch.assert_not_awaited()


@pytest.mark.uc("UC-016", br=["BR-011", "BR-016"])
async def test_only_the_display_pads_keys_bind_to_the_pages():
    display = fake_display((PAGES.index("AGENT"), False))
    display.display_usb = "/sys/devices/usb1/1-1"
    rig = Rig(display)
    await rig.key("K3", PRESS, 0, source="/sys/devices/usb1/1-2")
    await rig.key("K3", RELEASE, 100, source="/sys/devices/usb1/1-2")
    assert rig.bindings() == [page_keys.binding("K3", page_keys.FALLBACK_PAGE, fallback=True)]
    display.bind_page.assert_not_called()


@pytest.mark.uc("UC-016", br=["BR-011"])
async def test_attach_pushes_the_scroll_lock_led():
    rig = Rig()
    rig.actions.push_lock_led.assert_called_once_with(False)


@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
@pytest.mark.parametrize("before", [[], ["KNOB_PRESS"], ["K3"]], ids=["clean", "knob-down", "k3-down"])
async def test_the_k5_stop_goes_out_while_k5_is_still_held(before):
    rig = Rig()
    for name in before:
        await rig.key(name, PRESS, 0)
    await rig.key("K5", PRESS, 10)
    rig.actions.dispatch.reset_mock()
    await rig.pad.agent_due(1010)
    await rig.pad.drain()
    (call,) = rig.actions.dispatch.await_args_list
    assert call.args[0] == Gesture(AGENT_HOLD, "K5", held_ms=1000, clean=not before)
    assert rig.pad.machine.down("K5")


@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
async def test_the_k5_timer_fires_the_hold_without_a_release():
    clock = [0]

    async def sleep(seconds):
        clock[0] += int(seconds * 1000)

    config = create_autospec(PadConfig, instance=True)
    config.enabled.return_value = True
    config.timings.return_value = Timings()
    reader = create_autospec(MenuStatusReader, instance=True)
    reader.fetch.return_value = st()
    actions = create_autospec(Actions, instance=True)
    actions.dispatch.return_value = ("agent_switch", "ok")
    pad = PadController(
        config, reader, actions, create_autospec(CuePlayer, instance=True), clock_ms=lambda: clock[0], sleep=sleep
    )
    pad.attached(KEYMAP)
    await pad.on_code(CODE["K5"], PRESS, 0)
    for _ in range(5):
        await asyncio.sleep(0)
    await pad.drain()
    assert [c.args[0].kind for c in actions.dispatch.await_args_list] == [AGENT_HOLD]


# --- the display: binding, stages, screens (BR-006, BR-007, BR-011, BR-015) --------------------------


class Clock:
    def __init__(self) -> None:
        self.now = 100.0

    def __call__(self) -> float:
        return self.now


def model(version: int | None = 3) -> tuple[DisplayModel, Clock]:
    clock = Clock()
    m = DisplayModel(pad_config(autopage_ms=0), version="1.0.0", clock=clock)
    if version is not None:
        m.set_protocol(version)
        m.current(clock.now)
        m.acknowledged()
    m.note_status(st(), m.read_token())
    return m, clock


@pytest.mark.uc("UC-016", br=["BR-011"])
def test_a_page_binds_only_after_it_was_on_screen_400_ms():
    m, clock = model()
    m.page_step(1)  # GAME
    m.current(clock.now)
    m.acknowledged()
    clock.now += 0.399
    assert m.bind_page() == (PAGES.index("HOME"), False)  # the page shown before it
    clock.now += 0.001
    m.acknowledged()
    assert m.bind_page() == (PAGES.index("GAME"), False)


@pytest.mark.uc("UC-016", br=["BR-011", "BR-016"])
def test_row_keys_are_refused_while_the_link_is_uncertain_then_fall_back_to_home():
    m, clock = model()
    clock.now += 1.6  # no fresh ack: link down, but the screen may still show the frame
    assert m.bind_page() == "refused"
    clock.now += 1.5  # past NO HOST
    assert m.bind_page() == (page_keys.FALLBACK_PAGE, True)


@pytest.mark.uc("UC-016", br=["BR-016"])
def test_protocol_1_binds_the_fallback_page():
    m, _ = model(version=1)
    assert m.bind_page() == (page_keys.FALLBACK_PAGE, True)


def _down(m: DisplayModel, key: str, page: str, t_ms: int, status=None):
    m.key_down(key, t_ms, Timings(), on(page, key), status or st())


@pytest.mark.uc("UC-016", flow="A7", br=["BR-006"])
def test_the_highlight_inverts_the_held_rows_line_until_the_first_step():
    m, clock = model()
    _down(m, "K6", "HOME", int(clock.now * 1000))
    screen = m.current(clock.now)
    assert screen.lines[5][1] & screens.INVERTED
    assert not screen.lines[3][1] & screens.INVERTED
    m.hold_key("K6")
    m.layer_step("K6", "sensitivity", st(), 1, True)
    assert m.current(clock.now).bar is not None  # the edit screen replaced the highlight


@pytest.mark.uc("UC-016", br=["BR-006", "BR-011"])
def test_a_key_of_a_read_only_row_draws_no_highlight():
    m, clock = model()
    _down(m, "K6", "AGENT", int(clock.now * 1000))
    assert not any(attrs & screens.INVERTED for _, attrs in m.current(clock.now).lines[1:7])


@pytest.mark.uc("UC-016", flow="A8", br=["BR-007", "BR-011"])
def test_the_reset_screen_follows_the_press_time_not_the_hook_time():
    """O6: the hook runs after the press's status read; the stage comes from the press (mutation: own clock)."""
    m, clock = model()
    _down(m, "K7", "HOME", int(clock.now * 1000) - 1000)  # pressed 1 s before the hook
    assert m.current(clock.now).lines[0][0].rstrip().endswith("RESET?")


@pytest.mark.uc("UC-016", flow="A8", br=["BR-015"])
def test_k4_shows_reset_then_lock_and_a_confirmed_reset_never_the_lock():
    m, clock = model()
    t0 = int(clock.now * 1000)
    _down(m, "K4", "AGENT", t0, st(agent_level="full"))
    assert m.next_change(clock.now) == pytest.approx(clock.now + 1.0)
    clock.now += 1.0
    assert m.current(clock.now).lines[0][0].startswith("AGENT LEVEL")
    assert m.next_change(clock.now) == pytest.approx(clock.now + 2.0)
    clock.now += 2.0
    assert m.current(clock.now).lines[0][0].rstrip().endswith("LOCK?")
    m.confirm_state("K4", True)
    assert m.current(clock.now).lines[0][0].startswith("AGENT LEVEL")


@pytest.mark.uc("UC-016", flow="A9", br=["BR-007", "BR-014"])
def test_a_level_reset_that_raises_asks_for_the_confirmation():
    m, clock = model()
    _down(m, "K4", "AGENT", int(clock.now * 1000) - 1100, st(agent_level="ambient"))
    lines = [text.rstrip() for text, _ in m.current(clock.now).lines]
    assert lines[0].endswith("PRESS KNOB") and "PRESS TO CONFIRM" in lines[5]
    m.confirm_state("K4", True)
    assert "RELEASE TO RESET" in m.current(clock.now).lines[5][0]


@pytest.mark.uc("UC-016", flow="A8", br=["BR-007", "BR-016"])
def test_protocol_1_reset_and_lock_screens():
    plan = page_keys.Plan("medium", "automatic")
    reset = screens.reset_screen("SENSITIVITY", "sensitivity", plan.now, plan.new, screens.reset_hint(False, False))
    assert [reset.text(i) for i in (1, 3, 5, 8)] == ["SENSITIVITY", "NOW 2", "NEW AUTO", "RELEASE TO RESET"]
    lock = screens.lock_screen(True)
    assert [lock.text(i) for i in (1, 3, 8)] == ["PAD", "UNLOCK?", "RELEASE TO UNLOCK"]


@pytest.mark.uc("UC-016", flow="A8", br=["BR-007"])
@pytest.mark.parametrize("version", [2, 3, 4])
def test_reset_and_lock_screens_by_protocol(version):
    bottom = (" " * 21, 0)
    reset = render.reset_screen(version, "agent_level", page_keys.Plan("full", "standard"), False, bottom)
    assert reset.lines[0][0].rstrip().endswith("RESET?")
    assert "NOW" in reset.lines[1][0] and "NEW" in reset.lines[3][0]
    refused = render.reset_screen(version, "agent_level", page_keys.Plan("full", None, refused=True), False, bottom)
    assert refused.lines[5][0].strip() in ("NOT POSSIBLE", "NOT POSS.")
    lock = render.lock_screen(version, False, bottom)
    assert lock.lines[0][0].rstrip().endswith("LOCK?") and "RELEASE TO LOCK" in lock.lines[6][0]


@pytest.mark.uc("UC-016", flow="A9", br=["BR-007", "BR-012"])
@pytest.mark.parametrize("version", [2, 3, 4])
def test_the_interventions_switch_is_a_two_state_edit_that_asks_for_the_confirmation(version):
    status = st(agent_interventions_enabled_state="off")
    plan = page_keys.edit_plan(SWITCH, status, 1)
    shown = render.setting_screen(
        version, page_keys.INTERVENTIONS, status, 1, lobby_only=False, bottom=(" " * 21, 0), plan=plan
    )
    texts = [text.rstrip() for text, _ in shown.lines]
    assert texts[0].endswith("PRESS KNOB")
    assert texts[1].endswith("OFF") and texts[3].endswith("ON")


@pytest.mark.uc("UC-016", br=["BR-001", "BR-006", "BR-011"])
def test_a_watching_agent_shows_watch_on_the_page_and_its_icon():
    watch = st(agent_level="standard", agent_interventions_enabled_state="off")
    assert page_rows(PAGES.index("AGENT"), watch, PageContext(), 3)[0] == ("AGENT", "WATCH")
    assert screens.ICON_AGENT in render.status_line(watch, PageContext(), True)[0]
    assert screens.agent_text(watch) == "WATCH STANDARD"
