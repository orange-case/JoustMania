"""The pad's display (UC-016): what to show (`DisplayModel`) and the link that shows it (`PadDisplay`).

The key path (`PadController.on_code`, `Actions`) only calls the model's synchronous hooks: they
set fields and wake the link, and never raise. Every hidraw call runs on the display's own worker
thread, so a hung or failing write never delays or drops a key event (FR-240, A1).

With protocol 2 or higher the model shows the pages of BR-006 (`pages.py`, `render.py`); the
knob scrolls them while `pages_active` (BR-008). Only one screen per service: with several pads
attached, the first raw node of the matching device gets the screen, and the knob of any of them
scrolls it. The link takes the raw HID node from whoever else holds it only by
sharing it (every opener gets every report): with the Vial app open on the same host the
display goes off and on as their replies cross.
"""

from __future__ import annotations

import asyncio
import contextlib
import functools
import logging
import threading
import time
from collections.abc import Awaitable, Callable
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path

from lib.setting_ladders import VOLUME_LADDER, clamp_step
from services.operator_pad import hidraw, page_keys, render, screens
from services.operator_pad.config import K4, PadConfig, Timings
from services.operator_pad.device import RESCAN_SECONDS
from services.operator_pad.pages import HOME, NET_UNKNOWN, PageContext, Pager
from services.operator_pad.screens import (
    LINES,
    MAXIMUM,
    MINIMUM,
    Line,
    Screen,
    result_screen,
    setting_screen,
    status_screen,
    volume_screen,
)
from services.operator_pad.state import MenuStatusReader

logger = logging.getLogger(__name__)

HEARTBEAT_SECONDS = 1.0  # BR-002: a show at least once a second
LINK_FRESH_SECONDS = 1.5  # link up = a show acknowledged this recently (BR-004)
REPLY_TIMEOUT_SECONDS = 0.25  # per report; the firmware answers within a USB poll interval
IO_MARGIN_SECONDS = 0.5
PENDING_SECONDS = 6.0  # a command's label until its result: longer than its deadline (actions.py)
VOLUME = "audio_volume.master"
WAKE_MIN_SECONDS = 0.005  # a due page deadline never wakes the render loop in a busy loop
PAGE_SETTLE_SECONDS = 0.4  # UC-016 BR-011: a key binds to a page only after it was on screen this long
NO_HOST_SECONDS = 3.0  # BR-002: the firmware shows NO HOST this long after the last frame
REFUSED = "refused"  # `bind_page`: the link is uncertain, the row keys are refused
BLINK_SECONDS = 0.5  # UC-016 BR-014: the title bar of a raise awaiting its knob press, each half


def _quiet(method):
    """A display hook never raises into the key path (FR-240): a failure is logged once per hook."""

    @functools.wraps(method)
    def wrapper(self, *args, **kwargs):
        try:
            return method(self, *args, **kwargs)
        except Exception as e:
            if method.__name__ not in self._hook_warned:
                self._hook_warned.add(method.__name__)
                logger.warning(f"Pad display hook {method.__name__} failed: {e!r}")
            return None

    return wrapper


@dataclass(frozen=True)
class _Layer:
    key: str
    option: str
    press_status: object
    net: int
    lobby_only: bool
    plan: object = None  # UC-016 BR-014: `page_keys.Plan` of a page key


@dataclass
class _Held:
    """A key held (UC-016 BR-011): its press time and timing snapshot, its row, the status at the press."""

    key: str
    t_ms: int
    timings: Timings
    binding: object
    press_status: object
    confirmed: bool = False


class DisplayModel:
    """What the screen shows; synchronous hooks called from the key path and the poll."""

    def __init__(self, config: PadConfig, version: str | None = None, clock: Callable[[], float] = time.monotonic):
        self._config = config
        self._version = version
        self._clock = clock
        self.dirty = asyncio.Event()
        self._hook_warned: set[str] = set()
        self._status = None
        self._connection_ok = True
        self._locked = False
        self._hold: str | None = None
        self._layer: _Layer | None = None
        self._overlay: tuple | None = None  # ("result", label, text) | ("volume",)
        self._overlay_until = 0.0
        self._last_ack: float | None = None
        # Volume prediction (BR-001 volume screen): the base variant, the commits in flight (each
        # clamped on its own, as the menu does), then the turns not yet committed.
        self._vol_base: str | None = None
        self._vol_inflight: dict[int, tuple[str, object]] = {}
        self._vol_ticket = 0
        self._vol_pending = 0
        self._vol_limit: str | None = None
        self._vol_epoch = 0  # goes up when a volume commit ends; a read from before it never re-bases
        self._vol_now: str | None = None  # BR-007: the level when the volume turn began (the marker)
        # Protocol 2 and higher (BR-006 to BR-010).
        self._protocol: int | None = None  # the hello's version (above 4 used as 4); None: link down
        self._layout: int | None = None  # the version drawn: lower after the pad refused a bar or wheel
        self._pager = Pager(clock())
        self._keys_down = False
        self._page_epoch = 0  # goes up with every page step; a volume overlay from before it stays hidden
        self._vol_page_epoch = 0
        self._internet = NET_UNKNOWN
        self._down_since: float | None = None
        self.internet_wake = asyncio.Event()  # set on link-up: the internet check runs at once (BR-010)
        # UC-016 BR-011: the keys held, and the page the firmware acknowledged (for the binding).
        self._keys: dict[str, _Held] = {}
        self._drawn_page: int | None = None
        self._acked_page: int | None = None
        self._acked_at = 0.0
        self._prev_page = HOME
        self._last_pages_ack: float | None = None
        self._blink_since: float | None = None  # a raise awaits its confirmation since (BR-014)
        self.display_usb: str | None = None  # the USB device of the display link (BR-011: only its keys bind)

    # --- link state -------------------------------------------------------------------------

    @property
    def link_up(self) -> bool:
        """BR-004: a show was acknowledged within LINK_FRESH_SECONDS."""
        try:
            return self._last_ack is not None and self._clock() - self._last_ack < LINK_FRESH_SECONDS
        except Exception:
            return False

    def acknowledged(self) -> None:
        now = self._last_ack = self._clock()
        if (self._layout or 0) >= 2 and self._drawn_page is not None:
            self._last_pages_ack = now
            if self._drawn_page != self._acked_page:
                if self._acked_page is not None:
                    self._prev_page = self._acked_page
                self._acked_page, self._acked_at = self._drawn_page, now

    def bind_page(self, at: float | None = None) -> tuple[int, bool] | str:
        """UC-016 BR-011 "Which row": (page, fallback) for a row key pressed now, or REFUSED.

        The page the firmware acknowledged once it was on screen PAGE_SETTLE_SECONDS (a younger one:
        the page before it); while the screen may still show a frame (up to NO_HOST_SECONDS after the
        last ack) without a fresh link, refused; else the fallback page of BR-016. ``at``: the key event's
        time (the same monotonic basis), not when the event is processed.
        """
        try:
            now = self._clock() if at is None else at
            fresh = self._last_ack is not None and now - self._last_ack < LINK_FRESH_SECONDS
            if fresh and (self._protocol or 0) >= 2 and self._acked_page is not None:
                if now - self._acked_at >= PAGE_SETTLE_SECONDS:
                    return self._acked_page, False
                return self._prev_page, False
            if self._last_pages_ack is not None and now - self._last_pages_ack <= NO_HOST_SECONDS:
                return REFUSED
        except Exception as e:
            logger.debug(f"bind_page: {e!r}")
        return page_keys.FALLBACK_PAGE, True

    def link_down(self) -> None:
        self._last_ack = None

    @property
    def protocol(self) -> int | None:
        return self._protocol

    @property
    def pages_active(self) -> bool:
        """BR-008: the knob scrolls the pages (link up with protocol 2 or higher)."""
        try:
            return self.link_up and (self._protocol or 0) >= 2
        except Exception:
            return False

    @property
    def internet(self) -> str:
        return self._internet

    def set_protocol(self, version: int | None) -> None:
        """The hello's version at link-up (`HOME` shows, BR-006), None when the link goes down."""
        now = self._clock()
        if version is None:
            if self._protocol is not None:
                self._down_since = now
            self._protocol = self._layout = None
            return
        self._protocol = self._layout = min(version, render.MAX_VERSION)
        self._pager.reset(now)
        self._acked_page, self._drawn_page, self._prev_page = None, None, HOME
        # BR-010: a short flap keeps the internet state; a longer outage starts from `?`.
        if self._down_since is not None and now - self._down_since > self._config.internet_check_ms() / 1000:
            self._internet = NET_UNKNOWN
        self._down_since = None
        self.internet_wake.set()
        self.dirty.set()

    def layout_refused(self, op: int) -> None:
        """BR-002: the pad answered `FF` to a wheel (draw protocol 3) or a bar (protocol 2) for this link."""
        if self._layout is not None:
            self._layout = min(self._layout, 3 if op == hidraw.OP_WHEEL else 2)
        self.dirty.set()

    # --- hooks (key path and poll) ----------------------------------------------------------

    def read_token(self) -> int:
        return self._vol_epoch

    @_quiet
    def note_status(self, status, token: int | None = None) -> None:
        """A `GetMenuStatus` read; None = failed (A2: the last values stay, `NO CONNECTION`)."""
        if status is None:
            self._connection_ok = False
        else:
            self._status = status
            self._connection_ok = True
            if token == self._vol_epoch and not self._vol_inflight:
                self._vol_base = status.audio_volume_master or None
        self.dirty.set()

    @_quiet
    def set_locked(self, locked: bool) -> None:
        if locked != self._locked:
            self._locked = locked
            self.dirty.set()

    @_quiet
    def hold_key(self, key: str | None) -> None:
        """The layer key whose setting screen may show (the UC-015 BR-017 condition), or None."""
        if key != self._hold:
            self._hold = key
            self._layer = None  # a new hold starts without the last hold's values
            self.dirty.set()

    @_quiet
    def key_down(self, key: str, t_ms: int, timings: Timings, binding, press_status) -> None:
        """UC-016 BR-011: a key went down at ``t_ms`` (the gesture clock) with its timing snapshot."""
        self._keys[key] = _Held(key, t_ms, timings, binding, press_status)
        self.dirty.set()

    @_quiet
    def press_status(self, key: str, status) -> None:
        """The status read at the key's press, once it is there (the reset screen's NOW and NEW)."""
        held = self._keys.get(key)
        if held is not None:
            held.press_status = status
            self.dirty.set()

    @_quiet
    def key_up(self, key: str) -> None:
        if self._keys.pop(key, None) is not None:
            self.dirty.set()

    @_quiet
    def confirm_state(self, key: str, confirmed: bool) -> None:
        """UC-016 A9: the knob press confirmed the value showing (False: a step cleared it)."""
        held = self._keys.get(key)
        if held is not None and held.confirmed != confirmed:
            held.confirmed = confirmed
            self.dirty.set()

    @_quiet
    def layer_step(self, key: str, option: str, press_status, net: int, lobby_only: bool, plan=None) -> None:
        self._layer = _Layer(key, option, press_status, net, lobby_only, plan)
        self._overlay = None  # a new layer step replaces a result still showing
        self.dirty.set()

    @_quiet
    def pending(self, label: str) -> None:
        """A command was sent: its label until the result comes."""
        self._show_overlay(("result", label, ""), PENDING_SECONDS)

    @_quiet
    def clear_pending(self) -> None:
        if self._overlay is not None and self._overlay[0] == "result" and self._overlay[2] == "":
            self._overlay = None
            self.dirty.set()

    @_quiet
    def result(self, label: str, text: str) -> None:
        self._show_overlay(("result", label, text), self._config.display_result_ms() / 1000)

    @_quiet
    def reset_hold(self) -> None:
        """Device gone or events dropped: the gesture in progress and its screens are dropped."""
        self._hold = None
        self._layer = None
        self._overlay = None
        self._vol_pending = 0
        self._keys_down = False
        self._keys.clear()
        self.dirty.set()

    @_quiet
    def page_step(self, steps: int) -> None:
        """BR-008: a knob step with the pages: the next or previous page; ends a result, tap or volume screen."""
        self._overlay = None
        self._page_epoch += 1
        self._pager.step(steps, self._clock())
        self.dirty.set()

    @_quiet
    def note_input(self, keys_down: bool) -> None:
        """BR-009: any key or knob event; auto-paging pauses while a key or the knob press is down."""
        self._pager.input(self._clock())
        if keys_down != self._keys_down:
            self._keys_down = keys_down
            self.dirty.set()

    @_quiet
    def note_internet(self, state: str) -> None:
        """BR-010: the last internet check (`ok`, `down`, `off`); a change redraws."""
        if state != self._internet:
            self._internet = state
            self.dirty.set()

    # --- volume -----------------------------------------------------------------------------

    @_quiet
    def volume_turn(self, pending_steps: int) -> None:
        """A knob turn with no layer key: the variant the turns not yet committed would land on."""
        self._mark_volume_start(self._clock())
        self._vol_page_epoch = self._page_epoch
        self._vol_pending = pending_steps
        self._vol_limit = None
        if pending_steps:
            before = self._predict(pending_steps - (1 if pending_steps > 0 else -1))
            after = self._predict(pending_steps)
            if after is not None and after == before:
                self._vol_limit = MAXIMUM if pending_steps > 0 else MINIMUM
        self._show_overlay(("volume",), self._config.display_result_ms() / 1000)

    @_quiet
    def volume_dropped(self) -> None:
        self._vol_pending = 0

    def volume_commit_start(self, change: tuple[str, object]) -> int:
        """A volume commit leaves: ``("steps", n)`` or ``("set", variant)``; a ticket for its end."""
        try:
            if change[0] == "set":
                self._mark_volume_start(self._clock())
                self._vol_page_epoch = self._page_epoch
            self._vol_ticket += 1
            self._vol_inflight[self._vol_ticket] = change
            if change[0] == "steps":
                self._vol_pending = 0
            else:
                self._vol_limit = None
                self._show_overlay(("volume",), self._config.display_result_ms() / 1000)
            return self._vol_ticket
        except Exception as e:
            logger.debug(f"volume_commit_start: {e!r}")
            return 0

    @_quiet
    def volume_commit_end(self, ticket: int, result: str, value: str) -> None:
        change = self._vol_inflight.pop(ticket, None)
        self._vol_epoch += 1
        if result == "ok":
            # The value the menu committed; an older menu sends none: the change then lands as clamped.
            self._vol_base = (
                value
                if value in VOLUME_LADDER
                else (self._apply(self._vol_base, change) if change is not None else None)
            )
        elif result == "at_limit" and change is not None:
            self._vol_base = self._apply(self._vol_base, change)
            # A page step since this turn keeps the page (BR-008): no volume screen over it.
            if change[0] == "steps" and self._vol_page_epoch == self._page_epoch:
                self._vol_limit = MAXIMUM if change[1] > 0 else MINIMUM
                self._show_overlay(("volume",), self._config.display_result_ms() / 1000)
        elif result != "ok":
            self._vol_base = None  # unknown until a read that starts after this commit
        self.dirty.set()

    @staticmethod
    def _apply(variant: str | None, change: tuple[str, object]) -> str | None:
        if change[0] == "set":
            return change[1]
        if variant is None:
            return None
        return clamp_step(VOLUME_LADDER, variant, change[1], restart_outside=True)[0]

    def _predict(self, pending: int) -> str | None:
        variant = self._vol_base
        for change in self._vol_inflight.values():
            variant = self._apply(variant, change)
        return self._apply(variant, ("steps", pending)) if pending else variant

    def _mark_volume_start(self, now: float) -> None:
        """BR-007: a volume interaction starts while no volume screen shows; its level is the marker."""
        showing = self._overlay is not None and self._overlay[0] == "volume" and now < self._overlay_until
        if not showing:
            self._vol_now = self._predict(0)

    # --- rendering --------------------------------------------------------------------------

    def _show_overlay(self, overlay: tuple, seconds: float) -> None:
        self._overlay = overlay
        self._overlay_until = self._clock() + seconds
        self.dirty.set()

    def _layer_held(self) -> bool:
        return self._layer is not None and self._hold == self._layer.key

    def _confirmed(self, key: str) -> bool:
        held = self._keys.get(key)
        return held is not None and held.confirmed

    def _hold_stage(self, now: float) -> tuple[str, _Held] | None:
        """UC-016 BR-006, BR-013, BR-015: highlight, reset or lock of the one row key held, by its press snapshot."""
        if len(self._keys) != 1:
            return None  # a chord shows nothing of its own
        (held,) = self._keys.values()
        binding, timings = held.binding, held.timings
        if binding is None:
            return None
        held_ms = now * 1000 - held.t_ms
        if held.key == K4 and (self._locked or not held.confirmed) and held_ms >= timings.hold_lock_ms:
            return "lock", held
        if self._locked:
            return None
        if binding.role == page_keys.RESET_ROLE and binding.active and held_ms >= timings.hold_reset_ms:
            return "reset", held
        if binding.active and not binding.fallback and not self._layer_held():
            return "highlight", held
        return None

    def _stage_screen(
        self, stage: tuple[str, _Held], version: int, bottom: Line | None, now: float = 0.0
    ) -> Screen | None:
        kind, held = stage
        if kind == "lock":
            return (
                screens.lock_screen(self._locked) if version < 2 else render.lock_screen(version, self._locked, bottom)
            )
        if kind == "reset":
            spec = held.binding.spec
            plan = page_keys.reset_plan(spec, held.press_status)
            if version < 2:
                awaiting = plan.raises and not held.confirmed
                label = screens.SETTING_LABELS.get(spec.option, spec.option.upper())
                hint = screens.reset_hint(plan.refused, awaiting)
                return screens.reset_screen(label, spec.option, plan.now, plan.new, hint)
            awaiting = plan.raises and not plan.refused and not held.confirmed
            return render.reset_screen(
                version, spec.option, plan, held.confirmed, bottom, self._blink_on(now, awaiting)
            )
        return None  # the highlight is drawn on the page

    def _idle(self, now: float) -> bool:
        """BR-009: no key or knob press down and no setting, result, tap or volume screen."""
        overlay = self._overlay is not None and now < self._overlay_until
        return not self._keys_down and not overlay and not self._layer_held()

    def _page_times(self) -> tuple[float, float]:
        return self._config.display_autopage_ms() / 1000, self._config.display_home_ms() / 1000

    def next_change(self, now: float) -> float | None:
        """When the screen changes by itself (an overlay expires, a page turns), or None."""
        times = []
        if self._overlay is not None and self._overlay_until > now:
            times.append(self._overlay_until)
        if len(self._keys) == 1:
            (held,) = self._keys.values()
            for stage_ms in (held.timings.hold_reset_ms, held.timings.hold_lock_ms):
                due = (held.t_ms + stage_ms) / 1000
                if due > now:
                    times.append(due)
        if self._blink_since is not None:
            halves = int((now - self._blink_since) / BLINK_SECONDS) + 1
            times.append(self._blink_since + halves * BLINK_SECONDS)
        if (self._layout or 0) >= 2:
            wake = self._pager.next_wake(self._idle(now), *self._page_times())
            if wake is not None:
                times.append(max(wake, now + WAKE_MIN_SECONDS))
        return min(times) if times else None

    def current(self, now: float) -> Screen:
        """BR-001: the setting screen while its layer key is held, else an overlay, else the status.

        With protocol 2 or higher the same choice in the frame of BR-006, the status being the page.
        """
        if (self._layout or 0) >= 2:
            return self._current_frame(now, self._layout)
        layer = self._layer
        if layer is not None and self._hold == layer.key:
            return setting_screen(
                layer.option,
                layer.press_status,
                layer.net,
                lobby_only=layer.lobby_only,
                plan=layer.plan,
                confirmed=self._confirmed(layer.key),
            )
        stage = self._hold_stage(now)
        if stage is not None and stage[0] != "highlight":
            return self._stage_screen(stage, 1, None)
        if self._overlay is not None and now >= self._overlay_until:
            self._overlay = None
        if self._overlay is not None:
            if self._overlay[0] == "volume":
                return volume_screen(self._predict(self._vol_pending), limit=self._vol_limit)
            return result_screen(self._overlay[1], self._overlay[2])
        return status_screen(
            self._status, connection_ok=self._connection_ok, locked=self._locked, version=self._version
        )

    def _blink_on(self, now: float, awaiting: bool) -> bool:
        """The inverted half of the blink; a display-only timer that ends with the screen (BR-014)."""
        if not awaiting:
            self._blink_since = None
            return True
        if self._blink_since is None:
            self._blink_since = now
        return int((now - self._blink_since) / BLINK_SECONDS) % 2 == 0

    def _current_frame(self, now: float, version: int) -> Screen:
        if self._overlay is not None and now >= self._overlay_until:
            self._overlay = None
            self._pager.shown(now)  # the page under it comes back for a full period
        self._pager.tick(now, self._idle(now), *self._page_times())
        self._drawn_page = self._pager.page  # the page under any overlay (UC-016 BR-011)
        ctx = PageContext(locked=self._locked, internet=self._internet, version=self._version)
        bottom = render.status_line(self._status, ctx, self._connection_ok)
        layer = self._layer
        if layer is not None and self._layer_held():
            plan, confirmed = layer.plan, self._confirmed(layer.key)
            awaiting = plan is not None and plan.raises and not plan.refused and not confirmed
            return render.setting_screen(
                version,
                layer.option,
                layer.press_status,
                layer.net,
                lobby_only=layer.lobby_only,
                bottom=bottom,
                plan=plan,
                confirmed=confirmed,
                blink_on=self._blink_on(now, awaiting),
            )
        stage = self._hold_stage(now)
        if stage is None or stage[0] != "reset":
            self._blink_since = None
        if stage is not None and stage[0] == "highlight":
            highlight = stage[1].binding.row if stage[1].binding.page == self._pager.page else None
            return render.page_screen(version, self._pager.page, self._status, ctx, self._connection_ok, highlight)
        if stage is not None:
            return self._stage_screen(stage, version, bottom, now)
        if self._overlay is not None:
            if self._overlay[0] == "volume":
                new = self._predict(self._vol_pending)
                return render.volume_screen(version, self._vol_now, new, self._vol_limit, bottom)
            return render.result_screen(version, self._overlay[1], self._overlay[2], bottom)
        return render.page_screen(version, self._pager.page, self._status, ctx, self._connection_ok)


async def _wait(event: asyncio.Event, seconds: float) -> None:
    with contextlib.suppress(TimeoutError):
        await asyncio.wait_for(event.wait(), timeout=max(seconds, 0.0))


def _close_quietly(handle: hidraw.Handle) -> None:
    with contextlib.suppress(Exception):
        handle.close()


class PadDisplay:
    """The display link (BR-002) and the status poll (BR-003)."""

    def __init__(
        self,
        config: PadConfig,
        model: DisplayModel,
        reader: MenuStatusReader,
        *,
        backend: hidraw.Backend | None = None,
        sysfs_root: Path = hidraw.SYSFS_HIDRAW,
        dev_dirs: tuple[Path, ...] = hidraw.DEV_DIRS,
        rescan_seconds: float = RESCAN_SECONDS,
        heartbeat_seconds: float = HEARTBEAT_SECONDS,
        reply_timeout: float = REPLY_TIMEOUT_SECONDS,
        clock: Callable[[], float] = time.monotonic,
        sleep: Callable[[float], Awaitable[None]] = asyncio.sleep,
    ) -> None:
        self._config = config
        self.model = model
        self._reader = reader
        self._backend = backend or hidraw.HidrawBackend()
        self._sysfs_root = sysfs_root
        self._dev_dirs = dev_dirs
        self._rescan_seconds = rescan_seconds
        self._heartbeat = heartbeat_seconds
        self._reply_timeout = reply_timeout
        self._clock = clock
        self._sleep = sleep
        # One worker thread: a write stuck in the kernel holds only it, and a new link waits for it.
        self._executor = ThreadPoolExecutor(max_workers=1, thread_name_prefix="pad-display")
        self._busy: asyncio.Future | None = None
        self._warned: set[str] = set()
        self.protocol_version: int | None = None  # what the pad's hello reported; None while the link is down

    def _warn_once(self, message: str) -> None:
        if message in self._warned:
            logger.debug(message)
            return
        self._warned.add(message)
        logger.warning(message)

    def _enabled(self) -> bool:
        # D6: `operator_pad.enabled` off includes the display (UC-016 Pre-1).
        return self._config.display_enabled() and self._config.enabled()

    async def _io(self, deadline: float, fn, *args):
        """``fn`` on the worker thread; give up after ``deadline`` s (the call itself may run on there)."""
        loop = asyncio.get_running_loop()
        future = loop.run_in_executor(self._executor, fn, *args)
        self._busy = future
        return await asyncio.wait_for(asyncio.shield(future), timeout=deadline)

    def _io_busy(self) -> bool:
        return self._busy is not None and not self._busy.done()

    def _close(self, handle: hidraw.Handle) -> None:
        """Queued on the worker thread, behind a call still stuck there: never closed under it."""
        self._executor.submit(_close_quietly, handle)

    async def _open(self, deadline: float, path: str) -> hidraw.Handle:
        """The backend's open; a handle it returns after the caller gave up is closed on the worker."""
        lock = threading.Lock()
        state: dict[str, object] = {}

        def open_or_close() -> hidraw.Handle | None:
            handle = self._backend.open(path)
            with lock:
                if state.get("abandoned"):
                    _close_quietly(handle)
                    return None
                state["handle"] = handle
            return handle

        try:
            return await self._io(deadline, open_or_close)
        except BaseException:
            with lock:
                state["abandoned"] = True
                late = state.pop("handle", None)
            if late is not None:  # opened just as the wait ended: its result is lost here
                self._close(late)
            raise

    async def run(self, stop: asyncio.Event) -> None:
        poll = asyncio.create_task(self._poll(stop))
        try:
            while not stop.is_set():
                handle = None
                try:
                    handle = await self._attach()
                    if handle is not None:
                        await self._serve(handle, stop)
                except Exception as e:  # a failed write, a timeout, an error reply, a bug in a screen
                    self._warn_once(f"Pad display link down: {e!r}; retrying at the device rescan interval")
                finally:
                    self.model.link_down()
                    self.model.set_protocol(None)
                    self.protocol_version = None
                    if handle is not None:
                        self._close(handle)
                # A1 step 2: retry at the rescan interval, never in a busy loop.
                await _wait(stop, self._rescan_seconds)
        finally:
            poll.cancel()
            with contextlib.suppress(asyncio.CancelledError, Exception):
                await poll
            self._executor.shutdown(wait=False)

    async def _attach(self) -> hidraw.Handle | None:
        if not self._enabled() or self._io_busy():
            return None
        ids = self._config.device()
        if ids is None:
            return None
        path = hidraw.find_display_node(ids[0], ids[1], self._sysfs_root, self._dev_dirs)
        if path is None:
            self._warn_once(f"No raw HID node of {ids[0]}:{ids[1]} found: pad display off")
            return None
        io_timeout = self._reply_timeout + IO_MARGIN_SECONDS
        handle = await self._open(io_timeout, path)
        try:
            await self._io(io_timeout, handle.drain)
            version = await self._io(io_timeout, hidraw.hello, handle, self._reply_timeout)
        except BaseException:
            self._close(handle)
            raise
        if version < hidraw.PROTOCOL_VERSION:
            self._warn_once(f"Pad firmware on {path} answers display protocol {version}: pad display off")
            self._close(handle)
            return None
        # BR-002: each feature only when the hello reports its version (the model draws by it).
        self.protocol_version = version
        self.model.display_usb = hidraw.usb_device(self._sysfs_root / Path(path).name / "device")
        self.model.set_protocol(version)
        self._warned.clear()
        logger.info(f"Pad display link up on {path} (protocol {version})")
        return handle

    async def _serve(self, handle: hidraw.Handle, stop: asyncio.Event) -> None:
        frame_timeout = (LINES + 4) * self._reply_timeout + IO_MARGIN_SECONDS  # 8 lines, a bar or 3 slots, a show
        show_timeout = self._reply_timeout + IO_MARGIN_SECONDS
        model = self.model
        last: Screen | None = None  # after every hello: a full redraw
        last_show = 0.0
        seq = 0
        while not stop.is_set() and self._enabled():
            model.dirty.clear()  # before current(): a hook during the write wakes the next round
            screen = model.current(self._clock())
            if screen != last:
                seq = (seq + 1) & 0xFF
                try:
                    await self._io(frame_timeout, hidraw.send_frame, handle, screen, seq, self._reply_timeout)
                except hidraw.FeatureRefusedError as e:
                    # BR-002: a refused bar or wheel never drops the link: a lower layout for this link.
                    self._warn_once(f"Pad refused display op {e.op:#04x}: drawing a lower layout on this link")
                    model.layout_refused(e.op)
                    last = None
                    continue
                last, last_show = screen, self._clock()
                model.acknowledged()
            elif self._clock() - last_show >= self._heartbeat:
                seq = (seq + 1) & 0xFF
                await self._io(show_timeout, hidraw.send_show, handle, seq, self._reply_timeout)
                last_show = self._clock()
                model.acknowledged()
            now = self._clock()
            timeout = self._heartbeat - (now - last_show)
            expiry = model.next_change(now)
            if expiry is not None:
                timeout = min(timeout, expiry - now)
            await _wait(model.dirty, timeout)
        logger.info("Pad display off (operator_pad.display_enabled or operator_pad.enabled)")

    async def _poll(self, stop: asyncio.Event) -> None:
        """BR-003: `GetMenuStatus` every `operator_pad.display_poll_ms` while the link is up."""
        connected = True
        while not stop.is_set():
            try:
                if self.model.link_up:
                    token = self.model.read_token()
                    status = await self._reader.fetch()
                    self.model.note_status(status, token)
                    if (status is not None) != connected:
                        connected = status is not None
                        if connected:
                            logger.info("GetMenuStatus answers again: pad display shows the status")
                        else:
                            logger.warning("GetMenuStatus failed: pad display shows NO CONNECTION")
            except Exception as e:
                self._warn_once(f"Pad display poll failed: {e!r}")
            await self._sleep(self._config.display_poll_ms() / 1000)
