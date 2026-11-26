"""The screens of the pad's OLED (UC-016 BR-001, BR-002): pure builders, 8 lines of 21 characters."""

import itertools

import pytest

from lib.setting_ladders import (
    AGENT_POLICY_LEVELS,
    BACKENDS,
    KNOB_LADDERS,
    SENSITIVITY_LADDER,
    TEAMS_LADDER,
    VOLUME_LADDER,
)
from proto import menu_pb2
from services.operator_pad.screens import (
    WIDTH,
    fit,
    result_word,
    setting_screen,
    short,
    status_screen,
    volume_screen,
)


def status(**kw) -> menu_pb2.GetMenuStatusResponse:
    base = {
        "lifecycle_live": False,
        "selection": "JoustFFA",
        "ready_count": 3,
        "connected_count": 5,
        "agent_enabled": True,
        "agent_enabled_state": "act",
        "agent_level": "standard",
        "agent_backend": "claude",
        "sensitivity": 3,
        "num_teams": 4,
        "audio_volume_master": "normal",
        "shield_activation": "trigger",
        "operator_settings": {"sensitivity": "fast", "num_teams": "automatic", "shield.activation": "trigger"},
    }
    base.update(kw)
    return menu_pb2.GetMenuStatusResponse(**base)


def lines(screen) -> list[str]:
    return [screen.text(i) for i in range(1, 9)]


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[2])
def test_status_screen_lines():
    screen = status_screen(status(), connection_ok=True, locked=False, version="0.14.0")

    assert lines(screen) == [
        "JOUSTMANIA LOBBY",
        "FREE-FOR-ALL",  # BR-006 mode table: the full name wherever the pad names a mode
        "READY 3/5",
        "AGENT ACT STANDARD",
        "BACKEND CLAUDE",
        "SENS 3 TEAMS AUTO",
        "SHIELD TRIG VOL NORM",
        "V0.14.0",
    ]
    assert [inverted for _, inverted in screen.lines] == [True] + [False] * 7


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[2])
def test_status_screen_state_is_game_while_the_lifecycle_is_live_and_agent_off_when_stopped():
    screen = status_screen(
        status(lifecycle_live=True, agent_enabled=False, agent_enabled_state="off"),
        connection_ok=True,
        locked=False,
        version=None,
    )
    assert screen.text(1) == "JOUSTMANIA GAME"
    assert screen.text(4) == "AGENT OFF"


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[2])
@pytest.mark.parametrize(
    ("fields", "line"),
    [
        ({"agent_enabled_state": "watch"}, "AGENT WATCH STANDARD"),  # UC-016 BR-011
        ({"agent_enabled_state": "maybe"}, "AGENT ?"),
        ({"agent_level": "none"}, "AGENT ACT NONE"),  # a tuning level, not a stop
        (
            {"agent_enabled": False, "agent_enabled_state": "off", "agent_level": "probe"},
            "AGENT OFF",
        ),
        ({"agent_enabled": False, "agent_enabled_state": "off", "agent_level": ""}, "AGENT OFF"),
        ({"agent_enabled_state": "unknown"}, "AGENT ?"),  # BR-013
        ({"agent_level": "probe"}, "AGENT ?"),
        ({"agent_level": "full"}, "AGENT ACT FULL"),
    ],
)
def test_status_screen_agent_line_follows_uc015_stopped_and_unknown_rules(fields, line):
    assert status_screen(status(**fields), connection_ok=True, locked=False, version=None).text(4) == line


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[2])
def test_status_screen_unknown_values_show_question_mark():
    unknown = status(
        agent_enabled_state="unknown",
        agent_backend="",
        audio_volume_master="",
        shield_activation="",
        operator_settings={},
    )
    assert lines(status_screen(unknown, connection_ok=True, locked=False, version=None))[3:7] == [
        "AGENT ?",
        "BACKEND ?",
        "SENS 3 TEAMS 4",  # the value the menu uses, also with the operator value unknown
        "SHIELD ? VOL ?",
    ]
    nothing = lines(status_screen(None, connection_ok=True, locked=False, version=None))
    assert nothing[:7] == ["JOUSTMANIA ?", "?", "READY ?", "AGENT ?", "BACKEND ?", "SENS ? TEAMS ?", "SHIELD ? VOL ?"]


@pytest.mark.uc("UC-016", flow="A2", br=["BR-001"])
def test_status_screen_keeps_values_with_no_connection():
    screen = status_screen(status(), connection_ok=False, locked=True, version="0.14.0")
    assert screen.text(3) == "READY 3/5"
    assert screen.text(8) == "NO CONNECTION"  # D8: over LOCKED


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[2])
@pytest.mark.parametrize(
    ("version", "locked", "line"),
    [
        ("0.14.0", True, "LOCKED"),
        ("0.14.0", False, "V0.14.0"),
        (None, False, "DEV"),
        ("dev", False, "DEV"),
        ("0.0.0-dev", False, "DEV"),
        ("v0.14.0", False, "V0.14.0"),  # one V
        ("V1.2.3-rc.1", False, "V1.2.3-rc.1"),
        # Not a release: the IMAGE_TAG overrides for dev and dry runs, a commit sha.
        ("latest", False, "DEV"),
        ("main", False, "DEV"),
        ("a" * 40, False, "DEV"),
        ("", False, "DEV"),
        ("1.2", False, "DEV"),
        ("v", False, "DEV"),
        ("build-1.2.3x", False, "DEV"),  # the whole tag must be a release, not just contain one
    ],
)
def test_status_screen_last_line_locked_or_version(version, locked, line):
    """FR-241: the release tag the stack runs, `DEV` without one; UC-015 A6 `LOCKED` wins."""
    assert status_screen(status(), connection_ok=True, locked=locked, version=version).text(8) == line


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[2, 4])
def test_every_short_form_line_fits_21_characters():
    """BR-001: the short forms keep every status and ladder line within 21 characters."""
    for sens, teams in itertools.product(["automatic", *SENSITIVITY_LADDER], ["automatic", *TEAMS_LADDER]):
        line = f"SENS {short('sensitivity', sens)} TEAMS {short('num_teams', teams)}"
        assert len(line) <= WIDTH, line
    for shield, volume in itertools.product(KNOB_LADDERS["shield.activation"], VOLUME_LADDER):
        line = f"SHIELD {short('shield.activation', shield)} VOL {short('audio_volume.master', volume)}"
        assert len(line) <= WIDTH, line
    for level in AGENT_POLICY_LEVELS:
        assert len(f"AGENT ACT {short('agent_level', level)}") <= WIDTH
    for backend in BACKENDS:
        assert len(f"BACKEND {short('decision.backend', backend)}") <= WIDTH
    for option in ("sensitivity", "num_teams", "shield.activation"):
        for steps in range(-6, 7):
            screen = setting_screen(option, status(), steps, lobby_only=True)
            assert screen.text(6), (option, steps)  # the ladder line fits, so it is shown


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[4])
def test_setting_screen_now_new_and_ladder():
    screen = setting_screen("sensitivity", status(), -2, lobby_only=True)
    assert lines(screen) == ["SENSITIVITY", "", "NOW 3", "", "NEW 1", "AUTO 0 [1] 2 3 4", "", "RELEASE TO SET"]


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[4])
def test_setting_screen_without_a_press_status_shows_question_marks():
    screen = setting_screen("agent_level", None, 1, lobby_only=False)
    assert (screen.text(3), screen.text(5), screen.text(6)) == ("NOW ?", "NEW ?", "")


@pytest.mark.uc("UC-016", flow="main", br=["BR-001", "BR-007"], steps=[4])
@pytest.mark.parametrize(
    ("option", "steps", "word"),
    [
        ("sensitivity", 2, "MAXIMUM"),  # 3 -> 4 -> past the end
        ("sensitivity", 1, "RELEASE TO SET"),
        ("decision.backend", -1, "RELEASE TO SET"),  # claude -> rules
        # BR-007: the wrapping options never show a limit word, a full turn included.
        ("decision.backend", -2, "RELEASE TO SET"),
        ("decision.backend", 4, "RELEASE TO SET"),
        ("shield.activation", 5, "RELEASE TO SET"),
        ("current_game", -10, "RELEASE TO SET"),
    ],
)
def test_setting_screen_limit_words(option, steps, word):
    """BR-001: `MAXIMUM` / `MINIMUM` once the net steps reach past the end of a clamped range."""
    assert setting_screen(option, status(), steps, lobby_only=False).text(8) == word


@pytest.mark.uc("UC-016", flow="A4", br=["BR-001"])
def test_lobby_only_layer_while_live_shows_not_possible():
    screen = setting_screen("num_teams", status(lifecycle_live=True), 2, lobby_only=True)
    assert screen.text(8) == "NOT POSSIBLE"
    assert screen.text(3) == "NOW 4"
    assert screen.text(5) == ""  # no new value


@pytest.mark.uc("UC-016", flow="A3", br=["BR-001"])
@pytest.mark.parametrize(
    ("result", "up", "word"),
    [
        ("refused", True, "NOT POSSIBLE"),
        ("game_active", True, "NOT POSSIBLE"),
        ("bad_option", True, "NOT POSSIBLE"),
        ("menu_unavailable", True, "NO CONNECTION"),
        ("failed", True, "FAILED"),
        ("flag_service_unavailable", True, "FAILED"),
        ("cancelled", True, "CANCELLED"),
        ("stale", True, "CANCELLED"),
        ("at_limit", True, "MAXIMUM"),
        ("at_limit", False, "MINIMUM"),
        ("Exception: boom", True, "FAILED"),  # any error without a word
    ],
)
def test_result_words_per_result(result, up, word):
    assert result_word(result, up) == word


@pytest.mark.uc("UC-016", flow="main", br=["BR-001"], steps=[6])
def test_volume_screen():
    assert lines(volume_screen("loud")) == ["VOLUME", "", "LOUD", "", "### ### ### ### ...", "", "", ""]
    assert volume_screen("mute").text(8) == "MUTED"
    assert volume_screen("max", limit="MAXIMUM").text(8) == "MAXIMUM"
    assert volume_screen(None).text(3) == "?"


@pytest.mark.uc("UC-016", flow="main", br=["BR-002"])
def test_fit_ascii_21_chars():
    assert fit("abc") == "abc" + " " * 18
    assert fit("x" * 30) == "x" * 21
    assert fit("café\t~") == "caf??~" + " " * 15
