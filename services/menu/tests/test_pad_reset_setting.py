"""The pad's reset keys on the menu side: `reset_setting`, `setting_defaults`, `step_setting from` (#2747).

UC-016 A8, BR-013, BR-014; services.md §3.1. A real MenuServicer over the fake flag service of
test_pad_commands, which serves the layers a reset target is computed from (stack overlay, admin
value, real overlay, baseline).
"""

import pytest

from lib.types import Sound
from lib.voice_prompts import AGENT_LEVEL_VOICES, AUTOMATIC_VOICE, INSTRUCTIONS_VOICES
from proto import menu_pb2
from services.menu import settings_ops
from services.menu.settings_ops import Result
from services.menu.tests import test_pad_commands
from services.menu.tests.test_menu_status import FLAGS, in_process_client
from services.menu.tests.test_pad_commands import AGENT_LEVELS, BASELINE, failure_cues, live_lifecycle, pad

flags = test_pad_commands.flags
servicer = test_pad_commands.servicer

AUTOMATIC_SETTINGS = {"sensitivity": "fast", "num_teams": "four", "shield.activation": "hold"}
# option -> (flag key, operator value, baseline the reset puts back)
DEFAULT_SETTINGS = {
    "game_instructions": (("user", "game_instructions"), "on", "off"),
    "force_all_start": (("game", "force_all_start"), "on", "off"),
    "decision.backend": (("agent", "decision.backend"), "claude", "rules"),
}
LEVEL = ("agent", "interventions_allowed")
ENABLED = ("agent", "enabled")
GATE = ("agent", "interventions_enabled")


@pytest.fixture(autouse=True)
def no_flash_gap(monkeypatch):
    monkeypatch.setattr(settings_ops, "AUTOMATIC_FLASH_GAP_SECONDS", 0)


def operator_set(flags, key, variant: str) -> None:
    flags.admin[key] = flags.values[key] = variant


def voices(menu) -> list:
    return [c.args[0] for c in menu.state_manager.audio.play_voice.await_args_list]


async def status(menu) -> menu_pb2.GetMenuStatusResponse:
    menu.game_settings_client = in_process_client(FLAGS["game"])
    menu.user_prefs_client = in_process_client(FLAGS["user"])
    menu.agent_settings_client = in_process_client(FLAGS["agent"])
    return await menu.GetMenuStatus(menu_pb2.GetMenuStatusRequest(), None)


# --- settings with an `automatic` rung ------------------------------------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
@pytest.mark.parametrize("option", list(AUTOMATIC_SETTINGS))
async def test_reset_of_an_automatic_setting_clears_the_operator_value(servicer, flags, option):
    operator_set(flags, ("game", option), AUTOMATIC_SETTINGS[option])

    reply = await pad(servicer, "reset_setting", option=option, expect="automatic")

    assert (reply.success, reply.error, reply.value) == (True, "", "automatic")
    assert flags.deletes == [("game", option, "persist=true")]
    assert flags.puts == []
    assert servicer.settings_ops.operator_settings[option] == "automatic"
    assert voices(servicer) == [AUTOMATIC_VOICE]


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
async def test_reset_of_an_automatic_setting_already_automatic_is_a_silent_no_op(servicer, flags):
    reply = await pad(servicer, "reset_setting", option="sensitivity")

    assert (reply.success, reply.error, reply.value) == (True, "ok", "automatic")
    assert flags.deletes == []
    assert voices(servicer) == []


# --- settings whose default is the layer below the operator's --------------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
@pytest.mark.parametrize("option", list(DEFAULT_SETTINGS))
async def test_reset_clears_the_operator_value_and_answers_what_is_now_in_force(servicer, flags, option):
    key, operator, baseline = DEFAULT_SETTINGS[option]
    operator_set(flags, key, operator)

    reply = await pad(servicer, "reset_setting", option=option, expect=baseline)

    assert (reply.success, reply.error, reply.value) == (True, "", baseline)
    assert flags.deletes == [(*key, "persist=true")]
    assert flags.values[key] == baseline
    assert flags.puts == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
async def test_reset_plays_the_settings_voice_for_the_value_now_in_force(servicer, flags):
    operator_set(flags, ("user", "game_instructions"), "on")
    await pad(servicer, "reset_setting", option="game_instructions")
    assert voices(servicer) == [INSTRUCTIONS_VOICES[False]]


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
@pytest.mark.parametrize("option", list(DEFAULT_SETTINGS))
async def test_reset_without_an_operator_value_is_a_silent_no_op_with_the_value(servicer, flags, option):
    _key, _operator, baseline = DEFAULT_SETTINGS[option]

    reply = await pad(servicer, "reset_setting", option=option)

    assert (reply.success, reply.error, reply.value) == (True, "ok", baseline)
    assert (flags.deletes, flags.puts) == ([], [])
    assert voices(servicer) == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
async def test_reset_target_is_the_real_overlay_over_the_baseline(servicer, flags):
    key = ("agent", "decision.backend")
    flags.real[key] = "openai"
    operator_set(flags, key, "claude")

    reply = await pad(servicer, "reset_setting", option="decision.backend", expect="openai")

    assert (reply.success, reply.value) == (True, "openai")
    assert flags.values[key] == "openai"


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
async def test_reset_never_clears_an_operator_value_masked_by_a_stack_overlay(servicer, flags):
    key = ("agent", "decision.backend")
    flags.admin[key] = "claude"
    flags.stack[key] = flags.values[key] = "jev"

    reply = await pad(servicer, "reset_setting", option="decision.backend", expect="jev")

    assert (reply.success, reply.error, reply.value) == (True, "ok", "jev")
    assert flags.deletes == []
    assert flags.admin[key] == "claude"


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-013"])
@pytest.mark.parametrize(
    ("option", "expect"), [("game_instructions", "on"), ("decision.backend", "claude"), ("sensitivity", "fast")]
)
async def test_reset_with_a_different_expect_is_stale_and_clears_nothing(servicer, flags, option, expect):
    for key, operator, _baseline in DEFAULT_SETTINGS.values():
        operator_set(flags, key, operator)
    operator_set(flags, ("game", "sensitivity"), "fast")

    reply = await pad(servicer, "reset_setting", option=option, expect=expect)

    assert (reply.success, reply.error, reply.value) == (False, "stale", "")
    assert flags.deletes == []
    assert failure_cues(servicer) == 0


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
@pytest.mark.parametrize(
    "option",
    [
        "audio_volume.master",
        "current_game",
        "goal.profile",
        "enabled",
        "interventions_enabled",
        "interventions_allowed",
        "policy.max_interventions_per_minute",
        "",
    ],
)
async def test_reset_refuses_every_option_off_the_allowlist(servicer, flags, option):
    reply = await pad(servicer, "reset_setting", option=option, expect="x")
    assert (reply.success, reply.error) == (False, "bad_option")
    assert (flags.deletes, flags.puts) == ([], [])


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
@pytest.mark.uc("UC-015", flow="A1")
@pytest.mark.parametrize("option", sorted(settings_ops.RESET_LOBBY_ONLY_OPTIONS))
async def test_reset_of_a_lobby_setting_is_refused_while_a_game_is_live(servicer, flags, option):
    operator_set(flags, ("game", "sensitivity"), "fast")
    operator_set(flags, ("user", "game_instructions"), "on")
    live_lifecycle(servicer)

    reply = await pad(servicer, "reset_setting", option=option)

    assert (reply.success, reply.error) == (False, "game_active")
    assert flags.deletes == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
@pytest.mark.uc("UC-015", flow="A1")
@pytest.mark.parametrize(("option", "expect"), [("agent_level", "standard"), ("decision.backend", "rules")])
async def test_reset_of_the_agent_settings_works_while_a_game_is_live(servicer, flags, option, expect):
    operator_set(flags, ("agent", "decision.backend"), "claude")
    operator_set(flags, LEVEL, "full")
    live_lifecycle(servicer)

    reply = await pad(servicer, "reset_setting", option=option, expect=expect)

    assert (reply.success, reply.value) == (True, expect)


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-013"])
@pytest.mark.uc("UC-015", flow="A3", br=["BR-009", "BR-010"])
@pytest.mark.parametrize(
    ("option", "key"), [("game_instructions", ("user", "game_instructions")), ("num_teams", ("game", "num_teams"))]
)
async def test_a_failed_clear_fails_closed_with_the_failure_cue(servicer, flags, option, key):
    operator_set(flags, key, "on" if option == "game_instructions" else "four")
    flags.failing = {key}

    reply = await pad(servicer, "reset_setting", option=option)

    assert (reply.success, reply.error, reply.value) == (False, "flag_service_unavailable", "")
    assert failure_cues(servicer) == 2
    assert voices(servicer) == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-013"])
@pytest.mark.parametrize("option", ["game_instructions", "sensitivity", "agent_level"])
async def test_an_unreadable_setting_fails_closed(servicer, flags, option):
    flags.down = True
    reply = await pad(servicer, "reset_setting", option=option, expect="standard")
    assert (reply.success, reply.error) == (False, "flag_service_unavailable")
    assert flags.deletes == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-013"])
async def test_a_reset_never_answers_success_with_an_empty_value(servicer, monkeypatch):
    async def empty(_option, _expect):
        return Result(None, "")

    monkeypatch.setattr(servicer.settings_ops, "reset_setting", empty)
    reply = await pad(servicer, "reset_setting", option="game_instructions")
    assert (reply.success, reply.error, reply.value) == (False, "flag_service_unavailable", "")
    assert failure_cues(servicer) == 2


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-013"])
@pytest.mark.parametrize(
    "command",
    [
        {"command": "agent_switch", "direction": "stop"},
        {"command": "agent_switch", "direction": "resume"},
        {"command": "step_setting", "option": "agent_level", "direction": "down"},
        {"command": "reset_setting", "option": "agent_level", "expect": "standard"},
        {"command": "interventions_switch", "state": "off"},
    ],
    ids=["stop", "resume", "level-step", "level-reset", "interventions-switch"],
)
async def test_an_agent_command_makes_the_level_default_unknown(servicer, command):
    servicer.settings_ops.setting_defaults = {"agent_level": "standard", "game_instructions": "off"}

    await pad(servicer, **command)

    assert servicer.settings_ops.setting_defaults == {"game_instructions": "off"}


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-013"])
@pytest.mark.parametrize(
    ("command", "option"),
    [
        ({"command": "set_backend", "backend": "claude"}, "decision.backend"),
        ({"command": "step_setting", "option": "game_instructions", "direction": "up"}, "game_instructions"),
        ({"command": "step_setting", "option": "force_all_start", "direction": "up"}, "force_all_start"),
    ],
)
async def test_a_persisted_write_makes_its_default_unknown(servicer, command, option):
    """A persisted write drops a stack overlay (flags.md §13), so the reset target may move."""
    ops = servicer.settings_ops
    ops.setting_defaults = {"decision.backend": "rules", "game_instructions": "off", "force_all_start": "off"}
    generation = ops._defaults_generation

    reply = await pad(servicer, **command)

    assert reply.success
    assert option not in ops.setting_defaults
    assert len(ops.setting_defaults) == 2
    assert ops._defaults_generation > generation


# --- agent_level (D2: by the agent's state, under the agent lock) ----------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
async def test_agent_level_reset_needs_expect(servicer, flags):
    operator_set(flags, LEVEL, "full")
    reply = await pad(servicer, "reset_setting", option="agent_level")
    assert (reply.success, reply.error) == (False, "bad_option")
    assert flags.deletes == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
@pytest.mark.uc("UC-015", br=["BR-003"])
@pytest.mark.parametrize(("level", "enabled"), [("full", "off"), ("none", "on")])
async def test_a_stopped_agents_reset_moves_only_the_saved_level(servicer, flags, level, enabled):
    """Never clears, never resumes: the saved level becomes the baseline default (standard)."""
    operator_set(flags, LEVEL, level)
    flags.values[ENABLED] = enabled
    servicer.settings_ops.saved_agent_level = "full"

    reply = await pad(servicer, "reset_setting", option="agent_level", expect="standard")

    assert (reply.success, reply.error, reply.value) == (True, "", "standard")
    assert (flags.deletes, flags.puts) == ([], [])
    assert servicer.settings_ops.saved_agent_level == "standard"
    assert voices(servicer) == [AGENT_LEVEL_VOICES["standard"]]


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
@pytest.mark.uc("UC-015", br=["BR-003"])
async def test_a_stopped_agent_with_a_baseline_of_none_saves_ambient(servicer, flags, monkeypatch):
    monkeypatch.setitem(BASELINE, LEVEL, (AGENT_LEVELS, "none"))
    flags.values[ENABLED] = "off"
    servicer.settings_ops.saved_agent_level = "full"

    reply = await pad(servicer, "reset_setting", option="agent_level", expect="ambient")

    assert (reply.success, reply.value) == (True, "ambient")
    assert servicer.settings_ops.saved_agent_level == "ambient"


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013", "BR-012"])
@pytest.mark.parametrize("gate", ["on", "off"], ids=["running", "watching"])
async def test_a_running_or_watching_agents_reset_clears_the_live_level_only(servicer, flags, gate):
    operator_set(flags, LEVEL, "full")
    flags.values[GATE] = gate

    reply = await pad(servicer, "reset_setting", option="agent_level", expect="standard")

    assert (reply.success, reply.value) == (True, "standard")
    assert flags.deletes == [(*LEVEL, "persist=true")]
    assert flags.puts == []  # never enabled, never the gate
    assert (flags.values[ENABLED], flags.values[GATE]) == ("on", gate)


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
async def test_agent_level_reset_never_clears_a_value_masked_by_a_stack_overlay(servicer, flags):
    flags.admin[LEVEL] = "full"
    flags.stack[LEVEL] = flags.values[LEVEL] = "ambient"

    reply = await pad(servicer, "reset_setting", option="agent_level", expect="ambient")

    assert (reply.success, reply.error, reply.value) == (True, "ok", "ambient")
    assert flags.deletes == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-013", "BR-014"])
@pytest.mark.parametrize("off_ladder", ["probe", "shadow_experimental"])
async def test_agent_level_reset_to_a_level_off_the_ladder_is_refused(servicer, flags, off_ladder):
    flags.real[LEVEL] = off_ladder
    operator_set(flags, LEVEL, "full")

    reply = await pad(servicer, "reset_setting", option="agent_level", expect=off_ladder)

    assert (reply.success, reply.error) == (False, "bad_option")
    assert flags.deletes == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-013"])
async def test_agent_level_reset_is_stale_when_the_agent_stopped_since_the_screen(servicer, flags):
    """Shown NEW ambient (stack overlay, running); a stop since then makes the target the baseline."""
    flags.admin[LEVEL] = "full"
    flags.stack[LEVEL] = "ambient"
    flags.values[LEVEL] = "ambient"
    flags.values[ENABLED] = "off"

    reply = await pad(servicer, "reset_setting", option="agent_level", expect="ambient")

    assert (reply.success, reply.error) == (False, "stale")
    assert servicer.settings_ops.saved_agent_level is None


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
@pytest.mark.uc("UC-015", br=["BR-002", "BR-003"])
async def test_agent_level_reset_down_to_none_saves_the_level_and_gate_it_left(servicer, flags, monkeypatch):
    monkeypatch.setitem(BASELINE, LEVEL, (AGENT_LEVELS, "none"))
    operator_set(flags, LEVEL, "full")
    flags.values[GATE] = "on"

    reply = await pad(servicer, "reset_setting", option="agent_level", expect="none")

    assert (reply.success, reply.value) == (True, "none")
    assert (servicer.settings_ops.saved_agent_level, servicer.settings_ops.saved_agent_gate) == ("full", "on")


# --- setting_defaults (GetMenuStatus field 28) -----------------------------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
@pytest.mark.parametrize(
    ("layers", "expected"),
    [
        ({}, "standard"),
        ({"admin": "full"}, "standard"),
        ({"admin": "full", "real": "ambient"}, "ambient"),
        ({"admin": "ambient", "stack": "full"}, "full"),
        ({"enabled": "off", "admin": "full", "real": "ambient"}, "standard"),
    ],
    ids=["baseline", "admin", "real", "stack-masks-admin", "stopped"],
)
async def test_setting_defaults_is_what_the_reset_puts_in_force(servicer, flags, layers, expected):
    """One function (D2): a reset sent with the reported default as expect is never stale and lands on it."""
    if "admin" in layers:
        operator_set(flags, LEVEL, layers["admin"])
    if "real" in layers:
        flags.real[LEVEL] = layers["real"]
    if "stack" in layers:
        flags.stack[LEVEL] = flags.values[LEVEL] = layers["stack"]
    flags.values[ENABLED] = layers.get("enabled", "on")
    operator_set(flags, ("user", "game_instructions"), "on")

    await servicer.settings_ops.refresh_operator_settings()
    defaults = (await status(servicer)).setting_defaults

    assert set(defaults) == {"game_instructions", "force_all_start", "agent_level", "decision.backend"}
    assert (defaults["agent_level"], defaults["game_instructions"]) == (expected, "off")
    for option in ("agent_level", "game_instructions"):
        reply = await pad(servicer, "reset_setting", option=option, expect=defaults[option])
        assert (reply.success, reply.value) == (True, defaults[option])


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
@pytest.mark.parametrize(
    ("failing", "missing"),
    [
        ({("agent", "decision.backend")}, {"decision.backend"}),
        ({ENABLED}, {"agent_level"}),
        ({LEVEL}, {"agent_level"}),
    ],
)
async def test_setting_defaults_drops_a_key_whose_read_failed(servicer, flags, failing, missing):
    flags.failing_get = failing
    await servicer.settings_ops.refresh_operator_settings()
    defaults = (await status(servicer)).setting_defaults

    assert set(defaults) == {"game_instructions", "force_all_start", "agent_level", "decision.backend"} - missing
    assert all(defaults.values())


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-013"])
async def test_setting_defaults_are_never_read_inside_the_call(servicer):
    servicer.settings_ops.setting_defaults = {"agent_level": "ambient"}
    servicer.settings_ops._operator_read_at = float("inf")  # fresh: no background refresh either

    reply = await status(servicer)

    assert dict(reply.setting_defaults) == {"agent_level": "ambient"}


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-013"])
async def test_a_defaults_read_overlapping_an_agent_change_is_dropped(servicer):
    ops = servicer.settings_ops
    ops.setting_defaults = {"agent_level": "full"}
    read = ops._read_setting_defaults

    def read_then_stop():
        values = read()
        ops._defaults_generation += 1  # what an agent command under the agent lock does
        return values

    ops._read_setting_defaults = read_then_stop
    await ops.refresh_operator_settings()
    assert ops.setting_defaults == {"agent_level": "full"}


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-011"])
async def test_status_lists_the_pad_commands_this_menu_understands(servicer):
    reply = await status(servicer)
    assert list(reply.pad_commands) == [
        "reset_setting",
        "interventions_switch",
        "agent_switch.only_if_stopped",
        "step_setting.from",
    ]


# --- step_setting agent_level from=<level> (BR-014) ------------------------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
@pytest.mark.parametrize(
    ("enabled", "saved", "start", "direction"),
    [("on", None, "standard", "up"), ("off", "full", "full", "down"), ("off", None, "ambient", "up")],
)
async def test_level_step_from_the_shown_level_commits(servicer, flags, enabled, saved, start, direction):
    flags.values[ENABLED] = enabled
    servicer.settings_ops.saved_agent_level = saved

    reply = await pad(servicer, "step_setting", option="agent_level", direction=direction, **{"from": start})

    assert (reply.success, reply.error) == (True, "")


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-014"])
@pytest.mark.parametrize(("enabled", "saved", "shown"), [("on", None, "ambient"), ("off", "full", "standard")])
async def test_level_step_from_another_level_is_stale(servicer, flags, enabled, saved, shown):
    flags.values[ENABLED] = enabled
    servicer.settings_ops.saved_agent_level = saved

    reply = await pad(servicer, "step_setting", option="agent_level", direction="up", **{"from": shown})

    assert (reply.success, reply.error) == (False, "stale")
    assert flags.puts == []
    assert servicer.settings_ops.saved_agent_level == saved
    assert Sound.SFX_BEEP_LOUD not in [c.args[0] for c in servicer.state_manager.audio.play_sound.await_args_list]


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-014"])
async def test_from_on_another_option_is_a_bad_option(servicer, flags):
    reply = await pad(servicer, "step_setting", option="num_teams", direction="up", **{"from": "two"})
    assert (reply.success, reply.error) == (False, "bad_option")
    assert flags.puts == []
