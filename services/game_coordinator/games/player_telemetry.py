"""Player telemetry of a game: analytics, gauges, health and lifecycle spans.

Tests patch ``tracer``, ``time``, ``get_config_manager``, ``record_moment`` and
``inject_trace_context`` on ``games.base``: read them as ``_base.<name>``.
"""

import contextlib
from collections.abc import Collection

from opentelemetry import context as otel_context
from opentelemetry import trace

from lib.controller_constants import battery_to_pct
from lib.feature_flags import GAME_KIND_SHADOW
from lib.types import GameEvent
from services.game_coordinator import game_clock, metrics
from services.game_coordinator.games import base as _base
from services.game_coordinator.games.analytics import PlayerAnalytics
from services.game_coordinator.games.base import (
    GAME_MODE_ATTR,
    Player,
    PollDegradationError,
    logger,
)


class PlayerTelemetryMixin:
    """Player telemetry of a game: analytics, gauges, health and lifecycle spans. Mixed into ``BaseGameMode``."""

    def _set_players_alive_aggregate(self, alive_count: int) -> None:
        """Set the unlabeled ``players_alive`` aggregate gauge — primary only.

        ``game_players_alive`` is a single UNLABELED time series feeding the
        live dashboard's "players alive" count. With concurrent shadow games
        (#1018) every game-mode instance would otherwise write this one series,
        so a headless shadow could stomp the real game's count (last-writer
        wins). Only the primary (real, player-facing) session is the "live"
        game the aggregate represents, so shadows must not touch it. The
        per-serial ``player_alive`` gauge is game_id-labeled and stays isolated.
        """
        if self.game_kind != GAME_KIND_SHADOW:
            metrics.set_players_alive(self.game_id, alive_count)

    def _attach_player_analytics(self) -> None:
        """Give every player a ``PlayerAnalytics`` tracker, in every mode (UC-041 step 4, #2358).

        The tracker feeds the agent's per-player movement and skill inputs (the
        ``game_player_*`` gauges, emitted by ``_record_player_analytics``) and the
        end-of-game ``player_analytics`` summary. Skipped while ``analytics.enabled``
        is off; a player the mode already gave a tracker keeps it.
        """
        config = self._runtime_config()
        if not config.analytics.enabled:
            return
        game_start_time = game_clock.now()
        for serial, player in self.players.items():
            if player.analytics is None:
                player.analytics = PlayerAnalytics.from_config(
                    serial=serial,
                    game_start_time=game_start_time,
                    config=config.analytics,
                )

    @staticmethod
    def _analytics_span_attributes(player: Player) -> dict:
        """The ``analytics.*`` attributes for a player's end-of-game span event ({} without a tracker)."""
        if player.analytics is None:
            return {}
        return {f"analytics.{key}": value for key, value in player.analytics.get_summary().items()}

    def _player_analytics_extras(self, player: Player) -> dict:
        """Mode fields added to a player's ``player_analytics`` summary (override per mode)."""
        return {"survival_time_ms": player.analytics.total_time_ms}

    async def _publish_analytics_summaries(self, winner_serials: Collection[str]) -> None:
        """Publish one ``player_analytics`` summary per tracked player at game end (UC-041 step 4).

        Also counts the analytics samples and near-death events. ``winner_serials``
        are the players the mode declares winners (empty without a winner).
        """
        game_mode = self.get_game_name()
        for serial, player in self.players.items():
            if player.analytics is None:
                continue
            summary = player.analytics.get_summary()
            summary["game_id"] = self.game_id
            summary["winner"] = serial in winner_serials
            summary.update(self._player_analytics_extras(player))

            await self.event_publisher(GameEvent.PLAYER_ANALYTICS, summary)

            metrics.game_analytics_samples_total.labels(game_mode=game_mode).inc(player.analytics.sample_count)
            metrics.near_death_events_total.labels(serial=serial, game_mode=game_mode).inc(
                player.analytics.near_death_count
            )
            logger.info(
                f"Player {serial} analytics: peak={summary['peak_accel']:.2f}g, "
                f"playstyle={summary['playstyle']}, near_deaths={summary['near_death_count']}"
            )

    def _record_player_analytics(
        self,
        player: Player,
        serial: str,
        accel_mag: float,
        smoothed: float,
        effective_death: float,
    ) -> None:
        """
        Record analytics sample and emit periodic Prometheus metrics.

        Args:
            player: Player to record analytics for
            serial: Controller serial number
            accel_mag: Raw acceleration magnitude
            smoothed: Smoothed acceleration value
            effective_death: Current effective death threshold
        """
        config = self._runtime_config()
        if not config.analytics.enabled or player.analytics is None:
            return

        # Record sample (returns current movement zone)
        zone = player.analytics.record_sample(
            raw_accel_mag=accel_mag,
            smoothed_accel=smoothed,
            death_threshold=effective_death,
            config=config.analytics,
            frame_duration_ms=1000.0 / config.update_frequency_hz,
        )

        # Emit the analytics gauges every metrics_emit_interval_frames frames (6: ~10 Hz at 60 Hz)
        if player.analytics.sample_count % config.analytics.metrics_emit_interval_frames == 0:
            metrics.player_accel_magnitude.labels(serial=serial, game_id=self.game_id).set(accel_mag)
            metrics.player_movement_zone.labels(serial=serial, game_id=self.game_id).set(zone.value)
            metrics.player_peak_accel.labels(serial=serial, game_id=self.game_id).set(player.analytics.peak_accel)
            metrics.player_playstyle.labels(serial=serial, game_id=self.game_id).set(
                player.analytics.get_playstyle().value
            )
            # Intervention observability signals
            metrics.player_movement_variance.labels(serial=serial, game_id=self.game_id).set(
                player.analytics.windowed_variance
            )
            metrics.player_skill_level.labels(serial=serial, game_id=self.game_id).set(
                player.analytics.get_skill_level()
            )
            # Whole-game RETAINED movement-variance aggregate. Emitted
            # while alive like skill_level; the agent retains it into the
            # conclusion snapshot so balanced-fitness spike-survival is meaningful
            # post-game (vs. the frozen-last-sample game_player_movement_variance).
            metrics.player_movement_variance_aggregate.labels(serial=serial, game_id=self.game_id).set(
                player.analytics.cumulative_variance
            )

        # Record to histogram for distribution analysis
        self._accel_distribution().observe(accel_mag)

    def _stop_player_gauges(self, serial: str) -> None:
        """Remove a player's ``game_player_*`` gauges while it waits for its next fight (UC-041 BR-006).

        Fight Club and Tournament check only the two fighters, so a waiting player
        gets no sampled frame and its gauges would re-export the last fight's values
        (``game_player_alive`` 1 included) to the agent. The tracker keeps its samples
        for the end-of-game summary; ``_start_player_gauges`` brings the series back.
        """
        metrics.clear_player_analytics(serial, self.game_id)

    def _start_player_gauges(self, serial: str) -> None:
        """Re-export ``game_player_alive`` 1 for a player seated to fight; its frames set the rest."""
        metrics.player_alive.labels(serial=serial, game_id=self.game_id).set(1)

    def _process_health(self, player: Player, serial: str, controller_state) -> None:
        """Process controller health counters and manage degradation child spans.

        Uses a rolling window to calculate poll drop rate (drops/sec) rather
        than checking per-frame counts. This avoids false positives from single
        frames that accumulate pre-game drops and correctly detects sustained
        degradation at 100Hz polling with 60Hz streaming.

        Args:
            player: The player to process health for
            serial: Controller serial number
            controller_state: GameplayData protobuf (may have .health field)
        """
        health = controller_state.health if controller_state.HasField("health") else None
        config = self._runtime_config()
        threshold = config.poll_drop_threshold
        has_led_issues = health and health.led_failures

        # Always accumulate totals for final summary, even below threshold
        if health and health.poll_drops:
            player.total_poll_drops += health.poll_drops
        if health and health.poll_errors:
            player.total_poll_errors += health.poll_errors

        # --- Rolling window rate detection for poll drops ---
        now = _base.time.monotonic()
        window_seconds = 2.0  # evaluation window

        # Initialize window on first call
        if player._health_window_start is None:
            player._health_window_start = now
            player._health_window_drops = player.total_poll_drops

        elapsed = now - player._health_window_start
        if elapsed >= window_seconds:
            drops_in_window = player.total_poll_drops - player._health_window_drops
            player._health_drop_rate = drops_in_window / elapsed
            # Reset window
            player._health_window_start = now
            player._health_window_drops = player.total_poll_drops

        # Threshold is drops/sec (not drops/frame)
        has_poll_issues = player._health_drop_rate > threshold or (health and health.poll_errors > 0)

        # --- Poll degradation span (only when rolling rate exceeds threshold) ---
        if has_poll_issues:
            # Open child span on transition healthy -> degraded
            if player._poll_degraded_span is None and player.span:
                ctx = trace.set_span_in_context(player.span)
                player._poll_degraded_span = _base.tracer.start_span(
                    "controller_poll_degraded",
                    context=ctx,
                    attributes={"player.serial": serial},
                )
            # Add event for this frame's drops inside the child span
            if player._poll_degraded_span:
                attrs: dict[str, float | int] = {"drop_rate": player._health_drop_rate}
                if health and health.poll_drops:
                    attrs["poll_drops"] = health.poll_drops
                if health and health.poll_errors:
                    attrs["poll_errors"] = health.poll_errors
                player._poll_degraded_span.add_event("poll_issues", attrs)
        else:
            # Close child span on transition degraded -> healthy
            if player._poll_degraded_span is not None:
                from opentelemetry.trace import Status, StatusCode

                player._poll_degraded_span.set_attribute("health.total_poll_drops", player.total_poll_drops)
                player._poll_degraded_span.set_attribute("health.total_poll_errors", player.total_poll_errors)
                msg = f"drops={player.total_poll_drops} errors={player.total_poll_errors} serial={serial}"
                player._poll_degraded_span.record_exception(PollDegradationError(msg))
                player._poll_degraded_span.set_status(Status(StatusCode.ERROR, "poll degradation detected"))
                player._poll_degraded_span.end()
                player._poll_degraded_span = None

        # --- LED degradation span ---
        if has_led_issues:
            player.total_led_failures += health.led_failures
            if player._led_degraded_span is None and player.span:
                ctx = trace.set_span_in_context(player.span)
                player._led_degraded_span = _base.tracer.start_span(
                    "controller_led_degraded",
                    context=ctx,
                    attributes={"player.serial": serial},
                )
        else:
            if player._led_degraded_span is not None:
                player._led_degraded_span.set_attribute("health.total_led_failures", player.total_led_failures)
                player._led_degraded_span.end()
                player._led_degraded_span = None

    def _capture_controller_info(self, player: Player, serial: str, controller_state) -> None:
        """
        Capture the per-frame controller info that is not death detection: the
        name, the health counters and the battery. A mode that skips detection
        for some players (Fight Club waiting players, #1347) still calls this.
        """
        # Capture controller name on first frame and update span title
        if not player.name and controller_state.name:
            player.name = controller_state.name
            if player.span:
                player.span.update_name(f"player: {player.name}")
                player.span.set_attribute("player.name", player.name)

        # Process controller health counters
        self._process_health(player, serial, controller_state)

        # Capture the latest battery reading so the intervention battery
        # guard has a live in-process source. GameplayData.battery is the same
        # signal that feeds controller_battery_pct; normalize identically.
        player.battery_pct = battery_to_pct(controller_state.battery)

    def _create_player_lifecycle_span(self, serial: str, context=None) -> trace.Span:
        """
        Create a player lifecycle span.

        Args:
            serial: Controller serial number
            context: Parent span context (None to use current active span)

        Returns:
            Started span (caller is responsible for ending it)
        """
        player = self.players[serial]

        # If context is provided, use it; otherwise use current active span context
        if context is None:
            # Get current context from active span
            from opentelemetry import context as otel_context

            context = otel_context.get_current()

        return _base.tracer.start_span(
            "player_lifecycle",  # Consistent name for all players (OpenTelemetry best practice)
            context=context,
            attributes={
                "player.serial": serial,
                "player.team": player.team,
                "player.color": str(player.color),
                GAME_MODE_ATTR: self.get_game_name(),
                # stamp game identity so the agent can attribute a
                # player_lifecycle span to its game (mirrors game.id/game.kind on
                # the parent game-session span in game_session.py).
                "game.id": self.game_id,
                "game.kind": self.game_kind,
            },
        )

    def _finalize_player_health(self, player: Player) -> None:
        """Close health degradation spans and add summary attributes (#571).

        Called per-player before closing the player_lifecycle span.
        Safe to call even if no health spans were opened.
        """
        if not player.span:
            return

        # Close any open health degradation spans
        if player._poll_degraded_span is not None:
            from opentelemetry.trace import Status, StatusCode

            player._poll_degraded_span.set_attribute("health.total_poll_drops", player.total_poll_drops)
            player._poll_degraded_span.set_attribute("health.total_poll_errors", player.total_poll_errors)
            serial = getattr(player, "serial", "unknown")
            msg = f"drops={player.total_poll_drops} errors={player.total_poll_errors} serial={serial}"
            player._poll_degraded_span.record_exception(PollDegradationError(msg))
            player._poll_degraded_span.set_status(Status(StatusCode.ERROR, "poll degradation detected"))
            player._poll_degraded_span.end()
            player._poll_degraded_span = None
        if player._led_degraded_span is not None:
            player._led_degraded_span.set_attribute("health.total_led_failures", player.total_led_failures)
            player._led_degraded_span.end()
            player._led_degraded_span = None

        # Summary health attributes on player_lifecycle span
        player.span.set_attribute("health.total_poll_drops", player.total_poll_drops)
        player.span.set_attribute("health.total_poll_errors", player.total_poll_errors)
        player.span.set_attribute("health.total_led_failures", player.total_led_failures)
        player.span.set_attribute("health.final_drop_rate", player._health_drop_rate)
        player.span.set_attribute(
            "health.had_issues",
            player.total_poll_drops > 0 or player.total_poll_errors > 0 or player.total_led_failures > 0,
        )

    def _close_all_player_spans(self):
        """
        Close all open player lifecycle spans.

        Called at the end of gameplay_phase to ensure all player spans
        end with the gameplay phase, not during teardown.
        Subclasses can override to add custom attributes before closing.
        """
        from opentelemetry.trace import Status, StatusCode

        for serial, player in self.players.items():
            if player.span:
                self._finalize_player_health(player)

                # Add final event based on player state
                if player.alive:
                    player.span.add_event(
                        "game_ended",
                        attributes={
                            "survived": True,
                            "game_duration": game_clock.now() - self.start_time if self.start_time else 0,
                        },
                    )
                player.span.set_status(Status(StatusCode.OK))
                player.span.end()
                player.span = None  # Mark as closed
                logger.debug(f"Closed lifecycle span for player {serial}")

    def _end_open_player_spans_on_exit(self) -> None:
        """End every player span the run left open (#1997, telemetry.md §3.2).

        Idempotent: a span the mode already ended (``is_recording()`` false) or
        cleared (``player.span`` None) is skipped, so a natural end never ends a
        span twice. A span closed here gets ``game_ended`` with ``winner`` false
        and ``game.outcome``: the latched outcome, else ``force_ended`` (+
        ``game.end_reason``), else ``error`` after run() raised, else
        ``completed``. Status ERROR only after run() raised. Open countdown
        spans close too; the caller closes the grouping spans.
        """
        from opentelemetry.trace import Status, StatusCode

        if self._latched_outcome is not None or self.force_ended:
            outcome = self._game_outcome_attributes()
        elif self._run_failed:
            outcome = {"game.outcome": "error"}
        else:
            outcome = {"game.outcome": "completed"}
        for serial, player in self.players.items():
            if player._countdown_span is not None:
                player._countdown_span.end()
                player._countdown_span = None
            span = player.span
            if span is None:
                continue
            if span.is_recording():
                self._finalize_player_health(player)
                span.add_event(
                    "game_ended",
                    attributes={
                        "winner": False,
                        **outcome,
                        "game_duration": game_clock.now() - self.start_time if self.start_time else 0,
                    },
                )
                if self._run_failed:
                    span.set_status(Status(StatusCode.ERROR, "game ended before its end step"))
                else:
                    span.set_status(Status(StatusCode.OK))
                span.end()
                logger.debug(f"Closed lifecycle span for player {serial} on run exit")
            player.span = None

    def _close_grouping_spans(self):
        """Close the players and game_cycle grouping spans.

        Called after all player spans are closed, before gameplay_phase ends.
        """
        if self._players_span:
            self._players_span.end()
            self._players_span = None
        if self._game_cycle_span:
            self._game_cycle_span.end()
            self._game_cycle_span = None
            self.game_cycle_context = None

    @contextlib.contextmanager
    def _game_cycle_attached(self):
        """Make ``game_cycle`` the current span context for the block (no-op while it is not open).

        For an in-game audio RPC made outside ``_play_sound`` and the music loop, so its client span is
        a child of ``game_cycle`` and not of the phase span that happens to be current (#2671).
        """
        if self.game_cycle_context is None:
            yield
            return
        token = otel_context.attach(self.game_cycle_context)
        try:
            yield
        finally:
            otel_context.detach(token)
