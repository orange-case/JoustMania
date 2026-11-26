"""The lobby's warning before a planned Bluetooth restart (UC-001 BR-011, UC-002 A9, BR-011).

The pairing daemon asks for it through ``MenuService.WarnBluetoothRestart`` just before it
restarts bluetoothd, which drops every controller. The menu owns the lobby's audio and LEDs,
so it plays the cue and tells the daemon how long to wait.
"""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass
from typing import TYPE_CHECKING

from lib.colors import Colors
from lib.types import Sound
from proto import controller_manager_pb2
from services.menu.handlers.base import ControllerState
from services.menu.utils.detached import run_detached

if TYPE_CHECKING:
    from services.menu.state_manager import StateManager

logger = logging.getLogger(__name__)

WARNING_LEAD_MS = 3000
WARNING_SOUND = Sound.SFX_BEEP_LOUD
# IDLE controllers keep their LED off (UC-002 A3); they still hear the beep.
_PULSE_STATES = frozenset({ControllerState.CONNECTED, ControllerState.READY})


@dataclass(frozen=True)
class WarningAnswer:
    warned: bool
    game_live: bool
    connected_count: int
    lead_ms: int


class BluetoothRestartWarner:
    def __init__(self, state_manager: StateManager):
        self._state_manager = state_manager
        self._tasks: set[asyncio.Task] = set()

    def warn(self, *, menu_running: bool, lifecycle_live: bool) -> WarningAnswer:
        """Start the cue detached when the lobby runs with a controller connected; answer at once."""
        states = dict(self._state_manager.controller_states)
        connected = len(states)
        if lifecycle_live:
            logger.info("Bluetooth restart warning refused: a game is live")
            return WarningAnswer(warned=False, game_live=True, connected_count=connected, lead_ms=0)
        if not menu_running or not connected:
            return WarningAnswer(warned=False, game_live=False, connected_count=connected, lead_ms=0)
        pulse = sorted(serial for serial, state in states.items() if state in _PULSE_STATES)
        logger.info(f"Warning the lobby of a Bluetooth restart ({connected} controllers connected)")
        run_detached(self._state_manager.audio.play_sound(WARNING_SOUND), tasks=self._tasks, what="Restart beep")
        run_detached(self._pulse(pulse), tasks=self._tasks, what="Restart pulse")
        return WarningAnswer(warned=True, game_live=False, connected_count=connected, lead_ms=WARNING_LEAD_MS)

    async def _pulse(self, serials: list[str]) -> None:
        led = self._state_manager.led
        for serial in serials:
            try:
                await led.send_game_effect(
                    serial,
                    controller_manager_pb2.GAME_EFFECT_PULSE,
                    color=Colors.White.value,
                    duration_ms=WARNING_LEAD_MS,
                )
            except Exception as e:
                logger.warning(f"Restart pulse for {serial} failed: {e}")
