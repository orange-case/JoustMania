"""The rows iteration 4 makes settable (UC-016 BR-006, BR-007, BR-011 to BR-014; services.md §3.1): goal, rate,
group, retro AI, effort, brief, and the decision backend's free-to-paid raise (D12)."""

import itertools
import json
from pathlib import Path
from unittest.mock import MagicMock, create_autospec

import pytest

from proto import menu_pb2
from services.operator_pad import page_keys, render, row_ladders
from services.operator_pad.actions import Actions
from services.operator_pad.cues import CuePlayer
from services.operator_pad.gestures import LAYER, LAYER_STEP, RESET, TAP, Gesture
from services.operator_pad.pages import PAGES, PageContext, page_rows
from services.operator_pad.row_ladders import BRIEF, EFFORT, GOAL, GROUP, RATE, RETRO_BACKEND
from services.operator_pad.screens import CONFIRMED, INVERTED, NOT_POSSIBLE, PRESS_KNOB, WIDTH
from services.operator_pad.tests.fakes import MenuStubSpec
from services.operator_pad.tests.test_page_keys import ALL_COMMANDS, DEFAULTS

REPO = Path(__file__).resolve().parents[3]
ROWS = {GOAL: ("AGENT", "K6", "K7"), RATE: ("INTERV", "K3", "K4"), GROUP: ("INTERV", "K6", "K7")}
ROWS |= {RETRO_BACKEND: ("RETRO", "K1", "K2"), EFFORT: ("RETRO", "K3", "K4"), BRIEF: ("RETRO", "K6", "K7")}
SIX = tuple(ROWS)
ROW_COMMANDS = [f"{kind}:{option}" for option in SIX for kind in ("step_setting", "reset_setting")]
COMMANDS = [*ALL_COMMANDS, "set_backend.from", *ROW_COMMANDS]
BASELINES = {GOAL: "from_objectives", RATE: "2", GROUP: "off", RETRO_BACKEND: "stub", EFFORT: "medium", BRIEF: "off"}
MISSING = object()


class RowStatus:
    """A `GetMenuStatusResponse` with the status fields 22 to 27, which the menu's proto adds."""

    def __init__(self, message, planned: dict) -> None:
        self._message = message
        self.__dict__.update(planned)

    def __getattr__(self, name):
        return getattr(self._message, name)


def rs(values: dict | None = None, defaults: dict | None = None, commands=COMMANDS, **kw) -> RowStatus:
    """``values``: option -> value in force (MISSING: the menu does not report it)."""
    now = {**BASELINES, **(values or {})}
    base = {
        "lifecycle_live": False,
        "agent_enabled": True,
        "agent_enabled_state": "on",
        "agent_interventions_enabled_state": "on",
        "agent_level": "ambient",
        "agent_backend": "claude",
        "pad_commands": commands,
        "setting_defaults": {**DEFAULTS, **BASELINES} if defaults is None else defaults,
    }
    base.update(kw)
    planned = {row_ladders.STATUS_FIELDS[o]: ("" if v is MISSING else v) for o, v in now.items()}
    return RowStatus(menu_pb2.GetMenuStatusResponse(**base), planned)


def on(option: str, role: str = "edit") -> page_keys.Binding:
    page, edit_key, reset_key = ROWS.get(option, ("INTERV", "K1", "K2"))  # else the decision backend
    return page_keys.binding(edit_key if role == "edit" else reset_key, PAGES.index(page))


def make(value="x"):
    menu = create_autospec(MenuStubSpec, instance=True)
    menu.ProcessInput.return_value = menu_pb2.ProcessInputResponse(success=True, value=value)
    cues = create_autospec(CuePlayer, instance=True)
    return Actions(menu, None, cues, display=MagicMock(link_up=True)), menu, cues


def sent(menu) -> list[dict]:
    return [dict(c.args[0].data) for c in menu.ProcessInput.await_args_list]


async def edit(option, steps, press, release=None, confirmed=False):
    actions, menu, cues = make()
    result = await actions.dispatch(
        Gesture(LAYER, "K", steps=steps, confirmed=confirmed), release or press, press, on(option)
    )
    return result, sent(menu), cues


async def reset(option, press, release=None, confirmed=False, binding=None):
    actions, menu, cues = make()
    gesture = Gesture(RESET, "K", held_ms=1200, confirmed=confirmed)
    result = await actions.dispatch(gesture, release or press, press, binding or on(option, "reset"))
    return result, sent(menu), cues


# --- ladders (pad-local until the menu's lib/setting_ladders.py has them) -----------------------------


@pytest.mark.uc("UC-016", br=["BR-007", "BR-012"])
def test_the_ladders_are_the_spec_ladders_and_the_flag_files_variants():
    """services.md §3.1 iteration 4: the explicit ladders, never the flag file's declared order."""
    assert row_ladders.LADDERS == {
        GOAL: ["from_objectives", "long_running_game", "lively_finale", "balanced_contest", "quick_game"],
        RATE: ["1", "2", "4", "8", "20"],
        GROUP: ["off", "on"],
        RETRO_BACKEND: ["stub", "claude"],
        EFFORT: ["low", "medium", "high", "xhigh", "max"],
        BRIEF: ["off", "unkillable", "protect_weakest"],
    }
    assert {GOAL} == row_ladders.WRAPPING
    assert {RATE, GROUP, RETRO_BACKEND, EFFORT} == row_ladders.RAISES_UP
    flags = json.loads((REPO / "services/flagd/agent.json").read_text())["flags"]
    rate = flags[RATE]["variants"]
    assert rate == row_ladders.RATE_VARIANTS
    assert row_ladders.LADDERS[RATE] == [str(v) for v in sorted(rate.values())]
    for option in (GOAL, GROUP, RETRO_BACKEND, EFFORT, BRIEF):
        assert set(row_ladders.LADDERS[option]) == set(flags[option]["variants"]), option
        assert flags[option]["defaultVariant"] == BASELINES[option], option
    assert flags[RATE]["variants"][flags[RATE]["defaultVariant"]] == int(BASELINES[RATE])
    assert row_ladders.STATUS_FIELDS == {  # services.md §3.1 fields 22 to 27 (27 redefined by #2755)
        GOAL: "agent_goal_profile",
        RATE: "agent_max_interventions_per_minute",
        GROUP: "agent_group_grant_state",
        RETRO_BACKEND: "inference_backend",
        EFFORT: "inference_effort",
        BRIEF: "agent_operator_brief_variant",
    }


# --- rows and keys (BR-011, BR-012) -------------------------------------------------------------------


@pytest.mark.uc("UC-016", br=["BR-011", "BR-012"])
@pytest.mark.parametrize("option", SIX)
def test_each_new_row_has_an_edit_and_a_reset_and_only_the_goal_is_lobby_only(option):
    for role in ("edit", "reset"):
        binding = on(option, role)
        assert binding.active and binding.spec.option == option
        assert binding.spec.lobby_only is (option == GOAL)


@pytest.mark.uc("UC-016", flow="A7", br=["BR-012", "BR-014"])
@pytest.mark.parametrize("option", SIX)
async def test_an_edit_sends_one_step_with_from_in_the_canonical_format(option):
    ladder = row_ladders.LADDERS[option]
    now = ladder[-1]
    result, data, _ = await edit(option, -1, rs({option: now}))
    assert result == ("step_setting", "ok")
    assert data == [{"command": "step_setting", "option": option, "direction": "down", "steps": "1", "from": now}]


@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
@pytest.mark.parametrize("option", sorted(row_ladders.RAISES_UP))
async def test_a_raise_up_the_ladder_waits_for_the_confirmation(option):
    now = row_ladders.LADDERS[option][0]
    result, data, cues = await edit(option, 1, rs({option: now}))
    assert (result, data) == (("step_setting", "cancelled"), [])
    cues.cancel.assert_awaited_once()
    result, data, _ = await edit(option, 1, rs({option: now}), confirmed=True)
    assert result == ("step_setting", "ok") and data[0]["direction"] == "up" and data[0]["from"] == now


@pytest.mark.uc("UC-016", flow="A7", br=["BR-012", "BR-014"])
@pytest.mark.parametrize("option", [GOAL, BRIEF])
async def test_the_goal_and_the_brief_are_no_raise(option):
    result, data, _ = await edit(option, 1, rs({option: row_ladders.LADDERS[option][0]}))
    assert result == ("step_setting", "ok") and data[0]["direction"] == "up"


@pytest.mark.uc("UC-016", flow="A7", br=["BR-007"])
async def test_a_whole_turn_of_the_goal_wheel_sends_nothing_and_the_brief_stops_at_its_end():
    result, data, cues = await edit(GOAL, 5, rs())
    assert (result, data) == (("step_setting", "noop"), [])
    cues.limit.assert_not_awaited()
    result, data, cues = await edit(BRIEF, 4, rs({BRIEF: "protect_weakest"}))
    assert (result, data) == (("step_setting", "at_limit"), [])
    cues.limit.assert_awaited_once_with(True)


@pytest.mark.uc("UC-016", flow="A4", br=["BR-011"])
async def test_the_goal_is_lobby_only_and_the_other_rows_are_live():
    live = rs(lifecycle_live=True)
    assert (await edit(GOAL, 1, live))[:2] == (("step_setting", "refused"), [])
    assert (await reset(GOAL, rs({GOAL: "quick_game"}, lifecycle_live=True)))[:2] == (("reset_setting", "refused"), [])
    for option in SIX[1:]:
        result, data, _ = await edit(option, -1, rs({option: row_ladders.LADDERS[option][-1]}, lifecycle_live=True))
        assert result == ("step_setting", "ok"), option


# --- unknown and off-ladder values (BR-012, BR-013, D11, D4) ------------------------------------------


@pytest.mark.uc("UC-016", flow="A7", br=["BR-012", "BR-014"])
@pytest.mark.parametrize("option", SIX)
@pytest.mark.parametrize("now", [MISSING, "50", "custom", "Make player X unkillable."])
async def test_an_edit_from_an_unknown_or_off_ladder_now_is_refused(option, now):
    for steps in (-1, 1, 3):
        result, data, cues = await edit(option, steps, rs({option: now}), confirmed=True)
        assert (result, data) == (("step_setting", "refused"), []), (now, steps)
        cues.refusal.assert_awaited_once()
        plan = page_keys.edit_plan(on(option).spec, rs({option: now}), steps)
        assert plan.refused and not plan.raises


@pytest.mark.uc("UC-016", flow="A8", br=["BR-013", "BR-014"])
@pytest.mark.parametrize(
    "binding",
    [
        b
        for b in (page_keys.binding(k, p) for p in range(len(PAGES)) for k in ("K2", "K4", "K7"))
        if b.active and b.spec.reset == page_keys.DEFAULT
    ],
    ids=lambda b: b.spec.option,
)
async def test_a_reset_whose_new_is_unknown_is_refused_on_every_row(binding):
    """B1: `setting_defaults` without the key (right after a write) never sends an unguarded reset."""
    defaults = {k: v for k, v in {**DEFAULTS, **BASELINES}.items() if k != binding.spec.option}
    press = rs(defaults=defaults, agent_backend="rules", agent_saved_level="ambient")
    for confirmed in (False, True):
        result, data, _ = await reset(binding.spec.option, press, confirmed=confirmed, binding=binding)
        assert (result, data) == (("reset_setting", "refused"), [])


@pytest.mark.uc("UC-016", flow="A8", br=["BR-013", "BR-014"])
@pytest.mark.parametrize("option", SIX)
async def test_a_reset_whose_now_is_unknown_is_refused(option):
    result, data, _ = await reset(option, rs({option: MISSING}), confirmed=True)
    assert (result, data) == (("reset_setting", "refused"), [])


@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
async def test_a_custom_brief_reset_is_sent_as_the_menus_no_op_with_expect_and_from():
    result, data, _ = await reset(BRIEF, rs({BRIEF: "custom"}, defaults={**DEFAULTS, BRIEF: "custom"}))
    assert result == ("reset_setting", "ok")
    assert data == [{"command": "reset_setting", "option": BRIEF, "expect": "custom", "from": "custom"}]


@pytest.mark.uc("UC-016", flow="A8", br=["BR-013", "BR-014"])
@pytest.mark.parametrize("option", sorted(row_ladders.RAISES_UP))
async def test_a_reset_above_now_is_a_raise_and_below_it_is_sent_with_expect_and_from(option):
    ladder = row_ladders.LADDERS[option]
    low, high = ladder[0], ladder[-1]
    up = rs({option: low}, defaults={**DEFAULTS, option: high})
    assert (await reset(option, up))[:2] == (("reset_setting", "cancelled"), [])
    result, data, _ = await reset(option, up, confirmed=True)
    assert data == [{"command": "reset_setting", "option": option, "expect": high, "from": low}]
    result, data, _ = await reset(option, rs({option: high}, defaults={**DEFAULTS, option: low}))
    assert data == [{"command": "reset_setting", "option": option, "expect": low, "from": high}]


@pytest.mark.uc("UC-016", flow="A8", br=["BR-013", "BR-014"])
@pytest.mark.parametrize("option", sorted(row_ladders.RAISES_UP))
async def test_a_reset_to_an_off_ladder_value_of_a_raise_row_is_refused(option):
    """A script's overlay (a rate of 50) has no rung: the raise cannot be judged."""
    press = rs({option: row_ladders.LADDERS[option][0]}, defaults={**DEFAULTS, option: "50"})
    assert (await reset(option, press, confirmed=True))[:2] == (("reset_setting", "refused"), [])


# --- capability per option (BR-011 "Menu too old") ----------------------------------------------------


@pytest.mark.uc("UC-016", br=["BR-011"])
@pytest.mark.parametrize("option", SIX)
async def test_a_row_is_enabled_per_option_by_pad_commands(option):
    now = row_ladders.LADDERS[option][-1]
    without_step = rs({option: now}, commands=[c for c in COMMANDS if c != f"step_setting:{option}"])
    result, data, cues = await edit(option, -1, without_step)
    assert (result, data) == (("step_setting", "refused"), [])
    cues.refusal.assert_awaited_once()
    assert page_keys.edit_plan(on(option).spec, without_step, -1).refused  # the edit screen: NOT POSSIBLE
    without_reset = rs({option: now}, commands=[c for c in COMMANDS if c != f"reset_setting:{option}"])
    assert (await reset(option, without_reset, confirmed=True))[:2] == (("reset_setting", "refused"), [])
    assert page_keys.reset_plan(on(option, "reset").spec, without_reset).refused
    page, edit_key, _ = ROWS[option]
    row = page_keys.ROW_KEYS[edit_key][0]
    assert page_rows(PAGES.index(page), without_step, PageContext(), 3)[row][1] != "?"  # the value still shows
    # The release's menu counts too: a capability lost during the hold refuses.
    result, data, _ = await edit(option, -1, rs({option: now}), release=without_step)
    assert (result, data) == (("step_setting", "refused"), [])


# --- the decision backend, D12 ------------------------------------------------------------------------


@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
async def test_backend_target_and_raise_come_from_the_press_read_and_from_is_always_sent():
    """Opposer M2: CLAUDE to OPENAI judged at the press; RULES written during the hold is caught by `from`."""
    result, data, _ = await edit("decision.backend", 1, rs(agent_backend="claude"), release=rs(agent_backend="rules"))
    assert data == [{"command": "set_backend", "backend": "openai", "from": "claude"}]
    # Free to paid at the press: unconfirmed nothing, whatever the release read says.
    result, data, cues = await edit(
        "decision.backend", 1, rs(agent_backend="rules"), release=rs(agent_backend="claude")
    )
    assert (result, data) == (("set_backend", "cancelled"), [])
    result, data, _ = await edit("decision.backend", 1, rs(agent_backend="rules"), confirmed=True)
    assert data == [{"command": "set_backend", "backend": "claude", "from": "rules"}]
    result, data, _ = await edit("decision.backend", -1, rs(agent_backend="claude"))  # paid to free: at once
    assert data == [{"command": "set_backend", "backend": "rules", "from": "claude"}]


@pytest.mark.uc("UC-016", flow="A9", br=["BR-011", "BR-014"])
async def test_backend_raise_needs_set_backend_from_and_other_edits_go_without_it():
    old = [c for c in COMMANDS if c != "set_backend.from"]
    result, data, cues = await edit("decision.backend", 1, rs(agent_backend="rules", commands=old), confirmed=True)
    assert (result, data) == (("set_backend", "refused"), [])
    cues.refusal.assert_awaited_once()
    result, data, _ = await edit("decision.backend", 1, rs(agent_backend="claude", commands=old))
    assert data == [{"command": "set_backend", "backend": "openai"}]


@pytest.mark.uc("UC-016", flow="A9", br=["BR-011", "BR-014"])
async def test_backend_raise_is_refused_when_either_read_lacks_set_backend_from():
    old = [c for c in COMMANDS if c != "set_backend.from"]
    press_old = rs(agent_backend="rules", commands=old)
    assert page_keys.edit_plan(on("decision.backend").spec, press_old, 1).refused  # the screen: NOT POSSIBLE
    refused = screen(3, "decision.backend", None, 1, agent_backend="rules", commands=old)
    assert NOT_POSSIBLE.split()[-1] in " ".join(rows(refused))
    release = rs(commands=old)
    result, data, _ = await edit("decision.backend", 1, rs(agent_backend="rules"), release=release, confirmed=True)
    assert (result, data) == (("set_backend", "refused"), [])


@pytest.mark.uc("UC-016", flow="A8", br=["BR-013", "BR-014"])
@pytest.mark.parametrize("option", [*SIX, "decision.backend"])
async def test_a_reset_without_a_press_read_is_refused(option):
    """The raise is judged from the press read; the release's read never stands in for it."""
    ladder = row_ladders.LADDERS.get(option, ["rules", "claude"])
    release = rs({option: ladder[-1]} if option in ROWS else None, agent_backend="claude")
    actions, menu, _ = make()
    gesture = Gesture(RESET, "K", held_ms=1200, confirmed=True)
    result = await actions.dispatch(gesture, release, None, on(option, "reset"))
    assert result == ("reset_setting", "refused") and sent(menu) == []


@pytest.mark.uc("UC-016", flow="A7", br=["BR-014"])
async def test_backend_edit_without_a_press_read_or_from_an_unknown_backend_is_refused():
    actions, menu, _ = make()
    result = await actions.dispatch(
        Gesture(LAYER, "K1", steps=1), rs(), None, page_keys.binding("K1", PAGES.index("INTERV"))
    )
    assert result == ("set_backend", "refused") and sent(menu) == []
    result, data, _ = await edit("decision.backend", 1, rs(agent_backend=""), confirmed=True)
    assert (result, data) == (("set_backend", "refused"), [])


@pytest.mark.uc("UC-016", flow="A8", br=["BR-013", "BR-014"])
async def test_backend_reset_to_a_paid_default_from_rules_is_a_raise():
    binding = page_keys.binding("K2", PAGES.index("INTERV"))
    press = rs(agent_backend="rules", defaults={**DEFAULTS, "decision.backend": "claude"})
    assert (await reset(None, press, binding=binding))[:2] == (("reset_setting", "cancelled"), [])
    _, data, _ = await reset(None, press, confirmed=True, binding=binding)
    assert data == [{"command": "reset_setting", "option": "decision.backend", "expect": "claude"}]
    _, data, _ = await reset(None, rs(agent_backend="jev"), binding=binding)  # paid to `rules`: no raise
    assert data == [{"command": "reset_setting", "option": "decision.backend", "expect": "rules"}]


# --- the sweep: no unconfirmed raise is sendable ------------------------------------------------------


def _raising(data: dict) -> bool:
    """Whether a command sent raises what the agent may do or spend, judged from the command alone."""
    option = data.get("option")
    if data["command"] == "step_setting" and option in row_ladders.RAISES_UP:
        return data["direction"] == "up"
    if data["command"] == "set_backend":
        return data.get("from", "rules") == "rules" and data["backend"] != "rules"
    if data["command"] == "reset_setting" and option in row_ladders.RAISES_UP:
        ladder = row_ladders.LADDERS[option]
        return (
            data["expect"] not in ladder
            or data["from"] not in ladder
            or (ladder.index(data["expect"]) > ladder.index(data["from"]))
        )
    return False


@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
async def test_sweep_no_unconfirmed_raise_is_sent_from_any_new_row():
    """Every NOW (on, off the ladder, unknown) x every default x steps -6..6, edit and reset, unconfirmed."""
    actions, menu, _ = make()

    async def run(gesture, release, press, binding) -> list[dict]:
        menu.ProcessInput.reset_mock()
        await actions.dispatch(gesture, release, press, binding)
        return sent(menu)

    for option in SIX:
        ladder = row_ladders.LADDERS[option]
        values = [*ladder, MISSING, "50", "custom"]
        for now in values:
            press = rs({option: now})
            for steps in range(-6, 7):
                data = await run(Gesture(LAYER, "K", steps=steps), press, press, on(option))
                assert not any(map(_raising, data)), (option, now, steps, data)
                assert all(d["from"] == now for d in data)
            for default in values:
                defaults = {**DEFAULTS} if default is MISSING else {**DEFAULTS, option: default}
                press = rs({option: now}, defaults=defaults)
                data = await run(Gesture(RESET, "K", held_ms=1200), press, press, on(option, "reset"))
                assert not any(map(_raising, data)), (option, now, default, data)
                assert all(d["expect"] == default and d["from"] == now for d in data)
    backend_row = page_keys.binding("K1", PAGES.index("INTERV"))
    for backend, release in itertools.product(["rules", "claude", "openai", "jev", ""], ["rules", "claude"]):
        for steps in range(-6, 7):
            gesture = Gesture(LAYER, "K1", steps=steps)
            data = await run(gesture, rs(agent_backend=release), rs(agent_backend=backend), backend_row)
            assert not any(map(_raising, data)), (backend, release, steps, data)
            assert all(d["from"] == backend for d in data)


# --- the confirmation UX on the new rows (A9) ---------------------------------------------------------


@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
@pytest.mark.parametrize("option", sorted(row_ladders.RAISES_UP))
async def test_a_raising_step_on_a_new_row_plays_the_attention_cue_and_shows_the_raise(option):
    actions, _, cues = make()
    press = rs({option: row_ladders.LADDERS[option][0]})
    await actions.dispatch(Gesture(LAYER_STEP, "K", steps=1), press, press, on(option))
    cues.attention.assert_awaited_once()
    hook = actions._display.layer_step.call_args.args
    assert hook[1] == option and hook[-1].raises and not hook[-1].refused


@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
@pytest.mark.parametrize("option", sorted(row_ladders.RAISES_UP))
async def test_a_raising_reset_on_a_new_row_plays_the_attention_cue_at_the_reset_stage(option):
    actions, _, cues = make()
    ladder = row_ladders.LADDERS[option]
    await actions.reset_stage(
        "K", on(option, "reset"), rs({option: ladder[0]}, defaults={**DEFAULTS, option: ladder[-1]})
    )
    cues.attention.assert_awaited_once()


@pytest.mark.uc("UC-016", flow="A7", br=["BR-001", "BR-007"])
async def test_the_result_shows_the_committed_value_in_the_rows_short_form():
    actions, _, _ = make(value="8")
    await actions.dispatch(Gesture(LAYER, "K", steps=1, confirmed=True), rs({RATE: "4"}), rs({RATE: "4"}), on(RATE))
    actions._display.result.assert_called_with("RATE", "8/MIN")


@pytest.mark.uc("UC-016", flow="A7", br=["BR-007"])
async def test_a_tap_shows_the_rows_value_and_a_reset_tap_its_default():
    actions, _, _ = make()
    await actions.dispatch(Gesture(TAP, "K"), rs({EFFORT: "xhigh"}), None, on(EFFORT))
    actions._display.result.assert_called_with("EFFORT", "XHI")
    await actions.dispatch(Gesture(TAP, "K"), rs(), None, on(BRIEF, "reset"))
    actions._display.result.assert_called_with("BRIEF", "DEFAULT OFF")


# --- screens on protocols 2 to 4 (BR-006, BR-007) -----------------------------------------------------

BOTTOM = (" " * WIDTH, 0)


def screen(version, option, now, steps, confirmed=False, **status_kw):
    press = rs({option: now} if option in row_ladders.OPTIONS else None, **status_kw)
    plan = page_keys.edit_plan(on(option).spec, press, steps)
    return render.setting_screen(
        version, option, press, steps, lobby_only=False, bottom=BOTTOM, plan=plan, confirmed=confirmed
    )


def title(s) -> str:
    return s.lines[0][0].rstrip()


def rows(s) -> list[str]:
    return [s.lines[i][0].rstrip() for i in (1, 3, 5)]


@pytest.mark.uc("UC-016", flow="A5", br=["BR-006"])
@pytest.mark.parametrize(
    ("version", "expected"),
    [
        (2, {"AGENT": "FINALE", "INTERV": ["8/MIN", "ON"], "RETRO": ["CLAUDE", "XHI", "PROTECT"]}),
        (3, {"AGENT": "LIVELY FINALE", "INTERV": ["8/MIN", "ON"], "RETRO": ["CLAUDE", "XHI", "PROTECT WEAK"]}),
        (4, {"AGENT": "LIVELY FINALE", "INTERV": ["8/MIN", "ON"], "RETRO": ["CLAUDE", "XHI", "PROTECT WEAK"]}),
    ],
)
def test_page_rows_show_the_new_values_per_protocol(version, expected):
    status = rs(
        {
            GOAL: "lively_finale",
            RATE: "8",
            GROUP: "on",
            RETRO_BACKEND: "claude",
            EFFORT: "xhigh",
            BRIEF: "protect_weakest",
        }
    )
    assert page_rows(PAGES.index("AGENT"), status, PageContext(), version)[2][1] == expected["AGENT"]
    assert [v for _, v in page_rows(PAGES.index("INTERV"), status, PageContext(), version)[1:]] == expected["INTERV"]
    assert [v for _, v in page_rows(PAGES.index("RETRO"), status, PageContext(), version)] == expected["RETRO"]
    baseline = [v for _, v in page_rows(PAGES.index("RETRO"), rs(), PageContext(), version)]
    assert baseline == ["STUB", "MED", "OFF"]
    custom = rs({BRIEF: "custom", GOAL: "from_objectives"})
    assert page_rows(PAGES.index("RETRO"), custom, PageContext(), version)[2][1] == "CUSTOM"
    assert page_rows(PAGES.index("AGENT"), custom, PageContext(), version)[2][1] == "FROM OBJ"


@pytest.mark.uc("UC-016", flow="A7", br=["BR-007"])
def test_rate_and_effort_are_marker_bars_from_protocol_3_and_rows_on_protocol_2():
    for version in (3, 4):
        s = screen(version, RATE, "8", -2)
        assert s.bar == (0, 5, 2, 3, ("1", "2", "4", "8", "20")) and title(s) == "RATE" + " " * 13 + "EDIT"
        s = screen(version, EFFORT, "low", -1)
        assert s.bar == (0, 5, 1, 0, ("LOW", "", "", "", "MAX")) and title(s).endswith("MINIMUM")
    s = screen(2, RATE, "8", -2)
    assert s.bar is None and rows(s) == ["NOW  8/MIN", "NEW  2/MIN", "RELEASE"]


@pytest.mark.uc("UC-016", flow="A7", br=["BR-007"])
def test_the_goal_wheel_wraps_and_the_brief_wheel_has_empty_ends_on_protocol_4():
    s = screen(4, GOAL, "quick_game", 1)
    assert s.wheel == (("SHORT GAME", True), ("FROM OBJ", False), ("LONG GAME", False))
    assert title(s).endswith("EDIT")
    s = screen(4, BRIEF, "off", -1)
    assert s.wheel == (("", False), ("OFF", True), ("UNKILLABLE", False)) and title(s).endswith("MINIMUM")
    for version in (2, 3):
        s = screen(version, GOAL, "from_objectives", 2)
        assert s.wheel is None and rows(s)[1].endswith("FINALE")


@pytest.mark.uc("UC-016", flow="A7", br=["BR-007", "BR-012"])
@pytest.mark.parametrize("version", [2, 3, 4])
@pytest.mark.parametrize(("option", "now", "new"), [(GROUP, "off", "ON"), (RETRO_BACKEND, "stub", "CLAUDE")])
def test_two_state_rows_are_now_and_new_rows_on_every_protocol_and_their_raise_blinks(version, option, now, new):
    s = screen(version, option, now, 1)
    assert s.bar is None and s.wheel is None
    assert title(s).endswith(PRESS_KNOB) and rows(s)[1].endswith(new)
    assert title(s).startswith("GROUP" if option == GROUP else "RETRO AI")
    off = render.setting_screen(
        version,
        option,
        rs({option: now}),
        1,
        lobby_only=False,
        bottom=BOTTOM,
        plan=page_keys.edit_plan(on(option).spec, rs({option: now}), 1),
        blink_on=False,
    )
    assert not off.lines[0][1] & INVERTED and s.lines[0][1] & INVERTED
    assert title(screen(version, option, now, 1, confirmed=True)).endswith(CONFIRMED)
    top = screen(version, option, "on" if option == GROUP else "claude", 1)
    assert "MAXIMUM" in rows(top)[2] or title(top).endswith("MAXIMUM")


@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
@pytest.mark.parametrize("version", [2, 3, 4])
def test_raises_on_bars_and_wheels_show_press_knob_and_refusals_not_possible(version):
    assert title(screen(version, RATE, "2", 1)).endswith(PRESS_KNOB)
    assert title(screen(version, EFFORT, "low", 2, confirmed=True)).endswith(CONFIRMED)
    assert title(screen(version, "decision.backend", None, 1, agent_backend="rules")).endswith(PRESS_KNOB)
    refused = screen(version, BRIEF, "custom", 1)
    assert NOT_POSSIBLE.split()[-1] in " ".join(rows(refused)) and PRESS_KNOB not in title(refused)
    assert NOT_POSSIBLE.split()[-1] in " ".join(rows(screen(version, RATE, "50", 1)))


@pytest.mark.uc("UC-016", flow="A8", br=["BR-007", "BR-013", "BR-014"])
@pytest.mark.parametrize("version", [2, 3, 4])
@pytest.mark.parametrize("option", SIX)
def test_each_new_rows_reset_screen(version, option):
    ladder = row_ladders.LADDERS[option]
    press = rs({option: ladder[0]}, defaults={**DEFAULTS, option: ladder[-1]})
    plan = page_keys.reset_plan(on(option, "reset").spec, press)
    s = render.reset_screen(version, option, plan, False, BOTTOM)
    assert s.bar is None and s.wheel is None
    assert title(s).endswith(PRESS_KNOB if option in row_ladders.RAISES_UP else "RESET?")
    assert rows(s)[0].startswith("NOW") and rows(s)[1].startswith("NEW")
    unknown = page_keys.reset_plan(on(option, "reset").spec, rs({option: ladder[0]}, defaults=DEFAULTS))
    s = render.reset_screen(version, option, unknown, False, BOTTOM)
    assert rows(s)[1].endswith("?") and "NOT POSS" in rows(s)[2] and PRESS_KNOB not in title(s)
