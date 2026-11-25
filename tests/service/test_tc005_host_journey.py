"""TC-005 on the service tier (T2): the host tunes the game with the operator pad and force-starts.

Moved from tests/integration/test_admin_flow.py (#1562); since #2819 the host's
commands are the pad's ``ProcessInput`` commands (UC-015), not the removed
controller admin mode. LOBBY composition: real menu, game coordinator and
controller manager (mock backend) from source at HEAD on a fresh flag plane
(flag service + flagd); audio is FakeAudio, so the UC-011 mode announcement is
asserted on the recorded PlaySound calls.

The seams under test are the ones unit tests cannot reach: the menu -> flag
service -> flagd path the pad's setting writes take (persisted admin values,
UC-053 A2), and the menu -> coordinator force-start handoff (#1029).

- The knob ladders of sensitivity and num_teams start at the effective value when
  no operator value is set (UC-015 BR-018), so one step up is the next value.
- The next game uses the new sensitivity: read from the force-started game's
  ``game_sensitivity`` gauge, so the stack exports OTLP to its sink.
- The writes are persisted admin values on the flag service; the baseline flag
  files of the stack stay byte-for-byte unchanged.
- The test changes stack-wide state (the ``force_all_start`` overlay and
  persisted admin values), so it runs on ``fresh_stack``: a stack of its own.
"""

import dataclasses

import pytest
from openfeature.evaluation_context import EvaluationContext

from proto import game_coordinator_pb2
from tests.integration.flag_service import default as flag_service_default
from tests.integration.flag_service import variant_value
from tests.integration.helpers import (
    GameEventCollector,
    MenuEventCollector,
    async_poll_until,
    flagd_probe_client,
    get_game_client,
    get_menu_client,
    list_games,
    pad_command,
)
from tests.service.harness import LOBBY
from tests.service.lobby_flow import start_lobby

pytestmark = pytest.mark.tier("service")

SERVICE_COMPOSITION = dataclasses.replace(LOBBY, otlp=True)

GAME = "game"
USER = "user"
START_MODE = "JoustFFA"
# The next playable mode after JoustFFA in the game registry (UC-011 step 2).
EXPECTED_MODE = "JoustTeams"
# The voice the menu asks the audio service for (services/menu/utils/audio.py
# GAME_MODE_VOICE, lib/types.py Sound.MENU_VOX_JOUST_TEAMS).
EXPECTED_ANNOUNCEMENT = "menu Joust Teams"
SENSITIVITY_ORDER = ["ultra_slow", "slow", "medium", "fast", "ultra_fast"]
NUM_TEAMS_ORDER = ["two", "three", "four", "five", "six"]

PERSIST_TIMEOUT_SECONDS = 5.0
ANNOUNCE_TIMEOUT_SECONDS = 5.0
EVENT_TIMEOUT_SECONDS = 10.0
START_TIMEOUT_SECONDS = 15.0
# A gauge reaches the sink at the next metrics export.
EXPORT_TIMEOUT_SECONDS = 30.0
COORDINATOR = "game-coordinator-service"


def _default_variant(flag_set: str, flag_key: str) -> str:
    """The variant the flag service serves now (the effective default)."""
    return flag_service_default().get(flag_set, flag_key)["effective_default"]


def _persisted_admin_value(flag_set: str, flag_key: str) -> dict | None:
    """The persisted admin value of a flag (``admin_value`` in the view), if any."""
    return flag_service_default().get(flag_set, flag_key).get("admin_value")


def _flag_files(stack) -> dict[str, bytes]:
    """The stack's baseline flag files (the flag service's baseline directory), by name."""
    return {path.name: path.read_bytes() for path in sorted(stack.flag_plane.flag_dir.glob("*.json"))}


def _next(order: list[str], variant: str) -> str:
    return order[order.index(variant) + 1]


async def _serve_force_all_start(stack) -> None:
    """Precondition: ``force_all_start`` is ``on`` (a stack overlay), served by flagd."""
    flag_service_default().put(GAME, "force_all_start", variant="on")
    with flagd_probe_client(stack, GAME) as probe:
        served = await async_poll_until(
            lambda: probe.get_boolean_value("force_all_start", False, EvaluationContext()) is True,
            rewrite=lambda: flag_service_default().put(GAME, "force_all_start", variant="on"),
        )
    assert served, "flagd never served force_all_start=on"


async def _step_up(menu_client, option: str) -> str:
    """One knob step up of ``option`` committed by the pad (UC-015 step 4 / 5); returns the new value."""
    reply = await pad_command(menu_client, "step_setting", option=option, direction="up", steps=1)
    assert reply.success, f"step_setting {option} refused: {reply.error}"
    return reply.value


async def _end_all_games(stack) -> None:
    game_client, channel = await get_game_client(stack)
    try:
        await game_client.ForceEndGame(game_coordinator_pb2.ForceEndGameRequest(reason="tc005_teardown"))
    finally:
        await channel.close()


@pytest.mark.asyncio
@pytest.mark.uc("UC-015", flow="main", steps=[4, 5, 7], tc="TC-005")
@pytest.mark.uc("UC-011", flow="main", steps=[1, 2, 3, 4, 5], tc="TC-005")
@pytest.mark.uc("UC-014", flow="main", steps=[2, 3, 5], tc="TC-005")
@pytest.mark.uc("UC-010", br=["BR-015"], tc="TC-005")
@pytest.mark.uc("UC-053", flow="A2", tc="TC-005")
@pytest.mark.usefixtures("flag_service")  # binds the flag-service client to this stack
async def test_host_tunes_and_force_starts(fresh_stack):
    """The host steps sensitivity, num_teams and the game mode with the pad and force-starts a game in that mode."""
    stack = fresh_stack
    menu_client, menu_channel = await get_menu_client(stack)
    game_client, game_channel = await get_game_client(stack)
    try:
        flag_files_before = _flag_files(stack)
        assert flag_files_before, "the stack has no baseline flag files"
        await _serve_force_all_start(stack)
        serials = await start_lobby(stack, count=4, game_mode=START_MODE)
        sensitivity_baseline = _default_variant(GAME, "sensitivity")
        num_teams_baseline = _default_variant(GAME, "num_teams")
        assert sensitivity_baseline == "medium", "TC-005 precondition: sensitivity serves medium"

        # Steps 1 to 3: the sensitivity step is persisted as an admin value (UC-053 A2) and flagd serves it.
        sensitivity = await _step_up(menu_client, "sensitivity")
        assert sensitivity == _next(SENSITIVITY_ORDER, sensitivity_baseline)
        admin = _persisted_admin_value(GAME, "sensitivity")
        assert admin and admin.get("variant") == sensitivity, f"sensitivity not persisted as an admin value: {admin}"
        expected_level = variant_value(GAME, "sensitivity", sensitivity)
        with flagd_probe_client(stack, GAME) as probe:
            assert await async_poll_until(
                lambda: probe.get_integer_value("sensitivity", -1, EvaluationContext()) == expected_level
            ), f"flagd did not serve sensitivity={expected_level}"

        num_teams = await _step_up(menu_client, "num_teams")
        assert num_teams == _next(NUM_TEAMS_ORDER, num_teams_baseline)
        assert (_persisted_admin_value(GAME, "num_teams") or {}).get("variant") == num_teams

        async with (
            MenuEventCollector(menu_client) as menu_events,
            GameEventCollector(game_client) as game_events,
        ):
            # Step 4 (UC-011 A4): the mode steps, is announced and persisted.
            stack.fake_audio.clear()
            assert await _step_up(menu_client, "current_game") == EXPECTED_MODE
            selection = await menu_events.wait_for_event("selection_changed", timeout=EVENT_TIMEOUT_SECONDS)
            assert selection.data.get("game_name") == EXPECTED_MODE, dict(selection.data)
            announced = await async_poll_until(
                lambda: any(
                    call.request.get("file_path") == EXPECTED_ANNOUNCEMENT
                    for call in stack.fake_audio.calls_of("PlaySound")
                ),
                timeout=ANNOUNCE_TIMEOUT_SECONDS,
            )
            assert announced, f"no {EXPECTED_ANNOUNCEMENT!r} announcement: {stack.fake_audio.calls_of('PlaySound')}"
            assert await async_poll_until(
                lambda: (_persisted_admin_value(USER, "current_game") or {}).get("variant") == EXPECTED_MODE,
                timeout=PERSIST_TIMEOUT_SECONDS,
            ), f"current_game not persisted as an admin value: {_persisted_admin_value(USER, 'current_game')}"

            # Steps 5 and 6 (UC-014): the pad force-starts the selected mode.
            reply = await pad_command(menu_client, "force_start")
            assert reply.success, f"force_start refused: {reply.error}"
            requested = await menu_events.wait_for_event("game_requested", timeout=EVENT_TIMEOUT_SECONDS)
            assert requested.data.get("source") == "operator_pad", dict(requested.data)
            assert requested.data.get("game_name") == EXPECTED_MODE, dict(requested.data)
            await game_events.wait_for_event("game_started", timeout=START_TIMEOUT_SECONDS)

        # Validation 1: the started game runs in the selected mode with every connected controller.
        games = await list_games(game_client)
        started = [game for game in games if game.game_mode == EXPECTED_MODE]
        assert started, f"started game mode {[g.game_mode for g in games]}, expected {EXPECTED_MODE}"
        roster = {player.serial for player in started[0].players}
        assert roster == set(serials), f"force-start roster {sorted(roster)}, expected {sorted(serials)}"

        # The next game runs with the new sensitivity (UC-010 BR-015).
        gauge = await stack.otlp_sink.wait_metric_async(
            "game_sensitivity",
            {"game_id": started[0].game_id},
            service=COORDINATOR,
            timeout=EXPORT_TIMEOUT_SECONDS,
        )
        assert gauge.value == expected_level, f"game runs at sensitivity {gauge.value}, expected {expected_level}"

        # Validation 2: the settings live on the flag service; the flag files are unchanged.
        assert _flag_files(stack) == flag_files_before, "a pad write changed a baseline flag file"
    finally:
        await _end_all_games(stack)
        for channel in (menu_channel, game_channel):
            await channel.close()
