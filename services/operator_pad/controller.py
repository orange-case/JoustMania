"""Glue between the device and the action layer: key codes -> gestures -> commands (UC-015, UC-016).

One instance per service. The device layer calls `attached` / `on_code` / `detached`; K5's events
may come in on their own task (UC-016 A10: never queued behind a command), so the K5 hold time is
watched by a timer task of its own and its stop never waits for another command.

A row key binds at its press, before anything is awaited (UC-016 BR-011 "Which row"): the binding
first, then the paging stops (`note_input`), then the status read. The binding is kept for the
whole hold and passed with every gesture of that key.
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
import time
from collections.abc import Awaitable, Callable

from services.operator_pad import metrics, page_keys
from services.operator_pad.actions import Actions
from services.operator_pad.config import K5, KEYS, KNOB_PRESS, PadConfig
from services.operator_pad.cues import CuePlayer
from services.operator_pad.gestures import (
    AGENT_HOLD,
    CONFIRM,
    LAYER_STEP,
    PAGE_STEP,
    PRESS,
    RELEASE,
    GestureMachine,
    KeyEvent,
)
from services.operator_pad.state import MenuStatusReader

logger = logging.getLogger(__name__)

# Keys whose press and release read a fresh `GetMenuStatus` (the knob turns do not need one).
_STATUS_KEYS = frozenset(KEYS) | {KNOB_PRESS}
REFUSED_BINDING = "refused"  # UC-016 BR-011: a row key pressed while the link is uncertain


def monotonic_ms() -> int:
    return time.monotonic_ns() // 1_000_000


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
        self._press_status: dict[str, object] = {}
        self._bindings: dict[str, object] = {}
        self._known_seen = False
        self._unknown_warned = False
        self._agent_timer: asyncio.Task | None = None
        self._agent_tasks: set[asyncio.Task] = set()

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
        self._drop_holds()
        self._actions.cancel_pending()
        self._cues.cancel_all()  # no announcement plays on after an unplug
        self._show("reset_hold")

    def events_dropped(self) -> None:
        """The kernel dropped input events (SYN_DROPPED, A4 step 2): a release may be lost, reset the gestures."""
        logger.warning("Pad input events dropped (SYN_DROPPED): gestures in progress reset")
        self._drop_holds()
        self._actions.end_preview()
        self._show("reset_hold")

    def _drop_holds(self) -> None:
        self.machine.reset()
        self._press_status.clear()
        self._bindings.clear()
        self._cancel_agent_timer()

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

    def _bind(self, key: str, source: str | None):
        """UC-016 BR-011 "Which row" / BR-016: the row of the page shown, refused, or the fallback page."""
        if key not in page_keys.ROW_KEYS:
            return None
        fallback = page_keys.binding(key, page_keys.FALLBACK_PAGE, fallback=True)
        display = self._display
        if display is None:
            return fallback
        try:
            usb = display.display_usb
            if source is not None and usb is not None and source != usb:
                return fallback  # another pad's keys: no page binds them
            bound = display.bind_page()
        except Exception as e:
            logger.debug(f"Pad page binding failed: {e!r}")
            return fallback
        if bound == REFUSED_BINDING:
            return REFUSED_BINDING
        page, is_fallback = bound
        return page_keys.binding(key, page, fallback=is_fallback)

    async def on_code(self, code: int, value: int, t_ms: int, source: str | None = None) -> None:
        name = self._keymap.get(code)
        if name is None:
            await self._unknown(code, value)
            return
        self._known_seen = True
        if not self._config.enabled():
            # `operator_pad.enabled` off: the pad does nothing; gestures in progress are dropped.
            self.detached()
            return
        timings = self._config.timings()
        if value == PRESS and name in KEYS:
            self._bindings[name] = self._bind(name, source)
            self._show("note_input", True)  # the page stays from here on, before any await (BR-011)
        status = None
        if name in _STATUS_KEYS and value in (PRESS, RELEASE):
            token = self._token()
            status = await self._reader.fetch()  # fresh at every press and release, never cached
            self._show("note_status", status, token)  # the screen only; commands keep their own reads
            if value == PRESS:
                self._press_status[name] = status
        if value == RELEASE:
            press_status = self._press_status.pop(name, None)
            binding = self._bindings.pop(name, None)
        else:
            press_status = None
            binding = self._bindings.get(name)
        gestures = self.machine.feed(KeyEvent(name, value, t_ms), timings, pages=self._pages_active())
        if name in KEYS and value == PRESS and self.machine.press_ms(name) == t_ms:
            self._show("key_down", name, t_ms, timings, None if binding == REFUSED_BINDING else binding, status)
            if binding == REFUSED_BINDING:
                await self._cues.refusal()  # the link is uncertain: the key sends nothing
            if name == K5:
                self._start_agent_timer(t_ms, timings.hold_agent_ms)
        if name in KEYS and value == RELEASE:
            self._show("key_up", name)
            if name == K5:
                self._cancel_agent_timer()
        # Before any command is awaited: a release ends the setting screen at once (UC-016 BR-001).
        self._show("hold_key", self.machine.preview_key())
        self._show("set_locked", self.machine.locked)
        self._show("note_input", self.machine.keys_down())  # UC-016 BR-009
        for gesture in gestures:
            if gesture.kind == PAGE_STEP:
                self._show("page_step", gesture.steps)  # UC-016 BR-008: the screen only, no command, no cue
                continue
            key_binding = binding if gesture.key == name else self._bindings.get(gesture.key)
            if key_binding == REFUSED_BINDING:
                continue
            if gesture.kind in (LAYER_STEP, CONFIRM):
                # BR-017: the preview starts from the status read at the press, peeked (the release pops it).
                await self._actions.dispatch(gesture, None, self._press_status.get(gesture.key), key_binding)
                continue
            if gesture.kind == AGENT_HOLD:
                gesture_press = press_status if gesture.key == name else self._press_status.get(gesture.key)
                await self._actions.dispatch(gesture, status, gesture_press, None)
                continue
            await self._actions.dispatch(gesture, status, press_status, key_binding)
        # A chord or a lost key ends the hold without an edit release: the preview stops too.
        self._actions.end_preview(unless_key=self.machine.preview_key())

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
            task = asyncio.create_task(self._actions.dispatch(gesture, None, self._press_status.get(K5), None))
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
