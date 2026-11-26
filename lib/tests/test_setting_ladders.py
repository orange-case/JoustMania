"""Shared setting ladders and voice tables (#2106, UC-015 BR-004, BR-005, A7, BR-011).

The exhaustive preview-equals-commit check against the menu's `step_setting` is
`services/menu/tests/test_preview_equals_commit.py`; this file covers the pure helpers.
"""

import json
from pathlib import Path

import pytest
from proto import menu_pb2

from lib import setting_ladders, voice_prompts
from lib.setting_ladders import Preview, clamp_step, preview_value
from lib.types import Games

FLAGD_DIR = Path(__file__).resolve().parent.parent.parent / "services" / "flagd"
LADDER = ["a", "b", "c", "d"]


@pytest.mark.uc("UC-015", br=["BR-004", "BR-005"], flow="A7")
@pytest.mark.parametrize(
    ("current", "steps", "expected"),
    [
        ("b", 1, ("c", False)),
        ("b", 2, ("d", False)),
        ("b", 9, ("d", False)),  # passes the end: stops there and moves (BR-005)
        ("b", -1, ("a", False)),
        ("b", -5, ("a", False)),
        ("d", 1, ("d", True)),  # already at the end: at_limit (A7)
        ("a", -3, ("a", True)),
        ("b", 0, ("b", False)),
        ("x", 1, ("a", False)),  # outside: the first move lands on the first rung ...
        ("x", 3, ("c", False)),  # ... and the rest go on from there (`_ladder_step`)
        ("x", -3, ("a", False)),
        (None, 2, ("b", False)),
    ],
)
def test_clamp_step(current, steps, expected):
    assert clamp_step(LADDER, current, steps) == expected


@pytest.mark.uc("UC-015", br=["BR-004"], flow="A7")
@pytest.mark.parametrize("steps", [1, 3, -2])
def test_clamp_step_restart_outside_lands_on_the_first_rung(steps):
    # `FlagServiceWriter.step_variant`: a variant outside the ladder moves to its first entry.
    assert clamp_step(LADDER, "x", steps, restart_outside=True) == ("a", False)


# --- Wrap (UC-015 BR-005, A7; UC-016 BR-007) ---------------------------------------------------


@pytest.mark.uc("UC-015", br=["BR-005"], flow="A7")
@pytest.mark.uc("UC-016", br=["BR-007"])
@pytest.mark.parametrize(
    ("current", "steps", "expected"),
    [
        ("b", 1, "c"),
        ("d", 1, "a"),  # past the last option to the first ...
        ("a", -1, "d"),  # ... and back past the first to the last
        ("b", 4, "b"),  # n = len: a full turn, the no-op
        ("b", -4, "b"),  # n = -len
        ("b", 5, "c"),  # n = len + 1: one past
        ("b", -5, "a"),
        ("a", 12, "a"),  # 3 x len
        ("b", 0, "b"),
        ("x", 1, "a"),  # outside: the first move lands on the first option ...
        ("x", 3, "c"),  # ... and the rest go on from there
        ("x", -2, "d"),
        (None, 5, "a"),
    ],
)
def test_wrap_step(current, steps, expected):
    assert setting_ladders.wrap_step(LADDER, current, steps) == expected


@pytest.mark.uc("UC-015", br=["BR-004", "BR-005"], flow="A7")
@pytest.mark.uc("UC-016", br=["BR-007"])
def test_wrapping_is_an_explicit_set_of_options():
    """Named one by one, never a default: the kill-switch ladder (agent level) and the raise rows must never wrap."""
    assert {"current_game", "decision.backend", "shield.activation", "objectives"} == setting_ladders.WRAPPING


# Every preview option: its ladder, a status at a given start, and whether it wraps.
def _status_at(option: str, start: str) -> menu_pb2.GetMenuStatusResponse:
    # Effective sensitivity and teams off their ladders, so an `automatic` start is the automatic rung.
    status = menu_pb2.GetMenuStatusResponse(sensitivity=7, num_teams=0, agent_enabled_state="on")
    if option == "current_game":
        status.selection = start
    elif option == "decision.backend":
        status.agent_backend = start
    elif option == "agent_level":
        status.agent_level = start
    elif option in setting_ladders.KNOB_LADDERS:
        status.operator_settings[option] = start
        if option == "shield.activation":
            status.shield_activation = "trigger"
    elif option == "audio_volume.master":
        status.audio_volume_master = start
    elif option == "game_instructions":
        status.instructions = start == "on"
    elif option == "force_all_start":
        status.force_all_start = start == "on"
    elif option in setting_ladders.AGENT_ROW_STATUS_FIELDS:
        setattr(status, setting_ladders.AGENT_ROW_STATUS_FIELDS[option], start)
    return status


LADDERS = {
    "current_game": setting_ladders.MODE_LADDER,
    "decision.backend": setting_ladders.BACKENDS,
    "shield.activation": setting_ladders.SHIELD_KNOB_LADDER,
    "sensitivity": setting_ladders.KNOB_LADDERS["sensitivity"],
    "num_teams": setting_ladders.KNOB_LADDERS["num_teams"],
    "audio_volume.master": setting_ladders.VOLUME_LADDER,
    "game_instructions": setting_ladders.ON_OFF_LADDER,
    "force_all_start": setting_ladders.ON_OFF_LADDER,
    "agent_level": setting_ladders.AGENT_POLICY_LEVELS,
    **setting_ladders.AGENT_ROW_LADDERS,
}


@pytest.mark.uc("UC-015", br=["BR-004", "BR-005", "BR-018"], flow="A7")
@pytest.mark.uc("UC-016", br=["BR-007"])
@pytest.mark.parametrize("option", sorted(LADDERS))
def test_preview_wraps_only_the_wrapping_options(option):
    """Every start, every n up to 3 x len: wrapping options land on (i + n) % len and never report a
    limit (a multiple of len is the no-op); every other option clamps, with `at_limit` at its ends."""
    ladder = LADDERS[option]
    size = len(ladder)
    for i, start in enumerate(ladder):
        status = _status_at(option, start)
        for n in range(-3 * size, 3 * size + 1):
            preview = preview_value(option, status, n)
            case = f"{option} from {start} by {n}"
            if option in setting_ladders.WRAPPING:
                assert preview == Preview(ladder[(i + n) % size]), case
            else:
                target = min(max(i + n, 0), size - 1)
                assert preview == Preview(ladder[target], at_limit=n != 0 and target == i), case


@pytest.mark.uc("UC-015", br=["BR-004"], flow="A7")
@pytest.mark.parametrize(
    ("level", "enabled", "steps", "expected"),
    [
        ("none", "on", -1, Preview("none", at_limit=True)),
        ("none", "off", 1, Preview("ambient")),  # a stopped agent's live level moves too
        ("full", "on", 1, Preview("full", at_limit=True)),
        ("ambient", "on", -2, Preview("none")),  # down past none stops at none, never wraps to full
        ("ambient", "off", -3, Preview("none")),
    ],
)
def test_agent_level_stays_clamped_at_both_ends(level, enabled, steps, expected):
    """The agent level never wraps (none -> full would be an escalation)."""
    status = menu_pb2.GetMenuStatusResponse(agent_level=level, agent_enabled_state=enabled)
    assert preview_value("agent_level", status, steps) == expected


def _value_order(flag_set: str, key: str) -> list[str]:
    variants = json.loads((FLAGD_DIR / f"{flag_set}.json").read_text())["flags"][key]["variants"]
    return sorted(variants, key=lambda name: variants[name])


@pytest.mark.uc("UC-015", br=["BR-004", "BR-006"])
@pytest.mark.parametrize(
    ("ladder", "flag_set", "key"),
    [
        (setting_ladders.SENSITIVITY_LADDER, "game", "sensitivity"),
        (setting_ladders.TEAMS_LADDER, "game", "num_teams"),
        (setting_ladders.VOLUME_LADDER, "user", "audio_volume.master"),
    ],
)
def test_numeric_ladders_follow_the_flag_values(ladder, flag_set, key):
    """`step_variant` orders numeric variants by value; the shared ladder is that order."""
    assert ladder == _value_order(flag_set, key)


@pytest.mark.uc("UC-015", br=["BR-011"])
def test_voice_tables_cover_every_ladder_value():
    assert len(voice_prompts.SENSITIVITY_VOICES) == len(setting_ladders.SENSITIVITY_LADDER)
    teams = range(setting_ladders.TEAMS_MIN, setting_ladders.TEAMS_MIN + len(setting_ladders.TEAMS_LADDER))
    assert set(voice_prompts.NUM_TEAMS_VOICES) == set(teams)
    assert set(voice_prompts.AGENT_LEVEL_VOICES) == set(setting_ladders.AGENT_POLICY_LEVELS)
    assert set(setting_ladders.MODE_LADDER) <= set(voice_prompts.GAME_MODE_VOICE)
    assert set(voice_prompts.GAME_MODE_VOICE) <= {g.name for g in Games}


@pytest.mark.uc("UC-015", br=["BR-004"])
def test_preview_of_a_status_field_out_of_range_is_unknown():
    assert preview_value("sensitivity", menu_pb2.GetMenuStatusResponse(sensitivity=7), 1) == Preview(None)
    assert preview_value("num_teams", menu_pb2.GetMenuStatusResponse(num_teams=0), 1) == Preview(None)
    assert preview_value("decision.backend", menu_pb2.GetMenuStatusResponse(agent_backend="x"), 1) == Preview(None)
    assert preview_value("current_game", menu_pb2.GetMenuStatusResponse(), 1) == Preview(None)
    assert preview_value("sensitivity", None, 1) == Preview(None)
    assert preview_value("bogus", menu_pb2.GetMenuStatusResponse(), 1) == Preview(None)


@pytest.mark.uc("UC-015", br=["BR-013"])
@pytest.mark.parametrize("enabled", ["on", "off", "unknown"])
def test_agent_level_preview_with_the_level_unresolved_is_unknown(enabled):
    """The menu fails closed when the level cannot be read: no guessed value."""
    status = menu_pb2.GetMenuStatusResponse(agent_level="", agent_enabled_state=enabled)
    assert preview_value("agent_level", status, 1) == Preview(None)


@pytest.mark.uc("UC-015", br=["BR-009", "BR-011"])
def test_pad_words_name_every_backend_and_every_preview_option():
    """#2106: K7 says the backend by name; every setting the pad announces or previews has a label."""
    assert list(voice_prompts.BACKEND_VOICES) == setting_ladders.BACKENDS
    # The agent rows have no voice lines (#2755, D7): the pad shows them only.
    spoken = setting_ladders.PREVIEW_OPTIONS - {"force_all_start"} - setting_ladders.AGENT_ROW_OPTIONS
    assert spoken <= set(voice_prompts.LABEL_VOICES)
    assert voice_prompts.LIMIT_VOICES[True] != voice_prompts.LIMIT_VOICES[False]
    assert set(voice_prompts.LOCK_VOICES.values()).isdisjoint(voice_prompts.AGENT_SWITCH_VOICES.values())


# --- Shield mode (UC-015 BR-018, #2215) -------------------------------------------------------


@pytest.mark.uc("UC-015", br=["BR-018", "BR-005"], steps=[9])
def test_shield_knob_ladder_is_automatic_then_the_flag_files_variants():
    assert setting_ladders.SHIELD_ACTIVATION_LADDER == ["instant", "trigger", "hold"]
    assert setting_ladders.SHIELD_KNOB_LADDER == ["automatic", "instant", "trigger", "hold"]
    assert "shield.activation" not in setting_ladders.FLAG_LADDERS  # the knob ladder is not the flag's
    for flag_file in ("game.json", "ci/game.json"):
        variants = json.loads((FLAGD_DIR / flag_file).read_text())["flags"]["shield.activation"]["variants"]
        assert set(variants) == set(setting_ladders.SHIELD_ACTIVATION_LADDER), flag_file
        assert "automatic" not in variants  # `automatic` is the absence of an operator value


@pytest.mark.uc("UC-015", br=["BR-018", "BR-017", "BR-005"], flow="A7", steps=[9])
@pytest.mark.uc("UC-016", br=["BR-007"])
@pytest.mark.parametrize(
    ("operator", "effective", "steps", "expected"),
    [
        # UC-016 BR-007: no operator value starts at automatic (the wheel's AUTO), not the effective rung ...
        ("automatic", "trigger", 1, Preview("instant")),
        ("automatic", "hold", 3, Preview("hold")),
        ("automatic", "trigger", -1, Preview("hold")),  # ... and the ladder wraps
        ("automatic", "trigger", -2, Preview("trigger")),
        ("automatic", "trigger", 0, Preview("automatic")),  # a net of zero commits nothing
        ("automatic", "", 1, Preview("instant")),
        ("automatic", "hold", 0, Preview("automatic")),  # NOW on AUTO, not on the effective hold
        ("instant", "instant", 1, Preview("trigger")),
        (
            "instant",
            "hold",
            -1,
            Preview("automatic"),
        ),  # an operator value wins over the effective one (a stage overlay)
        ("hold", "hold", -3, Preview("automatic")),
        ("hold", "hold", 1, Preview("automatic")),  # wraps past the top, never at_limit
        ("hold", "hold", 4, Preview("hold")),  # a full turn: the no-op
        ("trigger", "trigger", 5, Preview("hold")),
        ("", "trigger", 1, setting_ladders.UNKNOWN),  # operator value unknown: silent, never a guess
    ],
)
def test_shield_preview_per_step(operator, effective, steps, expected):
    status = menu_pb2.GetMenuStatusResponse(
        shield_activation=effective, operator_settings={"shield.activation": operator} if operator else {}
    )
    assert preview_value("shield.activation", status, steps) == expected


@pytest.mark.uc("UC-015", br=["BR-018", "BR-011"], steps=[9])
def test_shield_voices_cover_the_knob_ladder_and_have_a_label():
    assert set(voice_prompts.SHIELD_ACTIVATION_VOICES) == set(setting_ladders.SHIELD_KNOB_LADDER)
    assert "shield.activation" in voice_prompts.LABEL_VOICES


@pytest.mark.uc("UC-015", br=["BR-018", "BR-017"], steps=[4, 5])
@pytest.mark.parametrize(
    ("option", "operator", "steps", "expected"),
    [
        (
            "sensitivity",
            "automatic",
            1,
            Preview("fast"),
        ),  # no operator value: starts at the effective rung (medium) ...
        ("sensitivity", "automatic", -3, Preview("automatic")),  # ... and automatic is reached below ultra_slow
        ("sensitivity", "automatic", -2, Preview("ultra_slow")),
        ("sensitivity", "slow", -2, Preview("automatic")),  # ... and automatic is the rung below the lowest value
        ("sensitivity", "ultra_fast", 1, Preview("ultra_fast", at_limit=True)),
        ("sensitivity", "", 1, setting_ladders.UNKNOWN),  # unreadable: silent, never a guess
        ("num_teams", "automatic", 5, Preview("six")),  # effective two (the status default)
        ("num_teams", "three", -9, Preview("automatic")),
        ("num_teams", "six", 1, Preview("six", at_limit=True)),
        ("num_teams", "", -1, setting_ladders.UNKNOWN),
    ],
)
def test_automatic_knob_preview_per_step(option, operator, steps, expected):
    status = menu_pb2.GetMenuStatusResponse(sensitivity=2, num_teams=2)  # the effective values never matter
    if operator:
        status.operator_settings[option] = operator
    assert preview_value(option, status, steps) == expected


@pytest.mark.uc("UC-015", br=["BR-018", "BR-011"], steps=[4, 5])
def test_the_automatic_rung_has_one_shared_voice_line():
    assert voice_prompts.SHIELD_ACTIVATION_VOICES["automatic"] == voice_prompts.AUTOMATIC_VOICE


# --- The agent rows (UC-016 BR-012 to BR-014, #2755) ---------------------------------------------


@pytest.mark.uc("UC-016", br=["BR-012"])
def test_agent_row_ladders_are_explicit_and_cover_the_flag_files_variants():
    """Explicit ladders, never the flag file's declared order: up means more (a higher rate, on, claude)."""
    assert setting_ladders.AGENT_ROW_LADDERS == {
        "objectives": ["endurance_focused", "balanced_focused", "accelerate_focused", "chaos_focused", "mixed"],
        "policy.max_interventions_per_minute": ["1", "2", "4", "8", "20"],
        "policy.group_grant.enabled": ["off", "on"],
        "inference.backend": ["stub", "claude"],
        "inference.effort": ["low", "medium", "high", "xhigh", "max"],
        "decision.operator_brief": ["off", "unkillable", "protect_weakest"],
    }
    flags = json.loads((FLAGD_DIR / "agent.json").read_text())["flags"]
    rate = flags["policy.max_interventions_per_minute"]["variants"]
    assert {name: rate[name] for name in setting_ladders.RATE_VARIANTS.values()} == {
        name: int(rung) for rung, name in setting_ladders.RATE_VARIANTS.items()
    }
    assert set(setting_ladders.RATE_VARIANTS) == set(setting_ladders.RATE_LADDER) and len(rate) == 5
    for option in setting_ladders.AGENT_ROW_LADDERS.keys() - {"policy.max_interventions_per_minute"}:
        assert set(flags[option]["variants"]) == set(setting_ladders.AGENT_ROW_LADDERS[option]), option
    assert flags["decision.operator_brief"]["variants"]["off"] == ""
    # The GOAL wheel is `objectives` in the flag's declared order.
    assert list(flags["objectives"]["variants"]) == setting_ladders.AGENT_ROW_LADDERS["objectives"]


@pytest.mark.uc("UC-016", br=["BR-012", "BR-014"])
@pytest.mark.parametrize("option", sorted(setting_ladders.AGENT_ROW_LADDERS))
@pytest.mark.parametrize("value", ["", "50", "custom", "lively", "8.0", "?"])
def test_agent_row_preview_from_an_unknown_or_off_ladder_value_is_unknown(option, value):
    """D4, D11: never moved to the first rung; an empty field (unknown), a fixture's rate of 50 or a free
    brief text (`custom`) previews nothing, as the menu refuses the step."""
    status = _status_at(option, value)
    for steps in (-6, -1, 1, 6):
        assert preview_value(option, status, steps) == Preview(None)


@pytest.mark.uc("UC-016", br=["BR-012", "BR-014"])
@pytest.mark.parametrize("steps", [-6, -1, 1, 6])
def test_retired_openai_backend_previews_like_any_unknown_value(steps):
    """A persisted `openai` (older release) is off the ladder: never moved to a rung, as the menu refuses."""
    assert "openai" not in setting_ladders.BACKENDS
    assert preview_value("decision.backend", _status_at("decision.backend", "openai"), steps) == Preview(None)


@pytest.mark.uc("UC-016", br=["BR-014"])
@pytest.mark.parametrize(
    ("option", "old", "new", "expected"),
    [
        ("policy.max_interventions_per_minute", "2", "8", True),
        ("policy.max_interventions_per_minute", "8", "2", False),
        ("policy.max_interventions_per_minute", "50", "2", None),  # off the ladder: never judged
        ("policy.group_grant.enabled", "off", "on", True),
        ("policy.group_grant.enabled", "on", "off", False),
        ("inference.backend", "stub", "claude", True),
        ("inference.backend", "claude", "stub", False),
        ("inference.effort", "medium", "high", True),
        ("inference.effort", "max", "low", False),
        ("inference.effort", "", "low", None),
        ("agent_level", "ambient", "full", True),
        ("agent_level", "full", "none", False),
        ("decision.backend", "rules", "claude", True),  # free to paid (D12)
        ("decision.backend", "rules", "jev", True),
        ("decision.backend", "claude", "jev", False),  # paid to paid
        ("decision.backend", "jev", "rules", False),
        ("decision.backend", "openai", "rules", False),  # retired backend persisted by an older release
        ("decision.backend", "openai", "claude", None),  # unknown NOW with a paid NEW: refused
        ("decision.backend", "", "claude", None),  # unknown NOW with a paid NEW: refused
        ("decision.backend", "", "rules", False),
        ("objectives", "balanced_focused", "chaos_focused", False),
        ("decision.operator_brief", "off", "unkillable", False),
    ],
)
def test_is_raise(option, old, new, expected):
    assert setting_ladders.is_raise(option, old, new) is expected


@pytest.mark.uc("UC-016", br=["BR-014"])
def test_paid_backends_are_every_backend_but_rules():
    assert set(setting_ladders.BACKENDS) - {"rules"} == setting_ladders.PAID_BACKENDS
    assert {
        "policy.max_interventions_per_minute",
        "policy.group_grant.enabled",
        "inference.backend",
        "inference.effort",
    } == setting_ladders.RAISE_LADDER_OPTIONS
