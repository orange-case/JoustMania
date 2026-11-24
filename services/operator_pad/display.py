"""The pad's display (UC-016): what to show (`DisplayModel`) and the link that shows it (`PadDisplay`).

The key path (`PadController.on_code`, `Actions`) only calls the model's synchronous hooks: they
set fields and wake the link, and never raise. Every hidraw call runs on the display's own worker
thread, so a hung or failing write never delays or drops a key event (FR-240, A1).

Only one screen per service: with several pads attached, the first raw node of the matching
device gets the screen. The link takes the raw HID node from whoever else holds it only by
sharing it (every opener gets every report): with the Vial app open on the same host the
display goes off and on as their replies cross.
"""

from __future__ import annotations

import asyncio
import contextlib
import functools
import logging
import time
from collections.abc import Awaitable, Callable
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path

from lib.setting_ladders import VOLUME_LADDER, clamp_step
from services.operator_pad import hidraw
from services.operator_pad.config import PadConfig
from services.operator_pad.device import RESCAN_SECONDS
from services.operator_pad.screens import (
    LINES,
    MAXIMUM,
    MINIMUM,
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

    # --- link state -------------------------------------------------------------------------

    @property
    def link_up(self) -> bool:
        """BR-004: a show was acknowledged within LINK_FRESH_SECONDS."""
        try:
            return self._last_ack is not None and self._clock() - self._last_ack < LINK_FRESH_SECONDS
        except Exception:
            return False

    def acknowledged(self) -> None:
        self._last_ack = self._clock()

    def link_down(self) -> None:
        self._last_ack = None

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
    def layer_step(self, key: str, option: str, press_status, net: int, lobby_only: bool) -> None:
        self._layer = _Layer(key, option, press_status, net, lobby_only)
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
        self.dirty.set()

    # --- volume -----------------------------------------------------------------------------

    @_quiet
    def volume_turn(self, pending_steps: int) -> None:
        """A knob turn with no layer key: the variant the turns not yet committed would land on."""
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
            if change[0] == "steps":
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

    # --- rendering --------------------------------------------------------------------------

    def _show_overlay(self, overlay: tuple, seconds: float) -> None:
        self._overlay = overlay
        self._overlay_until = self._clock() + seconds
        self.dirty.set()

    def next_change(self, now: float) -> float | None:
        """When the screen changes by itself (an overlay expires), or None."""
        if self._overlay is not None and self._overlay_until > now:
            return self._overlay_until
        return None

    def current(self, now: float) -> Screen:
        """BR-001: the setting screen while its layer key is held, else an overlay, else the status."""
        layer = self._layer
        if layer is not None and self._hold == layer.key:
            return setting_screen(layer.option, layer.press_status, layer.net, lobby_only=layer.lobby_only)
        if self._overlay is not None and now >= self._overlay_until:
            self._overlay = None
        if self._overlay is not None:
            if self._overlay[0] == "volume":
                return volume_screen(self._predict(self._vol_pending), limit=self._vol_limit)
            return result_screen(self._overlay[1], self._overlay[2])
        return status_screen(
            self._status, connection_ok=self._connection_ok, locked=self._locked, version=self._version
        )


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
        handle = await self._io(io_timeout, self._backend.open, path)
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
        # A later protocol keeps the v1 ops: the v1 screens are drawn; the version is kept for a later layout.
        self.protocol_version = version
        self._warned.clear()
        logger.info(f"Pad display link up on {path} (protocol {version})")
        return handle

    async def _serve(self, handle: hidraw.Handle, stop: asyncio.Event) -> None:
        frame_timeout = (LINES + 1) * self._reply_timeout + IO_MARGIN_SECONDS  # 8 lines and a show
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
                await self._io(frame_timeout, hidraw.send_frame, handle, screen, seq, self._reply_timeout)
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
