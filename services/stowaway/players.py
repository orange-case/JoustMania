"""Players mode: a pattern game on reserved mock controllers, only while no real match runs (UC-106 A2, BR-005)."""

from __future__ import annotations

import asyncio
import contextlib
import json
import logging
from collections.abc import Awaitable, Callable

from opentelemetry import trace

from proto import controller_manager_mock_pb2 as mock_pb2
from proto import game_coordinator_pb2 as gc_pb2
from services.stowaway.config import SettingsSource
from services.stowaway.match import TERMINAL_EVENTS
from services.stowaway.synthetic import SYNTHETIC_ATTR

logger = logging.getLogger(__name__)

TAG = "stowaway"
GAME_NAME = "JoustFFA"
SPAN_GAME = "stowaway.players.game"
# A copy of the agent.json `experiment.mock_archetype` variant (tests/contract keeps them equal).
ARCHETYPE = "quick_cascade"
ARCHETYPE_PARAMS = {
    "players": 3,
    "stream_rate_hz": 30,
    "base": 1.0,
    "drift": 0.02,
    "amp": 0.05,
    "period_s": 3.0,
    "phase": 0.0,
    "burst_h": 0.0,
    "burst_period_s": 0.0,
    "d_base": 0.0,
    "d_drift": 0.03,
    "d_amp": 0.0,
    "d_phase": 0.25,
}
PAUSE_S = 10.0
WATCH_S = 0.2
PRIMARY_CHECK_S = 1.0
RPC_TIMEOUT_S = 5.0

STOP_GAME_END = "game_end"
STOP_REAL_MATCH = "real_match"
STOP_KILL_SWITCH = "kill_switch"
STOP_ERROR = "error"

_BUSY_STATES = frozenset({gc_pb2.STARTING, gc_pb2.RUNNING})


class PatternPlayers:
    def __init__(
        self,
        coordinator,
        mock,
        settings: SettingsSource,
        on_event: Callable[[object], None] | None = None,
        on_end: Callable[[], Awaitable[None]] | None = None,
        tracer: trace.Tracer | None = None,
        pause_s: float = PAUSE_S,
        watch_s: float = WATCH_S,
        primary_check_s: float = PRIMARY_CHECK_S,
    ) -> None:
        self._gc = coordinator
        self._mock = mock
        self._settings = settings
        self._on_event = on_event
        self._on_end = on_end
        self._tracer = tracer or trace.get_tracer("stowaway")
        self._pause_s = pause_s
        self._watch_s = watch_s
        self._primary_check_s = primary_check_s

    async def real_match_running(self) -> bool:
        """Fails closed: an unreachable coordinator counts as a running real match."""
        try:
            resp = await self._gc.GetGameState(gc_pb2.GetGameStateRequest(), timeout=RPC_TIMEOUT_S)
        except Exception:
            return True
        return resp.success and resp.game_info.state in _BUSY_STATES

    async def run(self, stop: asyncio.Event) -> None:
        while not stop.is_set():
            if self._settings.get().players_on() and not await self.real_match_running():
                await self.play_one(stop)
                await _wait(stop, self._pause_s)
            else:
                await _wait(stop, self._watch_s)

    async def play_one(self, stop: asyncio.Event | None = None) -> str:
        span = self._tracer.start_span(SPAN_GAME, attributes={SYNTHETIC_ATTR: True, "stowaway.archetype": ARCHETYPE})
        serials: list[str] = []
        game_id = ""
        reason = STOP_ERROR
        call = None
        watcher: asyncio.Task | None = None
        verdict: dict[str, str] = {}
        try:
            added = await self._mock.AddControllers(
                mock_pb2.AddControllersRequest(
                    count=ARCHETYPE_PARAMS["players"],
                    reserved=True,
                    tag=TAG,
                    archetype=ARCHETYPE,
                    archetype_params=json.dumps(ARCHETYPE_PARAMS),
                ),
                timeout=RPC_TIMEOUT_S,
            )
            serials = list(added.serials)
            if not added.success:
                logger.warning("stowaway players: AddControllers refused: %s", added.error)
                return reason
            call = self._gc.StreamGameEvents(
                gc_pb2.StreamEventsRequest(
                    start_config=gc_pb2.StartGameConfig(
                        game_name=GAME_NAME,
                        players=[gc_pb2.Player(serial=s, team=-1, alive=True) for s in serials],
                        sensitivity=2,
                        ffa_config=gc_pb2.FFAConfig(),
                    )
                )
            )
            watcher = asyncio.create_task(self._watch(call, verdict, stop))
            async for event in call:
                game_id = game_id or event.game_id
                verdict["game_id"] = game_id
                if self._on_event is not None:
                    self._on_event(event)
                if event.event_type in TERMINAL_EVENTS:
                    break
            reason = verdict.get("reason", STOP_GAME_END)
        except Exception:
            # The watcher cancels the call to end the game; the stream then fails with CANCELLED.
            if "reason" in verdict:
                reason = verdict["reason"]
            else:
                logger.warning("stowaway players: the pattern game failed", exc_info=True)
        finally:
            if watcher is not None:
                watcher.cancel()
                with contextlib.suppress(asyncio.CancelledError, Exception):
                    await watcher
            if call is not None:
                call.cancel()
            if self._on_end is not None:
                with contextlib.suppress(Exception):
                    await self._on_end()
            for serial in serials:
                with contextlib.suppress(Exception):
                    await self._mock.RemoveController(
                        mock_pb2.RemoveControllerRequest(serial=serial), timeout=RPC_TIMEOUT_S
                    )
            span.set_attribute("game.id", game_id)
            span.set_attribute("stowaway.stop_reason", reason)
            span.end()
        return reason

    async def _watch(self, call, verdict: dict[str, str], stop: asyncio.Event | None) -> None:
        """Ends the pattern game on the kill switch (within 1 s) or when a real match starts."""
        since_primary = 0.0
        while True:
            await asyncio.sleep(self._watch_s)
            since_primary += self._watch_s
            reason = None
            if not self._settings.get().players_on() or (stop is not None and stop.is_set()):
                reason = STOP_KILL_SWITCH
            elif since_primary >= self._primary_check_s:
                since_primary = 0.0
                if await self.real_match_running():
                    reason = STOP_REAL_MATCH
            if reason is None:
                continue
            verdict["reason"] = reason
            game_id = verdict.get("game_id", "")
            if game_id:
                with contextlib.suppress(Exception):
                    await self._gc.ForceEndGame(
                        gc_pb2.ForceEndGameRequest(reason=f"stowaway: {reason}", game_id=game_id), timeout=RPC_TIMEOUT_S
                    )
            call.cancel()
            return


async def _wait(stop: asyncio.Event, seconds: float) -> None:
    with contextlib.suppress(TimeoutError):
        await asyncio.wait_for(stop.wait(), seconds)
