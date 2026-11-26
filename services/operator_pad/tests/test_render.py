"""The frame, pages and edit screens of protocol 2 and higher (UC-016 BR-002, BR-006, BR-007): pure renders."""

import itertools
from types import SimpleNamespace

import pytest

from lib.setting_ladders import (
    AGENT_POLICY_LEVELS,
    BACKENDS,
    MODE_LADDER,
    SENSITIVITY_LADDER,
    SHIELD_KNOB_LADDER,
    TEAMS_LADDER,
    VOLUME_LADDER,
    WRAPPING,
)
from lib.types import Games
from proto import menu_pb2
from services.operator_pad import render
from services.operator_pad.pages import NET_DOWN, NET_OFF, NET_OK, NET_UNKNOWN, PAGES, PageContext, page_rows
from services.operator_pad.screens import (
    BAR_EMPTY,
    BAR_FULL,
    ICON_AGENT,
    ICON_INSTRUCTIONS,
    ICON_INTERNET,
    ICON_LOCK,
    INVERTED,
    LARGE,
    MEDIUM,
    MODE_NAMES,
    UNDERLINE,
    WIDTH,
    mode_name,
)

CTX = PageContext(locked=False, internet=NET_OK, version="0.24.0")
LAYER_OPTIONS = ("current_game", "sensitivity", "agent_level", "num_teams", "shield.activation", "decision.backend")


def status(**kw) -> menu_pb2.GetMenuStatusResponse:
    base = {
        "selection": "JoustFFA",
        "ready_count": 3,
        "connected_count": 5,
        "agent_enabled": True,
        "agent_enabled_state": "on",
        "agent_level": "standard",
        "agent_backend": "claude",
        "sensitivity": 3,
        "num_teams": 4,
        "instructions": True,
        "audio_volume_master": "loud",
        "shield_activation": "trigger",
        "operator_settings": {"sensitivity": "fast", "num_teams": "automatic", "shield.activation": "trigger"},
    }
    base.update(kw)
    return menu_pb2.GetMenuStatusResponse(**base)


def bottom(st=None, ctx=CTX):
    return render.status_line(st if st is not None else status(), ctx, True)


def texts(screen) -> list[str]:
    return [screen.lines[i][0].rstrip() for i in range(8)]


def bar(filled: int, rungs: int) -> str:
    return BAR_FULL * filled + BAR_EMPTY * (rungs - filled)


@pytest.mark.uc("UC-016", flow="A5", br=["BR-006"], steps=[1])
def test_v3_home_page_frame():
    screen = render.page_screen(3, 0, status(), CTX, connection_ok=True)
    assert screen.lines[0] == ("HOME" + " " * 14 + "1/7", INVERTED)
    assert [screen.lines[i][1] for i in (1, 3, 5)] == [MEDIUM, MEDIUM, MEDIUM | UNDERLINE]  # divider under row 3
    assert [screen.lines[i][0][:16] for i in (1, 3, 5)] == [
        "    FREE-FOR-ALL",  # MODE and the full name do not fit 16 characters: the value alone
        "VOLUME     " + bar(4, 5),
        "SENS       " + bar(4, 5),  # sensitivity 3 of 0 to 4
    ]
    assert [screen.lines[i] for i in (2, 4, 6)] == [(" " * WIDTH, 0)] * 3
    assert screen.lines[7] == (ICON_INTERNET + " FFA 3/5" + " " * 8 + ICON_AGENT + ICON_INSTRUCTIONS + "  ", 0)
    assert screen.bar is None and screen.wheel is None


@pytest.mark.uc("UC-016", flow="A5", br=["BR-006"])
@pytest.mark.parametrize(
    ("st", "ctx", "left", "right"),
    [
        (status(), PageContext(internet=NET_DOWN), "  FFA 3/5", ICON_AGENT + ICON_INSTRUCTIONS + "  "),
        (
            status(agent_enabled=False, agent_enabled_state="off", instructions=False, selection="NonStop"),
            PageContext(internet=NET_UNKNOWN, locked=True),
            "  NSTOP 3/5",
            "   " + ICON_LOCK,  # a stopped agent: no icon
        ),
        (
            status(agent_enabled_state="unknown"),
            PageContext(internet=NET_OFF),
            "  FFA 3/5",
            "?" + ICON_INSTRUCTIONS + "  ",
        ),
        (None, PageContext(internet=NET_OK), ICON_INTERNET + " ? ?/?", "?   "),
    ],
)
def test_status_bar_icons_follow_their_flags(st, ctx, left, right):
    assert render.status_line(st, ctx, True) == (left + right.rjust(WIDTH - len(left)), 0)


@pytest.mark.uc("UC-016", flow="A2", br=["BR-006"])
def test_unreadable_status_replaces_status_bar_with_inverted_no_connection():
    line, attrs = render.status_line(status(), CTX, connection_ok=False)
    assert line.strip() == "NO CONNECTION" and attrs == INVERTED


@pytest.mark.uc("UC-016", flow="A5", br=["BR-006"])
def test_mode_table_covers_mode_ladder_and_follows_pretty_name_rule():
    assert list(MODE_NAMES) == MODE_LADDER
    for name, (full, short_name) in MODE_NAMES.items():
        pretty = Games[name].pretty_name.upper()
        assert full == pretty.removeprefix("JOUST ")
        assert len(full) <= 14 and len(short_name) <= 5
    assert mode_name("Commander") == "COMMANDER" and mode_name("Commander", short_name=True) == "COMMA"
    assert mode_name("Jo\x81sté") == "JO?ST?"  # data never sends a glyph byte (m5)


@pytest.mark.uc("UC-016", flow="A5", br=["BR-006"])
@pytest.mark.parametrize("version", [2, 3])
@pytest.mark.parametrize("page", range(len(PAGES)))
def test_every_row_fits_and_keeps_a_space_between_label_and_value(version, page):
    width = 16 if version >= 3 else 10
    for st in (status(), status(selection="NonStop", agent_backend="openai"), None):
        for ctx in (CTX, PageContext(version="v1.22.3-rc.1", internet=NET_OFF, locked=True)):
            rows = page_rows(page, st, ctx, version)
            assert 1 <= len(rows) <= 3
            for label, value in rows:
                text = render.page_row(label, value, width)
                assert len(text) == width or not value
                if label and value and len(label) + 1 + len(value) <= width:
                    assert text.startswith(label + " ") and text.endswith(value), (label, value, text)
                elif value:
                    assert text == value[:width].rjust(width), (label, value, text)


@pytest.mark.uc("UC-016", flow="A5", br=["BR-006"])
@pytest.mark.parametrize(
    ("label", "value", "width", "expected"),
    [
        ("MODE", "FREE-FOR-ALL", 16, "    FREE-FOR-ALL"),  # 4 + 1 + 12 > 16: the value alone, never cut
        ("MODE", "TEAMS", 16, "MODE       TEAMS"),
        ("MODE", "NON STOP JOUST", 16, "  NON STOP JOUST"),
        ("VERSION", "V1.22.3-RC.1-HOTFIX", 16, "V1.22.3-RC.1-HOT"),  # longer than the row: cut
        ("MODE", "TEAMS", 10, "MODE TEAMS"),  # protocol 2: exactly 10
        ("SHLD", "TRIGGER", 10, "   TRIGGER"),
        ("V", "1.22.3-RC.1", 10, "1.22.3-RC."),
        ("FORCE ALL", "", 16, "FORCE ALL"),
    ],
)
def test_page_row_shows_the_value_alone_when_label_and_value_do_not_fit(label, value, width, expected):
    assert render.page_row(label, value, width) == expected


@pytest.mark.uc("UC-016", flow="A5", br=["BR-006"])
def test_v2_home_and_system_rows_follow_the_row_rule():
    ctx = PageContext(version="v1.22.3-rc.1", internet=NET_OK)
    home = texts(render.page_screen(2, 0, status(selection="JoustTeams"), ctx, connection_ok=True))
    assert home[1][:10] == "MODE TEAMS"
    system = texts(render.page_screen(2, 6, status(), ctx, connection_ok=True))
    assert system[1][:10] == "1.22.3-rc."  # V and the version do not fit 10: the value alone, cut to the row


@pytest.mark.uc("UC-016", flow="A5", br=["BR-006"])
def test_v3_page_rows():
    st = status(agent_enabled=False, agent_enabled_state="off", agent_level="ambient")
    ctx = PageContext(locked=True, internet=NET_DOWN, version="0.24.0")
    assert page_rows(1, st, ctx, 3) == [("TEAMS", "AUTO"), ("SHIELD", "TRIGGER"), ("INSTRUCTIONS", "ON")]
    assert page_rows(2, st, ctx, 3) == [("FORCE ALL", "OFF")]
    # A stopped agent keeps its known level (m3); the planned fields are `?` until the menu reports them.
    assert page_rows(3, st, ctx, 3) == [("AGENT", "OFF"), ("LEVEL", bar(2, 4)), ("GOAL", "?")]
    assert page_rows(4, st, ctx, 3) == [("BACKEND", "CLAUDE"), ("RATE", "?"), ("GROUP", "?")]
    assert page_rows(5, st, ctx, 3) == [("RETRO AI", "?"), ("EFFORT", "?"), ("BRIEF", "?")]
    assert page_rows(6, st, ctx, 3) == [("VERSION", "V0.24.0"), ("NET", "DOWN"), ("LOCK", "ON")]
    assert page_rows(6, st, ctx, 2) == [("V", "0.24.0"), ("NET", "DOWN"), ("LOCK", "ON")]
    assert page_rows(1, st, ctx, 2) == [("TEAMS", "AUTO"), ("SHLD", "TRIG"), ("INSTR", "ON")]


@pytest.mark.uc("UC-016", flow="A5", br=["BR-006", "BR-001"])
def test_unknown_level_shows_question_mark_and_shield_operator_unknown_too():
    st = status(agent_level="probe", operator_settings={})
    assert page_rows(3, st, CTX, 3)[1] == ("LEVEL", "?")
    assert page_rows(1, st, CTX, 3)[1] == ("SHIELD", "?")
    assert page_rows(0, status(audio_volume_master="", sensitivity=9), CTX, 3)[1:] == [("VOLUME", "?"), ("SENS", "?")]


@pytest.mark.uc("UC-016", flow="A5", br=["BR-006"])
def test_planned_fields_show_values_only_as_non_empty_strings():
    """services.md §3.1: a proto3 zero default is never a value; a string field shows."""
    known = SimpleNamespace(
        **{
            f: getattr(status(), f)
            for f in (
                "agent_enabled_state",
                "agent_level",
                "agent_enabled",
                "agent_backend",
            )
        },
        agent_objectives="balanced_focused",
        agent_max_interventions_per_minute="6",
        agent_group_grant_state="on",
        inference_backend="claude",
        inference_effort="high",
        agent_operator_brief_variant="off",
    )
    assert page_rows(3, known, CTX, 3)[2] == ("GOAL", "BALANCED")
    assert page_rows(4, known, CTX, 3)[1:] == [("RATE", "6/MIN"), ("GROUP", "ON")]  # off the ladder: shown as is
    assert page_rows(5, known, CTX, 3) == [("RETRO AI", "CLAUDE"), ("EFFORT", "HIGH"), ("BRIEF", "OFF")]
    zeros = SimpleNamespace(
        **{
            **vars(known),
            "agent_max_interventions_per_minute": 0,
            "agent_group_grant_state": False,
            "agent_objectives": "",
            "agent_operator_brief_variant": None,
        }
    )
    assert page_rows(4, zeros, CTX, 3)[1:] == [("RATE", "?"), ("GROUP", "?")]
    assert page_rows(3, zeros, CTX, 3)[2] == ("GOAL", "?")
    assert page_rows(5, zeros, CTX, 3)[2] == ("BRIEF", "?")


@pytest.mark.uc("UC-016", flow="main", br=["BR-007"], steps=[4])
def test_v3_ladder_edit_is_a_marker_bar_after_the_lines():
    st = status()  # sensitivity operator `fast` (rung 4 of AUTO 0 1 2 3 4)
    screen = render.setting_screen(3, "sensitivity", st, -2, lobby_only=True, bottom=bottom())
    assert screen.lines[0][0].rstrip() == "SENSITIVITY" + " " * 6 + "EDIT"
    assert screen.bar == (0, 6, 3, 4, ("AUTO", "0", "1", "2", "3", "4"))  # marker NOW, filled to NEW (`medium`)
    assert screen.lines[5] == ("RELEASE TO SET".ljust(WIDTH), MEDIUM | UNDERLINE)
    assert all(screen.lines[i] == (" " * WIDTH, 0) for i in (1, 2, 3, 4, 6))
    top = render.setting_screen(3, "sensitivity", st, 5, lobby_only=True, bottom=bottom())
    assert top.lines[0][0].rstrip().endswith("MAXIMUM") and top.bar[2] == 6


@pytest.mark.uc("UC-016", flow="main", br=["BR-007"], steps=[6])
def test_v3_volume_edit_labels_at_the_ends_and_marker_at_the_turn_start():
    screen = render.volume_screen(3, "normal", "max", None, bottom())
    assert screen.bar == (0, 5, 5, 2, ("MUTE", "", "", "", "MAX"))
    assert screen.lines[0][0].rstrip().endswith("EDIT") and screen.lines[5][0].strip() == ""
    muted = render.volume_screen(3, "quiet", "mute", "MINIMUM", bottom())
    assert muted.lines[0][0].rstrip().endswith("MUTED") and muted.bar[2:4] == (1, 1)
    assert render.volume_screen(3, None, None, None, bottom()).bar == (0, 5, 0, None, ("MUTE", "", "", "", "MAX"))


@pytest.mark.uc("UC-016", flow="main", br=["BR-007"], steps=[4])
def test_v4_mode_wheel_wraps_with_now_arrows():
    st = status()
    up = render.setting_screen(4, "current_game", st, 1, lobby_only=True, bottom=bottom())
    assert up.wheel == (("FREE-FOR-ALL", True), ("TEAMS", False), ("RANDOM TEAMS", False))
    assert up.bar is None and up.lines[6] == (" " * WIDTH, UNDERLINE)
    assert up.lines[0][0].rstrip() == "GAME" + " " * 13 + "EDIT"
    assert all(up.lines[i] == (" " * WIDTH, 0) for i in range(1, 6))
    down = render.setting_screen(4, "current_game", st, -1, lobby_only=True, bottom=bottom())
    assert down.wheel == (("TOURNAMENT", False), ("NON STOP JOUST", False), ("FREE-FOR-ALL", True))
    far = render.setting_screen(4, "current_game", st, 4, lobby_only=True, bottom=bottom())
    assert not any(now for _, now in far.wheel)  # NOW not among the three: no arrows
    same = render.setting_screen(4, "current_game", st, len(MODE_LADDER), lobby_only=True, bottom=bottom())
    assert same.wheel[1] == ("FREE-FOR-ALL", True)


@pytest.mark.uc("UC-016", flow="main", br=["BR-007"], steps=[4])
def test_v4_shield_wheel_starts_at_auto_without_operator_value_and_backend_wraps():
    st = status(operator_settings={"shield.activation": "automatic"}, shield_activation="hold")
    shield = render.setting_screen(4, "shield.activation", st, 1, lobby_only=True, bottom=bottom())
    assert shield.wheel == (("AUTO", True), ("INSTANT", False), ("TRIGGER", False))
    backend = render.setting_screen(
        4, "decision.backend", status(agent_backend="rules"), -1, lobby_only=False, bottom=bottom()
    )
    assert backend.wheel == (("CLAUDE", False), ("JEV", False), ("RULES", True))


def agent_wheel(net: int):
    screen = render.setting_screen(4, "agent_level", status(), net, lobby_only=False, bottom=bottom())
    return screen.lines[0][0].rstrip(), screen.wheel, screen.bar


@pytest.mark.uc("UC-016", flow="main", br=["BR-007"], steps=[4])
def test_v4_agent_level_wheel_does_not_wrap_and_leaves_the_slot_past_an_end_empty():
    """Kill switch: NONE and FULL are never neighbours; a turn past an end keeps MAXIMUM / MINIMUM."""
    title, wheel, bar_ = agent_wheel(0)  # NOW `standard`
    assert wheel == (("AMBIENT", False), ("STANDARD", True), ("FULL", False)) and bar_ is None
    assert title == "AGENT LEVEL" + " " * 6 + "EDIT"
    assert agent_wheel(1) == (
        "AGENT LEVEL" + " " * 6 + "EDIT",
        (("STANDARD", True), ("FULL", False), ("", False)),
        None,
    )
    title, wheel, _ = agent_wheel(2)
    assert wheel == (("STANDARD", True), ("FULL", False), ("", False)) and title.endswith("MAXIMUM")
    assert agent_wheel(-2)[1] == (("", False), ("NONE", False), ("AMBIENT", False))
    title, wheel, _ = agent_wheel(-3)
    assert wheel == (("", False), ("NONE", False), ("AMBIENT", False)) and title.endswith("MINIMUM")
    for net in range(-6, 7):
        names = [text for text, _ in agent_wheel(net)[1]]
        assert names not in (["FULL", "NONE", "AMBIENT"], ["STANDARD", "FULL", "NONE"])  # never wrapped


@pytest.mark.uc("UC-016", flow="main", br=["BR-007"], steps=[4])
def test_v3_agent_level_is_now_and_new_rows():
    screen = render.setting_screen(3, "agent_level", status(), 1, lobby_only=False, bottom=bottom())
    assert screen.wheel is None and screen.bar is None
    assert [screen.lines[i][0][:16].rstrip() for i in (1, 3, 5)] == [
        render.lr("NOW", bar(3, 4), 16),
        render.lr("NEW", bar(4, 4), 16),
        "RELEASE TO SET",
    ]
    top = render.setting_screen(3, "agent_level", status(), 2, lobby_only=False, bottom=bottom())
    assert top.lines[5][0].rstrip() == "MAXIMUM"


@pytest.mark.uc("UC-016", flow="main", br=["BR-007"], steps=[4])
@pytest.mark.parametrize("version", [2, 3])
def test_wheel_settings_below_v4_are_now_and_new_rows(version):
    screen = render.setting_screen(version, "decision.backend", status(), 1, lobby_only=False, bottom=bottom())
    assert screen.wheel is None and screen.bar is None
    width = 16 if version >= 3 else 10
    assert [screen.lines[i][0][:width].rstrip() for i in (1, 3, 5)] == [
        render.lr("NOW", "CLAUDE", width),
        render.lr("NEW", "JEV", width),
        "RELEASE TO SET" if version >= 3 else "RELEASE",
    ]


@pytest.mark.uc("UC-016", flow="main", br=["BR-007"], steps=[4])
def test_v2_ladder_setting_is_rows_with_glyph_bars():
    screen = render.setting_screen(2, "agent_level", status(), -1, lobby_only=False, bottom=bottom())
    assert screen.bar is None
    assert [screen.lines[i] for i in (1, 3, 5)] == [
        (("NOW   " + bar(3, 4)).ljust(WIDTH), LARGE),  # value flush right in 10 characters
        (("NEW   " + bar(2, 4)).ljust(WIDTH), LARGE),
        ("RELEASE".ljust(WIDTH), LARGE | UNDERLINE),
    ]


@pytest.mark.uc("UC-016", flow="A4", br=["BR-007"])
@pytest.mark.parametrize("version", [2, 3, 4])
def test_refused_layer_shows_now_and_not_possible_on_every_protocol(version):
    screen = render.setting_screen(
        version, "current_game", status(lifecycle_live=True), 2, lobby_only=True, bottom=bottom()
    )
    rows = [screen.lines[i][0].rstrip() for i in (1, 3, 5)]
    assert screen.wheel is None and screen.bar is None
    if version >= 3:
        assert rows == ["NOW FREE-FOR-ALL", "NEW", "NOT POSSIBLE"]
    else:
        assert rows == ["NOW    FFA", "NOT", "POSSIBLE"]


@pytest.mark.uc("UC-016", flow="A3", br=["BR-007"])
def test_result_screen_label_in_title_and_word_in_row_two():
    v3 = render.result_screen(3, "SENSITIVITY", "NOT POSSIBLE", bottom())
    assert v3.lines[0][0].rstrip() == "SENSITIVITY" and v3.lines[3][0].strip() == "NOT POSSIBLE"
    assert v3.lines[1][0].strip() == "" and v3.lines[5][0].strip() == ""
    v2 = render.result_screen(2, "VOLUME", "NO CONNECTION", bottom())
    assert (v2.lines[3][0].strip(), v2.lines[5][0].strip()) == ("NO", "CONNECTION")
    pending = render.result_screen(3, "GAME", "", bottom())
    assert pending.lines[3][0].strip() == ""


def _statuses():
    """Every kind of value a status can carry, the unknown and out-of-ladder ones included."""
    yield None
    for selection, backend, level, enabled, operator in itertools.product(
        ("JoustFFA", "NonStop", "Commander", ""),
        ("rules", "jev", "", "gpt"),
        ("none", "full", "", "probe"),
        ("on", "off", "watch", "unknown", ""),
        (
            {},
            {"sensitivity": "automatic", "num_teams": "six", "shield.activation": "automatic"},
            {"sensitivity": "bogus", "num_teams": "automatic", "shield.activation": "hold"},
        ),
    ):
        yield status(
            selection=selection,
            agent_backend=backend,
            agent_level=level,
            agent_enabled_state=enabled,
            operator_settings=operator,
            sensitivity=(len(selection) % 7) - 1,
            num_teams=len(backend) + 1,
            audio_volume_master=("", "mute", "max", "x")[len(level) % 4],
            shield_activation=("hold", "", "weird")[len(enabled) % 3],
        )


@pytest.mark.uc("UC-016", flow="main", br=["BR-002", "BR-007"])
def test_render_is_total_over_the_firmware_limits():
    """B3: no input makes a bar, wheel or line the firmware would refuse (`FF`)."""
    for version, option, st, net in itertools.product(
        (2, 3, 4), LAYER_OPTIONS, list(_statuses()), (-13, -4, -1, 0, 1, 3, 10)
    ):
        for lobby_only in (False, True):
            _check_limits(
                version,
                render.setting_screen(
                    version, option, st, net, lobby_only=lobby_only, bottom=render.status_line(st, CTX, True)
                ),
            )
    volumes = [None, "", "x", *VOLUME_LADDER]
    for version, now, new in itertools.product((2, 3, 4), volumes, volumes):
        _check_limits(version, render.volume_screen(version, now, new, "MAXIMUM", bottom()))
    for version, page, st in itertools.product((2, 3, 4), range(len(PAGES)), list(_statuses())[:40]):
        _check_limits(version, render.page_screen(version, page, st, CTX, True))


def _check_limits(version: int, screen) -> None:
    last = 0x84 if version == 2 else 0x85
    assert len(screen.lines) == 8
    for i, (text, attrs) in enumerate(screen.lines):
        assert len(text) == WIDTH and all(0x20 <= ord(c) <= last for c in text), (version, text)
        if attrs & MEDIUM:
            assert version >= 3 and i in (1, 3, 5)
        if attrs & LARGE:
            assert i <= 6
    if screen.bar is not None:
        assert version >= 3
        row, rungs, filled, marker, labels = screen.bar
        assert row in (0, 1) and 2 <= rungs <= 8 and 0 <= filled <= rungs
        assert marker is None or 0 <= marker < rungs
        joined = "|".join(labels)
        assert len(joined) <= 26 and all("|" not in label for label in labels)
        assert all(0x20 <= ord(c) <= 0x7E for c in joined)
    if screen.wheel is not None:
        assert version >= 4 and len(screen.wheel) == 3
        for slot, (text, now) in enumerate(screen.wheel):
            # An empty slot (all spaces, flags 0) only past an end of a wheel that does not wrap.
            assert isinstance(now, bool) and len(text) <= (16 if slot == 1 else 21)
            assert text or (slot != 1 and not now)
            assert all(0x20 <= ord(c) <= 0x7E for c in text)
        assert screen.bar is None


@pytest.mark.uc("UC-016", flow="main", br=["BR-007"])
def test_ladders_assumed_by_the_bars():
    """The rung labels match the ladders the preview steps (drift guard)."""
    assert render.LADDER_BARS["sensitivity"][0][1:] == SENSITIVITY_LADDER
    assert render.LADDER_BARS["num_teams"][0][1:] == TEAMS_LADDER
    assert render.WHEEL_LADDERS["agent_level"] == AGENT_POLICY_LEVELS and "agent_level" not in render.LADDER_BARS
    assert "agent_level" not in WRAPPING
    assert render.WHEEL_LADDERS["decision.backend"] == BACKENDS
    assert render.WHEEL_LADDERS["shield.activation"] == SHIELD_KNOB_LADDER
