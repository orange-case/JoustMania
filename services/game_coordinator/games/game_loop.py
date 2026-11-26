"""The gameplay phase of a game: countdown, warm-up, frame loop, win hold.

Tests patch ``tracer``, ``time``, ``get_config_manager``, ``record_moment`` and
``inject_trace_context`` on ``games.base``: read them as ``_base.<name>``.
"""

import asyncio
import contextlib
import statistics
from collections.abc import Iterator
from dataclasses import dataclass, field
from typing import Any

from opentelemetry import context as otel_context
from opentelemetry import trace

from lib.types import GameEvent, Sound
from services.game_coordinator import game_clock, metrics
from services.game_coordinator.games import base as _base
from services.game_coordinator.games.base import (
    _MSG_COUNTDOWN_INTERRUPTED,
    COUNTDOWN_BEEP_COUNT,
    GameState,
    logger,
)
from services.game_coordinator.runtime_config import GamePerformanceConfig
from services.game_coordinator.stream_writes import serialized_write


@dataclass(slots=True)
class _LoopState:
    """The mutable state of one ``_game_loop`` run (frame timing, reconnects, alive set)."""

    loop_start_time: float
    last_iteration_time: float
    target_frame_time_ms: float
    last_alive_serials: set[str]
    loop_iterations: int = 0
    recent_frame_times: list[float] = field(default_factory=list)  # for jitter calculation
    frames_on_target: int = 0  # Frames within 50% of target time
    # Stream gameplay data and process game logic with reconnection
    max_reconnect_attempts: int = 3
    reconnect_attempt: int = 0
    frames_since_reconnect: int = 0
    frame_failed: bool = False


class GameLoopMixin:
    """The gameplay phase driver of a game: countdown, warm-up, frame loop, win hold. Mixed into ``BaseGameMode``."""

    async def _countdown(self):
        """Run countdown before game starts using unified countdown effect.

        Sends per-controller countdown effects so each controller.effect.countdown span
        appears under the corresponding player's countdown_phase span.
        Sound spans live under gameplay_phase (shared, not player-specific).
        """
        from proto import controller_manager_pb2

        # Get phase duration from runtime config (controlled via flagd game_settings)
        # phase_duration_ms == 0 means skip countdown entirely
        config = _base.get_config_manager().get_config()
        phase_duration_ms = config.countdown_phase_duration_ms

        logger.info(f"Starting countdown (phase_duration={phase_duration_ms}ms)...")
        await self.event_publisher(GameEvent.COUNTDOWN_START, {"phase_duration_ms": phase_duration_ms})

        if not self.running:
            logger.info(_MSG_COUNTDOWN_INTERRUPTED)
            return

        # Skip countdown entirely if phase duration is 0 (the "skip" variant)
        if phase_duration_ms == 0:
            logger.info("Countdown skipped (phase_duration_ms=0)")
            await self.event_publisher(GameEvent.COUNTDOWN_END, {})
            return

        # Send per-controller countdown effects via gameplay stream.
        # Each effect carries its player's countdown_phase span context so
        # controller.effect.countdown becomes a child of the correct countdown_phase.
        if self.gameplay_stream:
            for player in self.players.values():
                trace_parent, trace_state = _base.inject_trace_context(player._countdown_span)
                effect_cmd = controller_manager_pb2.GameplayStreamControl(
                    game_effect=controller_manager_pb2.GameEffectCommand(
                        serial=player.serial,
                        effect=controller_manager_pb2.GAME_EFFECT_COUNTDOWN,
                        duration_ms=phase_duration_ms,
                        trace_parent=trace_parent,
                        trace_state=trace_state,
                    )
                )
                await serialized_write(self.gameplay_stream, effect_cmd)

        # Play countdown beeps in sync with the visual countdown.
        # Sounds are shared (not per-player), so they inherit the active
        # gameplay_phase span as their parent — not any player's countdown_phase.
        beep_count = COUNTDOWN_BEEP_COUNT
        beep_interval_ms = phase_duration_ms  # Match LED phase duration

        for _ in range(beep_count):
            if not self.running:
                logger.info(_MSG_COUNTDOWN_INTERRUPTED)
                return

            # Play countdown beep — inherits gameplay_phase as parent span
            await self._play_sound(Sound.SFX_BEEP_LOUD, priority=2)

            # Wait for beep interval (configurable based on countdown duration)
            wait_iterations = beep_interval_ms // 50  # 50ms per iteration
            for _ in range(wait_iterations):
                if not self.running:
                    logger.info(_MSG_COUNTDOWN_INTERRUPTED)
                    return
                await game_clock.sleep(self._COUNTDOWN_BEEP_POLL_S)

        # Play start sound — inherits gameplay_phase as parent span
        await self._play_sound(Sound.SFX_START3, priority=2)

        await self.event_publisher(GameEvent.COUNTDOWN_END, {})
        logger.info("Countdown complete")

    async def _warmup_ema(self):
        """
        UC-030 BR-003: the startup guard's EMA warm-up (about 100 ms of buffered frames).

        Drain buffered stream data and prime EMA filters before game loop.

        The gameplay stream opens ~2.25s before the game loop starts (before
        countdown). Data buffered during that time can contain stale spikes
        that cause instant deaths when the EMA is uninitialized. This method
        reads and discards buffered frames while priming each player's EMA
        with real sensor data — no death checks run.

        Disconnect events are one-shot (controller-manager drains them once),
        so they are kept in ``_warmup_disconnects`` for ``_game_loop()`` to
        handle instead of being dropped with the frame (UC-020 A2, #1582).

        A stream that ends, fails or stalls here is opened once more and the
        warm-up repeats once (UC-030 BR-006, #1913); a second failure, and a
        reopen that fails to open, are left to the game loop's reconnect.
        """
        if await self._read_warmup_frames():
            return
        if self.running and self._gameplay_call_done():
            try:
                await self._reopen_gameplay_stream("the stream ended during the countdown or the EMA warm-up")
            except Exception as e:
                logger.warning(f"EMA warmup could not reopen the stream, left to the game loop: {e}")
                return
            await self._read_warmup_frames()

    async def _read_warmup_frames(self) -> bool:
        """Prime the EMA from the stream for ``_WARMUP_DURATION_S``.

        Returns True only when it read for the full duration. A read that gets
        no frame for ``_WARMUP_STALL_TIMEOUT_S`` is cancelled, which cancels the
        gRPC call (the call is done afterwards).
        """
        warmup_start = _base.time.monotonic()
        frames_consumed = 0
        completed = False

        async def read() -> bool:
            nonlocal frames_consumed
            async for gameplay_update in self.gameplay_stream:
                self._warmup_disconnects.extend(d.serial for d in gameplay_update.disconnects)
                for controller_data in gameplay_update.controllers:
                    serial = controller_data.serial
                    if serial in self.players and self.players[serial].alive:
                        accel_mag = self._compute_accel_magnitude(controller_data.accel)
                        self._update_ema(self.players[serial], accel_mag, self._ema_weight)
                frames_consumed += 1

                if _base.time.monotonic() - warmup_start >= self._WARMUP_DURATION_S:
                    return True
            return False

        try:
            completed = await asyncio.wait_for(read(), timeout=self._WARMUP_DURATION_S + self._WARMUP_STALL_TIMEOUT_S)
        except TimeoutError:
            logger.warning(f"EMA warmup got no frame in {self._WARMUP_STALL_TIMEOUT_S}s: cancelled the stream read")
        except Exception as e:
            logger.warning(f"EMA warmup interrupted: {e}")

        logger.info(f"EMA warmup complete: drained {frames_consumed} buffered frames")
        return completed

    async def _process_gameplay_update(self, gameplay_update) -> bool:
        """
        Process a single gameplay update: handle disconnects then run death detection.

        Disconnect events are processed first so that a disconnected player is
        killed before any remaining controller data is evaluated. Checks win
        condition after each disconnect and each controller to prevent
        simultaneous deaths, ensuring the last player standing can never die.

        Args:
            gameplay_update: GameplayDataUpdate protobuf with controller data

        Returns:
            True if game is over (win condition met), False otherwise
        """
        with self._frame_config_scope():
            # UC-020 BR-001: win check after every disconnect and every controller sample.
            # Handle disconnects first
            for disconnect in gameplay_update.disconnects:
                await self._handle_disconnect(disconnect.serial)
                if await self._check_win_condition():
                    return True

            for gameplay_data in gameplay_update.controllers:
                await self._process_controller_state(gameplay_data)

                if await self._check_win_condition():
                    return True
            return False

    @contextlib.contextmanager
    def _frame_config_scope(self) -> Iterator[GamePerformanceConfig]:
        """Read the runtime config once for one frame; every player of the frame uses it."""
        snapshot = _base.get_config_manager().get_config()
        self._frame_configs = (*self._frame_configs, snapshot)
        self._frame_config = snapshot
        try:
            yield snapshot
        finally:
            # Drop only this scope's own entry, so scopes that overlap and exit out
            # of order never leave a closed scope's snapshot in place.
            active = list(self._frame_configs)
            for index in range(len(active) - 1, -1, -1):
                if active[index] is snapshot:
                    del active[index]
                    break
            self._frame_configs = tuple(active)
            self._frame_config = active[-1] if active else None

    def _runtime_config(self) -> GamePerformanceConfig:
        """The frame's config snapshot inside a frame, otherwise the live one."""
        frame_config = self._frame_config
        return frame_config if frame_config is not None else _base.get_config_manager().get_config()

    def _accel_distribution(self) -> Any:
        """The ``accel_distribution`` child for this game's mode, bound once per game."""
        child = self._accel_distribution_child
        if child is None:
            child = self._accel_distribution_child = metrics.accel_distribution.labels(game_mode=self.get_game_name())
        return child

    async def _handle_warmup_disconnects(self) -> bool:
        """Run disconnects buffered by ``_warmup_ema()`` through the death path.

        Same order and win check as ``_process_gameplay_update`` (UC-020 A2, BR-001).

        Returns:
            True if game is over (win condition met), False otherwise
        """
        pending, self._warmup_disconnects = self._warmup_disconnects, []
        for serial in pending:
            await self._handle_disconnect(serial)
            if await self._check_win_condition():
                return True
        return False

    async def _handle_disconnect(self, serial: str) -> None:
        """Handle a controller disconnect by killing the player.

        Uses the existing _kill_player pipeline so that death events, audio
        feedback, metrics, and spans all fire correctly.

        Args:
            serial: Serial number of the disconnected controller.
        """
        player = self.players.get(serial)
        if not player or not player.alive:
            return

        logger.info(f"Player disconnected during game: {serial}")

        # Record disconnect on the player's span before kill closes it
        if player.span:
            player.span.add_event(
                "player_disconnected",
                attributes={"reason": "controller_disconnect"},
            )

        # Kill the player using the standard pipeline (accel_mag=0.0 — no motion)
        await self._kill_player(serial, accel_mag=0.0)

    def _update_frame_metrics(
        self,
        iteration_latency_ms: float,
        target_frame_time_ms: float,
        recent_frame_times: list[float],
        frames_on_target: int,
        loop_iterations: int,
        loop_start_time: float,
    ) -> int:
        """
        Update frame timing metrics for monitoring.

        Tracks frame consistency, dropped frames, actual Hz, and jitter.

        Args:
            iteration_latency_ms: Duration of this iteration in ms
            target_frame_time_ms: Expected frame time in ms
            recent_frame_times: Rolling window of recent frame times (mutated in place)
            frames_on_target: Running count of frames within target tolerance
            loop_iterations: Total iterations so far
            loop_start_time: Timestamp when loop started

        Returns:
            Updated frames_on_target count
        """
        recent_frame_times.append(iteration_latency_ms)
        if len(recent_frame_times) > 60:  # Keep last 60 frames (1 second at 60Hz)
            recent_frame_times.pop(0)

        if iteration_latency_ms <= target_frame_time_ms * 1.5:
            frames_on_target += 1

        if iteration_latency_ms > target_frame_time_ms * 2:
            metrics.game_loop_frames_dropped_total.labels(mode=self.get_game_name()).inc()

        if loop_iterations % 10 == 0:
            elapsed = game_clock.now() - loop_start_time
            actual_hz = loop_iterations / elapsed if elapsed > 0 else 0
            metrics.actual_update_frequency_hz.labels(**self._game_state_labels()).set(actual_hz)

            consistency_percent = (frames_on_target / loop_iterations) * 100
            metrics.game_loop_frame_consistency_percent.labels(**self._game_state_labels()).set(consistency_percent)

            if len(recent_frame_times) >= 2:
                jitter_ms = statistics.stdev(recent_frame_times)
                metrics.game_loop_jitter_ms.labels(**self._game_state_labels()).set(jitter_ms)

        return frames_on_target

    async def _hold_win_display(self) -> None:
        """Latch the outcome and keep the game active 1 s to show the winner (UC-020 BR-003).

        A mode that ends the loop on an error it raises from ``run()`` latches no
        outcome (Tournament bracket step, #2001): the loop ends at once, no hold.
        """
        self._latch_game_outcome()
        if self._latched_outcome is None:
            return
        logger.info("Win condition met, keeping game active for 1 second to show winner")
        await game_clock.sleep(self._WIN_HOLD_S)

    async def _game_loop(self):
        """Main game loop - processes controller states and checks for deaths."""
        logger.info("Starting game loop...")

        try:
            # Player spans already created in run() before countdown

            # Get runtime config for frame timing calculations
            config = _base.get_config_manager().get_config()
            update_frequency_hz = config.update_frequency_hz

            logger.info("Starting game loop (stream rate controlled by controller-manager)")

            last_alive_serials = self._open_game_loop()

            # UC-020 A2: disconnects seen during EMA warmup go through the death path now.
            # A game force-ended during the countdown or warmup stays force_ended (BR-003).
            if self.running and await self._handle_warmup_disconnects():
                await self._hold_win_display()
                return
            last_alive_serials = await self._send_alive_filter_update(last_alive_serials)

            # Track loop timing for actual Hz calculation (Phase 43) and frame consistency
            loop_start_time = game_clock.now()
            loop = _LoopState(
                loop_start_time=loop_start_time,
                last_iteration_time=loop_start_time,
                target_frame_time_ms=1000.0 / update_frequency_hz,
                last_alive_serials=last_alive_serials,
            )

            logger.info("Starting gameplay data stream loop, waiting for first update...")
            while self.running:
                try:
                    async for gameplay_update in self.gameplay_stream:
                        self._note_frame_received(loop)

                        if not self.running:
                            logger.info("Game running=False, breaking loop")
                            break

                        if await self._process_loop_frame(loop, gameplay_update):
                            await self._hold_win_display()
                            return

                        self._record_loop_timing(loop)

                    # A call that ended with an error status is dead, not finished: an
                    # exhausted grpc.aio reader ends the loop silently (UC-030 BR-006).
                    if self.running and await self._gameplay_call_failed():
                        raise RuntimeError("gameplay call ended with an error status")

                    # Stream ended normally (server closed it with OK) — exit loop
                    break

                except asyncio.CancelledError:
                    raise  # Never retry on cancellation

                except Exception as e:
                    if await self._on_loop_stream_error(loop, e):
                        break

        except Exception as e:
            logger.error(f"Game loop error: {e}", exc_info=True)
            raise

    def _open_game_loop(self) -> set[str]:
        """The loop's startup grace and shield hold latch; returns the alive serials."""
        # Stream was already created in _start_gameplay_stream() before countdown.
        # EMA filters were primed and buffered data drained by _warmup_ema().
        # UC-030 BR-003: startup grace period so any residual spikes can't cause instant death.
        # It never shortens a longer grace (an agent shield) granted before the loop.
        grace_until = game_clock.now() + 0.3
        for player in self.players.values():
            if player.alive:
                player.grace_until = max(player.grace_until, grace_until)
        # UC-048 BR-011: hold is latched here at the latest; the recharge starts now.
        self._latch_shield_hold()
        self._start_shield_hold_clock(game_clock.now())
        self._queue_hold_feedback_all()  # UC-048 BR-010: every player's energy layer

        # Track current alive set for detecting changes
        last_alive_serials = {p.serial for p in self.players.values() if p.alive}
        logger.info(f"Initial alive players: {len(last_alive_serials)}")
        return last_alive_serials

    def _note_frame_received(self, loop: "_LoopState") -> None:
        """The first frame's start time, and the reconnect count's healthy-run reset."""
        if loop.loop_iterations == 0:
            logger.info("Received first gameplay update from stream!")
            self.start_time = game_clock.now()

        # The count starts again only after a healthy run, not after
        # one frame (UC-030 BR-006).
        if loop.reconnect_attempt:
            loop.frames_since_reconnect += 1
            if loop.frames_since_reconnect >= self._RECONNECT_HEALTHY_FRAMES:
                logger.info("Gameplay stream healthy again, reconnect count reset")
                loop.reconnect_attempt = 0

    async def _process_loop_frame(self, loop: "_LoopState", gameplay_update) -> bool:
        """Process one frame and update the alive filter; True: the game is over."""
        # Process controllers and check for game over, then update the
        # alive filter if players changed. An error here that is not a
        # gRPC error of the call (a mode's kill handler, its win check)
        # ends the game, no reconnect (UC-030 BR-006).
        try:
            game_over = await self._process_gameplay_update(gameplay_update)
            if not game_over:
                loop.last_alive_serials = await self._send_alive_filter_update(loop.last_alive_serials)
        except self._STREAM_CALL_ERRORS:
            raise
        except Exception:
            loop.frame_failed = True
            raise
        return game_over

    def _record_loop_timing(self, loop: "_LoopState") -> None:
        """Track loop timing for metrics after a processed frame."""
        loop.loop_iterations += 1
        iteration_end = game_clock.now()
        iteration_latency_ms = (iteration_end - loop.last_iteration_time) * 1000

        loop.frames_on_target = self._update_frame_metrics(
            iteration_latency_ms,
            loop.target_frame_time_ms,
            loop.recent_frame_times,
            loop.frames_on_target,
            loop.loop_iterations,
            loop.loop_start_time,
        )

        loop.last_iteration_time = iteration_end

    async def _on_loop_stream_error(self, loop: "_LoopState", e: Exception) -> bool:
        """An error of the stream loop: True ends the loop, else reconnect (or raise)."""
        if not self.running:
            if loop.frame_failed:
                logger.warning(f"Frame processing failed after the game stopped: {e!r}", exc_info=True)
            return True
        if loop.frame_failed:
            logger.error("Frame processing failed, ending the game without a reconnect")
            raise e

        loop.reconnect_attempt += 1
        loop.frames_since_reconnect = 0
        loop.reconnect_attempt = await self._handle_stream_reconnection(
            e, loop.reconnect_attempt, loop.max_reconnect_attempts
        )
        return False

    async def _run_gameplay_phase(self):
        """Start the game and run gameplay_phase: countdown, game loop, outcome latch."""
        # Game starts
        self.state = GameState.RUNNING
        current_span = trace.get_current_span()
        current_span.add_event(
            "game.state_changed",
            {
                "game.state": "RUNNING",
                "player.count": len(self.players),
                "game.id": self.game_id,
            },
        )
        # Note: self.start_time is set in _game_loop when first data is received
        await self.event_publisher(
            GameEvent.GAME_STARTED,
            {"game_id": self.game_id, "player_count": len(self.players)},
        )

        # Phase 2: Gameplay (player spans created before countdown)
        with _base.tracer.start_as_current_span("gameplay_phase") as gameplay_span:
            self._open_gameplay_spans(gameplay_span)

            # Run countdown (sounds go under game_cycle via _play_sound)
            await self._countdown()

            self._close_countdown_spans()
            await self._raise_game_volume()

            # Drain buffered stream data and prime EMA filters
            await self._warmup_ema()

            # Start music loop as background task (uses game_cycle_context)
            self.music_loop_task = asyncio.create_task(self._music_loop())

            try:
                await self._game_loop()
            finally:
                self._latch_game_outcome()
                # Stop music loop
                if self.music_loop_task:
                    self.music_loop_task.cancel()
                    with contextlib.suppress(asyncio.CancelledError):
                        await self.music_loop_task

                # Close all player spans, then grouping spans
                self._close_all_player_spans()
                self._close_grouping_spans()

    def _open_gameplay_spans(self, gameplay_span) -> None:
        """Open the players, game_cycle, player lifecycle and per-player countdown spans."""
        # Store span reference and context for background tasks
        self.gameplay_span = gameplay_span
        self.gameplay_span_context = otel_context.get_current()
        # UC-041 BR-004: the agent's span ids come only from game.phase.start.
        self._send_phase_start(gameplay_span, "gameplay_phase")

        # Create "players" parent span (collapsible group in Jaeger)
        self._players_span = _base.tracer.start_span(
            "players",
            context=self.gameplay_span_context,
            attributes={"player_count": len(self.players)},
        )
        players_ctx = trace.set_span_in_context(self._players_span)

        # Create "game_cycle" span for instrumentation (sounds, music tempo)
        self._game_cycle_span = _base.tracer.start_span(
            "game_cycle",
            context=self.gameplay_span_context,
            attributes={"game.id": self.game_id},
        )
        self.game_cycle_context = trace.set_span_in_context(self._game_cycle_span)
        self._send_phase_start(self._game_cycle_span, "game_cycle")

        # Create player lifecycle spans under "players" parent
        self._create_player_spans(players_ctx)

        # Open per-player countdown child spans
        for player in self.players.values():
            if player.span:
                ctx = trace.set_span_in_context(player.span)
                player._countdown_span = _base.tracer.start_span(
                    "countdown_phase",
                    context=ctx,
                    attributes={"player.serial": player.serial},
                )

    def _close_countdown_spans(self) -> None:
        """Close the per-player countdown spans once the countdown ran."""
        # Close per-player countdown spans
        for player in self.players.values():
            if player._countdown_span:
                player._countdown_span.end()
                player._countdown_span = None

    async def _raise_game_volume(self) -> None:
        """Ramp the music volume up to the game level after the countdown."""
        # Ramp music volume up to game level after countdown (a shadow game is silent)
        if self.audio_client and not self._is_shadow_game():
            try:
                from proto import audio_pb2

                # game_cycle is open here: parent the RPC span to it like every in-game audio call (#2671)
                with self._game_cycle_attached():
                    await self.audio_client.SetVolume(audio_pb2.SetVolumeRequest(volume=self.game_volume))
            except Exception as e:
                logger.warning(f"Failed to set game volume: {e}")
