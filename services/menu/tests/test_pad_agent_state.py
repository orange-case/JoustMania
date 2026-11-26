"""The interventions switch and the guarded resume on the menu side (#2747).

UC-016 BR-011, BR-012, A10; UC-015 BR-002, BR-003, A9 (planned parts); services.md §3.1. A real
MenuServicer over the fake flag service of test_pad_commands.
"""

import pytest

from lib.types import Sound
from services.menu.tests import test_pad_commands
from services.menu.tests.test_pad_commands import failure_cues, pad

flags = test_pad_commands.flags
servicer = test_pad_commands.servicer

LEVEL = ("agent", "interventions_allowed")
ENABLED = ("agent", "enabled")
GATE = ("agent", "interventions_enabled")


# --- interventions_switch ---------------------------------------------------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-012"])
@pytest.mark.parametrize(("gate", "state"), [("off", "on"), ("on", "off")])
async def test_switch_writes_only_the_gate_in_memory(servicer, flags, gate, state):
    flags.values[GATE] = gate

    reply = await pad(servicer, "interventions_switch", state=state)

    assert (reply.success, reply.error, reply.value) == (True, "", state)
    assert flags.written() == [("agent", "interventions_enabled", state)]
    assert all("persist" not in body for _set, _key, body in flags.puts)
    assert (flags.values[ENABLED], flags.values[LEVEL]) == ("on", "standard")


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-012"])
@pytest.mark.uc("UC-015", br=["BR-003"])
@pytest.mark.parametrize(("level", "enabled"), [("standard", "off"), ("none", "on")])
async def test_switch_of_a_fully_stopped_agent_is_stale(servicer, flags, level, enabled):
    """Its resume restores the gate of the stop, so the switch would be overridden: refused, nothing written."""
    flags.values[LEVEL] = level
    flags.values[ENABLED] = enabled

    reply = await pad(servicer, "interventions_switch", state="on")

    assert (reply.success, reply.error, reply.value) == (False, "stale", "")
    assert flags.puts == []
    assert failure_cues(servicer) == 0


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-012"])
@pytest.mark.parametrize("state", ["", "ON", "toggle", "1"])
async def test_switch_with_a_bad_state_is_refused(servicer, flags, state):
    reply = await pad(servicer, "interventions_switch", state=state)
    assert (reply.success, reply.error) == (False, "bad_option")
    assert flags.puts == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-012"])
@pytest.mark.uc("UC-015", flow="A3", br=["BR-009", "BR-010"])
async def test_a_failed_switch_fails_closed(servicer, flags):
    flags.failing = {GATE}
    reply = await pad(servicer, "interventions_switch", state="on")
    assert (reply.success, reply.error, reply.value) == (False, "flag_service_unavailable", "")
    assert failure_cues(servicer) == 2


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-012"])
@pytest.mark.parametrize("unreadable", [LEVEL, ENABLED])
async def test_a_switch_with_the_agent_state_unreadable_writes_nothing(servicer, flags, unreadable):
    flags.failing_get = {unreadable}
    reply = await pad(servicer, "interventions_switch", state="on")
    assert (reply.success, reply.error) == (False, "flag_service_unavailable")
    assert flags.puts == []


# --- stop remembers, resume restores (maintainer decision 2 "Resume") ------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
@pytest.mark.uc("UC-015", flow="A9", br=["BR-002", "BR-003"])
async def test_k5_stop_of_a_watching_agent_writes_both_keys_and_the_resume_brings_it_back_watching(servicer, flags):
    """K5 "always stop": a gate-only (watching) agent is not stopped, so the stop writes both keys."""
    flags.values[GATE] = "off"

    stop = await pad(servicer, "agent_switch", direction="stop")

    assert (stop.success, stop.error) == (True, "ok")
    assert flags.written() == [("agent", "interventions_allowed", "none"), ("agent", "enabled", "off")]
    flags.puts.clear()

    resume = await pad(servicer, "agent_switch", direction="resume", only_if_stopped=1)

    assert (resume.success, resume.error) == (True, "ok")
    assert flags.written() == [
        ("agent", "interventions_allowed", "standard"),
        ("agent", "interventions_enabled", "off"),
        ("agent", "enabled", "on"),
    ]
    assert (servicer.settings_ops.saved_agent_level, servicer.settings_ops.saved_agent_gate) == (None, None)


@pytest.mark.asyncio
@pytest.mark.uc("UC-015", br=["BR-002", "BR-003"])
async def test_a_stop_with_the_gate_unreadable_remembers_it_closed(servicer, flags):
    flags.failing_get = {GATE}
    await pad(servicer, "agent_switch", direction="stop")
    assert servicer.settings_ops.saved_agent_gate == "off"


@pytest.mark.asyncio
@pytest.mark.uc("UC-015", flow="A9", br=["BR-002"])
async def test_a_stop_of_a_stopped_agent_keeps_what_the_first_stop_remembered(servicer, flags):
    flags.values[GATE] = "on"
    await pad(servicer, "agent_switch", direction="stop")
    flags.values[GATE] = "off"

    await pad(servicer, "agent_switch", direction="stop")

    assert (servicer.settings_ops.saved_agent_level, servicer.settings_ops.saved_agent_gate) == ("standard", "on")


@pytest.mark.asyncio
@pytest.mark.uc("UC-015", br=["BR-002", "BR-003", "BR-004"])
async def test_a_ladder_step_down_to_none_remembers_the_gate(servicer, flags):
    flags.values[GATE] = "on"
    await pad(servicer, "step_setting", option="agent_level", direction="down", steps=5)
    assert (servicer.settings_ops.saved_agent_level, servicer.settings_ops.saved_agent_gate) == ("standard", "on")


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
@pytest.mark.uc("UC-015", flow="A9", br=["BR-003"])
@pytest.mark.parametrize("gate", ["on", "off"], ids=["running", "watching"])
async def test_guarded_resume_of_an_agent_not_fully_stopped_is_stale(servicer, flags, gate):
    flags.values[GATE] = gate

    reply = await pad(servicer, "agent_switch", direction="resume", only_if_stopped=1)

    assert (reply.success, reply.error) == (False, "stale")
    assert flags.puts == []
    assert failure_cues(servicer) == 0
    servicer.state_manager.audio.play_voice.assert_not_awaited()


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-011"])
@pytest.mark.parametrize("unreadable", [LEVEL, ENABLED])
async def test_guarded_resume_with_the_agent_state_unreadable_writes_nothing(servicer, flags, unreadable):
    flags.values[ENABLED] = "off"
    flags.failing_get = {unreadable}

    reply = await pad(servicer, "agent_switch", direction="resume", only_if_stopped=1)

    assert (reply.success, reply.error) == (False, "flag_service_unavailable")
    assert flags.puts == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-011"])
@pytest.mark.parametrize("value", ["0", "true", ""])
async def test_guard_with_another_value_is_a_bad_option(servicer, flags, value):
    flags.values[ENABLED] = "off"
    reply = await pad(servicer, "agent_switch", direction="resume", only_if_stopped=value)
    assert (reply.success, reply.error) == (False, "bad_option")
    assert flags.puts == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
@pytest.mark.uc("UC-015", br=["BR-003"])
async def test_resume_never_brings_back_more_than_the_stop_left(servicer, flags):
    """A watching agent stopped, then raised and its gate opened by someone else: back to the stop's state."""
    flags.values[GATE] = "off"
    await pad(servicer, "agent_switch", direction="stop")
    flags.values[LEVEL] = "full"
    flags.values[GATE] = "on"
    flags.puts.clear()

    await pad(servicer, "agent_switch", direction="resume", only_if_stopped=1)

    assert flags.values[LEVEL] == "standard"
    assert flags.values[GATE] == "off"
    assert flags.values[ENABLED] == "on"


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-012"])
@pytest.mark.uc("UC-031", br=["BR-007"])
@pytest.mark.parametrize(
    ("state", "voice"), [("on", Sound.MENU_VOX_INTERVENTIONS_ON), ("off", Sound.MENU_VOX_INTERVENTIONS_OFF)]
)
async def test_switch_says_interventions_on_or_off(servicer, flags, state, voice):
    flags.values[GATE] = "off" if state == "on" else "on"

    await pad(servicer, "interventions_switch", state=state)

    servicer.state_manager.audio.play_voice.assert_awaited_once_with(voice)


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-012"])
async def test_a_refused_switch_says_nothing(servicer, flags):
    flags.values[ENABLED] = "off"

    await pad(servicer, "interventions_switch", state="on")

    servicer.state_manager.audio.play_voice.assert_not_awaited()


# --- a stale stop memory never raises the agent (review of #2753) --------------------------------


async def stop_then_bring_back_outside_and_kill(menu, flags) -> None:
    """Stop at full with the gate open; then, outside the menu, the agent comes back at ambient with
    the gate closed and is killed again by `agent-killswitch.sh off` (enabled off, level untouched)."""
    flags.values[LEVEL] = "full"
    flags.values[GATE] = "on"
    await pad(menu, "agent_switch", direction="stop")
    flags.values[LEVEL] = "ambient"
    flags.values[ENABLED] = "on"
    flags.values[GATE] = "off"


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
@pytest.mark.uc("UC-015", br=["BR-003"])
async def test_a_stop_memory_the_menu_saw_the_agent_leave_is_forgotten(servicer, flags):
    await stop_then_bring_back_outside_and_kill(servicer, flags)
    await servicer.settings_ops.refresh_operator_settings()  # the pad's status poll sees it running
    flags.values[ENABLED] = "off"
    flags.puts.clear()

    reply = await pad(servicer, "agent_switch", direction="resume", only_if_stopped=1)

    assert (reply.success, reply.error) == (True, "ok")
    # Nothing remembered: ambient, the gate as it stands (UC-015 BR-003).
    assert flags.written() == [("agent", "interventions_allowed", "ambient"), ("agent", "enabled", "on")]


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
@pytest.mark.uc("UC-015", br=["BR-003"])
async def test_a_resume_from_a_stale_stop_memory_only_lowers(servicer, flags):
    """The menu never saw the agent run in between: the memory is capped by the live level and gate."""
    await stop_then_bring_back_outside_and_kill(servicer, flags)
    flags.values[ENABLED] = "off"
    flags.puts.clear()

    await pad(servicer, "agent_switch", direction="resume", only_if_stopped=1)

    assert flags.written() == [
        ("agent", "interventions_allowed", "ambient"),
        ("agent", "interventions_enabled", "off"),
        ("agent", "enabled", "on"),
    ]


@pytest.mark.asyncio
@pytest.mark.uc("UC-015", br=["BR-003"])
@pytest.mark.parametrize(
    "observe",
    [
        lambda menu: menu.settings_ops.refresh_operator_settings(),
        lambda menu: pad(menu, "interventions_switch", state="off"),
        lambda menu: pad(menu, "step_setting", option="agent_level", direction="up"),
        lambda menu: pad(menu, "agent_switch", direction="resume"),
        lambda menu: pad(menu, "reset_setting", option="agent_level", expect="standard"),
    ],
    ids=["status-read", "interventions-switch", "level-step", "unguarded-resume", "level-reset"],
)
async def test_every_menu_read_of_a_running_agent_forgets_the_stop_memory(servicer, flags, observe):
    await stop_then_bring_back_outside_and_kill(servicer, flags)

    await observe(servicer)

    assert (servicer.settings_ops.saved_agent_level, servicer.settings_ops.saved_agent_gate) == (None, None)


@pytest.mark.asyncio
@pytest.mark.uc("UC-015", br=["BR-002", "BR-003"])
async def test_a_stop_at_a_level_it_cannot_save_forgets_the_older_memory(servicer, flags):
    servicer.settings_ops.saved_agent_level = "full"
    flags.values[LEVEL] = "probe"

    await pad(servicer, "agent_switch", direction="stop")

    assert servicer.settings_ops.saved_agent_level is None


@pytest.mark.asyncio
@pytest.mark.uc("UC-015", flow="A3", br=["BR-003", "BR-010"])
async def test_an_unguarded_resume_with_enabled_unreadable_fails_closed(servicer, flags):
    flags.failing_get = {ENABLED}
    reply = await pad(servicer, "agent_switch", direction="resume")
    assert (reply.success, reply.error) == (False, "flag_service_unavailable")
    assert flags.puts == []
