"""The `START` page's Non-Stop time limit row, `NS TIME` (UC-016 BR-006, BR-007, BR-011 to BR-013; #2940)."""

from unittest.mock import MagicMock, create_autospec

import pytest

from lib import setting_ladders
from lib.setting_ladders import TIME_LIMIT_LADDER, TIME_LIMIT_OPTION
from proto import menu_pb2
from services.operator_pad import page_keys
from services.operator_pad.actions import Actions
from services.operator_pad.cues import CuePlayer
from services.operator_pad.gestures import LAYER, RESET, Gesture
from services.operator_pad.page_keys import DEFAULT, ROWS
from services.operator_pad.pages import PAGES, PageContext, page_rows
from services.operator_pad.screens import NOW_NEW_FORMS, short
from services.operator_pad.tests.fakes import MenuStubSpec

START = PAGES.index("START")
COMMANDS = ["reset_setting", f"step_setting:{TIME_LIMIT_OPTION}", f"reset_setting:{TIME_LIMIT_OPTION}"]


def rs(limit: str = "three_min", commands=COMMANDS, **kw) -> menu_pb2.GetMenuStatusResponse:
    base = {
        "nonstop_time_limit": limit,
        "pad_commands": commands,
        "setting_defaults": {TIME_LIMIT_OPTION: "three_min"},
    }
    return menu_pb2.GetMenuStatusResponse(**{**base, **kw})


def make():
    menu = create_autospec(MenuStubSpec, instance=True)
    menu.ProcessInput.return_value = menu_pb2.ProcessInputResponse(success=True, value="x")
    cues = create_autospec(CuePlayer, instance=True)
    return Actions(menu, None, cues, display=MagicMock(link_up=True)), menu, cues


def sent(menu) -> list[dict]:
    return [dict(c.args[0].data) for c in menu.ProcessInput.await_args_list]


def on(role: str) -> page_keys.Binding:
    return page_keys.binding("K6" if role == "edit" else "K7", START)


async def edit(steps: int, press, release=None):
    actions, menu, cues = make()
    result = await actions.dispatch(Gesture(LAYER, "K", steps=steps), release or press, press, on("edit"))
    return result, sent(menu), cues


async def reset(press):
    actions, menu, cues = make()
    result = await actions.dispatch(Gesture(RESET, "K", held_ms=1200), press, press, on("reset"))
    return result, sent(menu), cues


@pytest.mark.uc("UC-016", br=["BR-006"])
@pytest.mark.parametrize(
    ("limit", "v3", "v2"),
    [
        ("one_min", "1 MIN", "1 MIN"),
        ("three_min", "3 MIN", "3 MIN"),
        ("five_min", "5 MIN", "5 MIN"),
        ("unlimited", "NO LIMIT", "NO LIMIT"),
        ("", "?", "?"),  # unknown, or a menu older than field 31
        ("ninety_seconds", "?", "?"),  # no declared variant: never a guess
    ],
)
def test_start_row_3_shows_the_time_limit(limit, v3, v2):
    assert page_rows(START, rs(limit), PageContext(), 3)[2] == ("NS TIME", v3)
    assert page_rows(START, rs(limit), PageContext(), 2)[2] == ("TIME", v2)
    assert page_rows(START, None, PageContext(), 3)[2] == ("NS TIME", "?")


@pytest.mark.uc("UC-016", br=["BR-011", "BR-012"])
def test_the_row_has_an_edit_and_a_default_reset_and_is_lobby_only():
    spec = ROWS["START"][2]
    assert (spec.option, spec.reset, spec.lobby_only) == (TIME_LIMIT_OPTION, DEFAULT, True)
    assert on("edit").active and on("reset").active


@pytest.mark.uc("UC-016", flow="A7", br=["BR-012"])
@pytest.mark.parametrize(
    ("start", "steps", "new", "at_limit"),
    [
        ("three_min", 1, "four_min", False),
        ("five_min", 1, "unlimited", False),  # up = longer: no limit is the top rung
        ("unlimited", 1, "unlimited", True),
        ("one_min", -1, "one_min", True),
        ("unlimited", -2, "four_min", False),
    ],
)
def test_the_ladder_is_clamped_and_ends_with_no_limit(start, steps, new, at_limit):
    assert setting_ladders.preview_value(TIME_LIMIT_OPTION, rs(start), steps) == setting_ladders.Preview(new, at_limit)
    assert TIME_LIMIT_LADDER[-1] == "unlimited"


@pytest.mark.uc("UC-016", flow="A7", br=["BR-012"])
async def test_an_edit_sends_one_step_setting():
    (_action, _result), commands, _cues = await edit(2, rs("two_min"))
    assert commands == [{"command": "step_setting", "option": TIME_LIMIT_OPTION, "direction": "up", "steps": "2"}]


@pytest.mark.uc("UC-016", flow="A7", br=["BR-011", "BR-012"])
@pytest.mark.parametrize(
    "press",
    [
        rs(""),  # NOW unknown: never stepped blind
        rs("three_min", commands=["reset_setting"]),  # a menu without the option ("Menu too old")
        None,  # no press read
    ],
)
async def test_an_edit_is_refused_with_an_unknown_now_or_an_older_menu(press):
    (_action, result), commands, cues = await edit(1, press, release=press if press is not None else rs())
    assert (result, commands) == ("refused", [])
    cues.refusal.assert_awaited_once()


@pytest.mark.uc("UC-016", flow="A4", br=["BR-011", "BR-012"])
async def test_an_edit_and_a_reset_are_refused_while_a_game_is_live():
    (_action, result), commands, _cues = await edit(1, rs(lifecycle_live=True))
    assert (result, commands) == ("refused", [])
    (_action, result), commands, _cues = await reset(rs(lifecycle_live=True))
    assert (result, commands) == ("refused", [])


@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
async def test_a_reset_sends_the_default_as_expect():
    (_action, _result), commands, _cues = await reset(rs("five_min"))
    assert commands == [{"command": "reset_setting", "option": TIME_LIMIT_OPTION, "expect": "three_min"}]


@pytest.mark.uc("UC-016", flow="A8", br=["BR-011", "BR-013"])
@pytest.mark.parametrize(
    "press",
    [rs("five_min", commands=["reset_setting"]), rs("five_min", setting_defaults={})],
)
async def test_a_reset_is_refused_by_an_older_menu_or_an_unknown_default(press):
    (_action, result), commands, _cues = await reset(press)
    assert (result, commands) == ("refused", [])


@pytest.mark.uc("UC-016", br=["BR-007"])
def test_now_and_new_fit_protocol_2():
    forms = {v: NOW_NEW_FORMS.get(TIME_LIMIT_OPTION, {}).get(v, short(TIME_LIMIT_OPTION, v)) for v in TIME_LIMIT_LADDER}
    assert forms["unlimited"] == "NO LIM"
    assert all(len(text) <= 6 for text in forms.values())
