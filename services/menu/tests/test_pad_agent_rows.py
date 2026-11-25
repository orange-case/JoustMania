"""The pad's agent rows on the menu side: goal (`objectives`), rate, group, retro backend, effort, brief (#2755).

UC-016 BR-006, BR-012 to BR-014, A3, A8; services.md §3.1 iteration 4. A real MenuServicer whose writers are
real ``FlagServiceWriter``s over a fake flag service that keeps the layers of each `agent` row (stack overlay
with its writer, persisted admin value) and the compare-and-delete ``if_writer``. The values come from the
real flag file (`services/flagd/agent.json`).
"""

import json
import urllib.error
import urllib.parse
from pathlib import Path
from unittest.mock import AsyncMock, patch

import pytest
import pytest_asyncio

from lib import setting_ladders
from lib.flag_service_writer import FlagServiceWriter
from lib.types import Sound
from proto import menu_pb2
from services.menu import settings_ops
from services.menu.tests.servicer_harness import (
    cancel_background_tasks,
    default_returning_flag_client,
    make_real_servicer,
)
from services.menu.tests.test_menu_status import in_process_client
from services.menu.tests.test_pad_commands import live_lifecycle, pad

FLAGD_DIR = Path(__file__).resolve().parents[3] / "services" / "flagd"
AGENT_FLAGS = json.loads((FLAGD_DIR / "agent.json").read_text())["flags"]
ROWS = list(setting_ladders.AGENT_ROW_LADDERS)
KEYS = [*ROWS, "decision.backend"]
PERSISTED = {"inference.backend", "decision.backend"}
GOAL, RATE, GROUP, RETRO, EFFORT, BRIEF = ROWS
RAISE_ROWS = sorted(setting_ladders.RAISE_LADDER_OPTIONS)
IN_MEMORY_ROWS = [GOAL, RATE, GROUP, EFFORT, BRIEF]
UNKILLABLE_TEXT = AGENT_FLAGS[BRIEF]["variants"]["unkillable"]
# One rung below and one rung above the baseline of each row, in canonical format (the from / expect / value).
LOW_HIGH = {GOAL: ("endurance_focused", "accelerate_focused"), RATE: ("1", "8"), GROUP: ("off", "on")}
LOW_HIGH |= {RETRO: ("stub", "claude"), EFFORT: ("low", "high"), BRIEF: ("off", "unkillable")}
# What a reset puts back once the menu's own value is gone (services.md §3.1).
BASELINE = {GOAL: "balanced_focused", RATE: "2", GROUP: "off", RETRO: "stub", EFFORT: "medium", BRIEF: "off"}


class AgentRowService:
    """The flag service's agent set, per row: baseline, stack overlay (with writer), admin value."""

    def __init__(self):
        self.stack: dict[str, dict] = {}
        self.admin: dict[str, dict] = {}
        self.requests: list[tuple[str, str, dict | None]] = []
        self.failing: set[str] = set()
        self.down = False
        # DELETE ?if_writer answers 412 although the read showed the menu's overlay (another writer in between).
        self.race_on_delete: set[str] = set()

    def variants(self, key: str) -> dict:
        return AGENT_FLAGS[key]["variants"]

    def view(self, key: str) -> dict:
        variants = self.variants(key)
        view = {
            "flag": key,
            "variants": list(variants),
            "variant_values": variants,
            "baseline_default": AGENT_FLAGS[key]["defaultVariant"],
            "policy": {"flag_set": "agent", "flag": key, "kind": "overlay", "persist": key in PERSISTED},
        }
        for name, layers in (("stack_overlay", self.stack), ("admin_value", self.admin)):
            if key in layers:
                layer = dict(layers[key])
                layer.setdefault("value", variants.get(layer["variant"]))
                view[name] = layer
        layer = view.get("stack_overlay") or view.get("admin_value")
        view["effective_default"] = layer["variant"] if layer else view["baseline_default"]
        return view

    def served(self, key: str) -> str:
        return self.view(key)["effective_default"]

    def set_stack(self, key: str, variant: str, writer: str = "menu", value=None) -> None:
        self.stack[key] = {"variant": variant, "writer": writer, **({"value": value} if value is not None else {})}

    def writes(self) -> list[tuple[str, str, dict | None]]:
        return [(method, url, body) for method, url, body in self.requests if method != "GET"]

    def __call__(self, method, url, body):
        self.requests.append((method, url, body))
        if self.down:
            raise urllib.error.URLError("connection refused")
        path, _, query = url.partition("?")
        if method == "GET" and path.endswith("/agent"):
            return 200, {"flag_set": "agent", "flags": [self.view(key) for key in KEYS]}
        flag_set, key = path.rsplit("/", 2)[-2:]
        key = urllib.parse.unquote(key)
        if flag_set != "agent" or key not in KEYS:
            return 404, {"error": "unknown_flag"}  # the other sets: not these tests' concern
        if method == "GET":
            return 200, self.view(key)
        params = dict(urllib.parse.parse_qsl(query))
        writer = (body or {}).get("writer") if method == "PUT" else params.get("writer", "")
        if key in self.failing:
            return 503, {"error": "no_sync_stream"}
        if method == "PUT":
            if body.get("persist"):
                self.admin[key] = {"variant": body["variant"], "writer": writer}
                self.stack.pop(key, None)  # a persisted write replaces an in-memory overlay
            else:
                self.set_stack(key, body["variant"], writer)
            return 200, {"variant": body["variant"], "persisted": bool(body.get("persist"))}
        if params.get("persist") == "true":
            self.admin.pop(key, None)
            return 204, {}
        if_writer = params.get("if_writer")
        stack = self.stack.get(key)
        if key in self.race_on_delete or (stack is not None and if_writer and stack.get("writer") != if_writer):
            return 412, {"error": "writer_mismatch"}
        self.stack.pop(key, None)
        return 204, {}


@pytest.fixture
def flags():
    return AgentRowService()


def agent_client(flags: AgentRowService, **overrides):
    """The in-process `agent` client: each row as the fake serves it (variant, value)."""
    values = {}
    for key in KEYS:
        view = flags.view(key)
        layer = view.get("stack_overlay") or view.get("admin_value")
        values[key] = (
            view["effective_default"],
            layer["value"] if layer else view["variant_values"][view["effective_default"]],
        )
    values |= {
        "enabled": ("on", True),
        "interventions_enabled": ("on", True),
        "interventions_allowed": ("standard", "a,b"),
    }
    values |= overrides
    return in_process_client(values)


@pytest_asyncio.fixture
async def servicer(flags):
    def writer_factory(flag_set, *args, **kwargs):
        return FlagServiceWriter(flag_set, "http://flag-service:8070", *args, transport=flags, **kwargs)

    menu = make_real_servicer(writer_factory=writer_factory)
    menu.state_manager.audio.play_sound = AsyncMock()
    menu.agent_settings_client = agent_client(flags)
    menu.game_settings_client = in_process_client({})
    menu.user_prefs_client = in_process_client({})
    with (
        patch.object(settings_ops, "FAILURE_CUE_GAP_SECONDS", 0),
        patch("lib.feature_flags.get_flag_client", side_effect=default_returning_flag_client),
    ):
        yield menu
    await cancel_background_tasks(menu)


async def status(menu, flags) -> menu_pb2.GetMenuStatusResponse:
    menu.agent_settings_client = agent_client(flags)
    menu.settings_ops._operator_read_at = float("inf")  # no background read inside the call
    return await menu.GetMenuStatus(menu_pb2.GetMenuStatusRequest(), None)


def failure_cues(menu) -> int:
    return [c.args[0] for c in menu.state_manager.audio.play_sound.await_args_list].count(Sound.SFX_BEEP_LOUD)


def canonical(flags: AgentRowService, key: str) -> str:
    view = flags.view(key)
    return settings_ops.row_in_force(key, view)


async def step(menu, option, direction="up", steps=1, **data):
    return await pad(menu, "step_setting", option=option, direction=direction, steps=steps, **data)


# --- The allowlists (explicit, never derived) ---------------------------------------------------------


@pytest.mark.uc("UC-016", br=["BR-011", "BR-012", "BR-013", "BR-014"])
def test_the_agent_row_tables_are_explicit():
    assert set(settings_ops.STEP_OPTIONS) == {
        "current_game",
        "sensitivity",
        "num_teams",
        "force_all_start",
        "game_instructions",
        "audio_volume.master",
        "agent_level",
        "shield.activation",
        *ROWS,
    }
    assert {
        "sensitivity",
        "num_teams",
        "shield.activation",
        "game_instructions",
        "force_all_start",
        "agent_level",
        "decision.backend",
        *ROWS,
    } == settings_ops.RESETTABLE_OPTIONS
    # D5: the goal is lobby-only, every other agent row resets while a game is live.
    assert {
        "sensitivity",
        "num_teams",
        "shield.activation",
        "game_instructions",
        "force_all_start",
        "objectives",
    } == settings_ops.RESET_LOBBY_ONLY_OPTIONS
    assert settings_ops.LOBBY_ONLY_OPTIONS & set(ROWS) == {GOAL}
    # BR-014: `from` is required exactly where a change can raise what the agent may do or spend.
    assert {RATE, GROUP, RETRO, EFFORT} == settings_ops.FROM_REQUIRED_OPTIONS
    assert {GOAL, RATE, GROUP, EFFORT, BRIEF} == settings_ops.AGENT_MEMORY_ROWS
    assert settings_ops.RESET_DEFAULT_OPTIONS[RETRO] == ("agent_settings_writer", RETRO)


# --- Status fields 22 to 27 ---------------------------------------------------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-006"])
async def test_status_reports_the_agent_rows_in_their_canonical_format(servicer, flags):
    flags.set_stack(GOAL, "chaos_focused", "flagsmith")
    flags.set_stack(RATE, "lively", "talk-stage")
    flags.set_stack(GROUP, "on", "group-ab")
    flags.admin[RETRO] = {"variant": "claude", "writer": "menu"}
    flags.set_stack(EFFORT, "xhigh", "operator")
    flags.set_stack(BRIEF, "protect_weakest", "menu")
    await servicer.settings_ops.refresh_operator_settings()

    reply = await status(servicer, flags)

    rows = [getattr(reply, field) for field in setting_ladders.AGENT_ROW_STATUS_FIELDS.values()]
    assert rows == ["chaos_focused", "8", "on", "claude", "xhigh", "protect_weakest"]


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-006"])
@pytest.mark.parametrize(("value", "expected"), [(8, "8"), (8.0, "8"), (50, "50"), (2.5, ""), (True, "")])
async def test_status_rate_is_an_integer_decimal(servicer, flags, value, expected):
    servicer.agent_settings_client = agent_client(flags, **{RATE: ("o1", value)})
    servicer.settings_ops._operator_read_at = float("inf")
    reply = await servicer.GetMenuStatus(menu_pb2.GetMenuStatusRequest(), None)
    assert reply.agent_max_interventions_per_minute == expected


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-006", "BR-012"])
@pytest.mark.parametrize(
    ("text", "expected"),
    [
        ("", "off"),
        (UNKILLABLE_TEXT, "unkillable"),
        (AGENT_FLAGS[BRIEF]["variants"]["protect_weakest"], "protect_weakest"),
        (UNKILLABLE_TEXT + " ", "custom"),  # one byte off a preset: a free text
        (UNKILLABLE_TEXT.lower(), "custom"),
        ("Make everyone unkillable.", "custom"),
    ],
)
async def test_status_brief_is_the_preset_whose_text_matches_exactly_never_the_text(servicer, flags, text, expected):
    flags.set_stack(BRIEF, "o7", "flags.sh", value=text)
    await servicer.settings_ops.refresh_operator_settings()

    reply = await status(servicer, flags)

    assert reply.agent_operator_brief_variant == expected
    assert text not in str(reply) or text == ""


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-006"])
async def test_status_brief_is_unknown_until_read_and_when_the_read_fails(servicer, flags):
    assert (await status(servicer, flags)).agent_operator_brief_variant == ""
    await servicer.settings_ops.refresh_operator_settings()
    assert (await status(servicer, flags)).agent_operator_brief_variant == "off"

    flags.down = True
    await servicer.settings_ops.refresh_operator_settings()
    reply = await status(servicer, flags)

    assert reply.agent_operator_brief_variant == ""
    assert not set(reply.setting_defaults) & set(ROWS)  # unknown, never an empty value


# --- setting_defaults and pad_commands ------------------------------------------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-013"])
async def test_setting_defaults_report_each_rows_reset_target(servicer, flags):
    flags.set_stack(GOAL, "chaos_focused", "menu")  # the menu's: the reset clears it
    flags.set_stack(RATE, "aggressive", "talk-stage")  # a stage script's: it stays (D14)
    flags.set_stack(BRIEF, "o3", "menu", value="free text")  # never the menu's to clear (D13)
    flags.admin[RETRO] = {"variant": "claude", "writer": "menu"}  # persisted: cleared like decision.backend
    flags.set_stack(EFFORT, "max", "menu")

    await servicer.settings_ops.refresh_operator_settings()

    defaults = servicer.settings_ops.setting_defaults
    assert {key: defaults[key] for key in ROWS} == {
        GOAL: "balanced_focused",
        RATE: "4",
        GROUP: "off",
        RETRO: "stub",
        EFFORT: "medium",
        BRIEF: "custom",
    }


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-011", "BR-012"])
@pytest.mark.parametrize("down", [False, True])
async def test_pad_commands_list_every_rows_step_and_reset(servicer, flags, down):
    """Never gated on the background read: `goal.profile` is no option (Flagsmith-only), `objectives` is."""
    flags.down = down
    await servicer.settings_ops.refresh_operator_settings()

    commands = list((await status(servicer, flags)).pad_commands)

    assert "set_backend.from" in commands
    for option in ROWS:
        assert f"step_setting:{option}" in commands, option
        assert f"reset_setting:{option}" in commands, option
    assert not [command for command in commands if "goal.profile" in command]


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-012"])
@pytest.mark.parametrize("command", ["step_setting", "reset_setting"])
async def test_goal_profile_is_no_pad_option(servicer, flags, command):
    reply = await pad(servicer, command, option="goal.profile", direction="up", steps=1, expect="quick_game")
    assert (reply.success, reply.error) == (False, "bad_option")
    assert flags.requests == []


# --- step_setting -----------------------------------------------------------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-012"])
@pytest.mark.parametrize("option", ROWS)
async def test_step_writes_the_next_rung_through_the_rows_writer(servicer, flags, option):
    start = canonical(flags, option)
    ladder = setting_ladders.AGENT_ROW_LADDERS[option]
    expected = ladder[ladder.index(start) + 1]

    reply = await step(servicer, option, **{"from": start})

    assert (reply.success, reply.error, reply.value) == (True, "", expected)
    assert canonical(flags, option) == expected
    [(method, url, body)] = flags.writes()
    variant = setting_ladders.RATE_VARIANTS[expected] if option == RATE else expected
    if option in PERSISTED:
        assert body == {"variant": variant, "writer": "menu", "persist": True}
    else:
        assert body == {"variant": variant, "writer": "menu"}  # in memory: no persist field
    assert method == "PUT" and url.endswith(f"/agent/{option}")
    assert servicer.state_manager.audio.play_voice.await_count == 0  # D7: no voice lines


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-012", "BR-007"])
async def test_goal_wraps_and_the_others_stop_at_their_ends(servicer, flags):
    flags.set_stack(GOAL, "mixed", "menu")
    reply = await step(servicer, GOAL)
    assert (reply.success, reply.value) == (True, "endurance_focused")

    flags.set_stack(RATE, "frenzy", "menu")
    reply = await step(servicer, RATE, **{"from": "20"})
    assert (reply.success, reply.error) == (False, "at_limit")
    assert [body["variant"] for _m, _u, body in flags.writes()] == ["endurance_focused"]


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-012"])
async def test_a_whole_turn_of_the_goal_is_a_no_op(servicer, flags):
    reply = await step(servicer, GOAL, steps=len(setting_ladders.GOAL_LADDER))
    assert (reply.success, reply.error, reply.value) == (True, "ok", "balanced_focused")
    assert flags.writes() == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-012", "BR-014"])
@pytest.mark.parametrize(
    ("option", "layer"),
    [
        (RATE, {"variant": "o1", "writer": "fixture", "value": 50}),  # D11: a fixture's rate of 50
        (BRIEF, {"variant": "o2", "writer": "flags.sh", "value": "Free text."}),  # D4: a free brief
        (BRIEF, {"variant": "o3", "writer": "flags.sh", "value": UNKILLABLE_TEXT + "\n"}),
    ],
)
@pytest.mark.parametrize("direction", ["up", "down"])
async def test_a_value_off_the_ladder_is_refused_never_moved_to_a_rung(servicer, flags, option, layer, direction):
    flags.stack[option] = layer
    data = {"from": canonical(flags, option)}

    reply = await step(servicer, option, direction, **data)

    assert (reply.success, reply.error, reply.value) == (False, "bad_option", "")
    assert flags.writes() == []
    assert flags.stack[option] == layer  # a free brief is never overwritten (D4)


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A4", br=["BR-011"])
@pytest.mark.uc("UC-015", flow="A1")
async def test_the_goal_is_lobby_only_and_the_other_rows_are_live(servicer, flags):
    live_lifecycle(servicer)

    goal = await step(servicer, GOAL)
    goal_reset = await pad(servicer, "reset_setting", option=GOAL, expect="balanced_focused")
    others = [await step(servicer, option, **{"from": canonical(flags, option)}) for option in ROWS[1:]]

    assert (goal.error, goal_reset.error) == ("game_active", "game_active")
    assert [reply.success for reply in others] == [True] * 5


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-012"])
@pytest.mark.parametrize("option", ROWS)
async def test_a_failed_write_is_flag_service_unavailable_with_the_failure_cue(servicer, flags, option):
    flags.failing.add(option)
    reply = await step(servicer, option, **{"from": canonical(flags, option)})
    assert (reply.success, reply.error) == (False, "flag_service_unavailable")
    assert failure_cues(servicer) == 2


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-012"])
@pytest.mark.parametrize("option", ROWS)
async def test_an_unreadable_row_writes_nothing(servicer, flags, option):
    flags.down = True
    reply = await step(servicer, option, **{"from": LOW_HIGH[option][0]})
    assert (reply.success, reply.error) == (False, "flag_service_unavailable")
    assert flags.writes() == []


# --- `from` (UC-016 BR-014) ---------------------------------------------------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
@pytest.mark.parametrize("option", RAISE_ROWS)
@pytest.mark.parametrize("direction", ["up", "down"])
async def test_a_raise_row_step_without_from_is_refused(servicer, flags, option, direction):
    """The pad judged the raise from the value it showed: without it the menu cannot guard it."""
    reply = await step(servicer, option, direction)
    assert (reply.success, reply.error) == (False, "bad_option")
    assert flags.requests == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-014"])
@pytest.mark.parametrize("option", [GOAL, BRIEF])
async def test_from_is_optional_for_the_goal_and_the_brief(servicer, flags, option):
    reply = await step(servicer, option)
    assert reply.success
    assert len(flags.writes()) == 1


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-014"])
@pytest.mark.parametrize("option", ROWS)
async def test_a_step_from_another_value_than_in_force_is_stale(servicer, flags, option):
    low, high = LOW_HIGH[option]
    if option in PERSISTED:
        flags.admin[option] = {"variant": high, "writer": "menu"}
    else:
        flags.set_stack(option, setting_ladders.RATE_VARIANTS.get(high, high), "talk-stage")

    reply = await step(servicer, option, "down", **{"from": low})

    assert (reply.success, reply.error, reply.value) == (False, "stale", "")
    assert flags.writes() == []
    assert failure_cues(servicer) == 0


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-014"])
@pytest.mark.parametrize(
    ("given", "ok"), [("2", True), ("default", True), ("lively", False), ("2.0", None), ("x", None)]
)
async def test_from_is_compared_by_value(servicer, flags, given, ok):
    """A variant name is mapped to its value first; a value not in the canonical format is malformed."""
    reply = await step(servicer, RATE, **{"from": given})
    expected = {True: (True, ""), False: (False, "stale"), None: (False, "bad_option")}[ok]
    assert (reply.success, reply.error) == expected
    assert len(flags.writes()) == (1 if ok else 0)


# --- reset_setting -------------------------------------------------------------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
@pytest.mark.parametrize("option", IN_MEMORY_ROWS)
async def test_reset_clears_only_the_menus_own_overlay(servicer, flags, option):
    _low, high = LOW_HIGH[option]
    flags.set_stack(option, setting_ladders.RATE_VARIANTS.get(high, high), "menu")
    baseline = BASELINE[option]

    reply = await pad(servicer, "reset_setting", option=option, expect=baseline, **{"from": high})

    assert (reply.success, reply.error, reply.value) == (True, "", baseline)
    [(method, url, body)] = flags.writes()
    assert (method, body) == ("DELETE", None)
    assert url.endswith(f"/agent/{option}?writer=menu&if_writer=menu")  # never persist (D14)
    assert option not in flags.stack
    assert servicer.state_manager.audio.play_voice.await_count == 0


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
@pytest.mark.parametrize("option", IN_MEMORY_ROWS)
async def test_reset_leaves_another_writers_value_alone(servicer, flags, option):
    """D14: `talk-stage.sh`'s rate or goal, `agent-group-ab.sh`'s group shield or Flagsmith's value stay: no-op `ok`."""
    _low, high = LOW_HIGH[option]
    flags.set_stack(option, setting_ladders.RATE_VARIANTS.get(high, high), "talk-stage")

    reply = await pad(servicer, "reset_setting", option=option, expect=high, **{"from": high})

    assert (reply.success, reply.error, reply.value) == (True, "ok", high)
    assert flags.writes() == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
async def test_reset_of_a_free_brief_is_always_a_no_op(servicer, flags):
    """D13: the menu writes presets only, so a free text is never its own, whatever writer label it carries."""
    flags.set_stack(BRIEF, "o5", "menu", value="Typed in Flagsmith.")

    reply = await pad(servicer, "reset_setting", option=BRIEF, expect="custom")

    assert (reply.success, reply.error, reply.value) == (True, "ok", "custom")
    assert flags.writes() == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
@pytest.mark.parametrize("option", IN_MEMORY_ROWS)
async def test_reset_without_an_overlay_is_a_no_op_with_the_value(servicer, flags, option):
    value = canonical(flags, option)
    reply = await pad(servicer, "reset_setting", option=option, expect=value, **{"from": value})
    assert (reply.success, reply.error, reply.value) == (True, "ok", value)
    assert flags.writes() == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
async def test_reset_of_the_retro_backend_clears_the_persisted_value(servicer, flags):
    flags.admin[RETRO] = {"variant": "claude", "writer": "menu"}

    reply = await pad(servicer, "reset_setting", option=RETRO, expect="stub", **{"from": "claude"})

    assert (reply.success, reply.error, reply.value) == (True, "", "stub")
    [(method, url, _body)] = flags.writes()
    assert (method, url) == ("DELETE", "http://flag-service:8070/agent/inference.backend?persist=true")


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
async def test_reset_of_the_retro_backend_masked_by_a_stack_overlay_is_a_no_op(servicer, flags):
    flags.admin[RETRO] = {"variant": "stub", "writer": "menu"}
    flags.set_stack(RETRO, "claude", "talk-stage")

    reply = await pad(servicer, "reset_setting", option=RETRO, expect="claude", **{"from": "claude"})

    assert (reply.success, reply.error, reply.value) == (True, "ok", "claude")
    assert flags.writes() == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013"])
@pytest.mark.parametrize("option", ROWS)
async def test_reset_without_expect_is_refused(servicer, flags, option):
    reply = await pad(servicer, "reset_setting", option=option, **{"from": canonical(flags, option)})
    assert (reply.success, reply.error) == (False, "bad_option")
    assert flags.writes() == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-013"])
@pytest.mark.parametrize("option", ROWS)
async def test_reset_whose_target_moved_under_the_hold_is_stale(servicer, flags, option):
    low, high = LOW_HIGH[option]
    if option in PERSISTED:
        flags.admin[option] = {"variant": high, "writer": "menu"}
    else:
        flags.set_stack(option, setting_ladders.RATE_VARIANTS.get(high, high), "menu")

    reply = await pad(servicer, "reset_setting", option=option, expect=high, **{"from": high})

    assert (reply.success, reply.error) == (False, "stale")
    assert flags.writes() == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-013", "BR-014"])
@pytest.mark.parametrize("option", ROWS)
async def test_reset_from_another_value_than_in_force_is_stale(servicer, flags, option):
    """The value the reset replaces changed under the hold: what the pad judged a lowering may be a raise."""
    low, high = LOW_HIGH[option]
    if option in PERSISTED:
        flags.admin[option] = {"variant": high, "writer": "menu"}
    else:
        flags.set_stack(option, setting_ladders.RATE_VARIANTS.get(high, high), "menu")
    reply = await pad(servicer, "reset_setting", option=option, expect=BASELINE[option], **{"from": low})

    assert (reply.success, reply.error) == (False, "stale")
    assert flags.writes() == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
@pytest.mark.parametrize("option", RAISE_ROWS)
async def test_a_raise_row_reset_without_from_is_refused(servicer, flags, option):
    reply = await pad(servicer, "reset_setting", option=option, expect=BASELINE[option])
    assert (reply.success, reply.error) == (False, "bad_option")
    assert flags.requests == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-013"])
async def test_reset_racing_another_writer_is_stale(servicer, flags):
    """The overlay was the menu's at the read and another writer's at the compare-and-delete (412)."""
    flags.set_stack(EFFORT, "high", "menu")
    flags.race_on_delete.add(EFFORT)

    reply = await pad(servicer, "reset_setting", option=EFFORT, expect="medium", **{"from": "high"})

    assert (reply.success, reply.error) == (False, "stale")
    assert failure_cues(servicer) == 0


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-013"])
async def test_a_failed_reset_is_flag_service_unavailable(servicer, flags):
    flags.set_stack(EFFORT, "high", "menu")
    flags.failing.add(EFFORT)

    reply = await pad(servicer, "reset_setting", option=EFFORT, expect="medium", **{"from": "high"})

    assert (reply.success, reply.error) == (False, "flag_service_unavailable")
    assert failure_cues(servicer) == 2
    assert EFFORT not in servicer.settings_ops.setting_defaults  # unknown until the next read


# --- set_backend `from` (UC-016 BR-014, D12, D16) ---------------------------------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-014"])
async def test_set_backend_from_the_backend_in_force_writes(servicer, flags):
    flags.admin["decision.backend"] = {"variant": "claude", "writer": "menu"}
    reply = await pad(servicer, "set_backend", backend="openai", **{"from": "claude"})
    assert (reply.success, reply.error, reply.value) == (True, "", "openai")
    assert flags.admin["decision.backend"]["variant"] == "openai"


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-014"])
async def test_set_backend_from_a_backend_no_longer_in_force_is_stale(servicer, flags):
    """The pad judged claude -> openai paid to paid (no confirmation); the backend went to rules under the
    hold, so the switch would be an unconfirmed free-to-paid raise: refused, nothing written. The in-process
    flagd client still serves claude (its sync lag): the check reads the flag service."""
    servicer.agent_settings_client = agent_client(flags, **{"decision.backend": ("claude", "claude")})

    reply = await pad(servicer, "set_backend", backend="openai", **{"from": "claude"})

    assert (reply.success, reply.error, reply.value) == (False, "stale", "")
    assert flags.writes() == []
    assert failure_cues(servicer) == 0


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-014"])
async def test_set_backend_from_an_unreadable_backend_writes_nothing(servicer, flags):
    flags.down = True
    reply = await pad(servicer, "set_backend", backend="claude", **{"from": "rules"})
    assert (reply.success, reply.error) == (False, "flag_service_unavailable")
    assert flags.writes() == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-014"])
@pytest.mark.parametrize("given", ["", "RULES", "stub", "paid"])
async def test_set_backend_from_outside_the_backends_is_bad_option(servicer, flags, given):
    reply = await pad(servicer, "set_backend", backend="rules", **{"from": given})
    assert (reply.success, reply.error) == (False, "bad_option")
    assert flags.writes() == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-014"])
async def test_set_backend_without_from_switches_as_before(servicer, flags):
    """D16: the dashboard, the admin controller, voice and older pads send no `from`."""
    reply = await pad(servicer, "set_backend", backend="claude")
    assert (reply.success, reply.value) == (True, "claude")
    assert flags.admin["decision.backend"]["variant"] == "claude"


# --- No unconfirmed raise at the menu boundary (UC-016 BR-014) ---------------------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A9", br=["BR-014"])
@pytest.mark.parametrize("option", RAISE_ROWS)
async def test_no_command_raises_a_raise_row_from_a_value_the_pad_did_not_show(servicer, flags, option):
    """Whatever a pad sends, a raise row only moves from the value the pad showed: a step or reset without
    `from`, or from a value that is no longer in force, writes nothing. The value in force dropped under the
    hold, so the pad's "down from high" (no confirmation) would now be a raise from low."""
    low, high = LOW_HIGH[option]
    if option in PERSISTED:
        flags.admin[option] = {"variant": low, "writer": "menu"}
    else:
        flags.set_stack(option, setting_ladders.RATE_VARIANTS.get(low, low), "talk-stage")
    ladder = setting_ladders.AGENT_ROW_LADDERS[option]
    replies = []
    for steps in range(1, len(ladder) + 1):
        for direction in ("up", "down"):
            replies.append(await step(servicer, option, direction, steps))
            replies.append(await step(servicer, option, direction, steps, **{"from": high}))
    for expect in ladder:
        replies.append(await pad(servicer, "reset_setting", option=option, expect=expect))
        replies.append(await pad(servicer, "reset_setting", option=option, expect=expect, **{"from": high}))

    assert {reply.error for reply in replies} <= {"bad_option", "stale"}
    assert flags.writes() == []


# --- reset_setting decision.backend `from` (UC-016 BR-014, D12) ---------------------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A3", br=["BR-013", "BR-014"])
async def test_backend_reset_from_a_backend_no_longer_in_force_is_stale(servicer, flags):
    """The press read claude with the default claude (a no-op, no raise); rules is written under the hold and
    `expect` still matches: without the `from` check the reset would commit claude, free to paid, unconfirmed."""
    # In force now: rules (the baseline); the pad pressed at claude and still expects the reset target it showed.
    reply = await pad(servicer, "reset_setting", option="decision.backend", expect="rules", **{"from": "claude"})

    assert (reply.success, reply.error, reply.value) == (False, "stale", "")
    assert flags.writes() == []
    assert failure_cues(servicer) == 0


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A8", br=["BR-013", "BR-014"])
async def test_backend_reset_from_the_backend_in_force_clears(servicer, flags):
    flags.admin["decision.backend"] = {"variant": "claude", "writer": "menu"}

    reply = await pad(servicer, "reset_setting", option="decision.backend", expect="rules", **{"from": "claude"})

    assert (reply.success, reply.error, reply.value) == (True, "", "rules")
    assert [(m, u) for m, u, _b in flags.writes()] == [
        ("DELETE", "http://flag-service:8070/agent/decision.backend?persist=true")
    ]


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-014"])
@pytest.mark.parametrize("given", ["", "RULES", "stub"])
async def test_backend_reset_from_outside_the_backends_is_bad_option(servicer, flags, given):
    reply = await pad(servicer, "reset_setting", option="decision.backend", expect="rules", **{"from": given})
    assert (reply.success, reply.error) == (False, "bad_option")
    assert flags.requests == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", br=["BR-012", "BR-014"])
async def test_a_rate_written_as_a_float_is_its_integer_rung(servicer, flags):
    """`8.0` from a script is the rung `8`: canonical everywhere, so `from=8` holds and the step moves on."""
    flags.set_stack(RATE, "o4", "flags.sh", value=8.0)

    reply = await step(servicer, RATE, **{"from": "8"})

    assert (reply.success, reply.value) == (True, "20")
    assert settings_ops.canonical_value(RATE, flags.view(RATE), 8.0) == "8"


@pytest.mark.asyncio
@pytest.mark.uc("UC-016", flow="A4", br=["BR-011"])
@pytest.mark.uc("UC-015", flow="A1")
async def test_a_goal_command_that_waited_for_the_lock_while_a_game_started_is_refused(servicer, flags):
    """The servicer's lobby check ran before the lock; the game started while the command waited."""
    flags.set_stack(GOAL, "chaos_focused", "menu")
    live_lifecycle(servicer)

    step_result = await servicer.settings_ops.step_setting(GOAL, "up", 1)
    reset_result = await servicer.settings_ops.reset_setting(GOAL, "balanced_focused")

    assert (step_result.error, reset_result.error) == ("game_active", "game_active")
    assert flags.writes() == []
