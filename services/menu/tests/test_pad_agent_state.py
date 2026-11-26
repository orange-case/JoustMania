"""The stop and the agent switch on the menu side.

UC-015 BR-002, BR-003, A3, A9; UC-016 BR-012; services.md §3.1. A real MenuServicer over the fake
flag service of test_pad_commands.
"""

import pytest

from lib.types import Sound
from services.menu.tests import test_pad_commands
from services.menu.tests.test_pad_commands import failure_cues, pad

flags = test_pad_commands.flags
servicer = test_pad_commands.servicer

LEVEL = ("agent", "interventions_allowed")
ENABLED = ("agent", "enabled")


def set_state(flags, state: str) -> None:
    flags.values[ENABLED] = state


# --- the stop (agent_switch) --------------------------------------------------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-015", flow="main", br=["BR-002"])
@pytest.mark.parametrize("state", ["act", "watch", "off"])
async def test_stop_writes_enabled_alone_whatever_the_state(servicer, flags, state):
    set_state(flags, state)

    reply = await pad(servicer, "agent_switch", direction="stop")

    assert reply.success
    assert flags.written() == [("agent", "enabled", "off")]
    assert all("persist" not in body for _set, _key, body in flags.puts)
    assert flags.values[LEVEL] == "standard"
    servicer.state_manager.audio.play_voice.assert_awaited_with(Sound.MENU_VOX_ADMINOP_OFF)


@pytest.mark.asyncio
@pytest.mark.uc("UC-015", flow="A9", br=["BR-002"])
async def test_stop_never_reads_first(servicer, flags):
    """Idempotent and never held up by a read: unreadable flags still get the stop."""
    flags.failing_get = {LEVEL, ENABLED}

    reply = await pad(servicer, "agent_switch", direction="stop")

    assert reply.success
    assert flags.written() == [("agent", "enabled", "off")]


@pytest.mark.asyncio
@pytest.mark.uc("UC-015", flow="A3", br=["BR-002", "BR-009", "BR-010"])
async def test_a_failed_stop_fails_closed_with_the_failure_cue(servicer, flags):
    flags.failing = {ENABLED}

    reply = await pad(servicer, "agent_switch", direction="stop")

    assert (reply.success, reply.error) == (False, "flag_service_unavailable")
    assert failure_cues(servicer) == 2


@pytest.mark.asyncio
@pytest.mark.uc("UC-015", br=["BR-003"])
@pytest.mark.parametrize("direction", ["resume", "", "STOP"])
async def test_agent_switch_takes_only_the_stop(servicer, flags, direction):
    reply = await pad(servicer, "agent_switch", direction=direction)
    assert (reply.success, reply.error) == (False, "bad_option")
    assert flags.puts == []


# --- agent_state: the AGENT row 1 switch -------------------------------------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-012"])
@pytest.mark.uc("UC-015", br=["BR-003"])
@pytest.mark.parametrize(
    ("now", "state", "written"),
    [
        ("off", "act", [("agent", "enabled", "act")]),
        ("off", "watch", [("agent", "enabled", "watch")]),
        ("watch", "act", [("agent", "enabled", "act")]),
        ("act", "watch", [("agent", "enabled", "watch")]),
        ("act", "off", [("agent", "enabled", "off")]),
        ("watch", "off", [("agent", "enabled", "off")]),
    ],
)
async def test_switch_writes_enabled_alone_and_never_the_level(servicer, flags, now, state, written):
    set_state(flags, now)

    reply = await pad(servicer, "agent_state", state=state, **{"from": now})

    assert (reply.success, reply.value) == (True, state)
    assert flags.written() == written
    assert all("persist" not in body for _set, _key, body in flags.puts)
    assert flags.values[LEVEL] == "standard"


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-012"])
async def test_switch_to_the_state_it_has_is_a_no_op(servicer, flags):
    set_state(flags, "watch")
    reply = await pad(servicer, "agent_state", state="watch", **{"from": "watch"})
    assert (reply.success, reply.error, reply.value) == (True, "ok", "watch")
    assert flags.puts == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-012", "BR-014"])
async def test_switch_from_a_state_that_moved_under_the_hold_is_stale(servicer, flags):
    """A stop from elsewhere during the hold is never undone unseen."""
    set_state(flags, "off")

    reply = await pad(servicer, "agent_state", state="watch", **{"from": "act"})

    assert (reply.success, reply.error) == (False, "stale")
    assert flags.puts == []
    assert failure_cues(servicer) == 0
    servicer.state_manager.audio.play_voice.assert_not_awaited()


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-012"])
@pytest.mark.parametrize(
    "data",
    [
        {"state": "act"},
        {"state": "ON", "from": "off"},
        {"state": "on", "from": "off"},  # the value before the rename to act
        {"state": "watch", "from": "on"},
        {"state": "act", "from": "stopped"},
        {"state": "", "from": "off"},
    ],
)
async def test_switch_with_a_bad_state_or_from_is_refused(servicer, flags, data):
    reply = await pad(servicer, "agent_state", **data)
    assert (reply.success, reply.error) == (False, "bad_option")
    assert flags.puts == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-012"])
async def test_switch_with_the_state_unreadable_writes_nothing(servicer, flags):
    set_state(flags, "act")
    flags.failing_get = {ENABLED}
    reply = await pad(servicer, "agent_state", state="off", **{"from": "act"})
    assert (reply.success, reply.error) == (False, "flag_service_unavailable")
    assert flags.puts == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-012"])
@pytest.mark.uc("UC-015", flow="A3", br=["BR-003", "BR-009", "BR-010"])
async def test_a_failed_write_leaves_the_state(servicer, flags):
    set_state(flags, "off")
    flags.failing = {ENABLED}

    reply = await pad(servicer, "agent_state", state="act", **{"from": "off"})

    assert (reply.success, reply.error) == (False, "flag_service_unavailable")
    assert flags.values[ENABLED] == "off"
    assert failure_cues(servicer) == 2


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-011", "BR-012"])
async def test_a_value_outside_the_three_states_is_unreadable(servicer, flags):
    """A value outside off / watch / act (a bool-era "true", the old "on") is no state: the switch writes nothing."""
    flags.values[ENABLED] = "true"
    reply = await pad(servicer, "agent_state", state="off", **{"from": "act"})
    assert (reply.success, reply.error) == (False, "flag_service_unavailable")
    assert flags.puts == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-012"])
@pytest.mark.uc("UC-031", br=["BR-007"])
@pytest.mark.parametrize(
    ("now", "state", "voice"),
    [
        ("act", "off", Sound.MENU_VOX_ADMINOP_OFF),
        ("off", "act", Sound.MENU_VOX_ADMINOP_ON),
        ("off", "watch", Sound.MENU_VOX_AGENT_WATCHING),
        ("watch", "act", Sound.MENU_VOX_INTERVENTIONS_ON),
        ("act", "watch", Sound.MENU_VOX_INTERVENTIONS_OFF),
    ],
)
async def test_switch_says_the_change(servicer, flags, now, state, voice):
    set_state(flags, now)

    await pad(servicer, "agent_state", state=state, **{"from": now})

    servicer.state_manager.audio.play_voice.assert_awaited_once_with(voice)
