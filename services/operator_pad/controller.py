"""Glue between the device and the action layer: key codes -> gestures -> commands (UC-015).

One instance per service. The device layer calls `attached` / `on_code` / `detached` from a single
task, so events are handled strictly in order.
"""

from __future__ import annotations

import logging

from services.operator_pad import metrics
from services.operator_pad.actions import Actions
from services.operator_pad.config import KEYS, KNOB_PRESS, PadConfig
from services.operator_pad.cues import CuePlayer
from services.operator_pad.gestures import LAYER_STEP, PAGE_STEP, PRESS, RELEASE, GestureMachine, KeyEvent
from services.operator_pad.state import MenuStatusReader

logger = logging.getLogger(__name__)

# Keys whose press and release read a fresh `GetMenuStatus` (the knob turns do not need one).
_STATUS_KEYS = frozenset(KEYS) | {KNOB_PRESS}


class PadController:
    def __init__(
        self,
        config: PadConfig,
        reader: MenuStatusReader,
        actions: Actions,
        cue_player: CuePlayer,
        machine: GestureMachine | None = None,
        display=None,
    ) -> None:
        self._config = config
        self._reader = reader
        self._actions = actions
        self._cues = cue_player
        self.machine = machine or GestureMachine()
        self._display = display  # UC-016 DisplayModel, or None (no display)
        self._keymap: dict[int, str] = {}
        self._press_status: dict[str, object] = {}
        self._known_seen = False
        self._unknown_warned = False

    def attached(self, keymap: dict[int, str]) -> None:
        """A (re)attached pad: the keymap read at this rescan, a fresh unknown-key warning budget."""
        self._keymap = dict(keymap)
        self._known_seen = False
        self._unknown_warned = False

    def detached(self) -> None:
        """Device gone (A4 step 2): every gesture in progress is dropped, nothing is sent later."""
        self.machine.reset()
        self._press_status.clear()
        self._actions.cancel_pending()
        self._cues.cancel_all()  # no announcement plays on after an unplug
        self._show("reset_hold")

    def events_dropped(self) -> None:
        """The kernel dropped input events (SYN_DROPPED, A4 step 2): a release may be lost, reset the gestures."""
        logger.warning("Pad input events dropped (SYN_DROPPED): gestures in progress reset")
        self.machine.reset()
        self._press_status.clear()
        self._actions.end_preview()
        self._show("reset_hold")

    def _show(self, hook: str, *args) -> None:
        """A display hook: synchronous, and never raises into the key path (UC-016 FR-240)."""
        if self._display is None:
            return
        try:
            getattr(self._display, hook)(*args)
        except Exception as e:
            logger.debug(f"Pad display hook {hook} failed: {e!r}")

    async def on_code(self, code: int, value: int, t_ms: int) -> None:
        name = self._keymap.get(code)
        if name is None:
            await self._unknown(code, value)
            return
        self._known_seen = True
        if not self._config.enabled():
            # `operator_pad.enabled` off: the pad does nothing; gestures in progress are dropped.
            self.detached()
            return
        status = None
        if name in _STATUS_KEYS and value in (PRESS, RELEASE):
            token = self._token()
            status = await self._reader.fetch()  # fresh at every press and release, never cached
            self._show("note_status", status, token)  # the screen only; commands keep their own reads
            if value == PRESS:
                self._press_status[name] = status
        press_status = self._press_status.pop(name, None) if value == RELEASE else None
        gestures = self.machine.feed(KeyEvent(name, value, t_ms), self._config.timings(), pages=self._pages_active())
        # Before any command is awaited: a release ends the setting screen at once (UC-016 BR-001).
        self._show("hold_key", self.machine.preview_key())
        self._show("set_locked", self.machine.locked)
        self._show("note_input", self.machine.keys_down())  # UC-016 BR-009
        for gesture in gestures:
            if gesture.kind == PAGE_STEP:
                self._show("page_step", gesture.steps)  # UC-016 BR-008: the screen only, no command, no cue
                continue
            if gesture.kind == LAYER_STEP:
                # BR-017: the preview starts from the status read at the press, peeked (the release pops it).
                await self._actions.dispatch(gesture, None, self._press_status.get(gesture.key))
                continue
            await self._actions.dispatch(gesture, status, press_status)
        # A chord or a lost key ends the hold without a layer release: the preview stops too.
        self._actions.end_preview(unless_key=self.machine.preview_key())

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
