"""The lobby's warning before a planned Bluetooth restart (UC-001 BR-011, UC-002 A9, BR-011).

The pairing daemon asks for it through ``MenuService.WarnBluetoothRestart`` just before it
restarts bluetoothd, which drops every controller. The menu owns the lobby's audio and LEDs,
so it plays the cue and tells the daemon how long to wait.
"""

from __future__ import annotations

import logging
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import TYPE_CHECKING

from lib.colors import Colors
from lib.types import Sound
from proto import controller_manager_pb2

if TYPE_CHECKING:
    from services.menu.state_manager import StateManager

logger = logging.getLogger(__name__)

WARNING_LEAD_MS = 3000
WARNING_SOUND = Sound.SFX_BEEP_LOUD


@dataclass(frozen=True)
class WarningAnswer:
    warned: bool
    game_live: bool
    connected_count: int
    lead_ms: int


class BluetoothRestartWarner:
    def __init__(
        self,
        state_manager: StateManager,
        effect_lobby: Callable[[int, tuple[int, int, int], int], Awaitable[None]],
    ):
        self._state_manager = state_manager
        # SettingsOps._effect_lobby: CONNECTED and READY only, IDLE keeps its LED off (UC-002 A3)
        self._effect_lobby = effect_lobby

    async def warn(self, *, menu_running: bool, game_live: bool, check_only: bool) -> WarningAnswer:
        """Play the cue when the lobby runs with a controller connected; ``check_only`` never plays.

        Both sends only queue (the sound RPC is detached, the pulse goes on the button stream),
        so the daemon gets its answer well inside its 2 s timeout.
        """
        connected = len(self._state_manager.controller_states)
        if game_live:
            if not check_only:
                logger.info("Bluetooth restart warning refused: a game is live")
            return WarningAnswer(warned=False, game_live=True, connected_count=connected, lead_ms=0)
        if check_only or not menu_running or not connected:
            return WarningAnswer(warned=False, game_live=False, connected_count=connected, lead_ms=0)
        logger.info(f"Warning the lobby of a Bluetooth restart ({connected} controllers connected)")
        await self._state_manager.audio.play_sound(WARNING_SOUND)
        await self._effect_lobby(controller_manager_pb2.GAME_EFFECT_PULSE, Colors.White.value, WARNING_LEAD_MS)
        return WarningAnswer(warned=True, game_live=False, connected_count=connected, lead_ms=WARNING_LEAD_MS)
