"""Glue between the device and the action layer: key codes -> gestures -> commands (UC-015, UC-016).

One instance per service. Each event goes through two halves:

- `accept` (synchronous, called by the device layer as the event arrives, in arrival order): the
  row binding at the event's time, the paging stop, the gesture machine, the K5 hold timer. Nothing
  awaits here, so a release is timed and a K5 hold settled before any status read (UC-016 A10).
- `finish` (async, on the device's ordered lanes, K5 on a lane of its own, UC-016 A10): the status
  reads and the commands.

`on_code` runs both (tests, and any caller without lanes).
"""

from __future__ import annotations

import asyncio
import contextlib
import dataclasses
import logging
import time
from collections.abc import Awaitable, Callable
from dataclasses import dataclass

from services.operator_pad import metrics, page_keys
from services.operator_pad.actions import Actions
from services.operator_pad.config import K5, KEYS, KNOB_PRESS, KNOB_TURN_A, KNOB_TURN_B, PadConfig, Timings
from services.operator_pad.cues import CuePlayer
from services.operator_pad.gestures import (
    AGENT_HOLD,
    CANCEL,
    CONFIRM,
    HOLD,
    LAYER_STEP,
    LOCK,
    PAGE_STEP,
    PRESS,
    RELEASE,
    TRANSITION,
    Gesture,
    GestureMachine,
    KeyEvent,
)
from services.operator_pad.state import STATUS_TIMEOUT_SECONDS, MenuStatusReader

logger = logging.getLogger(__name__)

# Keys whose press and release read a fresh `GetMenuStatus` (the knob turns do not need one).
_STATUS_KEYS = frozenset(KEYS) | {KNOB_PRESS}
REFUSED_BINDING = "refused"  # UC-016 BR-011: a row key pressed while the link is uncertain
# A release waits for its press's read at most this long (the read's own deadline and a margin): a press
# whose finish never ran must not wedge its lane.
PRESS_READ_WAIT_SECONDS = STATUS_TIMEOUT_SECONDS + 1.0
# Fixed-key functions a refused binding still does (they act on no row): the lock, the transition cue.
_ROWLESS = (LOCK, TRANSITION)


def monotonic_ms() -> int:
    return time.monotonic_ns() // 1_000_000


@dataclass
class Accepted:
    """One event after `accept`: what `finish` still has to read and send."""

    name: str
    value: int
    t_ms: int
    gestures: list[Gesture]
    binding: object = None
    press_read: asyncio.Future | None = None  # this press's status read, or the release's press read
    read: bool = False
    unknown_code: int | None = None


class PadController:
    def __init__(
        self,
        config: PadConfig,
        reader: MenuStatusReader,
        actions: Actions,
        cue_player: CuePlayer,
        machine: GestureMachine | None = None,
        display=None,
        clock_ms: Callable[[], int] = monotonic_ms,
        sleep: Callable[[float], Awaitable[None]] = asyncio.sleep,
    ) -> None:
        self._config = config
        self._reader = reader
        self._actions = actions
        self._cues = cue_player
        self.machine = machine or GestureMachine()
        self._display = display  # UC-016 DisplayModel, or None (no display)
        self._clock_ms = clock_ms
        self._sleep = sleep
        self._keymap: dict[int, str] = {}
        self._press_reads: dict[str, asyncio.Future] = {}
        self._bindings: dict[str, object] = {}
        self._sources: dict[str, str | None] = {}
        self._knob_ignored = False
        self._known_seen = False
        self._unknown_warned = False
        self._agent_timer: asyncio.Task | None = None
        self._stage_timers: dict[str, asyncio.Task] = {}  # reset key -> its reset-stage cue timer
        self._agent_tasks: set[asyncio.Task] = set()
        self.pad_devices = 1  # distinct USB devices of the attached pad nodes (set by the device layer)

    def is_priority(self, code: int) -> bool:
        """UC-016 A10: K5's events are handled at once, never behind a command still in flight."""
        return self._keymap.get(code) == K5

    def attached(self, keymap: dict[int, str]) -> None:
        """A (re)attached pad: the keymap read at this rescan, a fresh unknown-key warning budget."""
        self._keymap = dict(keymap)
        self._known_seen = False
        self._unknown_warned = False
        self._actions.push_lock_led(self.machine.locked)  # BR-011: a restart comes back unlocked, LED off

    def detached(self) -> None:
        """Device gone (A4 step 2): every gesture in progress is dropped, nothing is sent later."""
        self.machine.reset()
        self._forget_holds()
        self._cancel_agent_timer()
        self._actions.cancel_pending()
        self._cues.cancel_all()  # no announcement plays on after an unplug
        self._show("reset_hold")

    def events_dropped(self) -> None:
        """SYN_DROPPED (A4 step 2): a release may be lost; the gestures reset, but a K5 hold stays stop-only."""
        logger.warning("Pad input events dropped (SYN_DROPPED): gestures in progress reset, K5 kept stop-only")
        self.machine.dropped()
        k5 = self.machine.down(K5)
        kept = {K5: self._press_reads[K5]} if k5 and K5 in self._press_reads else {}
        self._forget_holds()
        self._press_reads.update(kept)
        if not k5:
            self._cancel_agent_timer()
        self._actions.end_preview()
        self._show("reset_hold")

    def _forget_holds(self) -> None:
        for task in self._stage_timers.values():
            task.cancel()
        self._stage_timers.clear()
        self._press_reads.clear()
        self._bindings.clear()
        self._sources.clear()
        self._knob_ignored = False

    async def drain(self) -> None:
        """Wait for the K5 commands in flight (tests, shutdown)."""
        while self._agent_tasks:
            await asyncio.gather(*self._agent_tasks, return_exceptions=True)

    def _show(self, hook: str, *args) -> None:
        """A display hook: synchronous, and never raises into the key path (UC-016 FR-240)."""
        if self._display is None:
            return
        try:
            getattr(self._display, hook)(*args)
        except Exception as e:
            logger.debug(f"Pad display hook {hook} failed: {e!r}")

    def _bind(self, key: str, source: str | None, t_ms: int):
        """UC-016 BR-011 "Which row" / BR-016: the row of the page shown at ``t_ms``, refused, or the fallback."""
        if key not in page_keys.ROW_KEYS:
            return None
        fallback = page_keys.binding(key, page_keys.FALLBACK_PAGE, fallback=True)
        display = self._display
        if display is None:
            return fallback
        try:
            usb = display.display_usb
            if source is not None and usb is not None:
                if source != usb:
                    return fallback  # another pad's keys: no page binds them
            elif self.pad_devices > 1:
                return fallback  # several pads and no way to tell which one has the screen
            bound = display.bind_page(t_ms / 1000)
        except Exception as e:
            logger.debug(f"Pad page binding failed: {e!r}")
            return fallback
        if bound == REFUSED_BINDING:
            return REFUSED_BINDING
        page, is_fallback = bound
        return page_keys.binding(key, page, fallback=is_fallback)

    def _knob_foreign(self, source: str | None) -> bool:
        """A knob press from another pad than the held keys' never acts on them."""
        return source is not None and any(s is not None and s != source for s in self._sources.values())

    # --- the two halves ------------------------------------------------------------------------

    def accept(self, code: int, value: int, t_ms: int, source: str | None = None) -> Accepted | None:
        """The synchronous half, at the event's arrival; None when nothing is left to do."""
        name = self._keymap.get(code)
        if name is None:
            return Accepted("", value, t_ms, [], unknown_code=code)
        self._known_seen = True
        if not self._config.enabled():
            # `operator_pad.enabled` off: the pad does nothing; gestures in progress are dropped.
            self.detached()
            return None
        if name in (KNOB_TURN_A, KNOB_TURN_B) and self._knob_foreign(source):
            return None  # another pad's knob never steps this pad's held key
        if name == KNOB_PRESS:
            if value == PRESS and self._knob_foreign(source):
                self._knob_ignored = True
                return None
            if value == RELEASE and self._knob_ignored:
                self._knob_ignored = False
                return None
        timings = self._config.timings()
        if value == PRESS and name in KEYS:
            self._bindings[name] = self._bind(name, source, t_ms)
            self._sources[name] = source
        read = name in _STATUS_KEYS and value in (PRESS, RELEASE)
        press_read = None
        if value == RELEASE:
            press_read = self._press_reads.pop(name, None)
            binding = self._bindings.pop(name, None)
            self._sources.pop(name, None)
        else:
            binding = self._bindings.get(name)
        gestures = self.machine.feed(KeyEvent(name, value, t_ms), timings, pages=self._pages_active())
        if name in KEYS and value == PRESS and self.machine.press_ms(name) == t_ms:
            self._key_down(name, t_ms, timings, binding)
        if name in KEYS and value == RELEASE:
            self._show("key_up", name)
            timer = self._stage_timers.pop(name, None)
            if timer is not None:
                timer.cancel()
            if name == K5:
                self._cancel_agent_timer()
        self._show("hold_key", self.machine.preview_key())
        self._show("set_locked", self.machine.locked)
        self._show("note_input", self.machine.keys_down())  # UC-016 BR-009; the page stays from here on
        if read and value == PRESS:
            # Last: nothing above may leave a future that no `finish` ever resolves.
            press_read = self._press_reads[name] = asyncio.get_running_loop().create_future()
        return Accepted(name, value, t_ms, gestures, binding, press_read, read)

    def _key_down(self, name: str, t_ms: int, timings: Timings, binding) -> None:
        self._show("key_down", name, t_ms, timings, None if binding == REFUSED_BINDING else binding, None)
        if name == K5:
            self._start_agent_timer(t_ms, timings.hold_agent_ms)
        elif binding not in (None, REFUSED_BINDING) and binding.role == page_keys.RESET_ROLE and binding.active:
            self._stage_timers[name] = asyncio.create_task(
                self._reset_stage(name, t_ms + timings.hold_reset_ms, binding)
            )

    async def _reset_stage(self, key: str, due_ms: int, binding) -> None:
        """Display and cue only: the attention cue when a reset that raises reaches its stage (BR-014)."""
        with contextlib.suppress(asyncio.CancelledError):
            await self._sleep(max(0, due_ms - self._clock_ms()) / 1000)
            if self.machine.down(key) and not self.machine.locked:
                await self._actions.reset_stage(key, binding, await self._read_of(self._press_reads.get(key)))

    async def finish(self, event: Accepted) -> None:
        """The async half: the status reads, then the commands of the gestures `accept` found."""
        if event.unknown_code is not None:
            await self._unknown(event.unknown_code, event.value)
            return
        name, value = event.name, event.value
        status = None
        try:
            if event.read:
                token = self._token()
                status = await self._reader.fetch()  # fresh at every press and release, never cached
                self._show("note_status", status, token)  # the screen only; commands keep their own reads
        finally:
            if value == PRESS and event.press_read is not None and not event.press_read.done():
                event.press_read.set_result(status)  # also on a failure: never left pending
        if value == PRESS:
            self._show("press_status", name, status)
        press_status = await self._read_of(event.press_read) if value == RELEASE else None
        if value == PRESS and event.binding == REFUSED_BINDING:
            await self._cues.refusal()  # the link is uncertain: the key sends nothing
        for gesture in event.gestures:
            if gesture.kind == PAGE_STEP:
                self._show("page_step", gesture.steps)  # UC-016 BR-008: the screen only, no command, no cue
                continue
            key_binding = event.binding if gesture.key == name else self._bindings.get(gesture.key)
            if key_binding == REFUSED_BINDING:
                if gesture.kind not in _ROWLESS and not (gesture.kind == CANCEL and gesture.stage):
                    continue
                key_binding = None
            if gesture.kind in (LAYER_STEP, CONFIRM):
                # BR-017: the preview starts from the status read at the press, peeked (the release pops it).
                peek = await self._read_of(self._press_reads.get(gesture.key))
                await self._actions.dispatch(gesture, None, peek, key_binding)
                continue
            if gesture.key == K5 and gesture.kind in (AGENT_HOLD, HOLD):
                await self._actions.dispatch(self._stop_only_in_flight(gesture), status, press_status, None)
                continue
            await self._actions.dispatch(gesture, status, press_status, key_binding)
        # A chord or a lost key ends the hold without an edit release: the preview stops too.
        self._actions.end_preview(unless_key=self.machine.preview_key())

    async def on_code(self, code: int, value: int, t_ms: int, source: str | None = None) -> None:
        event = self.accept(code, value, t_ms, source)
        if event is not None:
            await self.finish(event)

    @staticmethod
    async def _read_of(future: asyncio.Future | None):
        """A press's status read; None when it never comes in time (unknown: the safe direction)."""
        if future is None:
            return None
        try:
            return await asyncio.wait_for(asyncio.shield(future), PRESS_READ_WAIT_SECONDS)
        except TimeoutError:
            return None

    @staticmethod
    def _read_now(future: asyncio.Future | None):
        """The press read if it is already there, else None (unknown): the stop never waits for it."""
        return future.result() if future is not None and future.done() and not future.cancelled() else None

    def _stop_only_in_flight(self, gesture: Gesture) -> Gesture:
        """While a K5 stop is still in flight, a K5 hold is stop-only (never a resume over it)."""
        if self._agent_tasks and gesture.clean:
            return dataclasses.replace(gesture, clean=False)
        return gesture

    # --- K5: the stop at the hold time (UC-016 A10) ----------------------------------------------

    def _start_agent_timer(self, press_ms: int, hold_ms: int) -> None:
        self._cancel_agent_timer()
        self._agent_timer = asyncio.create_task(self._agent_wait(press_ms + hold_ms))

    def _cancel_agent_timer(self) -> None:
        if self._agent_timer is not None and self._agent_timer is not asyncio.current_task():
            self._agent_timer.cancel()
        self._agent_timer = None

    async def _agent_wait(self, due_ms: int) -> None:
        with contextlib.suppress(asyncio.CancelledError):
            await self._sleep(max(0, due_ms - self._clock_ms()) / 1000)
            await self.agent_due(max(due_ms, self._clock_ms()))

    async def agent_due(self, t_ms: int) -> None:
        """K5 held to its hold time: the stop goes out at once, in its own task (never behind a command)."""
        for gesture in self.machine.due(t_ms):
            gesture = self._stop_only_in_flight(gesture)
            press_status = self._read_now(self._press_reads.get(K5))
            self._actions.begin_agent_hold()  # before the K5 lane can see the release
            task = asyncio.create_task(self._actions.dispatch(gesture, None, press_status, None))
            self._agent_tasks.add(task)
            task.add_done_callback(self._agent_tasks.discard)
            await asyncio.shield(task)

    def _pages_active(self) -> bool:
        """UC-016 BR-008: the knob scrolls pages; any failure reads as no pages (UC-015 knob, FR-240)."""
        try:
            return self._display is not None and self._display.pages_active is True
        except Exception:
            return False

    def _token(self) -> int | None:
        try:
            return self._display.read_token() if self._display is not None else None
        except Exception:
            return None

    async def _unknown(self, code: int, value: int) -> None:
        """A4 step 3: ignored and counted; a pad that sends only unknown codes gets one warning and one cue."""
        if value != PRESS:
            return
        metrics.unknown_keys_total.inc()
        logger.debug(f"Unknown pad key code {code} ignored")
        if not self._known_seen and not self._unknown_warned:
            self._unknown_warned = True
            logger.warning(
                f"Pad sent key code {code}, which is not in operator_pad.keymap: firmware reset or remap? "
                "Keys outside the keymap are ignored."
            )
            await self._cues.unknown_key()
