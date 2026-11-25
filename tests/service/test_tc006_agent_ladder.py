"""TC-006 on the service tier (T2): the host restricts the agent with the operator pad (steps 1 to 4).

LOBBY composition on a fresh flag plane: real menu, game coordinator and
controller manager (mock backend) from source at HEAD, flag service + flagd;
audio is FakeAudio, so the per-level voice feedback is asserted on the recorded
PlaySound calls. Since #2819 the host's surface is the pad's ``step_setting``
``agent_level`` (UC-015 BR-004, BR-019), not the removed controller admin mode.

The seam under test is the one the menu's unit tests cannot reach: pad command ->
menu -> flag service (persisted admin value of ``interventions_allowed``, UC-053
A2) -> flagd. The journey leaves an off-ladder level, resumes, walks the ladder up
and down and steps past the top to assert it does not wrap (the #1340 regression,
clamped since #1373).

Steps 6 and 7 are covered in test_tc006_agent_blocked_by_host.py.

The ladder writes the GLOBAL ``interventions_allowed``, so the test runs on
``fresh_stack``: a stack of its own, nothing leaks to another test.
"""

import pytest
from openfeature.evaluation_context import EvaluationContext

from tests.integration.flag_service import default as flag_service_default
from tests.integration.flag_service import variant_value
from tests.integration.helpers import (
    async_poll_until,
    flagd_probe_client,
    get_menu_client,
    pad_command,
)
from tests.service.harness import LOBBY
from tests.service.lobby_flow import start_lobby

pytestmark = pytest.mark.tier("service")

SERVICE_COMPOSITION = LOBBY

AGENT = "agent"
FLAG = "interventions_allowed"
START_MODE = "JoustFFA"
PERSIST_TIMEOUT_SECONDS = 5.0
# The voice follows the persisted write; under load (parallel gates) PlaySound can take well over 5 s (#1643).
VOICE_TIMEOUT_SECONDS = 15.0

# The voice the menu asks the audio service for per level (lib/voice_prompts.py AGENT_LEVEL_VOICES).
LEVEL_VOICE = {"none": "adminop_off", "ambient": "adminop_1", "standard": "adminop_2", "full": "adminop_3"}


def _effective_variant() -> str:
    return flag_service_default().get(AGENT, FLAG)["effective_default"]


def _persisted_admin_variant() -> str | None:
    admin = flag_service_default().get(AGENT, FLAG).get("admin_value")
    return admin.get("variant") if admin else None


async def _step_and_expect(stack, menu_client, direction: str, expected: str) -> None:
    """One pad ladder step; assert the persisted level and the voice feedback."""
    stack.fake_audio.clear()
    reply = await pad_command(menu_client, "step_setting", option="agent_level", direction=direction, steps=1)
    assert reply.success and reply.value == expected, f"step {direction}: {reply.error!r} {reply.value!r}"
    assert await async_poll_until(lambda: _persisted_admin_variant() == expected, timeout=PERSIST_TIMEOUT_SECONDS), (
        f"after {direction}: persisted {FLAG} is {_persisted_admin_variant()!r}, expected {expected!r}"
    )
    assert _effective_variant() == expected
    voice = LEVEL_VOICE[expected]
    assert await async_poll_until(
        lambda: any(c.request.get("file_path") == voice for c in stack.fake_audio.calls_of("PlaySound")),
        timeout=VOICE_TIMEOUT_SECONDS,
    ), f"no {voice!r} feedback for {expected}; PlaySound: {[c.request for c in stack.fake_audio.calls_of('PlaySound')]}"


async def _flagd_serves(stack, variant: str) -> bool:
    expected = variant_value(AGENT, FLAG, variant)
    with flagd_probe_client(stack, AGENT) as probe:
        return await async_poll_until(
            lambda: probe.get_string_value(FLAG, "unset", EvaluationContext()) == expected,
            timeout=PERSIST_TIMEOUT_SECONDS,
        )


@pytest.mark.asyncio
@pytest.mark.uc("UC-015", flow="main", steps=[5], br=["BR-003", "BR-004", "BR-019"], tc="TC-006")
@pytest.mark.uc("UC-015", flow="A7", tc="TC-006")
@pytest.mark.uc("UC-053", flow="A2", tc="TC-006")
@pytest.mark.usefixtures("flag_service")  # binds the flag-service client to this stack
async def test_host_walks_the_agent_ladder_from_the_pad(fresh_stack):
    """The host leaves an off-ladder level, resumes and walks the ladder up and down with the pad;
    a step past the top is refused at_limit (TC-006 steps 1 to 4)."""
    stack = fresh_stack
    menu_client, menu_channel = await get_menu_client(stack)
    try:
        await start_lobby(stack, count=2, game_mode=START_MODE)
        # A running agent (the CI baseline has it off) at a level off the ladder.
        flag_service_default().put(AGENT, "enabled", variant="on")
        flag_service_default().put(AGENT, FLAG, variant="probe")
        assert await async_poll_until(lambda: _effective_variant() == "probe", timeout=PERSIST_TIMEOUT_SECONDS)

        # BR-019: the first step from an off-ladder level lands on none, which stops the agent (BR-002).
        await _step_and_expect(stack, menu_client, "down", "none")

        # BR-003: the resume of a stopped agent with no saved level brings it back at ambient.
        reply = await pad_command(menu_client, "agent_switch", direction="resume")
        assert reply.success, f"resume refused: {reply.error}"
        assert await async_poll_until(lambda: _effective_variant() == "ambient", timeout=PERSIST_TIMEOUT_SECONDS)

        for level in ("standard", "full"):
            await _step_and_expect(stack, menu_client, "up", level)
        # A7: past the top nothing is written (the #1340 wrap would give none).
        reply = await pad_command(menu_client, "step_setting", option="agent_level", direction="up", steps=1)
        assert (reply.success, reply.error) == (False, "at_limit")
        assert _persisted_admin_variant() == "full"

        for level in ("standard", "ambient", "none"):
            await _step_and_expect(stack, menu_client, "down", level)

        # Step 4 / validation 2: flagd serves none; the admin value is persisted.
        assert await _flagd_serves(stack, "none"), "flagd did not serve interventions_allowed=none"
        assert _persisted_admin_variant() == "none"
    finally:
        await menu_channel.close()
