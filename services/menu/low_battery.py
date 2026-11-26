"""The lobby's low-battery blink and the pad's LOW count (UC-002 A8, BR-009).

The idle monitor starts and stops the warner, so it runs exactly while the lobby does.
"""

from __future__ import annotations

import asyncio
import logging
import time
from collections.abc import Awaitable, Callable
from typing import TYPE_CHECKING

from opentelemetry import context as otel_context

from lib.controller_constants import battery_to_pct
from proto import controller_manager_pb2
from services.menu.handlers.base import ControllerState

if TYPE_CHECKING:
    from services.menu.state_manager import StateManager

logger = logging.getLogger(__name__)

LOW_BATTERY_PCT = 20.0
# A recovery-style reminder cadence, not a stage knob (C-035): a code constant, no flag.
LOW_BATTERY_BLINK_INTERVAL_S = 20.0
_TICK_S = 1.0
# IDLE controllers have their LED off to save battery (UC-002 A3); a blink would undo that.
_BLINK_STATES = frozenset({ControllerState.CONNECTED, ControllerState.READY})


def is_low(level: int) -> bool:
    return battery_to_pct(level) <= LOW_BATTERY_PCT


def low_battery_serials(state_manager: StateManager) -> set[str]:
    """Connected controllers (any state) whose last known battery is low; an unreported level is not low."""
    levels = state_manager.battery_levels
    return {serial for serial in state_manager.controller_states if serial in levels and is_low(levels[serial])}


class LowBatteryWarner:
    """Sends GAME_EFFECT_LOW_BATTERY to each low CONNECTED / READY controller, at most once per interval."""

    def __init__(
        self,
        state_manager: StateManager,
        send_effect: Callable[[str, int], Awaitable[bool]],
        interval_s: float = LOW_BATTERY_BLINK_INTERVAL_S,
        clock: Callable[[], float] = time.monotonic,
    ):
        self._state_manager = state_manager
        self._send_effect = send_effect
        self._interval_s = interval_s
        self._clock = clock
        self._tick_s = _TICK_S
        self._last_blink: dict[str, float] = {}
        self._task: asyncio.Task | None = None

    @property
    def running(self) -> bool:
        return self._task is not None and not self._task.done()

    def start(self) -> None:
        if self.running:
            return
        self._task = asyncio.create_task(self._loop())

    def stop(self) -> None:
        if self._task is not None:
            self._task.cancel()
            self._task = None
        # Back in the lobby after a game, a still-low controller blinks again at once.
        self._last_blink.clear()

    async def tick(self) -> None:
        now = self._clock()
        states = self._state_manager.controller_states
        low = {s for s in low_battery_serials(self._state_manager) if states.get(s) in _BLINK_STATES}
        for serial in [s for s in self._last_blink if s not in low]:
            del self._last_blink[serial]
        for serial in sorted(low):
            last = self._last_blink.get(serial)
            if last is not None and now - last < self._interval_s:
                continue
            self._last_blink[serial] = now
            await self._send_effect(serial, controller_manager_pb2.GAME_EFFECT_LOW_BATTERY)

    async def _loop(self) -> None:
        # Detached like the idle monitor: no inherited (possibly ended) span context.
        token = otel_context.attach(otel_context.Context())
        try:
            while True:
                await asyncio.sleep(self._tick_s)
                try:
                    await self.tick()
                except Exception:
                    logger.exception("Low-battery blink failed; retrying next tick")
        except asyncio.CancelledError:  # NOSONAR - intentional: background loop exits on cancellation
            logger.debug("Low-battery warner cancelled")
        finally:
            otel_context.detach(token)
