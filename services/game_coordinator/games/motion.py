"""Death detection of a game: acceleration, EMA, thresholds and warnings.

Tests patch ``tracer``, ``time``, ``get_config_manager``, ``record_moment`` and
``inject_trace_context`` on ``games.base``: read them as ``_base.<name>``.
"""

import math

from services.game_coordinator import game_clock, metrics
from services.game_coordinator.games import base as _base
from services.game_coordinator.games.base import (
    COMBINED_FACTOR_MAX,
    COMBINED_FACTOR_MIN,
    SENSITIVITY_FACTOR_FLOOR,
    SHIELD_CHARGE_RESEND_MARGIN_SECONDS,
    WARNING_DURATION,
    Player,
    logger,
)
from services.game_coordinator.stream_writes import serialized_write


class MotionMixin:
    """Death detection of a game: acceleration, EMA, thresholds and warnings. Mixed into ``BaseGameMode``."""

    @staticmethod
    def _compute_accel_magnitude(accel) -> float:
        """
        Compute acceleration magnitude from 3-axis vector (g-force units).

        Standing still: sqrt(0^2 + 0^2 + 1^2) ~ 1.0g.
        Movement adds to this, e.g., 1.8g = significant movement.

        Args:
            accel: Vector3 protobuf with x, y, z components

        Returns:
            Scalar magnitude in g-force units
        """
        return math.sqrt(accel.x**2 + accel.y**2 + accel.z**2)

    @staticmethod
    def _update_ema(player: Player, accel_mag: float, weight: float = 4.0) -> None:
        """
        Apply exponential moving average filter to acceleration.

        Formula: smoothed = (smoothed * weight + raw) / (weight + 1)
        With weight=4 (default): 80% weight to previous value, 20% to current —
        smooths sensor noise. First reading primes the filter to prevent false
        deaths at game start.

        ``weight`` is sourced from the agent-domain ``perception.ema_weight`` flag
        (#766 F4), frozen per game at init (``self._ema_weight``); see __init__
        for why it is never re-read mid-game (#722 §5 variance-baseline caveat).
        The default of 4.0 preserves the historical hardcoded behavior for
        callers that do not pass a weight (e.g. unit tests).

        Args:
            player: Player whose EMA to update
            accel_mag: Raw acceleration magnitude for this frame
            weight: EMA weight on the previous smoothed value (default 4.0)
        """
        if player.smoothed_accel < 1e-9:  # Check for uninitialized (avoids float equality)
            player.smoothed_accel = accel_mag  # Prime filter with first real reading
        else:
            player.smoothed_accel = (player.smoothed_accel * weight + accel_mag) / (weight + 1)
        player.last_accel_mag = player.smoothed_accel

    def _compute_effective_thresholds(self, player: Player) -> tuple[float, float]:
        """
        Compute effective warning and death thresholds for a player.

        Uses LERP between slow/fast threshold arrays by the tempo intensity
        (UC-030 step 5, UC-031 BR-005), then applies per-player sensitivity factor.

        Hot path (every frame, per player): no flag reads, no awaits, no locks.

        Args:
            player: Player to compute thresholds for

        Returns:
            Tuple of (effective_warning_threshold, effective_death_threshold)
        """
        sens_idx = self.sensitivity.value
        now = game_clock.now()

        # Tempo intensity now (0.0 = slow table, 1.0 = fast table), clamped to [0, 1].
        speed_percent = self._intensity.value_at(now)

        # LERP between slow and fast thresholds (instance tables)
        base_warn = self._lerp(self.slow_warning[sens_idx], self.fast_warning[sens_idx], speed_percent)
        base_death = self._lerp(self.slow_max[sens_idx], self.fast_max[sens_idx], speed_percent)

        # Apply per-player sensitivity factor combined with the live global
        # difficulty factor. Both are multiplied, then the COMBINED
        # factor is clamped for safety before dividing thresholds: to
        # [COMBINED_FACTOR_MIN, COMBINED_FACTOR_MAX] = [0.5, 2.0], with the floor
        # lowered only by a per-player factor below 0.5, to that factor and never
        # below SENSITIVITY_FACTOR_FLOOR (0.2; #2696, UC-040 BR-045), so the
        # threshold stays finite and global difficulty alone keeps its 0.5 floor.
        # Higher combined factor = lower threshold = easier to die. At the
        # neutral 1.0/1.0 this preserves the prior behavior exactly.
        combined_factor = player.sensitivity_factor * self.global_difficulty_factor
        floor = max(SENSITIVITY_FACTOR_FLOOR, min(COMBINED_FACTOR_MIN, player.sensitivity_factor))
        clamped_factor = max(floor, min(COMBINED_FACTOR_MAX, combined_factor))
        warn = base_warn / clamped_factor
        death = base_death / clamped_factor

        # Apply the per-player agent handicap as a SEPARATE multiplicative
        # knob that composes with — does not replace — the sensitivity/difficulty
        # factor above. It is clamped independently to [0.5, 2.0] and MULTIPLIES
        # the threshold (intent-framed, opposite direction to sensitivity_factor):
        # >1.0 raises the threshold (harder to die / "help"); <1.0 lowers it
        # (easier / "rein in"). At the neutral 1.0 this is a no-op, preserving the
        # prior behavior exactly.
        # A time-boxed toughness boost takes the MAX with the standing
        # handicap (it can only ever STRENGTHEN protection for its window, never
        # weaken a standing set_player_handicap delta). Precedence: while the
        # deadline is in the future the effective handicap is
        # max(handicap_factor, toughness_boost_factor); once it expires the boost
        # silently drops out and the standing handicap_factor resumes. Still
        # clamped [0.5, 2.0] below, so even a ~2.0 boost is "much harder to die",
        # never truly immune (the deliberate distinction from grant_shield).
        #
        # Composition precedence (last-applied wins by intent):
        #   1. standing handicap_factor (set_player_handicap)
        #   2. toughness boost — STRENGTHENS via max (protection)
        #   2b. auto_rubberband boost — STRENGTHENS via max (gap-compression)
        #   3. soft_penalty tighten — WEAKENS via min (pressure)
        # The two STRENGTHENING boosts (toughness_boost, auto_rubberband) both
        # compose by MAX so the strongest active protection wins; the soft_penalty
        # MIN is applied AFTER, so an active tighten can cut through either boost:
        # pressure deliberately overrides protection (the mirror of toughness_boost,
        # which could never weaken a standing handicap). The final clamp [0.5, 2.0]
        # keeps even a fully-tightened threshold finite — never instant-death — and
        # caps even a maxed boost at 2x — "harder to die", never immune.
        effective_handicap = player.handicap_factor
        if now < player.toughness_boost_until:
            effective_handicap = max(effective_handicap, player.toughness_boost_factor)
        if now < player.rubberband_until:
            effective_handicap = max(effective_handicap, player.rubberband_boost)
        if now < player.soft_penalty_until:
            effective_handicap = min(effective_handicap, player.soft_penalty_factor)
        handicap = max(0.5, min(2.0, effective_handicap))
        return warn * handicap, death * handicap

    async def _process_controller_state(self, controller_state):
        """
        Process a single controller's state and check for death.

        Args:
            controller_state: ControllerState protobuf message
        """
        serial = controller_state.serial

        if serial not in self.players:
            return  # Unknown controller

        player = self.players[serial]

        if not player.alive:
            return  # Dead player, ignore

        self._capture_controller_info(player, serial, controller_state)

        accel = controller_state.accel
        accel_mag = self._compute_accel_magnitude(accel)
        current_time = game_clock.now()

        # UC-030 BR-005: the trigger is evaluated BEFORE the grace and death checks,
        # so an activation in this frame makes this frame a grace frame (A6 step 3).
        # Hot path: a bool read and a few compares; no flag read, no allocation.
        hold = self._shield_hold
        if hold is not None:
            # UC-030 A7 / BR-008: compares only; nothing is awaited, and the work
            # runs only while the trigger is held or an activation is running.
            pressed = controller_state.trigger_pressed
            if not pressed:
                player.trigger_armed = True
            if pressed or player.hold_started_at:
                self._process_shield_hold(player, pressed, current_time, hold)
        elif self.SHIELD_TRIGGER_ALLOWED:
            pressed = controller_state.trigger_pressed
            edge = pressed and player.trigger_armed and not player.trigger_prev
            player.trigger_prev = pressed
            if not pressed:
                player.trigger_armed = True
            # Await only when work is due (an edge, the expiry or the re-send),
            # so a held charge costs a few compares per frame, not a coroutine.
            if edge or (
                player.shield_charge_seconds > 0
                and (
                    (player.shield_charge_expires_at and current_time >= player.shield_charge_expires_at)
                    or (
                        current_time >= player.shield_charge_layer_until - SHIELD_CHARGE_RESEND_MARGIN_SECONDS
                        and (
                            not player.shield_charge_expires_at
                            or player.shield_charge_expires_at > player.shield_charge_layer_until
                        )
                    )
                )
            ):
                await self._process_shield_charge(player, edge, current_time)

        # Check grace period first - no death or warning during grace period
        # Matches original JoustMania: if game_clock.now() > no_rumble. Uses the unified
        # effective-grace rule: max(agent shield grace_until, admin
        # invincible_until) so the agent shield and admin baseline compose here too.
        # UC-048 BR-015: the shield end grace window gates the same way (one extra compare).
        if current_time < self.effective_grace_until(player) or current_time < player.end_grace_until:
            # During grace, reset the EMA so it re-primes from the first
            # post-grace frame. Invincibility must *forget* the death spike:
            # otherwise the EMA still holds it when grace expires and the
            # player is instantly re-killed by stale filter memory.
            # Same re-prime pattern as NonStop revive (nonstop_joust.py).
            player.smoothed_accel = 0.0
            player.last_accel_mag = accel_mag
            self._record_player_analytics(
                player,
                serial,
                accel_mag,
                accel_mag,
                self._compute_effective_thresholds(player)[1],
            )
            return  # In grace period, skip death/warning checks

        if not (player.hold_prime_cap and self._prime_ema_after_hold(player, accel_mag)):
            self._update_ema(player, accel_mag, self._ema_weight)

        effective_warn, effective_death = self._compute_effective_thresholds(player)
        smoothed = player.smoothed_accel

        # Record analytics (handles enabled check internally)
        self._record_player_analytics(player, serial, accel_mag, smoothed, effective_death)

        # Death and warning checks (matches original JoustMania logic)
        # Key: warning is just feedback, NOT protection - player can die during warning!
        if smoothed > effective_death:
            # Player exceeded death threshold - kill them
            await self._kill_player(serial, smoothed)
        elif smoothed > effective_warn and current_time >= player.warning_until:
            # Player exceeded warning threshold and not already in warning state
            # Start warning feedback (flash + rumble)
            await self._warn_player(serial, smoothed, effective_warn)

    async def _warn_player(self, serial: str, accel_mag: float, threshold: float):
        """
        Warn a player that they're moving too much.

        This is purely visual/haptic feedback (flash + rumble) - NOT protection!
        Player can still die immediately if they exceed death threshold.
        Matches original JoustMania where warning was just feedback.

        Args:
            serial: Controller serial number
            accel_mag: Acceleration magnitude that triggered warning
            threshold: The effective warning threshold (after lerp)
        """
        from proto import controller_manager_pb2

        player = self.players.get(serial)
        if not player or not player.alive:
            return

        # Set warning feedback duration (prevents repeated warnings during flash)
        # This is NOT protection - player can still die during this time!
        player.warning_until = game_clock.now() + WARNING_DURATION

        # UC-100 BR-006: the warning's time, whatever analytics.enabled says; also the
        # timestamp of the warning's record, so both are one clock read.
        warn_ns = self._ground_truth.record_warning(serial)

        # Analytics: Record warning event
        if player.analytics is not None:
            player.analytics.record_warning()
            metrics.player_warnings_total.labels(serial=serial, game_mode=self.get_game_name()).inc()

        # The warning moment on the player's lifecycle span, in the game's moment shape
        # (UC-100 BR-001; span event by default, BR-004).
        if player.span:
            _base.record_moment(
                "death_warning",
                shape=self.moment_shape,
                parent_span=player.span,
                attributes={
                    "accel_magnitude": accel_mag,
                    "threshold": threshold,
                    "sensitivity": self.sensitivity.name,
                    "sensitivity_factor": player.sensitivity_factor,
                    "music_speed": self.music_speed,
                    **self._moment_identity(player, serial),
                },
                duration_s=WARNING_DURATION,
                tracer=_base.tracer,
                timestamp_ns=warn_ns,
            )
        logger.info(f"Player {serial} triggered warning (accel: {accel_mag:.2f}, threshold: {threshold:.2f})")

        # Send warning effect via stream (white flash + vibrate, auto-restore)
        # Use player's span as parent so effect appears under player_lifecycle in traces
        if self.gameplay_stream:
            trace_parent, trace_state = _base.inject_trace_context(player.span)
            effect_cmd = controller_manager_pb2.GameplayStreamControl(
                game_effect=controller_manager_pb2.GameEffectCommand(
                    serial=serial,
                    effect=controller_manager_pb2.GAME_EFFECT_PLAYER_WARNING,
                    trace_parent=trace_parent,
                    trace_state=trace_state,
                )
            )
            await serialized_write(self.gameplay_stream, effect_cmd)
