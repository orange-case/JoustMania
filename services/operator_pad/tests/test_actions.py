"""Gestures to commands (UC-015 steps 1 to 8, A1, A3, A6, A7, BR-002 to BR-012), autospec'd stubs."""

import asyncio
from unittest.mock import MagicMock, create_autospec, patch

import grpc
import pytest

from lib.setting_ladders import BACKENDS, MODE_LADDER, preview_value
from lib.types import Sound
from proto import game_coordinator_pb2, menu_pb2
from services.operator_pad import actions as actions_module
from services.operator_pad import page_keys
from services.operator_pad.actions import Actions
from services.operator_pad.cues import CuePlayer
from services.operator_pad.gestures import (
    AGENT_HOLD,
    CANCEL,
    HOLD,
    KNOB,
    KNOB_PRESS_GESTURE,
    KNOB_TURN,
    LAYER,
    LOCK,
    TAP,
    Gesture,
)
from services.operator_pad.pages import PAGES
from services.operator_pad.tests.fakes import CoordinatorStubSpec, GatedSleep, MenuStubSpec


def status(**kw) -> menu_pb2.GetMenuStatusResponse:
    base = {
        "lifecycle_live": False,
        "lifecycle_generation": 2,
        "agent_enabled": True,
        "agent_level": "standard",
        "agent_backend": "claude",
        "sensitivity": 2,
        "num_teams": 2,
        "instructions": False,
        "audio_volume_master": "normal",
        "pad_commands": ["reset_setting", "reset_setting.from", "agent_state", "step_setting.from"],
    }
    base.update(kw)
    # A resolved kill switch unless the test says otherwise (services.md §3.1, #2101).
    base.setdefault("agent_enabled_state", "on" if base["agent_enabled"] else "off")
    # An open, resolved write gate unless the test says otherwise (#2093).
    base.setdefault("agent_interventions_enabled_state", "on")
    return menu_pb2.GetMenuStatusResponse(**base)


def backend_status(backend: str) -> menu_pb2.GetMenuStatusResponse:
    """A menu that guards `set_backend` with `from` (UC-016 BR-014, D12)."""
    return status(agent_backend=backend, pad_commands=[*status().pad_commands, "set_backend.from"])


def reply(success=True, error="", value=""):
    return menu_pb2.ProcessInputResponse(success=success, error=error, value=value)


def on(page: str, key: str):
    """The row ``key`` binds to on ``page`` (UC-016 BR-011)."""
    return page_keys.binding(key, PAGES.index(page))


async def k5_hold(actions, at_press, at_release, locked=False, clean=True):
    """UC-016 A10: K5 reaches its hold time (the stop fires there), then is released."""
    first = await actions.dispatch(Gesture(AGENT_HOLD, "K5", held_ms=1000, locked=locked, clean=clean), None, at_press)
    second = await actions.dispatch(Gesture(HOLD, "K5", held_ms=1200, locked=locked, clean=clean), at_release, at_press)
    return first if first != ("none", "noop") else second


@pytest.fixture
def menu():
    stub = create_autospec(MenuStubSpec, instance=True)
    stub.ProcessInput.return_value = reply()
    return stub


@pytest.fixture
def coordinator():
    stub = create_autospec(CoordinatorStubSpec, instance=True)
    stub.ForceEndGame.return_value = game_coordinator_pb2.ForceEndGameResponse(success=True)
    return stub


@pytest.fixture
def cues():
    return create_autospec(CuePlayer, instance=True)


@pytest.fixture
def sleep():
    return GatedSleep()


@pytest.fixture
def actions(menu, coordinator, cues, sleep):
    return Actions(menu, coordinator, cues, sleep=sleep)


def sent(menu) -> list[dict]:
    return [dict(c.args[0].data) for c in menu.ProcessInput.await_args_list]


# --- K8 hold: end the game (step 7) ---------------------------------------------------------------------


@pytest.mark.uc("UC-015", flow="main", steps=[7])
async def test_k8_running_game_force_ends_through_the_coordinator(actions, menu, coordinator):
    live = status(lifecycle_live=True, lifecycle_generation=3)
    result = await actions.dispatch(Gesture(HOLD, "K8"), live, live)
    assert result == ("force_end", "ok")
    coordinator.ForceEndGame.assert_awaited_once()
    assert coordinator.ForceEndGame.await_args.args[0].reason == "operator_pad"
    menu.ProcessInput.assert_not_awaited()


@pytest.mark.uc("UC-015", flow="main", br=["BR-009"], steps=[7])
@pytest.mark.parametrize("at_press", [status(), status(lifecycle_live=True)], ids=["lobby", "live-at-press"])
async def test_k8_hold_in_the_lobby_is_refused_and_never_force_starts(actions, menu, coordinator, cues, at_press):
    """The K8 tap is the one start: a hold released in the lobby sends nothing."""
    result = await actions.dispatch(Gesture(HOLD, "K8", held_ms=1500), status(), at_press)
    assert result == ("force_end", "refused")
    menu.ProcessInput.assert_not_awaited()
    coordinator.ForceEndGame.assert_not_awaited()
    cues.refusal.assert_awaited_once()


@pytest.mark.uc("UC-015", flow="main", steps=[7])
async def test_k8_hold_is_decided_by_the_status_at_the_release_alone(actions, coordinator, cues):
    """No press/release comparison: a game that went live during the hold is ended."""
    release = status(lifecycle_live=True, lifecycle_generation=4)
    result = await actions.dispatch(Gesture(HOLD, "K8", held_ms=1500), release, status(lifecycle_generation=2))
    assert result == ("force_end", "ok")
    coordinator.ForceEndGame.assert_awaited_once()
    cues.cancel.assert_not_awaited()


# --- disconnected: never queued (A3, BR-010) ------------------------------------------------


@pytest.mark.uc("UC-015", flow="A3", br=["BR-009", "BR-010"])
@pytest.mark.parametrize(
    "gesture",
    [
        Gesture(TAP, "K8"),
        Gesture(TAP, "K6"),
        Gesture(HOLD, "K8"),
        Gesture(LAYER, "K6", steps=1),
        Gesture(LAYER, "K3", steps=1),
        Gesture(KNOB_PRESS_GESTURE, KNOB),
    ],
)
async def test_menu_unreachable_sends_nothing_and_plays_the_failure_cue(actions, menu, coordinator, cues, gesture):
    _, result = await actions.dispatch(gesture, None, None)
    assert result == "menu_unavailable"
    menu.ProcessInput.assert_not_awaited()
    coordinator.ForceEndGame.assert_not_awaited()
    cues.failure.assert_awaited_once()


@pytest.mark.uc("UC-015", flow="A3", br=["BR-010"], cond=["Post-F-1"])
async def test_a_failed_command_rpc_is_not_retried(actions, menu, cues):
    menu.ProcessInput.side_effect = grpc.aio.AioRpcError(
        grpc.StatusCode.UNAVAILABLE, grpc.aio.Metadata(), grpc.aio.Metadata(), "down"
    )
    result = await actions.dispatch(Gesture(AGENT_HOLD, "K5", held_ms=1000), status(), None)
    assert result == ("agent_switch", "menu_unavailable")
    assert menu.ProcessInput.await_count == 1
    cues.failure.assert_awaited_once()


@pytest.mark.uc("UC-015", flow="A4", br=["BR-010"])
async def test_a_volume_turn_pending_at_disconnect_is_dropped(actions, menu, sleep):
    await actions.dispatch(Gesture(KNOB_TURN, KNOB, steps=1), None)
    actions.cancel_pending()
    sleep.gate.set()
    await actions.drain()
    await asyncio.sleep(0)
    menu.ProcessInput.assert_not_awaited()


# --- error code -> cue (BR-009, BR-012) -----------------------------------------------------


@pytest.mark.uc("UC-015", flow="A3", br=["BR-009", "BR-012"])
@pytest.mark.parametrize(
    ("success", "error", "label", "cue"),
    [
        (False, "game_active", "game_active", "refusal"),
        (False, "at_limit", "at_limit", "limit"),
        (False, "stale", "stale", "cancel"),
        (False, "bad_option", "bad_option", "refusal"),
        (False, "flag_service_unavailable", "flag_service_unavailable", None),
        (True, "ok", "ok", None),
        (True, "", "ok", None),
    ],
)
async def test_error_codes_map_to_cues(actions, menu, cues, success, error, label, cue):
    menu.ProcessInput.return_value = reply(success, error)
    with patch.object(actions_module.metrics, "gestures_total") as counter:
        result = await actions.dispatch(Gesture(LAYER, "K3", steps=1), status())
    assert result == ("step_setting", label)
    counter.labels.assert_called_once_with(action="step_setting", result=label)
    for name in ("refusal", "limit", "cancel", "failure"):
        mock = getattr(cues, name)
        if name == cue:
            mock.assert_awaited_once()
        else:
            mock.assert_not_awaited()


@pytest.mark.uc("UC-015", flow="A7", br=["BR-009"])
@pytest.mark.parametrize(("steps", "up"), [(2, True), (-3, False)])
async def test_a_menu_at_limit_says_maximum_up_and_minimum_down(actions, menu, cues, steps, up):
    """#2106: the limit word follows the direction of the step the menu refused."""
    menu.ProcessInput.return_value = reply(False, "at_limit")
    await actions.dispatch(Gesture(LAYER, "K6", steps=steps), status())
    cues.limit.assert_awaited_once_with(up)


# --- coalescing (BR-005) --------------------------------------------------------------------


@pytest.mark.uc("UC-015", flow="main", br=["BR-005", "BR-006"], steps=[8])
async def test_volume_turns_coalesce_into_one_write_after_the_pause(actions, menu, sleep):
    for step in (1, 1, 1, -1):
        await actions.dispatch(Gesture(KNOB_TURN, KNOB, steps=step), None)
        await asyncio.sleep(0)
    menu.ProcessInput.assert_not_awaited()
    assert sleep.calls and set(sleep.calls) == {0.25}
    sleep.gate.set()
    await actions.drain()
    assert sent(menu) == [{"command": "step_setting", "option": "audio_volume.master", "direction": "up", "steps": "2"}]


@pytest.mark.uc("UC-015", flow="main", br=["BR-005"], steps=[8])
async def test_volume_turns_that_net_to_zero_write_nothing(actions, menu, sleep):
    await actions.dispatch(Gesture(KNOB_TURN, KNOB, steps=1), None)
    await actions.dispatch(Gesture(KNOB_TURN, KNOB, steps=-1), None)
    sleep.gate.set()
    await actions.drain()
    menu.ProcessInput.assert_not_awaited()


@pytest.mark.uc("UC-015", flow="main", br=["BR-004", "BR-005"], steps=[4, 5])
@pytest.mark.uc("UC-016", flow="A7", br=["BR-012"])
@pytest.mark.parametrize(
    ("page", "key", "steps", "expected"),
    [
        ("HOME", "K1", 2, {"option": "current_game", "direction": "up", "steps": "2"}),
        ("HOME", "K6", -3, {"option": "sensitivity", "direction": "down", "steps": "3"}),
        ("GAME", "K1", 1, {"option": "num_teams", "direction": "up", "steps": "1"}),
        ("GAME", "K6", 1, {"option": "game_instructions", "direction": "up", "steps": "1"}),
        ("START", "K1", 1, {"option": "force_all_start", "direction": "up", "steps": "1"}),
        ("AGENT", "K3", -1, {"option": "agent_level", "direction": "down", "steps": "1", "from": "standard"}),
    ],
)
async def test_edit_release_commits_one_step_setting_with_steps_n(actions, menu, page, key, steps, expected):
    await actions.dispatch(Gesture(LAYER, key, steps=steps), status(), status(), on(page, key))
    assert sent(menu) == [{"command": "step_setting", **expected}]


@pytest.mark.uc("UC-015", flow="main", br=["BR-005"], steps=[5])
async def test_layer_release_with_zero_net_turns_writes_nothing(actions, menu):
    assert await actions.dispatch(Gesture(LAYER, "K6", steps=0), status()) == ("step_setting", "noop")
    menu.ProcessInput.assert_not_awaited()


# --- lobby-only refusals (A1) ---------------------------------------------------------------


@pytest.mark.uc("UC-015", flow="A1", br=["BR-009"])
@pytest.mark.uc("UC-016", br=["BR-011"])
@pytest.mark.parametrize(
    ("gesture", "binding"),
    [
        (Gesture(TAP, "K8"), None),
        (Gesture(LAYER, "K1", steps=1), None),
        (Gesture(LAYER, "K6", steps=1), None),
        (Gesture(LAYER, "K1", steps=-1), on("GAME", "K1")),
        (Gesture(LAYER, "K6", steps=1), on("GAME", "K6")),
    ],
)
async def test_lobby_only_gestures_are_refused_by_the_pad_while_a_game_is_live(actions, menu, cues, gesture, binding):
    _, result = await actions.dispatch(gesture, status(lifecycle_live=True), None, binding)
    assert result == "refused"
    menu.ProcessInput.assert_not_awaited()
    cues.refusal.assert_awaited_once()


@pytest.mark.uc("UC-015", flow="A1", br=["BR-004"])
@pytest.mark.uc("UC-016", br=["BR-011"])
async def test_agent_level_volume_and_backend_are_not_refused_while_a_game_is_live(actions, menu):
    live = status(lifecycle_live=True, pad_commands=[*status().pad_commands, "set_backend.from"])
    await actions.dispatch(Gesture(LAYER, "K3", steps=-1), live, live, on("AGENT", "K3"))
    await actions.dispatch(Gesture(LAYER, "K1", steps=1), live, live, on("INTERV", "K1"))
    await actions.dispatch(Gesture(LAYER, "K3", steps=1), live, live, on("HOME", "K3"))
    assert [d["command"] for d in sent(menu)] == ["step_setting", "set_backend", "step_setting"]


@pytest.mark.uc("UC-015", flow="main", steps=[1, 2])
@pytest.mark.uc("UC-016", br=["BR-011"])
async def test_k8_tap_in_the_lobby_force_starts_without_a_generation(actions, menu):
    assert await actions.dispatch(Gesture(TAP, "K8"), status()) == ("start_game", "ok")
    assert sent(menu) == [{"command": "force_start"}]


@pytest.mark.uc("UC-015", flow="A1", br=["BR-012"])
async def test_k8_tap_refused_by_the_menu_plays_the_refusal_cue(actions, menu, cues):
    """#2176: a game started after the status read; the menu's A8 guard answers game_active."""
    menu.ProcessInput.return_value = reply(False, "game_active")
    assert await actions.dispatch(Gesture(TAP, "K8"), status()) == ("start_game", "game_active")
    cues.refusal.assert_awaited_once()
    cues.failure.assert_not_awaited()


# --- backend (BR-004, A7) -------------------------------------------------------------------


@pytest.mark.uc("UC-015", flow="A7", br=["BR-004", "BR-005"])
@pytest.mark.uc("UC-016", br=["BR-007"])
@pytest.mark.parametrize(
    ("current", "steps", "target"),
    [
        ("claude", 1, "openai"),
        ("jev", 1, "rules"),  # past the last to the first ...
        ("rules", -1, "jev"),  # ... and back past the first to the last
        ("rules", 5, "claude"),  # n = len + 1: one past
        ("openai", -1, "claude"),
        ("claude", -9, "rules"),
    ],
)
async def test_backend_layer_steps_wrap(actions, menu, cues, current, steps, target):
    st = backend_status(current)
    await actions.dispatch(Gesture(LAYER, "K1", steps=steps, confirmed=True), st, st, on("INTERV", "K1"))
    assert sent(menu) == [{"command": "set_backend", "backend": target, "from": current}]
    cues.limit.assert_not_awaited()


@pytest.mark.uc("UC-015", flow="A7", br=["BR-004", "BR-005", "BR-009"])
@pytest.mark.uc("UC-016", br=["BR-007"])
@pytest.mark.parametrize("link_up", [False, True])
@pytest.mark.parametrize(("current", "steps"), [("jev", 4), ("rules", -4), ("claude", 8), ("openai", -12)])
async def test_a_full_turn_of_the_backend_wheel_sends_nothing_and_plays_nothing(
    menu, coordinator, cues, sleep, link_up, current, steps
):
    """n % len == 0 is a no-op: no `set_backend`, no cue, never `at_limit`, with the display link up or down."""
    actions = Actions(menu, coordinator, cues, sleep=sleep, display=MagicMock(link_up=link_up))
    st = backend_status(current)
    result = await actions.dispatch(Gesture(LAYER, "K1", steps=steps), st, st, on("INTERV", "K1"))
    assert result == ("set_backend", "noop")
    menu.ProcessInput.assert_not_awaited()
    cues.limit.assert_not_awaited()
    cues.refusal.assert_not_awaited()
    cues.failure.assert_not_awaited()


@pytest.mark.uc("UC-015", flow="A7", br=["BR-005"])
@pytest.mark.uc("UC-016", br=["BR-007"])
@pytest.mark.parametrize("turns", [1, -1, 2])
async def test_a_full_turn_of_the_mode_wheel_sends_nothing_and_plays_nothing(actions, menu, cues, turns):
    gesture = Gesture(LAYER, "K1", steps=turns * len(MODE_LADDER))
    assert await actions.dispatch(gesture, status(selection=MODE_LADDER[1])) == ("step_setting", "noop")
    menu.ProcessInput.assert_not_awaited()
    cues.limit.assert_not_awaited()


@pytest.mark.uc("UC-015", flow="A7", br=["BR-004", "BR-005"])
@pytest.mark.uc("UC-016", br=["BR-007"])
@pytest.mark.parametrize("current", BACKENDS)
async def test_backend_preview_equals_commit(menu, coordinator, cues, sleep, current):
    """#2106: the backend a K7 release commits is the shared ladder's preview, for every start and every
    step up to 3 x len; a preview back at the start is the no-op, nothing sent."""
    reach = 3 * len(BACKENDS)
    for steps in [n for n in range(-reach, reach + 1) if n != 0]:
        actions = Actions(menu, coordinator, cues, sleep=sleep)
        menu.ProcessInput.reset_mock()
        st = backend_status(current)
        preview = preview_value("decision.backend", st, steps)
        result = await actions.dispatch(Gesture(LAYER, "K1", steps=steps, confirmed=True), st, st, on("INTERV", "K1"))
        assert not preview.at_limit, (current, steps)
        if preview.value == current:
            assert result == ("set_backend", "noop"), (current, steps)
            assert sent(menu) == []
        else:
            expected = {"command": "set_backend", "backend": preview.value, "from": current}
            assert sent(menu) == [expected], (current, steps)
    cues.limit.assert_not_awaited()


# --- K5 hold: always stop (step 6, A6, A9, BR-002, BR-003) -----------------------------------


STOP = {"command": "agent_switch", "direction": "stop"}
UNKNOWN_AGENTS = [
    None,  # GetMenuStatus failed or timed out
    status(agent_enabled=False, agent_enabled_state="unknown"),
    status(agent_enabled=False, agent_enabled_state=""),  # a menu older than #2101
    status(agent_interventions_enabled_state="unknown"),
    status(agent_interventions_enabled_state=""),  # a menu older than #2093
    status(agent_level="probe"),
]


@pytest.mark.uc("UC-015", flow="main", br=["BR-002", "BR-003"], steps=[6])
@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
@pytest.mark.parametrize("locked", [False, True], ids=["unlocked", "locked"])
@pytest.mark.parametrize("clean", [True, False], ids=["clean", "chord"])
@pytest.mark.parametrize(
    "agent",
    [
        status(),
        status(agent_level="none"),  # ON NONE: a tuning level, not a stop
        status(agent_interventions_enabled_state="off"),
        status(agent_enabled=False),
        status(agent_enabled=False, agent_level=""),
        *UNKNOWN_AGENTS,
    ],
    ids=[
        "on",
        "on-none",
        "watch",
        "off",
        "off-no-level",
        "unread",
        "enabled-unknown",
        "enabled-missing",
        "gate-unknown",
        "gate-missing",
        "level-off-the-ladder",
    ],
)
async def test_k5_hold_sends_the_stop_at_the_hold_time_whatever_the_state(actions, menu, cues, agent, clean, locked):
    """Never a resume and never cancelled: the release after the stop sends nothing more."""
    first = await actions.dispatch(Gesture(AGENT_HOLD, "K5", held_ms=1000, locked=locked, clean=clean), None, agent)
    assert first == ("agent_switch", "ok")
    second = await actions.dispatch(Gesture(HOLD, "K5", held_ms=1200, locked=locked, clean=clean), agent, agent)
    assert second == ("none", "noop")
    assert sent(menu) == [STOP]
    cues.cancel.assert_not_awaited()


@pytest.mark.uc("UC-015", flow="A9", br=["BR-002"])
@pytest.mark.uc("UC-016", flow="A10", br=["BR-011"])
@pytest.mark.parametrize(
    ("at_press", "at_release"),
    [(status(agent_enabled=False), status()), (status(), status(agent_enabled=False)), (None, None)],
    ids=["resumed-during-the-hold", "stopped-during-the-hold", "unread"],
)
async def test_k5_hold_release_without_a_prior_hold_time_event_sends_the_stop(actions, menu, at_press, at_release):
    """A lost hold-time event: the release still stops, never resumes."""
    result = await actions.dispatch(Gesture(HOLD, "K5", held_ms=1200), at_release, at_press)
    assert result == ("agent_switch", "ok")
    assert sent(menu) == [STOP]


@pytest.mark.uc("UC-015", br=["BR-002"])
async def test_the_stop_uses_the_command_deadline(actions, menu):
    await k5_hold(actions, status(agent_enabled=False), status(agent_enabled=False))
    assert menu.ProcessInput.await_args.kwargs["timeout"] == actions_module.COMMAND_TIMEOUT_SECONDS


@pytest.mark.uc("UC-015", flow="A6", br=["BR-009"])
async def test_lock_gesture_sets_the_scroll_lock_led_and_plays_the_direction(menu, coordinator, cues):
    leds: list[bool] = []
    actions = Actions(menu, coordinator, cues, set_lock_led=leds.append)
    await actions.dispatch(Gesture(LOCK, "K4", locked=True), None)
    await actions.dispatch(Gesture(LOCK, "K4", locked=False), None)
    assert leds == [True, False]
    assert [c.args[0] for c in cues.lock.await_args_list] == [True, False]
    menu.ProcessInput.assert_not_awaited()


@pytest.mark.uc("UC-015", flow="main", br=["BR-007", "BR-009"])
async def test_cancel_gesture_plays_the_cancel_cue_only(actions, menu, cues):
    assert await actions.dispatch(Gesture(CANCEL, "K5"), status()) == ("cancel", "cancelled")
    cues.cancel.assert_awaited_once()
    menu.ProcessInput.assert_not_awaited()


# --- mute (BR-006) --------------------------------------------------------------------------


@pytest.mark.uc("UC-015", flow="main", br=["BR-006"], steps=[8])
async def test_mute_remembers_the_variant_and_unmute_restores_it(actions, menu):
    assert await actions.dispatch(Gesture(KNOB_PRESS_GESTURE, KNOB), status(audio_volume_master="loud")) == (
        "mute",
        "ok",
    )
    await actions.dispatch(Gesture(KNOB_PRESS_GESTURE, KNOB), status(audio_volume_master="mute"))
    assert sent(menu) == [
        {"command": "step_setting", "option": "audio_volume.master", "direction": "down", "steps": "3"},
        {"command": "step_setting", "option": "audio_volume.master", "direction": "up", "steps": "3"},
    ]


@pytest.mark.uc("UC-015", flow="main", br=["BR-006"], steps=[8])
async def test_unmute_without_a_remembered_variant_restores_the_default(actions, menu):
    result = await actions.dispatch(Gesture(KNOB_PRESS_GESTURE, KNOB), status(audio_volume_master="mute"))
    assert result == ("unmute", "ok")
    assert sent(menu) == [{"command": "step_setting", "option": "audio_volume.master", "direction": "up", "steps": "4"}]


# --- tap announcements (BR-011; UC-016 BR-011, BR-016) ----------------------------------------


@pytest.mark.uc("UC-015", flow="main", br=["BR-011"])
@pytest.mark.uc("UC-016", flow="A7", br=["BR-011", "BR-016"])
async def test_taps_announce_the_label_then_the_value_and_send_nothing(actions, menu, cues):
    """#2106: each tap says what it is before the value; the backend by name."""
    for gesture, binding in [
        (Gesture(TAP, "K6"), None),
        (Gesture(TAP, "K5"), None),
        (Gesture(TAP, "K1"), on("GAME", "K1")),
        (Gesture(TAP, "K1"), on("INTERV", "K1")),
    ]:
        assert await actions.dispatch(gesture, status(num_teams=3), None, binding) == ("announce", "ok")
    assert [c.args for c in cues.announce.await_args_list] == [
        (Sound.MENU_VOX_LABEL_SENSITIVITY, Sound.MENU_VOX_SENSITIVITY_MEDIUM),
        (Sound.MENU_VOX_LABEL_AGENT, Sound.MENU_VOX_ADMINOP_2),
        (Sound.MENU_VOX_LABEL_TEAMS, Sound.MENU_VOX_ADMINOP_3),
        (Sound.MENU_VOX_LABEL_BACKEND, Sound.MENU_VOX_BACKEND_CLAUDE),
    ]
    cues.voice.assert_not_awaited()
    menu.ProcessInput.assert_not_awaited()


@pytest.mark.uc("UC-016", br=["BR-016"])
async def test_volume_row_tap_without_a_screen_says_volume_alone(actions, cues):
    """D6: the catalog has no voice for a volume value."""
    await actions.dispatch(Gesture(TAP, "K3"), status())
    cues.announce.assert_awaited_once_with(Sound.MENU_VOX_LABEL_VOLUME, None)


@pytest.mark.uc("UC-015", flow="main", br=["BR-011"])
@pytest.mark.parametrize(
    ("backend", "voice"),
    [
        ("rules", Sound.MENU_VOX_BACKEND_RULES),
        ("claude", Sound.MENU_VOX_BACKEND_CLAUDE),
        ("openai", Sound.MENU_VOX_BACKEND_OPENAI),
        ("jev", Sound.MENU_VOX_BACKEND_JEV),
    ],
)
async def test_backend_row_tap_says_the_backend_by_name(actions, cues, backend, voice):
    await actions.dispatch(Gesture(TAP, "K1"), status(agent_backend=backend), None, on("INTERV", "K1"))
    cues.announce.assert_awaited_once_with(Sound.MENU_VOX_LABEL_BACKEND, voice)


@pytest.mark.uc("UC-015", flow="main", br=["BR-011", "BR-009"])
async def test_backend_row_tap_with_an_unknown_backend_refuses(actions, cues):
    await actions.dispatch(Gesture(TAP, "K1"), status(agent_backend="mystery"), None, on("INTERV", "K1"))
    cues.refusal.assert_awaited_once()
    cues.announce.assert_not_awaited()


@pytest.mark.uc("UC-015", flow="main", br=["BR-011", "BR-013"])
@pytest.mark.parametrize("state", ["unknown", ""], ids=["unknown", "missing"])
async def test_k5_tap_with_an_unresolved_kill_switch_refuses_never_announces_stopped(actions, menu, cues, state):
    result = await actions.dispatch(Gesture(TAP, "K5"), status(agent_enabled=False, agent_enabled_state=state))
    assert result == ("announce", "ok")
    cues.refusal.assert_awaited_once()
    cues.announce.assert_not_awaited()
    menu.ProcessInput.assert_not_awaited()


@pytest.mark.uc("UC-015", flow="main", br=["BR-011", "BR-013"])
async def test_k5_tap_with_a_resolved_off_and_an_empty_level_announces_stopped(actions, cues):
    await actions.dispatch(Gesture(TAP, "K5"), status(agent_enabled=False, agent_level=""))
    cues.announce.assert_awaited_once_with(Sound.MENU_VOX_LABEL_AGENT, actions_module.cues.AGENT_STOPPED_VOICE)


@pytest.mark.uc("UC-016", br=["BR-011", "BR-016"])
async def test_k5_tap_with_the_write_gate_off_announces_watching(actions, cues):
    """A watching agent (only the gate closed) is not stopped: the tap says "Watching"."""
    await actions.dispatch(Gesture(TAP, "K5"), status(agent_interventions_enabled_state="off"))
    cues.announce.assert_awaited_once_with(Sound.MENU_VOX_LABEL_AGENT, Sound.MENU_VOX_AGENT_WATCHING)


@pytest.mark.uc("UC-015", flow="main", br=["BR-011", "BR-013"])
@pytest.mark.parametrize("state", ["unknown", ""], ids=["unknown", "missing"])
async def test_k5_tap_with_an_unresolved_write_gate_refuses(actions, menu, cues, state):
    """#2093: an unresolved gate is neither announced as running nor as stopped."""
    result = await actions.dispatch(Gesture(TAP, "K5"), status(agent_interventions_enabled_state=state))
    assert result == ("announce", "ok")
    cues.refusal.assert_awaited_once()
    cues.announce.assert_not_awaited()
    menu.ProcessInput.assert_not_awaited()
