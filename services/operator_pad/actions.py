"""Gestures to menu and coordinator commands (UC-015; the page keys of UC-016 BR-011 to BR-016).

The pad writes no flag itself (UC-015 BR-001): every setting goes to the menu as a `ProcessInput`
`web_command` (services.md §3.1), force-end goes to the game coordinator (UC-027). A command that
cannot be sent now is dropped with a cue, never queued or retried (BR-010).

A row key acts on the row it bound to at its press (`page_keys.Binding`); without one (no screen)
on the fallback page `HOME` (UC-016 BR-016). A raise of the agent (BR-014) is sent only through
`_level_step`, `_row_step`, `_set_backend`, `_reset_send` and `_interventions_switch`, which take
the confirmation and refuse an unconfirmed raise with an explicit check.
"""

from __future__ import annotations

import asyncio
import contextlib
import contextvars
import logging
from collections.abc import Awaitable, Callable

from opentelemetry import trace

from lib import setting_ladders, voice_prompts
from lib.setting_ladders import PAID_BACKENDS
from lib.telemetry import get_tracer
from proto import game_coordinator_pb2, menu_pb2
from services.operator_pad import cues, page_keys
from services.operator_pad.config import K5, K8
from services.operator_pad.gestures import (
    AGENT_HOLD,
    CANCEL,
    CONFIRM,
    HOLD,
    KNOB_PRESS_GESTURE,
    KNOB_TURN,
    LAYER,
    LAYER_STEP,
    LOCK,
    RESET,
    TAP,
    TRANSITION,
    Gesture,
)
from services.operator_pad.page_keys import (
    BACKEND,
    EDIT,
    INTERVENTIONS,
    RESET_BACKEND_FROM,
    SET_BACKEND_FROM,
    SWITCH,
    Binding,
)
from services.operator_pad.preview import Previewer, value_voice
from services.operator_pad.screens import SETTING_LABELS, TRANSITION_TEXT, agent_text, result_word, short
from services.operator_pad.state import AGENT_OFF, AGENT_UNKNOWN, AGENT_WATCH, agent_state, is_known, pad_commands

logger = logging.getLogger(__name__)
tracer = get_tracer(__name__)

# Every command but the agent switch: at most two flag reads and one write settled by a re-read (#2095).
COMMAND_TIMEOUT_SECONDS = 4.0
# The resume is the menu's slowest command (#2093): from level `none` two flag reads (0.5 s each) and three
# writes whose timeout is settled by a re-read (0.5 s + 0.5 s each, #2095), 4.0 s of flag-service deadlines,
# plus the menu's own work. A cut still leaves the agent stopped: `enabled` is written last (UC-015 BR-003).
AGENT_SWITCH_TIMEOUT_SECONDS = 5.0
VOLUME_PAUSE_SECONDS = 0.25  # BR-005: the volume is committed after this pause in turning
FORCE_END_REASON = "operator_pad"

# Menu error codes (services.md §3.1, UC-015 BR-012).
OK = "ok"
GAME_ACTIVE = "game_active"
AT_LIMIT = "at_limit"
STALE = "stale"
BAD_OPTION = "bad_option"
FLAG_SERVICE_UNAVAILABLE = "flag_service_unavailable"
FAILED = "failed"
PARTIAL_PREFIX = "partial:"
# Pad-side results (telemetry.md §6).
REFUSED = "refused"
CANCELLED = "cancelled"
MENU_UNAVAILABLE = "menu_unavailable"
NOOP = "noop"

# UC-016 BR-011 "Menu too old": the commands a menu must list in `pad_commands` before the pad sends them.
RESET_COMMAND = "reset_setting"
SWITCH_COMMAND = "interventions_switch"
RESET_FROM = "reset_setting.from"  # the menu refuses a level reset whose level moved under the hold
STEP_FROM = "step_setting.from"  # the same for a level step
# The edits whose screen and release follow a `page_keys.Plan` (NOW, NEW, raise, refusal).
PLANNED_EDITS = frozenset({"agent_level", INTERVENTIONS, "decision.backend", *setting_ladders.AGENT_ROW_OPTIONS})

VOLUME_VARIANTS = setting_ladders.VOLUME_LADDER
MUTE = "mute"
UNMUTE_DEFAULT = "max"
VOLUME_OPTION = "audio_volume.master"

# UC-016 BR-001 result labels of the commands that are not a row setting.
COMMAND_LABELS = {"start_game": "START", "force_end": "END GAME", "force_start": "FORCE START", "agent_switch": "AGENT"}
# The last command sent in this task and the value the menu reports as committed (UC-016 BR-001).
# A context variable, so a volume commit running in its own task never mixes into a key's result.
_SENT: contextvars.ContextVar[tuple[dict[str, str], str] | None] = contextvars.ContextVar("pad_sent", default=None)


def result_label(error: str) -> str:
    """A menu error code as a metric label: `partial:<key>` folds to `partial` (bounded cardinality)."""
    if error in ("", OK):
        return OK
    if error.startswith(PARTIAL_PREFIX):
        return "partial"
    return error


def row_binding(key: str, binding: Binding | None) -> Binding | None:
    """The row of a row key: the one bound at its press, else the fallback page (UC-016 BR-016)."""
    if binding is not None:
        return binding
    return page_keys.binding(key, page_keys.FALLBACK_PAGE, fallback=True)


def voice_of(option: str, value: str | None):
    """The voice line of a row value; the menu's tables (lib/voice_prompts.py)."""
    if value is None:
        return None
    if option == "game_instructions":
        return voice_prompts.INSTRUCTIONS_VOICES.get(value == "on") if value in ("on", "off") else None
    if option == "force_all_start":
        return voice_prompts.FORCE_ALL_START_VOICES.get(value == "on") if value in ("on", "off") else None
    if option == INTERVENTIONS:
        return voice_prompts.INTERVENTIONS_VOICES.get(value)
    return value_voice(option, value)


def row_value(option: str, status) -> str | None:
    """The value a row shows (UC-016 BR-006): `automatic` while the operator left it, else the value in force."""
    if option in setting_ladders.AUTOMATIC_OPTIONS:
        if status.operator_settings.get(option) == setting_ladders.AUTOMATIC:
            return setting_ladders.AUTOMATIC
        if option == setting_ladders.SHIELD_OPTION:
            return status.shield_activation or None
        return setting_ladders._flag_variant(option, status)
    return page_keys.row_preview(option, status, 0).value


def label_of(option: str) -> str:
    return SETTING_LABELS.get(option, option.upper())


class Actions:
    def __init__(
        self,
        menu_stub,
        coordinator_stub,
        cue_player: cues.CuePlayer,
        sleep: Callable[[float], Awaitable[None]] = asyncio.sleep,
        set_lock_led: Callable[[bool], None] | None = None,
        previewer: Previewer | None = None,
        display=None,
    ) -> None:
        self._menu = menu_stub
        self._coordinator = coordinator_stub
        self._cues = cue_player
        self._sleep = sleep
        self._set_lock_led = set_lock_led
        self._previewer = previewer
        self._volume_steps = 0
        self._volume_task: asyncio.Task | None = None  # waiting out the pause; cancelled by a new turn
        self._volume_sends: set[asyncio.Task] = set()  # past the pause: never cancelled mid-command
        self._pre_mute_variant: str | None = None
        self._display = display  # UC-016 DisplayModel, or None (no display)
        self._tap_screen: tuple[str, str] | None = None  # (label, text) a tap showed (UC-016 BR-007)
        self._agent_stopped = False  # UC-016 A10: this K5 hold already sent its stop at the hold time
        self._awaiting: dict[str, bool] = {}  # key -> its raise waits for the knob press (the attention cue)

    # --- entry points ---------------------------------------------------------------------

    async def dispatch(
        self,
        gesture: Gesture,
        status: menu_pb2.GetMenuStatusResponse | None,
        press_status: menu_pb2.GetMenuStatusResponse | None = None,
        binding: Binding | None = None,
    ) -> tuple[str, str]:
        """Run one gesture; (action, result). ``status`` is read at the event, ``press_status`` at the press."""
        if gesture.kind == KNOB_TURN:
            self._queue_volume_step(gesture.steps)
            return "step_setting", "pending"
        binding = row_binding(gesture.key, binding)
        if gesture.kind == LAYER_STEP:
            # BR-017: a preview only, from the status read at the press; nothing is sent (BR-005).
            plan = self._preview_step(gesture, press_status, binding)
            await self._note_awaiting(gesture.key, plan is not None and plan.raises and not plan.refused)
            return "preview", "pending"
        if gesture.kind == CONFIRM:
            self._show("confirm_state", gesture.key, True)  # UC-016 A9: the screen only
            if self._awaiting.get(gesture.key):
                self._awaiting[gesture.key] = False
                await self._cues.confirmed()
            return "confirm", "pending"
        self._awaiting.pop(gesture.key, None)
        preview_refused = False
        if gesture.kind == LAYER:
            preview_refused = self._previewer is not None and self._previewer.refused(gesture.key)
            # The preview never plays on over the commit's voice line.
            self.end_preview(commit_steps=self._release_commit_steps(gesture, status, binding))
        if gesture.kind == AGENT_HOLD and not self._agent_hold_stops(gesture, press_status):
            self._agent_stopped = False
            return "none", NOOP  # a clean hold of a stopped agent: the resume waits for the release
        pending = self._pending_label(gesture, status, binding)
        if pending is not None:
            self._show("pending", pending)  # the label while the command runs (up to its deadline)
        token = _SENT.set(None)
        self._tap_screen = None
        try:
            with tracer.start_as_current_span("operator_pad.gesture") as span:
                span.set_attribute("pad.key", gesture.key)
                span.set_attribute("pad.gesture", gesture.kind)
                action, result = await self._run(gesture, status, press_status, binding, span, preview_refused)
                self._record(span, action, result)
            self._show_result(gesture, press_status, binding, action, result, _SENT.get())
            return action, result
        finally:
            _SENT.reset(token)

    def end_preview(self, unless_key: str | None = None, commit_steps: int = 0) -> None:
        """End the live preview (BR-017) unless ``unless_key`` is the key whose hold it previews.

        ``commit_steps``: the net steps the release goes on to send (0: no commit follows).
        """
        if self._previewer is not None and (unless_key is None or unless_key != self._previewer.hold_key):
            self._previewer.end_hold(commit_steps)

    def begin_agent_hold(self, gesture: Gesture, press_status) -> None:
        """K5 reached its hold time: settle at once whether this hold stops, before its release is handled."""
        self._agent_stopped = self._agent_hold_stops(gesture, press_status)

    def push_lock_led(self, locked: bool) -> None:
        """UC-016 BR-011: the Scroll Lock LED shows the pad's lock state (at every attach)."""
        if self._set_lock_led is not None:
            try:
                self._set_lock_led(locked)
            except Exception as e:
                logger.warning(f"Scroll Lock LED failed: {e}")

    @staticmethod
    def _release_commit_steps(gesture: Gesture, status, binding: Binding | None) -> int:
        """The net steps an edit release sends to the menu; 0 when it sends nothing (BR-005, A1)."""
        if binding is None or not binding.active or binding.role != EDIT or status is None:
            return 0
        if binding.spec.lobby_only and status.lifecycle_live:
            return 0
        return gesture.steps

    async def _note_awaiting(self, key: str, awaiting: bool) -> None:
        """UC-016 BR-014: one attention cue when the held key's change becomes a raise waiting for the knob."""
        was, self._awaiting[key] = self._awaiting.get(key, False), awaiting
        if awaiting and not was:
            await self._cues.attention()

    async def reset_stage(self, key: str, binding: Binding | None, press_status) -> None:
        """A reset key reached its reset stage (the controller's timer): the attention cue for a raise."""
        if binding is None or not binding.active or binding.role == EDIT:
            return
        plan = page_keys.reset_plan(binding.spec, press_status)
        await self._note_awaiting(key, plan.raises and not plan.refused)

    def _preview_step(self, gesture: Gesture, press_status, binding: Binding | None):
        if binding is None or not binding.active or binding.role != EDIT:
            return None
        spec = binding.spec
        plan = None
        if spec.option in PLANNED_EDITS:
            plan = page_keys.edit_plan(spec, press_status, gesture.steps)
        self._show("confirm_state", gesture.key, False)  # A9: a later step clears the confirmation
        # UC-016 step 4: the setting screen, whatever `operator_pad.preview_enabled` says.
        self._show("layer_step", gesture.key, spec.option, press_status, gesture.steps, spec.lobby_only, plan)
        if self._previewer is not None and spec.option not in (INTERVENTIONS, VOLUME_OPTION):  # no value voice (D6)
            self._previewer.step(gesture.key, spec.option, press_status, gesture.steps, spec.lobby_only)
        return plan

    def cancel_pending(self) -> None:
        """Device gone: a volume turn not yet committed is dropped, not sent later (BR-010)."""
        self.end_preview()
        self._volume_steps = 0
        self._show("volume_dropped")
        if self._volume_task is not None:
            self._volume_task.cancel()
            self._volume_task = None

    async def drain(self) -> None:
        """Wait for a pending volume commit (tests, shutdown)."""
        tasks = [t for t in (self._volume_task, *self._volume_sends) if t is not None]
        for task in tasks:
            with contextlib.suppress(asyncio.CancelledError):
                await task

    # --- dispatch -------------------------------------------------------------------------

    async def _run(self, gesture: Gesture, status, press_status, binding, span, preview_refused) -> tuple[str, str]:
        kind, key = gesture.kind, gesture.key
        if kind == CANCEL:
            if binding is not None and not binding.active and not gesture.stage:
                return "none", NOOP  # UC-016 BR-011: a row without that key is silent
            await self._cues.cancel()
            return "cancel", CANCELLED
        if kind == TRANSITION:
            await self._cues.unknown_key()  # UC-016 BR-011: "the agent is K5", not the cancel cue
            return "transition", REFUSED
        if kind == LOCK:
            self.push_lock_led(gesture.locked)
            await self._cues.lock(gesture.locked)
            return ("lock" if gesture.locked else "unlock"), OK
        if kind == KNOB_PRESS_GESTURE:
            return await self._mute_toggle(status)
        if kind == TAP:
            return await self._tap(key, status, binding)
        if kind == AGENT_HOLD:
            self._agent_stopped = True
            return "agent_switch", await self._agent_send("stop")
        if kind == HOLD:
            if key == K5:
                return await self._agent_release(gesture, status)
            if key == K8:
                return await self._end_or_start(status, press_status)
            return "none", NOOP
        if kind == LAYER:
            span.set_attribute("pad.steps", gesture.steps)
            return await self._edit(gesture, status, press_status, binding, preview_refused)
        if kind == RESET:
            return await self._reset(gesture, status, press_status, binding)
        return "none", NOOP

    # --- taps -----------------------------------------------------------------------------

    async def _tap(self, key: str, status, binding: Binding | None) -> tuple[str, str]:
        if key == K8:
            return await self._start_game(status)
        if key == K5:
            return await self._agent_tap(status)
        if binding is None or not binding.active:
            return "none", NOOP
        if status is None:
            await self._cues.failure()
            return "announce", MENU_UNAVAILABLE
        option = binding.spec.option
        if binding.role == EDIT:
            if option == INTERVENTIONS:
                return await self._agent_tap(status)
            value = row_value(option, status)
            self._tap_screen = (label_of(option), short(option, value))
        else:
            value = page_keys.default_of(option, status)
            self._tap_screen = (label_of(option), f"DEFAULT {short(option, value)}")
        await self._announce(option, value)
        return "announce", OK

    async def _announce(self, option: str, value: str | None) -> None:
        """Without a screen a tap says the label, then the value (UC-015 BR-011); the volume its label only (D6)."""
        label = voice_prompts.LABEL_VOICES.get(option)
        voice = voice_of(option, value)
        if option == VOLUME_OPTION and label is not None:
            await self._cues.announce(label, None)
            return
        if voice is None:
            if value is None or option not in setting_ladders.AGENT_ROW_OPTIONS:
                await self._cues.refusal()
            return  # D7: the agent rows have no voice lines; a known value is the screen's alone
        await self._cues.announce(label, voice)  # in the background (BR-009)

    async def _agent_tap(self, status) -> tuple[str, str]:
        """K5 tap (UC-016 BR-011): `ON` / `WATCH` and the level, `OFF`; the refusal cue while unknown."""
        if status is None:
            await self._cues.failure()
            return "announce", MENU_UNAVAILABLE
        self._tap_screen = ("AGENT", agent_text(status))
        state = agent_state(status)
        if state == AGENT_UNKNOWN:
            voice = None
        elif state == AGENT_OFF:
            voice = cues.AGENT_STOPPED_VOICE
        elif state == AGENT_WATCH:
            voice = voice_prompts.AGENT_WATCHING_VOICE
        else:
            voice = cues.AGENT_LEVEL_VOICES.get(status.agent_level)
        if voice is None:
            await self._cues.refusal()
        else:
            await self._cues.announce(cues.LABEL_VOICES["agent_level"], voice)
        return "announce", OK

    async def _start_game(self, status) -> tuple[str, str]:
        if status is None:
            await self._cues.failure()
            return "start_game", MENU_UNAVAILABLE
        if status.lifecycle_live:
            await self._cues.refusal()  # A1: the pad refuses itself; the menu's A8 guard is the backstop
            return "start_game", REFUSED
        return "start_game", await self._send({"command": "start_game"})

    # --- K5: the agent (UC-016 A10, "always stop") ------------------------------------------

    @staticmethod
    def _agent_hold_stops(gesture: Gesture, press_status) -> bool:
        """At the hold time: stop unless a clean hold of an agent read fully stopped at the press."""
        if not gesture.clean or gesture.locked:
            return True
        return not (is_known(press_status) and agent_state(press_status) == AGENT_OFF)

    async def _agent_release(self, gesture: Gesture, status) -> tuple[str, str]:
        """K5 released after its hold time; the stop at the hold time ends the gesture."""
        if self._agent_stopped:
            self._agent_stopped = False
            return "none", NOOP
        if not gesture.clean or gesture.locked:
            return "agent_switch", await self._agent_send("stop")
        if not is_known(status):
            return "agent_switch", await self._agent_send("stop")  # UC-015 A10 step 2
        if agent_state(status) != AGENT_OFF:
            await self._cues.cancel()  # UC-015 A10: no longer fully stopped; a resume intent only
            return "agent_switch", CANCELLED
        return "agent_switch", await self._agent_send("resume")

    async def _agent_send(self, direction: str) -> str:
        data = {"command": "agent_switch", "direction": direction}
        if direction == "resume":
            data["only_if_stopped"] = "1"  # the menu refuses (`stale`) an agent not fully stopped
        return await self._send(data, deadline_seconds=AGENT_SWITCH_TIMEOUT_SECONDS)

    # --- K8: end / force-start (UC-015 step 7, A5) -----------------------------------------

    async def _end_or_start(self, status, press_status) -> tuple[str, str]:
        """K8 hold: cancel when the state changed between press and release."""
        if status is None or press_status is None:
            await self._cues.failure()
            return "force_end", MENU_UNAVAILABLE
        if (
            status.lifecycle_generation != press_status.lifecycle_generation
            or status.lifecycle_live != press_status.lifecycle_live
        ):
            await self._cues.cancel()
            return ("force_end" if press_status.lifecycle_live else "force_start"), CANCELLED
        if status.lifecycle_live:
            return "force_end", await self._force_end()
        # The generation captured at the press, not the one read at release: the menu judges the whole hold.
        return "force_start", await self._send(
            {"command": "force_start", "expected_generation": str(press_status.lifecycle_generation)}
        )

    # --- edit and reset (UC-016 A7 to A9, BR-012 to BR-014) ---------------------------------

    async def _refuse(self, action: str) -> tuple[str, str]:
        await self._cues.refusal()
        return action, REFUSED

    async def _edit(self, gesture: Gesture, status, press_status, binding, preview_refused) -> tuple[str, str]:
        """Edit release (steps 4, 5, BR-004, BR-005): one write for the net turns, none for zero."""
        if binding is None or not binding.active or binding.role != EDIT:
            return "none", NOOP
        spec, steps = binding.spec, gesture.steps
        action = {BACKEND: "set_backend", SWITCH: SWITCH_COMMAND}.get(spec.edit, "step_setting")
        if steps == 0:
            return action, NOOP
        if status is None:
            await self._cues.failure()
            return action, MENU_UNAVAILABLE
        if spec.lobby_only and status.lifecycle_live:
            if not preview_refused:  # BR-017: one "Not possible" per hold
                await self._cues.refusal()
            return action, REFUSED
        if spec.option in PLANNED_EDITS and press_status is None:
            return await self._refuse(action)  # BR-014: judged from the press read, never confirmed blind
        if spec.edit == BACKEND:
            return await self._backend_edit(action, spec, steps, status, press_status, gesture.confirmed)
        if spec.option in setting_ladders.AGENT_ROW_OPTIONS:
            return await self._row_edit(action, spec, steps, status, press_status, gesture.confirmed)
        if spec.edit == SWITCH:
            if SWITCH_COMMAND not in pad_commands(status):
                return await self._refuse(action)  # BR-011 "Menu too old"
            plan = page_keys.edit_plan(spec, press_status, steps)
            if plan.refused:
                return await self._refuse(action)
            if plan.new is None or plan.new == plan.now:
                return action, NOOP
            return action, await self._interventions_switch(plan.new, gesture.confirmed)
        option = spec.option
        if option == "agent_level":
            plan = page_keys.edit_plan(spec, press_status, steps)
            if plan.refused or STEP_FROM not in pad_commands(status):
                return await self._refuse(action)
            if plan.new == plan.now:
                await self._cues.limit(steps > 0)  # clamped at the end: nothing to send
                return action, AT_LIMIT
            return action, await self._level_step(steps, plan.now, plan.raises, gesture.confirmed)
        if option in setting_ladders.WRAPPING:
            # UC-016 BR-007: a whole turn of the wheel sends nothing; the menu's `ok` no-op stays the
            # backstop for a start the pad cannot read.
            start = setting_ladders.preview_value(option, status, 0).value
            if start is not None and setting_ladders.preview_value(option, status, steps).value == start:
                return action, NOOP
        return action, await self._step(option, "up" if steps > 0 else "down", abs(steps))

    async def _unconfirmed(self) -> str:
        """BR-014: a raise released without its confirmation sends nothing."""
        await self._cues.cancel()
        return CANCELLED

    async def _level_step(self, steps: int, from_level: str, raises: bool, confirmed: bool) -> str:
        if raises and not confirmed:
            return await self._unconfirmed()
        data = {
            "command": "step_setting",
            "option": "agent_level",
            "direction": "up" if steps > 0 else "down",
            "steps": str(abs(steps)),
            "from": from_level,
        }
        return await self._send(data)

    async def _interventions_switch(self, state: str, confirmed: bool) -> str:
        """The only builder of `interventions_switch`: `on` opens the write gate, a raise (BR-014)."""
        if state == "on" and not confirmed:
            return await self._unconfirmed()
        return await self._send({"command": SWITCH_COMMAND, "state": state}, expect_value=True)

    async def _reset(self, gesture: Gesture, status, press_status, binding) -> tuple[str, str]:
        """Reset release (A8, BR-013): one `reset_setting` with `expect`, the NEW the screen showed."""
        if binding is None or not binding.active or binding.role == EDIT:
            return "none", NOOP
        spec = binding.spec
        if status is None:
            await self._cues.failure()
            return RESET_COMMAND, MENU_UNAVAILABLE
        if RESET_COMMAND not in pad_commands(status):
            return await self._refuse(RESET_COMMAND)  # BR-011 "Menu too old"
        if spec.lobby_only and status.lifecycle_live:
            return await self._refuse(RESET_COMMAND)
        level = spec.option == "agent_level"
        row = spec.option in setting_ladders.AGENT_ROW_OPTIONS
        if level and RESET_FROM not in pad_commands(status):
            return await self._refuse(RESET_COMMAND)  # BR-014: the base must be checked
        if row and page_keys.reset_command(spec.option) not in pad_commands(status):
            return await self._refuse(RESET_COMMAND)
        if (level or row or spec.edit == BACKEND) and press_status is None:
            return await self._refuse(RESET_COMMAND)  # BR-014: the raise is judged from the press read
        plan = page_keys.reset_plan(spec, press_status if press_status is not None else status)
        if plan.refused:
            return await self._refuse(RESET_COMMAND)
        base = plan.now if level or row else None
        if spec.edit == BACKEND:
            guarded = RESET_BACKEND_FROM in pad_commands(status)
            if plan.new in PAID_BACKENDS and not guarded:
                return await self._refuse(RESET_COMMAND)  # a backend moved under the hold would go unseen
            base = plan.now if guarded else None
        return RESET_COMMAND, await self._reset_send(spec.option, plan.new, base, plan.raises, gesture.confirmed)

    async def _reset_send(self, option: str, expect: str, from_value: str | None, raises: bool, confirmed: bool) -> str:
        if raises and not confirmed:
            return await self._unconfirmed()
        data = {"command": RESET_COMMAND, "option": option, "expect": expect}
        if from_value is not None:
            data["from"] = from_value
        return await self._send(data, expect_value=True)

    async def _backend_edit(self, action, spec, steps: int, status, press_status, confirmed: bool) -> tuple[str, str]:
        # The press read, never the release's: a backend changed under the hold must not turn a
        # paid-to-paid switch into an unconfirmed free-to-paid one (the menu's `from` catches it).
        plan = page_keys.edit_plan(spec, press_status, steps)
        if plan.refused:
            return await self._refuse(action)
        if plan.new == plan.now:
            return action, NOOP  # a full turn of the wheel: nothing sent, no cue
        guarded = SET_BACKEND_FROM in pad_commands(status)
        if plan.new in PAID_BACKENDS and not guarded:
            return await self._refuse(action)
        return action, await self._set_backend(plan.new, plan.now if guarded else None, plan.raises, confirmed)

    async def _set_backend(self, backend: str, from_backend: str | None, raises: bool, confirmed: bool) -> str:
        if raises and not confirmed:
            return await self._unconfirmed()
        data = {"command": "set_backend", "backend": backend}
        if from_backend is not None:
            data["from"] = from_backend
        return await self._send(data)

    async def _row_edit(self, action, spec, steps: int, status, press_status, confirmed: bool) -> tuple[str, str]:
        """BR-012 iteration 4: one `step_setting` with `from`, enabled per option by `pad_commands`."""
        option = spec.option
        if page_keys.step_command(option) not in pad_commands(status):
            return await self._refuse(action)
        plan = page_keys.edit_plan(spec, press_status, steps)
        if plan.refused:
            return await self._refuse(action)
        if plan.new == plan.now:
            if option in setting_ladders.WRAPPING:
                return action, NOOP
            await self._cues.limit(steps > 0)
            return action, AT_LIMIT
        return action, await self._row_step(option, steps, plan.now, plan.raises, confirmed)

    async def _row_step(self, option: str, steps: int, from_value: str, raises: bool, confirmed: bool) -> str:
        if raises and not confirmed:
            return await self._unconfirmed()
        data = {
            "command": "step_setting",
            "option": option,
            "direction": "up" if steps > 0 else "down",
            "steps": str(abs(steps)),
            "from": from_value,
        }
        return await self._send(data, expect_value=True)

    # --- volume and mute (step 8, BR-005, BR-006) ----------------------------------------

    def _queue_volume_step(self, step: int) -> None:
        self._volume_steps += step
        self._show("volume_turn", self._volume_steps)  # UC-016 step 6
        if self._volume_task is not None:
            self._volume_task.cancel()
        self._volume_task = asyncio.create_task(self._commit_volume_after_pause())

    async def _commit_volume_after_pause(self) -> None:
        await self._sleep(VOLUME_PAUSE_SECONDS)
        steps, self._volume_steps = self._volume_steps, 0
        me = asyncio.current_task()
        if self._volume_task is me:
            self._volume_task = None
        if me is not None:
            self._volume_sends.add(me)
            me.add_done_callback(self._volume_sends.discard)
        with tracer.start_as_current_span("operator_pad.gesture") as span:
            span.set_attribute("pad.key", "KNOB")
            span.set_attribute("pad.gesture", KNOB_TURN)
            span.set_attribute("pad.steps", steps)
            if steps == 0:
                result = NOOP
            else:
                ticket = self._volume_display_start(("steps", steps))
                _SENT.set(None)
                result = await self._step(VOLUME_OPTION, "up" if steps > 0 else "down", abs(steps))
                self._volume_display_end(ticket, result)
                if result == OK:
                    self._pre_mute_variant = None  # a turn after a mute is the new level
            self._record(span, "step_setting", result)

    async def _mute_toggle(self, status) -> tuple[str, str]:
        self.cancel_pending()  # the mute wins over a turn not yet committed
        if status is None:
            await self._cues.failure()
            return "mute", MENU_UNAVAILABLE
        current = status.audio_volume_master
        if current == MUTE:
            target = self._pre_mute_variant or UNMUTE_DEFAULT
            steps = VOLUME_VARIANTS.index(target) if target in VOLUME_VARIANTS else len(VOLUME_VARIANTS) - 1
            ticket = self._volume_display_start(("set", target))
            result = await self._step(VOLUME_OPTION, "up", steps)
            self._volume_display_end(ticket, result)
            if result == OK:
                self._pre_mute_variant = None
            return "unmute", result
        # An unreadable variant ("") steps down past the end, which stops at `mute` (A7).
        known = current in VOLUME_VARIANTS
        steps = VOLUME_VARIANTS.index(current) if known else len(VOLUME_VARIANTS)
        ticket = self._volume_display_start(("set", MUTE))
        result = await self._step(VOLUME_OPTION, "down", steps)
        self._volume_display_end(ticket, result)
        if result == OK:
            self._pre_mute_variant = current if known else None
        return "mute", result

    def _volume_display_start(self, change: tuple[str, object]) -> int:
        if self._display is None:
            return 0
        try:
            return self._display.volume_commit_start(change)
        except Exception as e:
            logger.debug(f"Pad display hook volume_commit_start failed: {e!r}")
            return 0

    def _volume_display_end(self, ticket: int, result: str) -> None:
        sent = _SENT.get()
        self._show("volume_commit_end", ticket, result, sent[1] if sent else "")
        if result not in (OK, AT_LIMIT):
            self._show("result", "VOLUME", result_word(result))  # A3 for the volume and the mute

    # --- sending --------------------------------------------------------------------------

    async def _step(self, option: str, direction: str, steps: int) -> str:
        data = {"command": "step_setting", "option": option, "direction": direction, "steps": str(steps)}
        if option == VOLUME_OPTION and self._link_up():
            data["cue"] = "none"  # UC-016 BR-004: the screen shows the level, no step sound
        return await self._send(data)

    # --- display (UC-016) -------------------------------------------------------------------

    def _link_up(self) -> bool:
        try:
            return self._display is not None and bool(self._display.link_up)
        except Exception:
            return False

    def _show(self, hook: str, *args) -> None:
        """A display hook: synchronous, and never raises into the key path (UC-016 FR-240)."""
        if self._display is None:
            return
        try:
            getattr(self._display, hook)(*args)
        except Exception as e:
            logger.debug(f"Pad display hook {hook} failed: {e!r}")

    @staticmethod
    def _row_label(binding: Binding | None) -> str | None:
        if binding is None or not binding.active:
            return None
        return label_of(binding.spec.option)

    def _pending_label(self, gesture: Gesture, status, binding: Binding | None) -> str | None:
        """The label of a command this gesture sends, shown until its result (None: sends nothing)."""
        if status is None:
            return None
        if gesture.kind in (LAYER, RESET) and (gesture.steps != 0 or gesture.kind == RESET):
            return self._row_label(binding)
        if gesture.kind in (AGENT_HOLD, HOLD) and gesture.key == K5:
            return None if self._agent_stopped else COMMAND_LABELS["agent_switch"]
        if gesture.kind == HOLD and gesture.key == K8:
            return COMMAND_LABELS["force_end" if status.lifecycle_live else "force_start"]
        if gesture.kind == TAP and gesture.key == K8 and not status.lifecycle_live:
            return COMMAND_LABELS["start_game"]
        return None

    def _show_result(self, gesture: Gesture, press_status, binding, action: str, result: str, sent) -> None:
        """UC-016 step 5, A3: the result screen with the committed value or the UC-015 BR-009 word."""
        if gesture.kind == TRANSITION:
            self._show("result", "", TRANSITION_TEXT)
            return
        if action == "announce" and gesture.kind == TAP:
            if result == OK and self._tap_screen is not None:
                self._show("result", *self._tap_screen)
            elif result != OK:
                self._show("result", self._tap_screen[0] if self._tap_screen else "", result_word(result))
            return
        label = self._result_label(gesture, press_status, binding, action)
        if label is None or result in (NOOP, "pending") or (gesture.kind == KNOB_PRESS_GESTURE and result == OK):
            self._show("clear_pending")  # a mute shows on the volume screen; nothing else to show
            return
        data, value = sent if sent else ({}, "")
        if result == OK:
            text = self._committed_text(action, data, value)
        else:
            up = data.get("direction") != "down" if data else gesture.steps >= 0
            text = result_word(result, up=up)
        self._show("result", label, text)

    def _result_label(self, gesture: Gesture, press_status, binding, action: str) -> str | None:
        if gesture.kind == CANCEL:
            if gesture.key == K8:
                live = press_status is not None and press_status.lifecycle_live
                return COMMAND_LABELS["force_end" if live else "force_start"]
            if gesture.key == K5:
                return COMMAND_LABELS["agent_switch"]
            return self._row_label(binding)
        if gesture.kind == KNOB_PRESS_GESTURE:
            return "VOLUME"
        if action in COMMAND_LABELS:
            return COMMAND_LABELS[action]
        if gesture.kind in (LAYER, RESET):
            return self._row_label(binding)
        return None  # lock, a key with no command

    @staticmethod
    def _committed_text(action: str, data: dict[str, str], value: str) -> str:
        """The value the menu reports as committed, never the pad's own prediction; `OK` without one (BR-001)."""
        if not value:
            return "OK"  # a command that names no value, or a menu older than `ProcessInputResponse.value`
        if action == "set_backend":
            option = "decision.backend"
        elif action == SWITCH_COMMAND:
            option = INTERVENTIONS
        else:
            option = data.get("option", "")
        return short(option, value)

    async def _send(
        self, data: dict[str, str], deadline_seconds: float = COMMAND_TIMEOUT_SECONDS, expect_value: bool = False
    ) -> str:
        """One `ProcessInput` web command; the result label. Never retried (BR-010).

        ``expect_value``: a success without `value` is a menu that did not run the command (an
        unknown `web_command` answers success, UC-016 BR-011 "Menu too old"): `failed`, never `OK`.
        """
        try:
            response = await self._menu.ProcessInput(
                menu_pb2.ProcessInputRequest(input_type="web_command", data=data), timeout=deadline_seconds
            )
        except Exception as e:
            logger.warning(f"Pad command {data.get('command')} not sent: {e}")
            await self._cues.failure()
            return MENU_UNAVAILABLE
        error = response.error if not response.success else (response.error or OK)
        if expect_value and response.success and not response.value:
            logger.warning(f"Pad command {data.get('command')} answered without a value: menu too old?")
            await self._cues.refusal()
            return FAILED
        _SENT.set((data, response.value if response.success else ""))
        await self._cue_for(error, up=data.get("direction") != "down")
        return result_label(error)

    async def _force_end(self) -> str:
        try:
            response = await self._coordinator.ForceEndGame(
                game_coordinator_pb2.ForceEndGameRequest(reason=FORCE_END_REASON), timeout=COMMAND_TIMEOUT_SECONDS
            )
        except Exception as e:
            logger.warning(f"ForceEndGame not sent: {e}")
            await self._cues.failure()
            return MENU_UNAVAILABLE
        if not response.success:
            logger.warning(f"ForceEndGame refused: {response.error}")
            await self._cues.refusal()
            return REFUSED
        return OK

    async def _cue_for(self, error: str, up: bool = True) -> None:
        """Menu error code -> pad cue. Success and failed writes are voiced by the menu itself."""
        if error in (GAME_ACTIVE, BAD_OPTION):
            await self._cues.refusal()
        elif error == AT_LIMIT:
            await self._cues.limit(up)  # "Maximum" stepping up, "Minimum" stepping down
        elif error == STALE:
            await self._cues.cancel()
        # ok, flag_service_unavailable, failed, partial:<key>: the menu played the voice or the failure cue

    @staticmethod
    def _record(span, action: str, result: str) -> None:
        span.set_attribute("pad.action", action)
        span.set_attribute("pad.result", result)
        if result == MENU_UNAVAILABLE:
            span.set_status(trace.Status(trace.StatusCode.ERROR, result))
