"""Settings core shared by the controller admin mode and the operator pad commands (#2050, UC-015).

Admin mode (UC-012, UC-013, UC-014) and the pad's ``ProcessInput`` commands
(services.md §3.1) change the same settings. This module holds what both need
and nothing that is tied to a controller: the force-start roster with ghost
pruning, the clamped setting step, the agent level ladder, the kill switch and
resume of UC-015 BR-002 / BR-003, the backend write, and the voice tables.

``AdminModeHandler`` keeps its LEDs, its admin session span and its wrapping
``cycle_variant`` steps, and delegates the rest here. The pad commands pass no
serial: no LED pulse, no roster entry for the caller, an error code instead of
a log line.

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
from lib.flag_service_writer import STEP_AT_LIMIT
from lib.setting_ladders import (
    AGENT_POLICY_LEVELS,
    AGENT_RESUME_FALLBACK_LEVEL,
    AGENT_SAVED_LEVELS,
    AUTOMATIC,
    AUTOMATIC_OPTIONS,
    BACKENDS,
    KNOB_LADDERS,
    SENSITIVITY_LADDER,
    SHIELD_KNOB_LADDER,
    knob_start,
    step_ladder,
)
from lib.types import Sound
from lib.voice_prompts import (  # re-exported: admin mode imports the voice tables from here
    AGENT_LEVEL_VOICES,
    AGENT_SWITCH_VOICES,
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

# UC-013 BR-001: the only agent key admin mode writes (persisted).
AGENT_POLICY_FLAG = "interventions_allowed"
# UC-015 BR-001: the pad also writes the kill switch and the write gate (in memory only) and the backend (persisted).
AGENT_ENABLED_FLAG = "enabled"
# UC-015 BR-003 (#2093): the agent's write gate (FR-051); only the resume writes it, in memory only.
AGENT_WRITE_GATE_FLAG = "interventions_enabled"
DECISION_BACKEND_FLAG = "decision.backend"
# UC-015 BR-018 (#2215): the shield mode the pad's K6 layer steps; a persisted admin value of the `game` set.
SHIELD_ACTIVATION_FLAG = "shield.activation"

# Ladders (UC-013, UC-015 BR-004): one source with the pad's preview, lib/setting_ladders.py (#2106).
DECISION_BACKENDS = tuple(BACKENDS)

# --- Error codes (UC-015 BR-012, services.md §3.1) ------------------------------------------

GAME_ACTIVE = "game_active"
AT_LIMIT = "at_limit"
FLAG_SERVICE_UNAVAILABLE = "flag_service_unavailable"
PARTIAL_PREFIX = "partial:"
FAILED = "failed"
STALE = "stale"
BAD_OPTION = "bad_option"
# agent_switch reply when every write (or the no-op of UC-015 A9) succeeded.
OK = "ok"


def is_failure(error: str | None) -> bool:
    """A failed write (UC-015 A3): the error codes that get the failure cue (BR-009)."""
    return bool(error) and (error in (FLAG_SERVICE_UNAVAILABLE, FAILED) or error.startswith(PARTIAL_PREFIX))


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


def agent_level_next(current: str | None, forward: bool = True) -> str:
    """The next level along AGENT_POLICY_LEVELS, clamped at both ends (UC-013).

    The ladder never wraps: restrict at ``none`` stays ``none`` (a kill switch that
    escalates to ``full`` when pressed while off is unsafe) and escalate at ``full``
    stays ``full``. A current value outside the ladder (``probe``, UC-013 A1) collapses to
    ``none``, the benign fail-safe. ``None`` (an unreadable level) also maps to ``none``, but
    only the restrict uses it: the callers refuse an escalation from an unreadable level (UC-013 A5).
    """
    return step_ladder("agent_level", AGENT_POLICY_LEVELS, current, 1 if forward else -1)[0]


def _ladder_step(option: str, ladder: list[str], current: str | None, forward: bool, steps: int) -> str:
    """``steps`` moves along ``ladder``, wrapped or clamped per `WRAPPING`; outside it the first move lands on it."""
    new, _at_limit = step_ladder(option, ladder, current, steps if forward else -steps)
    return new


# UC-015 BR-015: the mode pulse on every lobby controller, the pulse admin mode shows on its own
# controller (UC-011 step 3, ``AdminModeHandler.handle_game_mode``).
MODE_PULSE_MS = 600
MODE_PULSE_SPEED = 6
# UC-015 BR-018 (#2215): `automatic` (the operator value cleared) shows as a double white flash, a pattern
# no mode or shield mode uses (they pulse once).
AUTOMATIC_FLASH_MS = 150
AUTOMATIC_FLASH_GAP_SECONDS = 0.3
# The menu re-reads which settings the operator set at most this often (GetMenuStatus, BR-018, #2222).
OPERATOR_REFRESH_SECONDS = 2.0
# UC-012: one pulse colour per sensitivity level (0 Ultra Slow .. 4 Ultra Fast), used by every admin path.
SENSITIVITY_COLORS = [Colors.Blue, Colors.Turquoise, Colors.Green, Colors.Orange, Colors.Red]
# Admin agent-policy pulse: red = off (panic), escalating warmth toward full (#819).
AGENT_LEVEL_COLORS = {
    "none": Colors.Red,
    "ambient": Colors.Turquoise,
    "standard": Colors.Orange,
    "full": Colors.Green,
}
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
}
# UC-015 A1: refused with game_active while the game lifecycle is live.
LOBBY_ONLY_OPTIONS = frozenset(name for name, opt in STEP_OPTIONS.items() if opt.lobby_only)
DIRECTIONS = {"up": True, "down": False}
AGENT_SWITCH_DIRECTIONS = ("stop", "resume")

# --- reset_setting (UC-016 A8, BR-013; services.md §3.1) ---------------------------------------

# A reset of these clears the operator value, exactly as their `automatic` rung.
RESET_AUTOMATIC_OPTIONS = ("sensitivity", "num_teams", SHIELD_ACTIVATION_FLAG)
# option -> (StateManager writer attribute, flag key); the reset puts the layer below the operator's in force.
RESET_DEFAULT_OPTIONS: dict[str, tuple[str, str]] = {
    "game_instructions": ("user_prefs_writer", "game_instructions"),
    "force_all_start": ("game_settings_writer", "force_all_start"),
    "agent_level": ("agent_settings_writer", AGENT_POLICY_FLAG),
    "decision.backend": ("agent_settings_writer", DECISION_BACKEND_FLAG),
}
# Explicit allowlist, not derived from STEP_OPTIONS: the volume, the mode and the agent's other keys never reset.
RESETTABLE_OPTIONS = frozenset(RESET_AUTOMATIC_OPTIONS) | frozenset(RESET_DEFAULT_OPTIONS)
RESET_LOBBY_ONLY_OPTIONS = RESETTABLE_OPTIONS - {"agent_level", "decision.backend"}
INTERVENTIONS_SWITCH_STATES = ("on", "off")
# GetMenuStatus.pad_commands: what a pad may send this menu (an older menu answers unknown commands with success).
PAD_CAPABILITIES = ("reset_setting", "interventions_switch", "agent_switch.only_if_stopped", "step_setting.from")


def _layer_variant(view: dict, layer: str) -> str | None:
    value = view.get(layer)
    return (value.get("variant") or None) if isinstance(value, dict) else None


def _lower_level(saved: str, live: str) -> str:
    """The lower of the saved and the live level by ladder rank; a live level off the ladder caps nothing."""
    if live not in AGENT_POLICY_LEVELS or live == "none":
        return saved
    return min(saved, live, key=AGENT_POLICY_LEVELS.index)


def reset_target(option: str, view: dict | None, enabled: str | None) -> str | None:
    """What a reset of a RESET_DEFAULT_OPTIONS option puts in force (a stopped agent's level: saves); None = unknown.

    The one computation behind both `reset_setting` and `setting_defaults` (decision D2).
    """
    if view is None:
        return None
    if option == "agent_level":
        if enabled is None:
            return None
        if enabled != "on" or view.get("effective_default") == "none":
            baseline = view.get("baseline_default")
            return baseline if baseline in AGENT_SAVED_LEVELS else AGENT_RESUME_FALLBACK_LEVEL
    return (
        _layer_variant(view, "stack_overlay")
        or _layer_variant(view, "real_overlay")
        or (view.get("baseline_default") or None)
    )


@dataclass(frozen=True)
class Result:
    """Outcome of a settings operation: the value before and after, and an error code (or None)."""

    old: str | None
    new: str | None
    error: str | None = None


class SettingsOps:
    """The settings core; one instance per menu, so the saved agent level is menu memory (BR-003)."""

    def __init__(
        self,
        *,
        get_state_manager: Callable[[], StateManager | None],
        controller_channel,
        metrics,
        callbacks=None,
        agent_enabled_writer: FlagServiceWriter | None = None,
    ):
        self._get_state_manager = get_state_manager
        self.controller_channel = controller_channel
        self.metrics = metrics
        # AdminModeCallbacks: get_game_options and select_game_mode for the mode step.
        self.callbacks = callbacks
        # UC-015 BR-001: `enabled` cannot persist (the registry row has no persist), so it has its own writer.
        self.agent_enabled_writer = agent_enabled_writer
        # UC-015 BR-002 / BR-003: the level a resume restores; lost on a menu restart.
        self.saved_agent_level: str | None = None
        # The write gate the stop found (`off` when unreadable); the resume restores it. None = not remembered.
        self.saved_agent_gate: str | None = None
        # Serializes the kill, the resume and the ladder, so a saved level is never read half-written.
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

    def _observe_agent(self, level: str | None, enabled: str | None) -> None:
        """Running (enabled on, a level set) means any remembered stop is stale: forget it, so a later
        resume can never restore a level or gate the agent left since (it falls back to ambient)."""
        if enabled == "on" and level not in (None, "none"):
            self.saved_agent_level = None
            self.saved_agent_gate = None

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

    async def prune_ghosts(self, controllers: list[str], admin_serial: str | None, span) -> list[str]:
        """Drop ghost controllers from ``controllers`` using the controller manager's live roster.

        Queries ``GetConnectedControllers`` (#1153) and removes any serial the backend no
        longer reports (the #551 ghost window). Fail-soft: on any RPC error, or with no
        channel, the list is returned unchanged so a force-start is never blocked. The admin
        serial, when there is one, is kept even if the live roster omits it; the pad has none.
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

        if admin_serial and admin_serial not in pruned:
            pruned.append(admin_serial)
        return pruned

    async def force_start_roster(self, admin_serial: str | None, span) -> list[str]:
        """The roster of a force-start: all connected with force_all_start, else the ready ones.

        UC-012 BR-001: force_all_start is read now, at the force start. The admin serial is
        added to a ready roster and survives ghost pruning; ``None`` (the pad) adds nobody.
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
        if admin_serial and not force_all and admin_serial not in controllers:
            controllers.append(admin_serial)
        return await self.prune_ghosts(controllers, admin_serial, span)

    # --- Agent level reads ---------------------------------------------------------------

    def current_agent_level(self) -> str | None:
        """The agent level the flag service serves (``effective_default``), None when it cannot be read.

        UC-013 BR-003 (#2095): never the in-process flagd value. The flag service applies a
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
        writer = self.agent_enabled_writer or self.state_manager.agent_settings_writer
        try:
            view = writer.get_flag_view(key)
        except Exception as e:
            logger.warning(f"Reading agent/{key} failed: {e}")
            return None
        return view.get("effective_default") if view else None

    def _read_agent_state(self) -> tuple[str | None, str | None]:
        """(level, enabled); either is None when unreadable. Blocking."""
        try:
            level = self.current_agent_level()
        except Exception as e:
            logger.warning(f"Reading agent/{AGENT_POLICY_FLAG} failed: {e}")
            level = None
        return level, self._agent_enabled()

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

    async def step_setting(self, option: str, direction: str, steps: int, from_level: str | None = None) -> Result:
        """Step ``option`` by ``steps`` in ``direction`` with one write, stopping at the ends or wrapping (`WRAPPING`).

        Errors: ``bad_option``, ``at_limit`` (nothing written; clamped options only), ``flag_service_unavailable``,
        ``stale`` (``from_level``, agent_level only, is not the level the step would start from).
        ``ok``: a wrapping option's net steps come back to the start, a no-op (nothing written, no cue).
        The caller checks the lobby-only refusal (``game_active``) first.
        """
        spec = STEP_OPTIONS.get(option)
        if spec is None or direction not in DIRECTIONS or steps < 1:
            return Result(None, None, BAD_OPTION)
        if from_level is not None and option != "agent_level":
            return Result(None, None, BAD_OPTION)
        forward = DIRECTIONS[direction]

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
        """The level ladder of BR-004; while the agent is stopped only the saved level moves (BR-003)."""
        async with self._agent_change():
            level, enabled = await asyncio.to_thread(self._read_agent_state)
            if level is None or enabled is None:
                # Fail closed in both directions: with the switch unreadable it is unknown whether
                # the step moves the live level or the saved one, so nothing is written (BR-010).
                return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
            # BR-002 "stopped": enabled off or level none; only the saved level moves (BR-003).
            if enabled != "on" or level == "none":
                old = self.saved_agent_level or AGENT_RESUME_FALLBACK_LEVEL
                if from_level is not None and from_level != old:
                    return Result(old, None, STALE)
                new = _ladder_step("agent_level", AGENT_SAVED_LEVELS, old, forward, steps)
                if new == old:
                    return Result(old, old, AT_LIMIT)
                self.saved_agent_level = new
                return Result(old, new)

            self._observe_agent(level, enabled)
            if from_level is not None and from_level != level:
                return Result(level, None, STALE)
            new = _ladder_step("agent_level", AGENT_POLICY_LEVELS, level, forward, steps)
            if new == level:
                return Result(level, level, AT_LIMIT)
            writer = self.state_manager.agent_settings_writer
            if not await asyncio.to_thread(self._write, writer, AGENT_POLICY_FLAG, new):
                return Result(level, None, FLAG_SERVICE_UNAVAILABLE)
            if new == "none" and level in AGENT_SAVED_LEVELS:
                # BR-002 / BR-003: a step down to "none" stops the agent like the kill does, so a
                # resume restores the level (and gate) it left.
                self.saved_agent_level = level
                self.saved_agent_gate = await asyncio.to_thread(self._gate_at_stop)
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

    def _read_setting_defaults(self) -> tuple[dict[str, str], tuple[str | None, str | None]]:
        """reset_target of every RESET_DEFAULT_OPTIONS option (an unknown one left out), and the agent's
        (level, enabled) as read for it. Blocking."""
        values = {}
        agent: tuple[str | None, str | None] = (None, None)
        for option in RESET_DEFAULT_OPTIONS:
            enabled = self._agent_enabled() if option == "agent_level" else None
            view = self._default_view(option)
            if option == "agent_level":
                agent = (view.get("effective_default") if view else None, enabled)
            target = reset_target(option, view, enabled)
            if target:
                values[option] = target
        return values, agent

    async def refresh_operator_settings(self) -> None:
        """Re-read the operator's values for GetMenuStatus (BR-018); an unreadable one is left out (unknown)."""
        generation = self._operator_generation
        defaults_generation = self._defaults_generation
        self._operator_read_at = time.monotonic()
        values = await asyncio.to_thread(self._read_all_operators)
        defaults, agent = await asyncio.to_thread(self._read_setting_defaults)
        if generation == self._operator_generation:
            self.operator_settings = values  # else a commit landed during the read: its value stands
        if defaults_generation == self._defaults_generation:
            # Only when no agent command overlapped the read: a stop that landed meanwhile must keep its memory.
            self.setting_defaults = defaults
            self._observe_agent(*agent)

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

    # --- agent_switch (UC-015 step 6, A3, A9, BR-002, BR-003) --------------------------------

    async def agent_switch(self, direction: str, only_if_stopped: bool = False) -> Result:
        """Stop or resume the agent; ``error`` is ``ok``, ``partial:<key>``, ``failed``, ``stale`` or ``bad_option``.

        ``only_if_stopped`` (resume only): refuse with ``stale``, nothing written, unless the agent is fully stopped.
        """
        if direction not in AGENT_SWITCH_DIRECTIONS:
            return Result(None, None, BAD_OPTION)
        async with self._agent_change():
            if direction == "stop":
                return await self._stop_agent()
            return await self._resume_agent(only_if_stopped)

    def _gate_at_stop(self) -> str:
        """The write gate to remember at a stop: an unreadable gate counts as closed. Blocking."""
        gate = self._agent_switch_flag(AGENT_WRITE_GATE_FLAG)
        return gate if gate in ("on", "off") else "off"

    async def _stop_agent(self) -> Result:
        level, enabled = await asyncio.to_thread(self._read_agent_state)
        if enabled == "off" or level == "none":
            # A9: already stopped; the saved level is not touched.
            return Result("stopped", "stopped", OK)
        self._observe_agent(level, enabled)
        if level in AGENT_SAVED_LEVELS:
            self.saved_agent_level = level
        # BR-002 order: the level first (it blocks the agent at the coordinator gate and ends
        # remote decide calls), then the kill switch. Both are attempted whatever the first did.
        level_ok = await asyncio.to_thread(
            self._write, self.state_manager.agent_settings_writer, AGENT_POLICY_FLAG, "none"
        )
        enabled_ok = await asyncio.to_thread(self._write, self.agent_enabled_writer, AGENT_ENABLED_FLAG, "off")
        # Read after the kill writes so the extra read never delays them; the stop does not write the gate.
        self.saved_agent_gate = await asyncio.to_thread(self._gate_at_stop)
        return Result(
            "running", "stopped", _writes_result([(AGENT_POLICY_FLAG, level_ok), (AGENT_ENABLED_FLAG, enabled_ok)])
        )

    async def _resume_agent(self, only_if_stopped: bool = False) -> Result:
        level, enabled = await asyncio.to_thread(self._read_agent_state)
        if enabled is None or (only_if_stopped and level is None):
            return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
        if level is None:
            # The resume is the permissive direction: never enable an agent whose level is unknown.
            return Result(None, None, FAILED)
        if enabled == "on" and level != "none":
            self._observe_agent(level, enabled)
            if only_if_stopped:
                return Result("running", "running", STALE)
            # A9: already running only while the write gate is open too (BR-002, #2093); a closed or
            # unreadable gate is opened (an unguarded caller of a not fully stopped agent, as before #2747).
            gate = await asyncio.to_thread(self._agent_switch_flag, AGENT_WRITE_GATE_FLAG)
            if gate == "on":
                return Result("running", "running", OK)
            return await self._resume_writes(None, "on")
        # As at the stop, never more: the saved level (also over a level raised meanwhile) and the remembered
        # gate (left as it stands when nothing is remembered, after a menu restart), each capped by what is
        # live now, so a memory the agent has left since can only lower it.
        target = _lower_level(self.saved_agent_level or AGENT_RESUME_FALLBACK_LEVEL, level)
        gate = self.saved_agent_gate
        if gate == "on" and await asyncio.to_thread(self._agent_switch_flag, AGENT_WRITE_GATE_FLAG) != "on":
            gate = "off"
        return await self._resume_writes(target, gate)

    async def _resume_writes(self, level: str | None, gate: str | None) -> Result:
        level_ok = gate_ok = None
        # BR-003 order: the level, then the write gate, then enabled last, so enabled is never switched
        # on before the gate. Every write is attempted whatever an earlier one did (A3).
        if level is not None:
            level_ok = await asyncio.to_thread(
                self._write, self.state_manager.agent_settings_writer, AGENT_POLICY_FLAG, level
            )
        if gate is not None:
            gate_ok = await asyncio.to_thread(self._write, self.agent_enabled_writer, AGENT_WRITE_GATE_FLAG, gate)
        enabled_ok = await asyncio.to_thread(self._write, self.agent_enabled_writer, AGENT_ENABLED_FLAG, "on")
        result = _writes_result(
            [(AGENT_POLICY_FLAG, level_ok), (AGENT_WRITE_GATE_FLAG, gate_ok), (AGENT_ENABLED_FLAG, enabled_ok)]
        )
        if result == OK:
            self.saved_agent_level = None
            self.saved_agent_gate = None
        return Result("stopped", "running", result)

    # --- interventions_switch (UC-016 BR-012, BR-014) -----------------------------------------

    async def interventions_switch(self, state: str) -> Result:
        """Write the write gate in memory; never ``enabled`` or the level. ``stale`` for a fully stopped agent."""
        if state not in INTERVENTIONS_SWITCH_STATES:
            return Result(None, None, BAD_OPTION)
        async with self._agent_change():
            level, enabled = await asyncio.to_thread(self._read_agent_state)
            if level is None or enabled is None:
                return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
            if enabled != "on" or level == "none":
                # Its resume restores the gate of the stop; a switch now would be overridden.
                return Result(None, None, STALE)
            self._observe_agent(level, enabled)
            if not await asyncio.to_thread(self._write, self.agent_enabled_writer, AGENT_WRITE_GATE_FLAG, state):
                return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
            return Result(None, state)

    # --- reset_setting (UC-016 A8, BR-013) ----------------------------------------------------

    async def reset_setting(self, option: str, expect: str | None) -> Result:
        """Clear the operator's persisted value so the layer below applies; ``new`` is what is now in force.

        ``ok``: nothing to clear (no operator value, or one masked by a stack overlay), nothing written.
        ``stale``: ``expect`` differs from the recomputed target. The caller checks ``game_active`` first.
        """
        if option not in RESETTABLE_OPTIONS:
            return Result(None, None, BAD_OPTION)
        if option in RESET_AUTOMATIC_OPTIONS:
            return await self._reset_automatic(option, expect)
        if option == "agent_level":
            if not expect:
                return Result(None, None, BAD_OPTION)
            async with self._agent_change():
                return await self._reset_agent_level(expect)
        view = await asyncio.to_thread(self._default_view, option)
        target = reset_target(option, view, None)
        if target is None:
            return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
        if expect is not None and expect != target:
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

    async def _reset_agent_level(self, expect: str) -> Result:
        view = await asyncio.to_thread(self._default_view, "agent_level")
        enabled = await asyncio.to_thread(self._agent_enabled)
        target = reset_target("agent_level", view, enabled)
        if target is None:
            return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
        if target not in AGENT_POLICY_LEVELS:
            return Result(None, None, BAD_OPTION)
        if expect != target:
            return Result(None, None, STALE)
        level = view.get("effective_default")
        if enabled != "on" or level == "none":
            # Stopped: only the saved level moves, so a reset never resumes (UC-015 BR-003).
            old = self.saved_agent_level or AGENT_RESUME_FALLBACK_LEVEL
            if old == target:
                return Result(old, target, OK)
            self.saved_agent_level = target
            return Result(old, target)
        self._observe_agent(level, enabled)
        result = await self._clear_admin("agent_level", view, target)
        if result.error is None and target == "none" and level in AGENT_SAVED_LEVELS:
            # As a ladder step down to none: the agent is now stopped, so a resume restores what it left.
            self.saved_agent_level = level
            self.saved_agent_gate = await asyncio.to_thread(self._gate_at_stop)
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

    async def set_backend(self, backend: str) -> Result:
        """Write ``decision.backend`` (persisted); ``bad_option`` or ``flag_service_unavailable`` on error."""
        if backend not in DECISION_BACKENDS:
            return Result(None, None, BAD_OPTION)
        writer = self.state_manager.agent_settings_writer
        if not await asyncio.to_thread(self._write, writer, DECISION_BACKEND_FLAG, backend):
            return Result(None, None, FLAG_SERVICE_UNAVAILABLE)
        # A persisted write drops a stack overlay, which moves the reset target.
        self._defaults_changed(DECISION_BACKEND_FLAG)
        return Result(None, backend)

    # --- Feedback ---------------------------------------------------------------------------

    async def announce(self, option: str, variant: str | None, quiet: bool = False) -> None:  # noqa: C901 - one branch per option voice; split later
        """The voice line for a new value (UC-012 voices, UC-015 step 4, 5, 8). Best effort.

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
            elif option == "interventions_switch":
                voice = INTERVENTIONS_VOICES.get(variant)  # UC-016 BR-012, UC-031 BR-007
                if voice is not None:
                    await audio.play_voice(voice)
            elif option == "agent_switch":
                voice = AGENT_SWITCH_VOICES.get(variant)
                if voice is not None:
                    await audio.play_voice(voice)
        except Exception as e:
            logger.warning(f"Announcing {option}={variant} failed: {e}")

    async def show_mode_on_lobby(self, game_mode: str) -> None:
        """UC-015 BR-015: a short pulse of the mode's colour on every CONNECTED or READY controller.

        The controller manager restores each controller's base colour (its dim / bright lobby
        colour) when the pulse ends; the admin controller and IDLE controllers are left alone.
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
        """One game effect on every CONNECTED or READY controller (not admin, not IDLE)."""
        # Imported here: services.menu.handlers imports this module (admin mode).
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


def _writes_result(writes: list[tuple[str, bool | None]]) -> str:
    """UC-015 A3 result of ordered writes (None = not attempted).

    ``ok`` when every attempted write succeeded, ``failed`` when none did, else
    ``partial:<key>`` naming the first key in write order that failed.
    """
    attempted = [(key, ok) for key, ok in writes if ok is not None]
    failed = [key for key, ok in attempted if not ok]
    if not failed:
        return OK
    if len(failed) == len(attempted):
        return FAILED
    return PARTIAL_PREFIX + failed[0]
