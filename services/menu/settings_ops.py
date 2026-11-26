"""Settings core of the operator pad commands (#2050, UC-015).

The pad's ``ProcessInput`` commands (services.md Â§3.1) land here: the force-start
roster with ghost pruning (UC-014), the clamped setting step, the agent level
ladder, the kill switch and resume of UC-015 BR-002 / BR-003, the backend write,
and the voice tables. A refusal is an error code, not a log line.

Flag-service calls are blocking with a 0.5 s deadline and fail closed; they run
through ``asyncio.to_thread`` and are never retried or queued (UC-015 BR-010).
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
import time
from collections.abc import Callable
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any

from lib.colors import Colors
from lib.flag_service_writer import (
    HTTP_NO_CONTENT,
    HTTP_OK,
    HTTP_PRECONDITION_FAILED,
    STATUS_UNKNOWN,
    STEP_AT_LIMIT,
    WRITER_LABEL,
    match_variant,
)
from lib.setting_ladders import (
    AGENT_POLICY_LEVELS,
    AGENT_ROW_LADDERS,
    AGENT_ROW_OPTIONS,
    AGENT_STATE_LADDER,
    AUTOMATIC,
    AUTOMATIC_OPTIONS,
    BACKENDS,
    BRIEF_CUSTOM,
    BRIEF_OPTION,
    EFFORT_OPTION,
    GOAL_OPTION,
    GROUP_OPTION,
    KNOB_LADDERS,
    RAISE_LADDER_OPTIONS,
    RATE_OPTION,
    RATE_VARIANTS,
    RETRO_BACKEND_OPTION,
    SENSITIVITY_LADDER,
    SHIELD_KNOB_LADDER,
    knob_start,
    step_ladder,
)
from lib.types import Sound
from lib.voice_prompts import (
    AGENT_LEVEL_VOICES,
    AGENT_SWITCH_VOICES,
    AGENT_WATCHING_VOICE,
    AUTOMATIC_VOICE,
    FORCE_ALL_START_VOICES,
    INSTRUCTIONS_VOICES,
    INTERVENTIONS_VOICES,
    NUM_TEAMS_VOICES,
    SENSITIVITY_VOICES,
    SHIELD_ACTIVATION_VOICES,
)
from proto import controller_manager_pb2
from services.menu.utils.led import DEFAULT_COLOR, GAME_MODE_COLORS

if TYPE_CHECKING:
    from lib.flag_service_writer import FlagServiceWriter
    from services.menu.state_manager import StateManager

logger = logging.getLogger(__name__)

# --- Agent set keys -------------------------------------------------------------------------

# UC-015 BR-019: the agent level, written persisted.
AGENT_POLICY_FLAG = "interventions_allowed"
# UC-015 BR-001: the pad also writes the kill switch and the write gate (in memory only) and the backend (persisted).
AGENT_ENABLED_FLAG = "enabled"
# UC-016 BR-012: the agent's write gate (FR-051); only the agent switch writes it, in memory only.
AGENT_WRITE_GATE_FLAG = "interventions_enabled"
DECISION_BACKEND_FLAG = "decision.backend"
# UC-015 BR-018 (#2215): the shield mode the pad's K6 layer steps; a persisted admin value of the `game` set.
SHIELD_ACTIVATION_FLAG = "shield.activation"

# Ladders (UC-015 BR-004, BR-019): one source with the pad's preview, lib/setting_ladders.py (#2106).
DECISION_BACKENDS = tuple(BACKENDS)

# --- Error codes (UC-015 BR-012, services.md Â§3.1) ------------------------------------------

GAME_ACTIVE = "game_active"
AT_LIMIT = "at_limit"
FLAG_SERVICE_UNAVAILABLE = "flag_service_unavailable"
STALE = "stale"
BAD_OPTION = "bad_option"
OK = "ok"


def is_failure(error: str | None) -> bool:
    """A failed write (UC-015 A3): the error codes that get the failure cue (BR-009)."""
    return error == FLAG_SERVICE_UNAVAILABLE


# --- Voice tables ---------------------------------------------------------------------------

# The voice tables live in lib/voice_prompts.py, shared with the operator pad (#2106).
# UC-015 BR-009: the failure cue is two loud beeps, this far apart so they do not merge.
FAILURE_CUE_SOUND = Sound.SFX_BEEP_LOUD
FAILURE_CUE_GAP_SECONDS = 0.3
# Deadline of the live-roster lookup (GetConnectedControllers) of a force start (#2378,
# UC-014 A2): a hung controller manager falls back to the menu roster instead of holding
# the start. A recovery bound, not a stage knob, so a code constant (C-035).
LIVE_ROSTER_RPC_TIMEOUT_SECONDS = 2.0
# UC-015 step 8: a short sound at the new master level.
VOLUME_STEP_SOUND = Sound.SFX_JOIN

# --- Variant values -------------------------------------------------------------------------

SENSITIVITY_VALUES = {name: index for index, name in enumerate(SENSITIVITY_LADDER)}
_VARIANT_VALUES: dict[str, dict[str, Any]] = {
    "sensitivity": SENSITIVITY_VALUES,
    "num_teams": {"two": 2, "three": 3, "four": 4, "five": 5, "six": 6},
    "force_all_start": {"on": True, "off": False},
    "game_instructions": {"on": True, "off": False},
}


def variant_to_value(option_name: str, variant_name: str) -> int | float | bool:
    """A flag variant name as the typed value used for feedback (0 when unknown)."""
    return _VARIANT_VALUES.get(option_name, {}).get(variant_name, 0)


def _ladder_step(option: str, ladder: list[str], current: str | None, forward: bool, steps: int) -> str:
    """``steps`` moves along ``ladder``, wrapped or clamped per `WRAPPING`; outside it the first move lands on it."""
    new, _at_limit = step_ladder(option, ladder, current, steps if forward else -steps)
    return new


# UC-015 BR-015: the mode pulse on every lobby controller.
MODE_PULSE_MS = 600
MODE_PULSE_SPEED = 6
# UC-015 BR-018 (#2215): `automatic` (the operator value cleared) shows as a double white flash, a pattern
# no mode or shield mode uses (they pulse once).
AUTOMATIC_FLASH_MS = 150
AUTOMATIC_FLASH_GAP_SECONDS = 0.3
# The menu re-reads which settings the operator set at most this often (GetMenuStatus, BR-018, #2222).
OPERATOR_REFRESH_SECONDS = 2.0
# UC-015 BR-018 (#2215): the colour of each shield mode's pulse on the lobby controllers (the BR-015 pulse).
SHIELD_MODE_COLORS: dict[str, tuple[int, int, int]] = {
    "instant": Colors.White.value,
    "trigger": Colors.Yellow.value,
    "hold": Colors.Blue.value,
}


# --- Options of step_setting ----------------------------------------------------------------


@dataclass(frozen=True)
class _Option:
    writer_attr: str | None  # StateManager attribute of the writer; None = handled specially
    order: list[str] | None = None  # ladder instead of the declared variants ("up" = later entry)
    lobby_only: bool = False


STEP_OPTIONS: dict[str, _Option] = {
    "current_game": _Option(None, lobby_only=True),
    # UC-015 BR-018 (#2222): sensitivity and num_teams have the knob ladder with `automatic` first.
    "sensitivity": _Option("game_settings_writer", order=KNOB_LADDERS["sensitivity"], lobby_only=True),
    "num_teams": _Option("game_settings_writer", order=KNOB_LADDERS["num_teams"], lobby_only=True),
    "force_all_start": _Option("game_settings_writer", order=["off", "on"], lobby_only=True),
    "game_instructions": _Option("user_prefs_writer", order=["off", "on"], lobby_only=True),
    "audio_volume.master": _Option("user_prefs_writer"),
    "agent_level": _Option(None),
    # UC-015 BR-018: the knob ladder automatic, instant, trigger, hold (`_step_knob_setting`); persisted.
    SHIELD_ACTIVATION_FLAG: _Option("game_settings_writer", order=SHIELD_KNOB_LADDER, lobby_only=True),
    # UC-016 BR-012 (iteration 4): the agent rows, stepped from the value in force (`_step_agent_row`).
    GOAL_OPTION: _Option(None, lobby_only=True),
    RATE_OPTION: _Option(None),
    GROUP_OPTION: _Option(None),
    RETRO_BACKEND_OPTION: _Option(None),
    EFFORT_OPTION: _Option(None),
    BRIEF_OPTION: _Option(None),
}
# UC-015 A1: refused with game_active while the game lifecycle is live.
LOBBY_ONLY_OPTIONS = frozenset(name for name, opt in STEP_OPTIONS.items() if opt.lobby_only)
DIRECTIONS = {"up": True, "down": False}
AGENT_SWITCH_DIRECTIONS = ("stop",)
# UC-016 BR-014: `from` is required where a step or reset can raise what the agent may do or spend, so a
# value changed under the hold never turns the change the pad judged into an unconfirmed raise.
FROM_REQUIRED_OPTIONS = RAISE_LADDER_OPTIONS
# The agent rows written in memory (writer `menu`, no persist field); `inference.backend` is persisted.
AGENT_MEMORY_ROWS = frozenset({GOAL_OPTION, RATE_OPTION, GROUP_OPTION, EFFORT_OPTION, BRIEF_OPTION})

# --- reset_setting (UC-016 A8, BR-013; services.md Â§3.1) ---------------------------------------

# A reset of these clears the operator value, exactly as their `automatic` rung.
RESET_AUTOMATIC_OPTIONS = ("sensitivity", "num_teams", SHIELD_ACTIVATION_FLAG)
# option -> (StateManager writer attribute, flag key); the reset puts the layer below the operator's in force.
RESET_DEFAULT_OPTIONS: dict[str, tuple[str, str]] = {
    "game_instructions": ("user_prefs_writer", "game_instructions"),
    "force_all_start": ("game_settings_writer", "force_all_start"),
    "agent_level": ("agent_settings_writer", AGENT_POLICY_FLAG),
    "decision.backend": ("agent_settings_writer", DECISION_BACKEND_FLAG),
    RETRO_BACKEND_OPTION: ("agent_settings_writer", RETRO_BACKEND_OPTION),
}
# Explicit allowlists, not derived from STEP_OPTIONS: the volume, the mode and the agent's other keys never reset.
RESETTABLE_OPTIONS = frozenset(
    {
        "sensitivity",
        "num_teams",
        SHIELD_ACTIVATION_FLAG,
        "game_instructions",
        "force_all_start",
        "agent_level",
        "decision.backend",
        GOAL_OPTION,
        RATE_OPTION,
        GROUP_OPTION,
        RETRO_BACKEND_OPTION,
        EFFORT_OPTION,
        BRIEF_OPTION,
    }
)
RESET_LOBBY_ONLY_OPTIONS = frozenset(
    {"sensitivity", "num_teams", SHIELD_ACTIVATION_FLAG, "game_instructions", "force_all_start", GOAL_OPTION}
)
# GetMenuStatus.pad_commands: what a pad may send this menu (an older menu answers unknown commands with success).
PAD_CAPABILITIES = (
    "reset_setting",
    "reset_setting.from",
    "agent_state",
    "step_setting.from",
    "set_backend.from",
    "reset_setting.from:decision.backend",
)
# Per-option strings (`step_setting:<option>`, `reset_setting:<option>`).
AGENT_ROW_CAPABILITIES = {option: (f"step_setting:{option}", f"reset_setting:{option}") for option in AGENT_ROW_LADDERS}


def _layer_variant(view: dict, layer: str) -> str | None:
    value = view.get(layer)
    return (value.get("variant") or None) if isinstance(value, dict) else None


def agent_state_of(enabled: str | None, gate: str | None) -> str | None:
    """UC-016 BR-011: `off` (enabled off), `watch` (gate off) or `on`; None when a flag is unreadable."""
    if enabled == "off":
        return "off"
    if enabled != "on" or gate not in ("on", "off"):
        return None
    return "on" if gate == "on" else "watch"


def reset_target(view: dict | None) -> str | None:
    """What a reset of a RESET_DEFAULT_OPTIONS option puts in force; None = unknown.

    The one computation behind both `reset_setting` and `setting_defaults` (decision D2).
    """
    if view is None:
        return None
    return (
        _layer_variant(view, "stack_overlay")
        or _layer_variant(view, "real_overlay")
        or (view.get("baseline_default") or None)
    )


# --- The agent rows' canonical values (services.md Â§3.1, iteration 4) ------------------------------

_MISSING = object()


def _layer_value(view: dict, layer: dict):
    """The value of one layer of a flag view: its ``value``, else its declared variant's value."""
    if "value" in layer:
        return layer["value"]
    return (view.get("variant_values") or {}).get(layer.get("variant"), _MISSING)


def _value_below_stack(view: dict):
    """The value that applies once the stack overlay is gone: the admin value, the real overlay, the baseline."""
    for name in ("admin_value", "real_overlay"):
        layer = view.get(name)
        if isinstance(layer, dict):
            return _layer_value(view, layer)
    return (view.get("variant_values") or {}).get(view.get("baseline_default"), _MISSING)


def _value_in_force(view: dict):
    for name in ("stack_overlay", "admin_value", "real_overlay"):
        layer = view.get(name)
        if isinstance(layer, dict):
            return _layer_value(view, layer)
    return (view.get("variant_values") or {}).get(view.get("effective_default"), _MISSING)


def canonical_value(option: str, view: dict, value) -> str | None:
    """``value`` of an agent row in the option's canonical format; None when it has none.

    The rate is the integer decimal of its value; the brief is the preset whose text equals the value
    exactly (``off`` for the empty text), else ``custom``; the others are the declared variant's name.
    """
    if value is _MISSING:
        return None
    if option == RATE_OPTION:
        if isinstance(value, bool) or not isinstance(value, int | float) or value != int(value):
            return None
        return str(int(value))
    if option == BRIEF_OPTION:
        if not isinstance(value, str):
            return None
        if value == "":
            return "off"
        declared = view.get("variant_values") or {}
        presets = [name for name in AGENT_ROW_LADDERS[BRIEF_OPTION] if name != "off"]
        return next((name for name in presets if declared.get(name) == value), BRIEF_CUSTOM)
    return match_variant(view, value)


def row_in_force(option: str, view: dict) -> str | None:
    """The agent row's value in force (what the flag service serves now), canonical; None = none."""
    return canonical_value(option, view, _value_in_force(view))


def _menu_owns_stack(view: dict) -> bool:
    stack = view.get("stack_overlay")
    return isinstance(stack, dict) and stack.get("writer") == WRITER_LABEL


def row_reset_target(option: str, view: dict | None) -> str | None:
    """What a `reset_setting` of an agent row puts in force, canonical; None = unknown.

    The same function fills `setting_defaults` (decision D2). `inference.backend` takes the default reset
    of `decision.backend`. The others clear only the menu's own stack overlay (D14): with another writer's
    overlay, or none, the target is the value in force (a no-op); a free brief text (`custom`) is never the
    menu's, so it stays (D13).
    """
    if view is None:
        return None
    if option == RETRO_BACKEND_OPTION:
        return reset_target(view)
    in_force = row_in_force(option, view)
    if not _menu_owns_stack(view) or in_force == BRIEF_CUSTOM:
        return in_force
    return canonical_value(option, view, _value_below_stack(view))


def canonical_given(option: str, raw: str, view: dict) -> str | None:
    """A `from` or `expect` of an agent row in canonical format: a declared variant name is mapped to its
    value first, so `lively` and `8` are the same rate. None = neither a variant nor a canonical value."""
    if raw in (view.get("variants") or []):
        return canonical_value(option, view, (view.get("variant_values") or {}).get(raw, _MISSING))
    if option == RATE_OPTION and raw.isdigit() and str(int(raw)) == raw:
        return raw
    if option == BRIEF_OPTION and raw == BRIEF_CUSTOM:
        return raw
    return None


@dataclass(frozen=True)
class Result:
    """Outcome of a settings operation: the value before and after, and an error code (or None)."""

    old: str | None
    new: str | None
    error: str | None = None


class SettingsOps:
    """The settings core; one instance per menu."""

    def __init__(
        self,
        *,
        get_state_manager: Callable[[], StateManager | None],
        controller_channel,
        metrics,
        callbacks=None,
        agent_memory_writer: FlagServiceWriter | None = None,
        lifecycle_live: Callable[[], bool] | None = None,
    ):
        self._get_state_manager = get_state_manager
        self.controller_channel = controller_channel
        self.metrics = metrics
        # The servicer: get_game_options and select_game_mode for the mode step.
        self.callbacks = callbacks
        # UC-015 BR-001: the `agent` keys whose rows take no persist (`enabled`, the write gate, the in-memory
        # agent rows) have their own writer, with no persist field.
        self.agent_memory_writer = agent_memory_writer
        # The game lifecycle is live: the lobby-only goal is re-checked under the settings lock.
        self._lifecycle_live = lifecycle_live or (lambda: False)
        # Serializes the agent rows' and the backend's read-then-write (decision D15).
        self._settings_lock = asyncio.Lock()
        # GetMenuStatus.agent_operator_brief_variant from the background read; None = unknown.
        self.brief_variant: str | None = None
        # Serializes the agent's read-then-write commands (the switch's `from` check, the ladder).
        self._agent_lock = asyncio.Lock()
        # UC-015 BR-018 (#2222): the operator's persisted value of each setting with an `automatic` rung as
        # last read from the flag service (`automatic` when none; a missing key = unknown), for
        # GetMenuStatus, which never calls HTTP itself.
        self.operator_settings: dict[str, str] = {}
        # GetMenuStatus.setting_defaults: reset_target per RESET_DEFAULT_OPTIONS option from the same
        # background read; a missing key = unknown.
        self.setting_defaults: dict[str, str] = {}
        self._operator_read_at = -OPERATOR_REFRESH_SECONDS
        # Goes up on every commit: a background read that started before it is dropped (stale).
        self._operator_generation = 0
        self._defaults_generation = 0
        self.operator_refresh_task: asyncio.Task | None = None

    @property
    def state_manager(self) -> StateManager | None:
        return self._get_state_manager()

    def _defaults_changed(self, option: str) -> None:
        """A write moved ``option``'s reset target: unknown until the next background read."""
        self._defaults_generation += 1
        self.setting_defaults.pop(option, None)

    @contextlib.asynccontextmanager
    async def _agent_change(self):
        """The agent lock; a setting_defaults read overlapping the change is dropped (bumped before and after)."""
        async with self._agent_lock:
            self._defaults_generation += 1
            try:
                yield
            finally:
                # The agent's reset target may have moved: unknown until the next background read.
                self.setting_defaults.pop("agent_level", None)
                self._defaults_generation += 1

    # --- Force-start roster (UC-014 steps 3-4, UC-015 BR-008) -------------------------------

    async def prune_ghosts(self, controllers: list[str], span) -> list[str]:
        """Drop ghost controllers from ``controllers`` using the controller manager's live roster.

        Queries ``GetConnectedControllers`` (#1153) and removes any serial the backend no
        longer reports (the #551 ghost window). Fail-soft: on any RPC error, or with no
        channel, the list is returned unchanged so a force-start is never blocked.
        """
        if self.controller_channel is None or not controllers:
            return controllers

        from proto import controller_manager_pb2, controller_manager_pb2_grpc

        try:
            stub = controller_manager_pb2_grpc.ControllerManagerServiceStub(self.controller_channel)
            response = await stub.GetConnectedControllers(
                controller_manager_pb2.GetConnectedControllersRequest(), timeout=LIVE_ROSTER_RPC_TIMEOUT_SECONDS
            )
        except Exception as e:
            logger.warning(f"Live roster query failed, using menu roster as-is: {e}")
            span.set_attribute("force_start.roster_reconciled", False)
            self.metrics.force_start_reconcile_failed_total.inc()
            return controllers

        live_serials = set(response.connected_serials)
        pruned = [s for s in controllers if s in live_serials]
        ghosts = [s for s in controllers if s not in live_serials]

        if ghosts:
            logger.warning(f"Force-start: pruning {len(ghosts)} ghost controller(s) via live roster: {ghosts}")
            for _ in ghosts:
                self.metrics.ghost_controllers_pruned_total.inc()

        span.set_attribute("force_start.roster_reconciled", True)
        span.set_attribute("force_start.ghosts_pruned", len(ghosts))
        return pruned

    async def force_start_roster(self, span) -> list[str]:
        """The roster of a force-start: all connected with force_all_start, else the ready ones.

        UC-014 step 3: force_all_start is read now, at the force start.
        """
        from lib.feature_flags import get_flag_client

        manager = self.state_manager
        force_all = get_flag_client("game").get_boolean_value("force_all_start", False)
        span.set_attribute("force_all_start", force_all)
        if manager is None:
            controllers: list[str] = []
        elif force_all:
            controllers = list(manager.connected_controllers)
        else:
            controllers = list(manager.ready_controllers)
        return await self.prune_ghosts(controllers, span)

    # --- Agent level reads ---------------------------------------------------------------

    def current_agent_level(self) -> str | None:
        """The agent level the flag service serves (``effective_default``), None when it cannot be read.

        UC-015 BR-019 (#2095): never the in-process flagd value. The flag service applies a
        write before it answers, while flagd catches up only within NFR-050, so a step read
        from flagd right after a write (or after a write that timed out but landed) steps
        from the old level again. Blocking (one flag-service read): run via to_thread.
        """
        view = self.state_manager.agent_settings_writer.get_flag_view(AGENT_POLICY_FLAG)
        return view.get("effective_default") if view else None

    def _agent_enabled(self) -> str | None:
        """The kill switch as the flag service serves it (``on`` / ``off``), None when unreadable."""
        return self._agent_switch_flag(AGENT_ENABLED_FLAG)

    def _agent_switch_flag(self, key: str) -> str | None:
        """An agent on/off flag (``enabled``, ``interventions_enabled``) as the flag service serves it.

        ``on`` / ``off``, None when unreadable. Blocking.
        """
        writer = self.agent_memory_writer or self.state_manager.agent_settings_writer
        try:
            view = writer.get_flag_view(key)
        except Exception as e:
            logger.warning(f"Reading agent/{key} failed: {e}")
            return None
        return view.get("effective_default") if view else None

    def _read_agent_level(self) -> str | None:
        """The level, None when unreadable. Blocking."""
        try:
            return self.current_agent_level()
        except Exception as e:
            logger.warning(f"Reading agent/{AGENT_POLICY_FLAG} failed: {e}")
            return None

    @staticmethod
    def _write(writer, key: str, variant: str) -> bool:
        """One flag-service write; any error counts as a failed write (fail closed, never retried)."""
        if writer is None:
            return False
        try:
            return bool(writer.update_default_variant(key, variant))
        except Exception as e:
            logger.error(f"Write {key}={variant} failed: {e}")
            return False

    # --- step_setting (UC-015 steps 4, 5, 8, A7, A8, BR-004, BR-005) --------------------------

    async def step_setting(self, option: str, direction: str, steps: int, from_value: str | None = None) -> Result:
        """Step ``option`` by ``steps`` in ``direction`` with one write, stopping at the ends or wrapping (`WRAPPING`).

        Errors: ``bad_option``, ``at_limit`` (nothing written; clamped options only), ``flag_service_unavailable``,
        ``stale`` (``from_value``, agent_level and the agent rows only, is not the value the step would start from).
        ``ok``: a wrapping option's net steps come back to the start, a no-op (nothing written, no cue).
        The caller checks the lobby-only refusal (``game_active``) first.
        """
        spec = STEP_OPTIONS.get(option)
        if spec is None or direction not in DIRECTIONS or steps < 1:
            return Result(None, None, BAD_OPTION)
        forward = DIRECTIONS[direction]
        if option in AGENT_ROW_OPTIONS:
            return await self._step_agent_row(option, steps if forward else -steps, from_value)
        if from_value is not None and option != "agent_level":
            return Result(None, None, BAD_OPTION)
        from_level = from_value

        if option == "current_game":
            return await self._step_game_mode(forward, steps)
        if option == "agent_level":
            return await self._step_agent_level(forward, steps, from_level)
        if option in AUTOMATIC_OPTIONS:
            return await self._step_knob_setting(option, spec.writer_attr, forward, steps)

        writer = getattr(self.state_manager, spec.writer_attr, None)
        if writer is None:
            return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
        step = await asyncio.to_thread(writer.step_variant, option, forward, False, steps=steps, order=spec.order)
        if step.error == STEP_AT_LIMIT:
            return Result(step.old, step.old, AT_LIMIT)
        if step.error is not None:
            return Result(step.old, None, FLAG_SERVICE_UNAVAILABLE)
        if option in RESET_DEFAULT_OPTIONS:
            self._defaults_changed(option)
        return Result(step.old, step.new)

    async def _step_game_mode(self, forward: bool, steps: int) -> Result:
        """The mode step of UC-011 steps 2-5 over the selectable modes, wrapping (UC-015 BR-005)."""
        options = list(self.callbacks.get_game_options()) if self.callbacks is not None else []
        manager = self.state_manager
        current = manager.current_game_mode.name if manager is not None else None
        if not options:
            return Result(current, None, BAD_OPTION)
        new = _ladder_step("current_game", options, current, forward, steps)
        if new == current:
            return Result(current, current, OK)  # a full turn of the wheel: nothing to select
        if not await self.callbacks.select_game_mode(new, source="operator_pad"):
            return Result(current, None, BAD_OPTION)
        return Result(current, new)

    async def _step_agent_level(self, forward: bool, steps: int, from_level: str | None = None) -> Result:
        """The level ladder of BR-004: the live level, whatever `enabled` is (a tuning setting, not a kill)."""
        async with self._agent_change():
            level = await asyncio.to_thread(self._read_agent_level)
            if level is None:
                return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
            if from_level is not None and from_level != level:
                return Result(level, None, STALE)
            new = _ladder_step("agent_level", AGENT_POLICY_LEVELS, level, forward, steps)
            if new == level:
                return Result(level, level, AT_LIMIT)
            writer = self.state_manager.agent_settings_writer
            if not await asyncio.to_thread(self._write, writer, AGENT_POLICY_FLAG, new):
                return Result(level, None, FLAG_SERVICE_UNAVAILABLE)
            return Result(level, new)

    # --- settings with an `automatic` rung (UC-015 step 9, BR-018, #2222) -----------------------

    def _read_knob(self, option: str) -> tuple[str | None, str | None]:
        """One read of ``option``: (operator value, effective variant).

        The operator value is a variant, `automatic` (none) or None (unreadable).
        """
        writer = getattr(self.state_manager, STEP_OPTIONS[option].writer_attr, None)
        view = writer.get_flag_view(option) if writer is not None else None
        if view is None:
            return None, None
        effective = view.get("effective_default")
        admin = view.get("admin_value")
        if not admin:
            return AUTOMATIC, effective
        variant = admin.get("variant")
        return (variant if variant in KNOB_LADDERS[option] else None), effective

    def _read_operator(self, option: str) -> str | None:
        return self._read_knob(option)[0]

    def _read_all_operators(self) -> dict[str, str]:
        values = {option: self._read_operator(option) for option in KNOB_LADDERS}
        return {option: value for option, value in values.items() if value is not None}

    def _default_view(self, option: str) -> dict | None:
        attr, key = RESET_DEFAULT_OPTIONS[option]
        writer = getattr(self.state_manager, attr, None)
        if writer is None:
            return None
        try:
            return writer.get_flag_view(key)
        except Exception as e:
            logger.warning(f"Reading {key} failed: {e}")
            return None

    def _read_setting_defaults(self) -> dict[str, str]:
        """reset_target of every RESET_DEFAULT_OPTIONS option (an unknown one left out). Blocking."""
        values = {}
        for option in RESET_DEFAULT_OPTIONS:
            if option in AGENT_ROW_OPTIONS:
                continue  # `_read_agent_rows`
            target = reset_target(self._default_view(option))
            if target:
                values[option] = target
        return values

    def _read_agent_rows(self) -> tuple[dict[str, str], str | None]:
        """One read of the `agent` set for the agent rows: their reset targets (an unknown one left out) and
        the brief in force (None = unknown). Blocking."""
        writer = self.agent_memory_writer or self.state_manager.agent_settings_writer
        try:
            views = writer.get_set_view()
        except Exception as e:
            logger.warning(f"Reading the agent set failed: {e}")
            views = None
        if not isinstance(views, dict):
            return {}, None
        targets = {}
        for option in AGENT_ROW_LADDERS:
            target = row_reset_target(option, views.get(option))
            if target is not None:
                targets[option] = target
        brief = views.get(BRIEF_OPTION)
        return targets, (row_in_force(BRIEF_OPTION, brief) if brief else None)

    async def refresh_operator_settings(self) -> None:
        """Re-read the operator's values for GetMenuStatus (BR-018); an unreadable one is left out (unknown)."""
        generation = self._operator_generation
        defaults_generation = self._defaults_generation
        self._operator_read_at = time.monotonic()
        values = await asyncio.to_thread(self._read_all_operators)
        defaults = await asyncio.to_thread(self._read_setting_defaults)
        row_targets, brief = await asyncio.to_thread(self._read_agent_rows)
        if generation == self._operator_generation:
            self.operator_settings = values  # else a commit landed during the read: its value stands
        if defaults_generation == self._defaults_generation:
            # Only when no agent command overlapped the read: its change may have moved a target.
            self.setting_defaults = {**defaults, **row_targets}
            self.brief_variant = brief

    def pad_commands(self) -> list[str]:
        """GetMenuStatus.pad_commands: the commands, guard keys and agent-row options this menu takes."""
        return [*PAD_CAPABILITIES, *(name for names in AGENT_ROW_CAPABILITIES.values() for name in names)]

    @staticmethod
    def _clear(writer, key: str) -> bool:
        """One clear of a persisted admin value; any error counts as failed (fail closed, never retried)."""
        try:
            return bool(writer.clear_persisted(key))
        except Exception as e:
            logger.error(f"Clear {key} failed: {e}")
            return False

    def schedule_operator_refresh(self) -> None:
        """GetMenuStatus: refresh the cached operator values in the background when they are old.

        At most one read per OPERATOR_REFRESH_SECONDS, also while a value is unknown: the pad's display
        polls GetMenuStatus every 500 ms (UC-016 BR-003), and an unreadable flag must not turn every
        poll into three flag-service reads.
        """
        running = self.operator_refresh_task is not None and not self.operator_refresh_task.done()
        fresh = time.monotonic() - self._operator_read_at < OPERATOR_REFRESH_SECONDS
        if running or fresh:
            return
        self.operator_refresh_task = asyncio.create_task(self.refresh_operator_settings())

    async def _step_knob_setting(self, option: str, writer_attr: str, forward: bool, steps: int) -> Result:
        """BR-018: step the operator value along automatic and the value ladder; one write or one clear."""
        writer = getattr(self.state_manager, writer_attr, None)
        operator, effective = await asyncio.to_thread(self._read_knob, option)
        old = knob_start(option, operator, effective)  # the operator value, else where the setting stands
        if writer is None or old is None:
            return Result(None, None, FLAG_SERVICE_UNAVAILABLE)  # fail closed: the knob's start is unknown
        new, at_limit = step_ladder(option, KNOB_LADDERS[option], old, steps if forward else -steps)
        if at_limit:
            return Result(old, old, AT_LIMIT)
        if new == old:
            return Result(old, old, OK)  # a wrapping ladder turned full circle: nothing written, no cue
        self._operator_generation += 1  # a background read already in flight must not put the old value back
        if new == AUTOMATIC:
            ok = await asyncio.to_thread(self._clear, writer, option)
        else:
            ok = await asyncio.to_thread(self._write, writer, option, new)
        self._operator_generation += 1  # and a read that started during the write may predate it
        if not ok:
            self.operator_settings.pop(option, None)  # the outcome is unknown until the next read
            return Result(old, None, FLAG_SERVICE_UNAVAILABLE)
        self.operator_settings[option] = new
        return Result(old, new)

    # --- the agent rows (UC-016 BR-012 to BR-014, iteration 4) ---------------------------------

    def _row_writer(self, option: str):
        if option in AGENT_MEMORY_ROWS:
            return self.agent_memory_writer
        return self.state_manager.agent_settings_writer

    def _row_view(self, option: str) -> dict | None:
        """The agent row's flag view from the flag service; None when unreadable. Blocking."""
        writer = self.agent_memory_writer or self.state_manager.agent_settings_writer
        try:
            view = writer.get_flag_view(option)
        except Exception as e:
            logger.warning(f"Reading agent/{option} failed: {e}")
            return None
        return view if isinstance(view, dict) else None

    def _write_row(self, option: str, new: str) -> bool:
        """One write of an agent row's rung (the rate by its declared variant). Blocking."""
        return self._write(self._row_writer(option), option, RATE_VARIANTS[new] if option == RATE_OPTION else new)

    def _clear_row_overlay(self, option: str) -> int:
        try:
            return int(self.agent_memory_writer.clear_overlay(option))
        except Exception as e:
            logger.error(f"Clear of the menu's {option} overlay failed: {e}")
            return STATUS_UNKNOWN

    def _row_committed(self, option: str, value: str | None) -> None:
        """A row write or clear ended (``value`` None: its outcome is unknown): its reset target moves."""
        self._defaults_changed(option)
        if option == BRIEF_OPTION:
            self.brief_variant = value

    @staticmethod
    def _check_from(option: str, from_value: str | None, in_force: str | None, view: dict) -> str | None:
        """The error of a `from` guard of an agent row, None when it holds (or none is given)."""
        if from_value is None:
            return None
        given = canonical_given(option, from_value, view)
        if given is None:
            return BAD_OPTION
        return STALE if given != in_force else None

    async def _step_agent_row(self, option: str, net: int, from_value: str | None) -> Result:
        """Step an agent row from the value in force along its ladder; never from off the ladder (D4, D11)."""
        if from_value is None and option in FROM_REQUIRED_OPTIONS:
            return Result(None, None, BAD_OPTION)
        async with self._settings_lock:
            if option in LOBBY_ONLY_OPTIONS and self._lifecycle_live():
                return Result(None, None, GAME_ACTIVE)  # a game started while the command waited for the lock
            view = await asyncio.to_thread(self._row_view, option)
            if view is None:
                return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
            current = row_in_force(option, view)
            ladder = AGENT_ROW_LADDERS[option]
            if current not in ladder:
                return Result(current, None, BAD_OPTION)
            error = self._check_from(option, from_value, current, view)
            if error is not None:
                return Result(current, None, error)
            new, at_limit = step_ladder(option, ladder, current, net)
            if at_limit:
                return Result(current, current, AT_LIMIT)
            if new == current:
                return Result(current, current, OK)  # a wrapping wheel turned full circle: nothing written
            ok = await asyncio.to_thread(self._write_row, option, new)
            self._row_committed(option, new if ok else None)
            if not ok:
                return Result(current, None, FLAG_SERVICE_UNAVAILABLE)
            return Result(current, new)

    async def _reset_agent_row(self, option: str, expect: str | None, from_value: str | None) -> Result:
        """Reset an agent row: `inference.backend` as `decision.backend`, the others clear the menu's own overlay."""
        if not expect or (from_value is None and option in FROM_REQUIRED_OPTIONS):
            return Result(None, None, BAD_OPTION)
        async with self._settings_lock:
            if option in RESET_LOBBY_ONLY_OPTIONS and self._lifecycle_live():
                return Result(None, None, GAME_ACTIVE)
            view = await asyncio.to_thread(self._row_view, option)
            target = row_reset_target(option, view)
            if view is None or target is None:
                return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
            current = row_in_force(option, view)
            expected = canonical_given(option, expect, view)
            if expected is None:
                return Result(current, None, BAD_OPTION)
            if expected != target:
                return Result(current, None, STALE)
            error = self._check_from(option, from_value, current, view)
            if error is not None:
                return Result(current, None, error)
            if option == RETRO_BACKEND_OPTION:
                result = await self._clear_admin(option, view, target)
                if result.error is None:
                    self._row_committed(option, target)
                return Result(current, result.new, result.error)
            if not _menu_owns_stack(view) or current == BRIEF_CUSTOM:
                return Result(current, target, OK)  # another writer's value, or none: nothing to clear (D14)
            status = await asyncio.to_thread(self._clear_row_overlay, option)
            if status == HTTP_PRECONDITION_FAILED:
                return Result(current, None, STALE)  # another writer replaced the menu's overlay under the reset
            self._row_committed(option, target if status in (HTTP_OK, HTTP_NO_CONTENT) else None)
            if status not in (HTTP_OK, HTTP_NO_CONTENT):
                return Result(current, None, FLAG_SERVICE_UNAVAILABLE)
            return Result(current, target)

    # --- agent_switch (UC-015 step 6, A3, A9, BR-002) ----------------------------------------

    async def agent_switch(self, direction: str) -> Result:
        """The stop: `enabled` off and nothing else, never read first (idempotent, A9)."""
        if direction not in AGENT_SWITCH_DIRECTIONS:
            return Result(None, None, BAD_OPTION)
        async with self._agent_change():
            if not await asyncio.to_thread(self._write, self.agent_memory_writer, AGENT_ENABLED_FLAG, "off"):
                return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
            return Result(None, "off")

    # --- agent_state (UC-016 BR-012, BR-014; UC-015 BR-003) -----------------------------------

    def _read_agent_switch_state(self) -> str | None:
        """The switch state from the flag service, None when unreadable. Blocking."""
        return agent_state_of(self._agent_enabled(), self._agent_switch_flag(AGENT_WRITE_GATE_FLAG))

    async def agent_state(self, state: str, from_state: str | None) -> Result:
        """The `AGENT` row 1 switch: `off` is the stop; `watch` / `on` write the gate, then `enabled` on."""
        if state not in AGENT_STATE_LADDER or from_state not in AGENT_STATE_LADDER:
            return Result(None, None, BAD_OPTION)
        async with self._agent_change():
            now = await asyncio.to_thread(self._read_agent_switch_state)
            if now is None:
                return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
            if now != from_state:
                return Result(now, None, STALE)
            if now == state:
                return Result(now, state, OK)
            writer = self.agent_memory_writer
            if state == "off":
                ok = await asyncio.to_thread(self._write, writer, AGENT_ENABLED_FLAG, "off")
            else:
                # The gate first, so `enabled` is never on before it (#2093); a failure stops here.
                gate = "on" if state == "on" else "off"
                ok = await asyncio.to_thread(self._write, writer, AGENT_WRITE_GATE_FLAG, gate)
                if ok and now == "off":
                    ok = await asyncio.to_thread(self._write, writer, AGENT_ENABLED_FLAG, "on")
            if not ok:
                return Result(now, None, FLAG_SERVICE_UNAVAILABLE)
            return Result(now, state)

    # --- reset_setting (UC-016 A8, BR-013) ----------------------------------------------------

    async def reset_setting(self, option: str, expect: str | None, from_level: str | None = None) -> Result:
        """Clear the operator's persisted value so the layer below applies; ``new`` is what is now in force.

        ``ok``: nothing to clear (no operator value, or one masked by a stack overlay), nothing written.
        ``stale``: ``expect`` differs from the recomputed target, or ``from_level`` (agent_level only) from the
        level the reset replaces. The caller checks ``game_active`` first.
        """
        if option not in RESETTABLE_OPTIONS:
            return Result(None, None, BAD_OPTION)
        if option in AGENT_ROW_OPTIONS:
            return await self._reset_agent_row(option, expect, from_level)
        if from_level is not None and option not in ("agent_level", DECISION_BACKEND_FLAG):
            return Result(None, None, BAD_OPTION)
        if option == DECISION_BACKEND_FLAG:
            if from_level is not None and from_level not in DECISION_BACKENDS:
                return Result(None, None, BAD_OPTION)
            async with self._settings_lock:
                return await self._reset_default(option, expect, from_level)
        if option in RESET_AUTOMATIC_OPTIONS:
            return await self._reset_automatic(option, expect)
        if option == "agent_level":
            if not expect:
                return Result(None, None, BAD_OPTION)
            async with self._agent_change():
                return await self._reset_agent_level(expect, from_level)
        return await self._reset_default(option, expect, None)

    async def _reset_default(self, option: str, expect: str | None, from_value: str | None) -> Result:
        """One clear of the operator's persisted value; ``from_value`` (decision.backend) must be the value in force,
        so a backend changed under the hold never turns a no-op into an unconfirmed free-to-paid reset (BR-014)."""
        view = await asyncio.to_thread(self._default_view, option)
        target = reset_target(view)
        if target is None:
            return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
        if expect is not None and expect != target:
            return Result(None, None, STALE)
        if from_value is not None and from_value != view.get("effective_default"):
            return Result(None, None, STALE)
        return await self._clear_admin(option, view, target)

    async def _clear_admin(self, option: str, view: dict, target: str) -> Result:
        operator = _layer_variant(view, "admin_value")
        if operator is None or _layer_variant(view, "stack_overlay") is not None:
            # Nothing set, or masked by a stack overlay: clearing would not change the value in force.
            return Result(operator, target, OK)
        attr, key = RESET_DEFAULT_OPTIONS[option]
        if not await asyncio.to_thread(self._clear, getattr(self.state_manager, attr), key):
            return Result(operator, None, FLAG_SERVICE_UNAVAILABLE)
        return Result(operator, target)

    async def _reset_agent_level(self, expect: str, from_level: str | None = None) -> Result:
        view = await asyncio.to_thread(self._default_view, "agent_level")
        target = reset_target(view)
        if target is None:
            return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
        if target not in AGENT_POLICY_LEVELS:
            return Result(None, None, BAD_OPTION)
        if expect != target:
            return Result(None, None, STALE)
        level = view.get("effective_default")
        if from_level is not None and from_level != level:
            # The level dropped (or rose) under the hold: what the pad judged a lowering may now be a raise.
            return Result(None, None, STALE)
        result = await self._clear_admin("agent_level", view, target)
        return Result(level, result.new, result.error)

    async def _reset_automatic(self, option: str, expect: str | None) -> Result:
        """As the `automatic` rung of `_step_knob_setting`: one clear of the operator value."""
        writer = getattr(self.state_manager, STEP_OPTIONS[option].writer_attr, None)
        operator, _effective = await asyncio.to_thread(self._read_knob, option)
        if writer is None or operator is None:
            return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
        if expect is not None and expect != AUTOMATIC:
            return Result(operator, None, STALE)
        if operator == AUTOMATIC:
            return Result(AUTOMATIC, AUTOMATIC, OK)
        self._operator_generation += 1
        ok = await asyncio.to_thread(self._clear, writer, option)
        self._operator_generation += 1
        if not ok:
            self.operator_settings.pop(option, None)
            return Result(operator, None, FLAG_SERVICE_UNAVAILABLE)
        self.operator_settings[option] = AUTOMATIC
        return Result(operator, AUTOMATIC)

    # --- set_backend (UC-015 BR-004) -------------------------------------------------------

    async def set_backend(self, backend: str, from_backend: str | None = None) -> Result:
        """Write ``decision.backend`` (persisted); ``bad_option``, ``stale`` or ``flag_service_unavailable`` on error.

        ``from_backend`` (UC-016 BR-014, D12): the backend the pad's edit started from; ``stale``, nothing
        written, when the backend the flag service serves differs. Optional (D16).
        """
        if backend not in DECISION_BACKENDS or (from_backend is not None and from_backend not in DECISION_BACKENDS):
            return Result(None, None, BAD_OPTION)
        async with self._settings_lock:
            if from_backend is not None:
                # The flag service, not flagd: flagd's sync lag could let a backend changed under the hold pass.
                view = await asyncio.to_thread(self._default_view, DECISION_BACKEND_FLAG)
                if view is None:
                    return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
                if view.get("effective_default") != from_backend:
                    return Result(view.get("effective_default"), None, STALE)
            writer = self.state_manager.agent_settings_writer
            if not await asyncio.to_thread(self._write, writer, DECISION_BACKEND_FLAG, backend):
                return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
            # A persisted write drops a stack overlay, which moves the reset target.
            self._defaults_changed(DECISION_BACKEND_FLAG)
            return Result(from_backend, backend)

    # --- Feedback ---------------------------------------------------------------------------

    async def announce(self, option: str, variant: str | None, quiet: bool = False) -> None:  # noqa: C901 - one branch per option voice; split later
        """The voice line for a new value (UC-015 step 4, 5, 8). Best effort.

        ``quiet``: no volume step sound (UC-016 BR-004, the pad shows the level); every other option ignores it.

        A new mode also pulses its colour on every lobby controller (UC-015 BR-015), and so does a
        new shield mode (BR-018).
        """
        manager = self.state_manager
        if manager is None or variant is None:
            return
        audio = manager.audio
        try:
            if option == "current_game":
                await self.show_mode_on_lobby(variant)
                await audio.play_game_mode_voice(variant)
            elif variant == AUTOMATIC and option in AUTOMATIC_OPTIONS:
                # BR-018: one voice line and one double white flash for the `automatic` rung of any setting.
                await self.show_automatic_on_lobby()
                await audio.play_voice(AUTOMATIC_VOICE)
            elif option == "sensitivity":
                index = SENSITIVITY_VALUES.get(variant)
                if index is not None:
                    await audio.play_voice(SENSITIVITY_VOICES[index])
            elif option == "num_teams":
                voice = NUM_TEAMS_VOICES.get(variant_to_value("num_teams", variant))
                if voice is not None:
                    await audio.play_voice(voice)
            elif option == "force_all_start":
                await audio.play_voice(FORCE_ALL_START_VOICES[variant == "on"])
            elif option == "game_instructions":
                await audio.play_voice(INSTRUCTIONS_VOICES[variant == "on"])
            elif option == "agent_level":
                voice = AGENT_LEVEL_VOICES.get(variant)
                if voice is not None:
                    await audio.play_voice(voice)
            elif option == "audio_volume.master":
                if not quiet:
                    await audio.play_sound(VOLUME_STEP_SOUND)
            elif option == SHIELD_ACTIVATION_FLAG:
                await self.show_shield_mode_on_lobby(variant)
                voice = SHIELD_ACTIVATION_VOICES.get(variant)
                if voice is not None:
                    await audio.play_voice(voice)
            elif option == "agent_switch":
                voice = AGENT_SWITCH_VOICES.get(variant)
                if voice is not None:
                    await audio.play_voice(voice)
        except Exception as e:
            logger.warning(f"Announcing {option}={variant} failed: {e}")

    async def announce_agent_state(self, old: str | None, new: str) -> None:
        """UC-016 BR-012: the stop's or the resume's line across `off`, "Interventions on/off" between the others."""
        if new == "off":
            voice = AGENT_SWITCH_VOICES["stop"]
        elif old == "off":
            voice = AGENT_SWITCH_VOICES["resume"] if new == "on" else AGENT_WATCHING_VOICE
        else:
            voice = INTERVENTIONS_VOICES["on" if new == "on" else "off"]
        manager = self.state_manager
        if manager is None:
            return
        try:
            await manager.audio.play_voice(voice)
        except Exception as e:
            logger.warning(f"Announcing agent_state={new} failed: {e}")

    async def show_mode_on_lobby(self, game_mode: str) -> None:
        """UC-015 BR-015: a short pulse of the mode's colour on every CONNECTED or READY controller.

        The controller manager restores each controller's base colour (its dim / bright lobby
        colour) when the pulse ends; IDLE controllers are left alone.
        """
        await self._pulse_lobby(GAME_MODE_COLORS.get(game_mode, DEFAULT_COLOR))

    async def show_shield_mode_on_lobby(self, shield_mode: str) -> None:
        """UC-015 BR-018: the BR-015 pulse in the shield mode's colour; `automatic` a double white flash."""
        if shield_mode == AUTOMATIC:
            await self.show_automatic_on_lobby()
            return
        color = SHIELD_MODE_COLORS.get(shield_mode)
        if color is not None:
            await self._pulse_lobby(color)

    async def show_automatic_on_lobby(self) -> None:
        """UC-015 BR-018: the `automatic` cue of every setting, two 150 ms white flashes 300 ms apart."""
        flash = controller_manager_pb2.GAME_EFFECT_FLASH
        await self._effect_lobby(flash, Colors.White.value, AUTOMATIC_FLASH_MS)
        await asyncio.sleep(AUTOMATIC_FLASH_GAP_SECONDS)
        await self._effect_lobby(flash, Colors.White.value, AUTOMATIC_FLASH_MS)

    async def _pulse_lobby(self, color: tuple[int, int, int]) -> None:
        """One 600 ms pulse of ``color`` on every CONNECTED or READY controller (BR-015, BR-018)."""
        await self._effect_lobby(controller_manager_pb2.GAME_EFFECT_PULSE, color, MODE_PULSE_MS, MODE_PULSE_SPEED)

    async def _effect_lobby(self, effect: int, color: tuple[int, int, int], duration_ms: int, speed: int = 0) -> None:
        """One game effect on every CONNECTED or READY controller (not IDLE)."""
        # Imported here to keep the handlers package out of this module's import time.
        from services.menu.handlers.base import ControllerState

        manager = self.state_manager
        if manager is None:
            return
        for serial, state in list(manager.controller_states.items()):
            if state not in (ControllerState.CONNECTED, ControllerState.READY):
                continue
            try:
                await manager.led.send_game_effect(serial, effect, color=color, duration_ms=duration_ms, speed=speed)
            except Exception as e:
                logger.warning(f"Lobby pulse for {serial} failed: {e}")

    async def play_failure_cue(self) -> None:
        """UC-015 BR-009: two loud beeps for a write that failed. Best effort."""
        manager = self.state_manager
        if manager is None:
            return
        try:
            await manager.audio.play_sound(FAILURE_CUE_SOUND)
            await asyncio.sleep(FAILURE_CUE_GAP_SECONDS)
            await manager.audio.play_sound(FAILURE_CUE_SOUND)
        except Exception as e:
            logger.warning(f"Failure cue failed: {e}")
