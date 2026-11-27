"""stowaway's main loop: follow the primary game, run the challenge per match, run players mode (UC-106)."""

from __future__ import annotations

import asyncio
import contextlib
import logging

from proto import game_coordinator_pb2 as gc_pb2
from services.stowaway.challenges import Run
from services.stowaway.config import MODE_BOTH, MODE_TELEMETRY, SettingsSource
from services.stowaway.match import Match, MatchTracker
from services.stowaway.players import PatternPlayers

logger = logging.getLogger(__name__)

RECONNECT_S = 2.0


class Stowaway:
    def __init__(
        self,
        settings: SettingsSource,
        coordinator,
        generators: dict,
        mock=None,
        reconnect_s: float = RECONNECT_S,
    ) -> None:
        self._settings = settings
        self._gc = coordinator
        self._generators = generators
        self._reconnect_s = reconnect_s
        # Primary game: telemetry and both; stowaway's own pattern game: both only.
        self.primary = MatchTracker(self._run_challenge, lambda: self._wants(MODE_TELEMETRY, MODE_BOTH))
        self.pattern = MatchTracker(self._run_challenge, lambda: self._wants(MODE_BOTH))
        self.players = (
            PatternPlayers(coordinator, mock, settings, on_event=self.pattern.feed, on_end=self.pattern.close)
            if mock is not None
            else None
        )

    def _wants(self, *modes: str) -> bool:
        settings = self._settings.get()
        return settings.telemetry_on() and settings.mode in modes

    async def _run_challenge(self, match: Match) -> None:
        name = self._settings.get().challenge
        generator = self._generators.get(name)
        if generator is None:
            return
        reason = await generator.run(Run(name, match, self._settings))
        logger.info("stowaway: %s in game %s stopped: %s", name, match.game_id, reason)

    async def follow_primary(self, stop: asyncio.Event) -> None:
        """Consume the primary game's events until ``stop``; reconnects after a stream error."""
        while not stop.is_set():
            call = self._gc.StreamGameEvents(gc_pb2.StreamEventsRequest())
            stopper = asyncio.create_task(stop.wait())
            try:
                reader = asyncio.create_task(self._read(call))
                await asyncio.wait({reader, stopper}, return_when=asyncio.FIRST_COMPLETED)
                if not reader.done():
                    call.cancel()
                with contextlib.suppress(asyncio.CancelledError, Exception):
                    await reader
            finally:
                stopper.cancel()
                await self.primary.close()
            if not stop.is_set():
                with contextlib.suppress(TimeoutError):
                    await asyncio.wait_for(stop.wait(), self._reconnect_s)

    async def _read(self, call) -> None:
        try:
            async for event in call:
                self.primary.feed(event)
        except asyncio.CancelledError:
            raise
        except Exception as e:
            logger.info("stowaway: game event stream ended (%s); reconnecting", e)

    async def run(self, stop: asyncio.Event) -> None:
        tasks = [asyncio.create_task(self.follow_primary(stop))]
        if self.players is not None:
            tasks.append(asyncio.create_task(self.players.run(stop)))
        try:
            await asyncio.gather(*tasks)
        finally:
            for task in tasks:
                task.cancel()
            await self.pattern.close()
