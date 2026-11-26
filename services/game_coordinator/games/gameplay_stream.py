"""The gameplay stream of a game: open, reopen, drain, guarded writes, release.

Split out of ``BaseGameMode`` (#2633): the ``StreamGameplayData`` call, the early drain
before the additional phases, the alive filter and the reconnect path (UC-030 BR-006).
Names that tests patch on ``games.base`` (``tracer``, ``time``, ``get_config_manager``,
``record_moment``, ``inject_trace_context``) are looked up on the base module at call
time (``_base.``), as in ``games/shields.py``.
"""

import asyncio
import inspect

import grpc

from services.game_coordinator.games import base as _base
from services.game_coordinator.games.base import (
    DEAD_PLAYER_COLOR,
    logger,
)
from services.game_coordinator.stream_writes import serialized_write


class GameplayStreamMixin:
    """The gameplay stream of a game: open, reopen, drain, guarded writes, release. Mixed into ``BaseGameMode``."""

    def _init_stream_state(self) -> None:
        """The gameplay call, its early drain and the disconnects buffered before the loop."""
        # Disconnects seen by _warmup_ema(); _game_loop() runs them through the
        # death path before its first frame (UC-020 A2).
        self._warmup_disconnects: list[str] = []
        # Early stream drain (UC-021 A1, D-G10): a mode that opens its stream before
        # its additional phases reads and discards frames until the start.
        self._early_drain_task: asyncio.Task | None = None
        self._early_drain_stop = asyncio.Event()
        self._early_drain_disconnects: dict[str, None] = {}
        self.gameplay_stream = None  # Phase 46: Bidirectional stream for feedback commands
        # Deaths whose PLAYER_DEATH write failed on a dying call: the next stream sends it (UC-030 BR-006, #2471).
        self._undelivered_deaths: set[str] = set()

    async def _create_gameplay_stream(self):
        """
        Create gameplay stream, send config, and update alive filter.

        Encapsulates stream creation so it can be called for both initial
        connection and reconnection.
        """
        from proto import controller_manager_pb2

        config = _base.get_config_manager().get_config()
        update_frequency_hz = config.update_frequency_hz

        logger.info(f"Creating gameplay stream at {update_frequency_hz}Hz...")

        # Create bidirectional stream
        self.gameplay_stream = self.controller_client.StreamGameplayData()

        # Build player colors for stream init
        # A dead player's LED is off (UC-030 step 9): the config never lights it again (BR-006, #2471).
        player_colors = []
        for serial, player in self.players.items():
            r, g, b = player.color if player.alive else DEAD_PLAYER_COLOR
            player_colors.append(
                controller_manager_pb2.ControllerColorConfig(
                    serial=serial,
                    color=controller_manager_pb2.RGB(r=r, g=g, b=b),
                )
            )

        # Send initial configuration
        initial_config = controller_manager_pb2.GameplayStreamControl(
            config=controller_manager_pb2.GameplayStreamConfig(
                update_frequency_hz=update_frequency_hz,
                colors=player_colors,
            )
        )
        await serialized_write(self.gameplay_stream, initial_config)

        # Send current alive filter so reconnected stream only gets alive players
        current_alive = [s for s, p in self.players.items() if p.alive]
        if len(current_alive) < len(self.players):
            filter_msg = controller_manager_pb2.GameplayStreamControl(
                filter_update=controller_manager_pb2.FilterUpdate(serials=current_alive)
            )
            await serialized_write(self.gameplay_stream, filter_msg)

        logger.info(f"Gameplay stream created with {len(player_colors)} players ({len(current_alive)} alive)")

        # UC-040 BR-021: after a reconnect, re-send every held charge and running
        # shield with the time left, so the new stream owns the layers again.
        await self._resend_shield_layers()
        await self._resend_dead_player_state()

    def _is_mid_swap(self, serial: str) -> bool:  # noqa: ARG002
        """True for a player whose ``alive`` is False only while a swap repaints it (Swapper, #2484).

        Such a player is alive in the game: a reopened stream sends it no ``SHIELD`` 0.
        """
        return False

    async def _resend_dead_player_state(self) -> None:
        """End a dead player's feedback on a (re)created stream (UC-030 BR-006, #2471).

        A death whose ``PLAYER_DEATH`` write failed on the dying call gets it now; every other dead
        player gets ``GAME_EFFECT_SHIELD`` 0, which ends any shield, charge or hold
        layer the old stream left running. The first stream of a game has no dead player.
        """
        from proto import controller_manager_pb2

        for serial, player in self.players.items():
            if player.alive or self._is_mid_swap(serial):
                continue
            if serial in self._undelivered_deaths:
                effect, duration_ms = controller_manager_pb2.GAME_EFFECT_PLAYER_DEATH, 0
            else:
                effect, duration_ms = controller_manager_pb2.GAME_EFFECT_SHIELD, 0
            trace_parent, trace_state = _base.inject_trace_context(player.span)
            await serialized_write(
                self.gameplay_stream,
                controller_manager_pb2.GameplayStreamControl(
                    game_effect=controller_manager_pb2.GameEffectCommand(
                        serial=serial,
                        effect=effect,
                        duration_ms=duration_ms,
                        trace_parent=trace_parent,
                        trace_state=trace_state,
                    )
                ),
            )
        self._undelivered_deaths.clear()

    async def _start_gameplay_stream(self):
        """
        Create and configure the gameplay stream.

        Called before countdown to allow EMA warmup during countdown phase.
        The stream is stored in self.gameplay_stream for use by _game_loop().
        """
        await self._create_gameplay_stream()

    def _opens_stream_before_additional_phases(self) -> bool:
        """Whether run() opens the gameplay stream before the additional phases.

        A mode whose phases send effects over the stream (Random Teams formation
        pulse, UC-021 A1; Traitor signal, UC-022 BR-003) returns True. The stream
        config carries the player colours as they are before the phases; a mode
        that sets them later (Traitor) writes them as base colours over the
        stream, and a reopen after the phases carries the final ones. Default False: the stream
        opens after the phases and the instructions, as for FFA, Teams and Swapper,
        whose colour_assignment phase sets the colours the config sends.
        """
        return False

    def _start_early_stream_drain(self) -> None:
        """Read and discard frames of the early-opened stream (UC-021 A1).

        Without a reader the server fills the HTTP/2 window with formation motion,
        which the 100 ms EMA warmup would not clear before death detection.
        """
        self._early_drain_stop.clear()
        self._early_drain_disconnects = {}
        self._early_drain_task = self._track_task(asyncio.create_task(self._drain_early_stream()))

    async def _drain_early_stream(self) -> bool:
        """Discard frames until told to stop, keeping disconnects (UC-020 A2).

        A controller whose frames come back after its disconnect is dropped from
        the pending list, as the controller manager does for a drop and reconnect
        before a stream opens (UC-002 BR-004, #1607). It stops with ``break`` after
        the next frame: cancelling a grpc.aio read cancels the whole call, which
        would end the game at the start.

        Returns True only when it stopped on request with the call still usable.
        """
        frames = 0
        stopped = False
        try:
            async for gameplay_update in self.gameplay_stream:
                frames += 1
                for disconnect in gameplay_update.disconnects:
                    self._early_drain_disconnects.pop(disconnect.serial, None)
                    self._early_drain_disconnects[disconnect.serial] = None
                for controller_data in gameplay_update.controllers:
                    self._early_drain_disconnects.pop(controller_data.serial, None)
                if self._early_drain_stop.is_set():
                    stopped = True
                    break
        except Exception as e:
            # grpc.aio keeps one reader per call: after an error it yields nothing
            # more, so _stop_early_stream_drain() opens a new call.
            logger.warning(f"Early stream drain interrupted: {e}")
        logger.info(f"Early stream drain stopped: discarded {frames} frames")
        return stopped

    async def _stop_early_stream_drain(self, reopen: bool = True) -> None:
        """Stop the drain where the stream used to open, and keep its disconnects.

        Waits for the drain's next frame; only a drain that gets none within
        ``_EARLY_DRAIN_STOP_TIMEOUT_S`` is cancelled. The drain has finished when this returns, so it
        never reads alongside ``_warmup_ema()``.

        A drain that ended by a stream error, by the stream ending, or by the
        timeout (which cancels the call) leaves a call that yields no more frames.
        When ``reopen`` is set and the game still runs, the stream is then opened
        once more, as it would open here without the early stream; a failure of
        that open raises. The collected disconnects are kept either way.
        """
        task, self._early_drain_task = self._early_drain_task, None
        if task is None:
            return
        self._early_drain_stop.set()
        stopped = False
        try:
            stopped = await asyncio.wait_for(task, timeout=self._EARLY_DRAIN_STOP_TIMEOUT_S)
        except TimeoutError:
            logger.warning(f"Early stream drain got no frame in {self._EARLY_DRAIN_STOP_TIMEOUT_S}s: cancelled")
        pending, self._early_drain_disconnects = self._early_drain_disconnects, {}
        self._warmup_disconnects.extend(pending)
        if reopen and self.running and (not stopped or self._gameplay_call_done()):
            await self._reopen_gameplay_stream("the early stream ended during the formation")

    def _gameplay_call_done(self) -> bool:
        """Whether the gameplay call has finished (test fakes without ``done()`` never do)."""
        done = getattr(self.gameplay_stream, "done", None)
        return bool(done()) if callable(done) else False

    def _cancel_gameplay_call(self) -> None:
        """Cancel the gameplay call, if it has a ``cancel()`` (a no-op on a finished call)."""
        cancel = getattr(self.gameplay_stream, "cancel", None)
        if callable(cancel):
            cancel()

    async def _write_end_effect(self, message) -> bool:
        """Write an end-of-game effect (winner rainbow, loser colour) while the gameplay call is open.

        The controller manager can end the call after the win, before teardown
        (UC-030 BR-006): the effect is skipped with a warning and the game keeps
        its outcome, never ``game_error`` (UC-020 BR-003, #1956). grpc.aio rejects
        a write on a finished call with ``InvalidStateError`` and raises
        ``AioRpcError`` when the call fails under the write. A cancel of this
        task still propagates. Returns whether the effect was written.
        """
        return await self._write_effect_guarded(message, "End-of-game effect")

    async def _write_effect_guarded(self, message, what: str) -> bool:
        """Write a gameplay effect, skipping it with a warning when the call has ended.

        Shared by ``_write_end_effect`` and in-game effects that must not abort a
        state transition (Fight Club round loser, #1345). ``what`` names the
        effect in the warning. Skips on a finished call; catches
        ``InvalidStateError`` / ``AioRpcError`` from the write; a cancel still
        propagates. Returns whether the effect was written.
        """
        stream = self.gameplay_stream
        if stream is None:
            return False
        if self._gameplay_call_done():
            logger.warning(f"{what} skipped: the gameplay stream has already ended")
            return False
        try:
            await serialized_write(stream, message)
        except (asyncio.InvalidStateError, grpc.aio.AioRpcError) as e:
            logger.warning(f"{what} skipped: the gameplay stream ended during the write ({e!r})")
            return False
        return True

    async def _gameplay_call_failed(self) -> bool:
        """Whether the gameplay call has finished with a non-OK gRPC status (UC-030 BR-006).

        A call that ended with OK, a live call, and a test fake without
        ``done()`` / ``code()`` are not failed.
        """
        if not self._gameplay_call_done():
            return False
        code = getattr(self.gameplay_stream, "code", None)
        if not callable(code):
            return False
        status = code()
        if inspect.isawaitable(status):
            status = await status
        return isinstance(status, grpc.StatusCode) and status != grpc.StatusCode.OK

    async def _reopen_gameplay_stream(self, reason: str) -> None:
        """Open a new gameplay call, then close the old one (UC-030 BR-006, #1913).

        The new call is in ``self.gameplay_stream`` before the old one is
        cancelled, so a concurrent effect write never sees a missing stream. A
        failed open raises; the old call is closed either way.
        """
        logger.warning(f"Reopening the gameplay stream: {reason}")
        # UC-030 BR-008: no drain crosses a reconnect.
        self._force_release_shield_holds()
        old_stream = self.gameplay_stream
        try:
            await self._create_gameplay_stream()
        finally:
            cancel = getattr(old_stream, "cancel", None)
            if old_stream is not self.gameplay_stream and callable(cancel):
                cancel()

    def _release_gameplay_stream(self) -> None:
        """Close the gameplay call and drop it (UC-030 BR-006, #1914). Idempotent.

        Without the cancel the call stays open until the garbage collector finds
        it, and the controller manager keeps the game's roster until then (UC-004).
        """
        self._cancel_gameplay_call()
        self.gameplay_stream = None

    async def _send_alive_filter_update(self, last_alive_serials: set[str]) -> set[str]:
        """
        Check if alive players changed and send filter update if so.

        Args:
            last_alive_serials: The previous set of alive player serials

        Returns:
            The current set of alive player serials
        """
        from proto import controller_manager_pb2

        current_alive_serials = {p.serial for p in self.players.values() if p.alive}

        if current_alive_serials != last_alive_serials:
            filter_msg = controller_manager_pb2.GameplayStreamControl(
                filter_update=controller_manager_pb2.FilterUpdate(serials=list(current_alive_serials))
            )
            await serialized_write(self.gameplay_stream, filter_msg)

            logger.info(
                f"Updated controller filter: {len(last_alive_serials)} → {len(current_alive_serials)} alive players"
            )

        return current_alive_serials

    async def _handle_stream_reconnection(self, error: Exception, attempt: int, max_attempts: int) -> int:
        """
        Handle gameplay stream reconnection after an error.

        Args:
            error: The exception that caused the stream failure
            attempt: Current reconnection attempt number (pre-incremented)
            max_attempts: Maximum allowed reconnection attempts

        Returns:
            The new attempt number after reconnection

        Raises:
            Exception: If max reconnection attempts exceeded
        """
        if attempt > max_attempts:
            logger.error(f"Gameplay stream failed after {max_attempts} reconnect attempts, ending game")
            raise error

        backoff = attempt * self._RECONNECT_BACKOFF_S  # Linear backoff: 1s, 2s, 3s
        logger.warning(
            f"Gameplay stream error (attempt {attempt}/{max_attempts}): {error}. Reconnecting in {backoff}s..."
        )
        await asyncio.sleep(backoff)
        if not self.running:
            # Force-ended during the backoff: the outer loop ends, no reopen (UC-030 BR-006).
            return attempt

        await self._reopen_gameplay_stream(f"game loop reconnect {attempt}/{max_attempts}")
        return attempt
