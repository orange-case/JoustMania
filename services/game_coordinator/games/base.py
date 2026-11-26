"""
BaseGameMode - Abstract base class for all game modes

Phase 36b: Extracts common patterns from FFA, Teams, Random Teams, and Nonstop Joust.
Provides consistent span hierarchy orchestration and common game operations.

Uses Template Method pattern:
- run() orchestrates the entire game lifecycle with spans
- Concrete methods implement shared behavior (settings, countdown, game loop)
- Abstract methods define game-specific behavior (team assignment, win conditions, etc.)

Phase 70: Added dynamic music tempo system.
"""

import asyncio
import contextlib
import logging
import math
import random
import time
from abc import ABC, abstractmethod
from collections.abc import Callable
from dataclasses import dataclass
from enum import Enum
from typing import Any

import grpc
from openfeature.evaluation_context import EvaluationContext
from opentelemetry import context as otel_context
from opentelemetry import trace
from opentelemetry.trace import Status, StatusCode

from lib.feature_flags import (
    GAME_KIND_SHADOW,
    build_session_kind_context,
    read_bool_flag,  # noqa: F401  games/shields.py reads it as base.read_bool_flag (tests patch it here)
    read_float_flag,
    read_int_flag,  # noqa: F401  games/shields.py reads it as base.read_int_flag (tests patch it here)
    read_object_flag,
    read_string_flag,  # noqa: F401  games/shields.py reads it as base.read_string_flag (tests patch it here)
    set_game_transaction_context,
)
from lib.moments import (
    MOMENT_SHAPE_SPAN_EVENT,
    emit_agent_event,
    record_moment,  # noqa: F401  games/rubberband.py reads it as base.record_moment (tests patch it here)
)
from lib.telemetry import inject_trace_context  # noqa: F401  the split-out mixins read it as base.inject_trace_context (tests patch it here)
from lib.types import GameEvent, Sensitivity, Sound
from services.game_coordinator import game_clock, metrics
from services.game_coordinator.games.analytics import PlayerAnalytics
from services.game_coordinator.games.flag_snapshot import (  # noqa: F401  re-exported: games/shields.py and the tests import them from base
    CHARGE_EXPIRY_MAX_SECONDS,
    DEFAULT_CHARGE_EXPIRY_SECONDS,
    DEFAULT_MAX_SHIELDS_PER_PLAYER_PER_GAME,
    DEFAULT_RAMP_DOWN_SECONDS,
    DEFAULT_RAMP_UP_SECONDS,
    DEFAULT_SHIELD_ACTIVATION,
    DEFAULT_SHIELD_COOLDOWN_SECONDS,
    DEFAULT_SHIELD_END_GRACE_SECONDS,
    END_MAX_MUSIC_FAST_TIME,
    END_MAX_MUSIC_SLOW_TIME,
    END_MIN_MUSIC_FAST_TIME,
    END_MIN_MUSIC_SLOW_TIME,
    FAST_MAX,
    FAST_WARNING,
    MAX_MUSIC_FAST_TIME,
    MAX_MUSIC_SLOW_TIME,
    MIN_MUSIC_FAST_TIME,
    MIN_MUSIC_SLOW_TIME,
    RAMP_SECONDS_MAX,
    RAMP_SECONDS_MIN,
    SENSITIVITY_LEVELS,
    SHIELD_ACTIVATION_HOLD,
    SHIELD_ACTIVATION_INSTANT,
    SHIELD_ACTIVATION_TRIGGER,
    SHIELD_END_GRACE_MAX_SECONDS,
    SHIELD_HOLD_CUE_FLAGS,
    SHIELD_HOLD_FLAGS,
    SLOW_MAX,
    SLOW_WARNING,
    GameInitFlags,
    MusicWindows,
    _read_volume_flag,
    resolve_base_thresholds,
    resolve_charge_expiry,
    resolve_end_grace,
    resolve_hold_value,
    resolve_mode_thresholds,
    resolve_music_windows,
    resolve_non_negative_duration,
    resolve_ramp_seconds,
    resolve_shield_activation,
    resolve_shield_limit,
)
from services.game_coordinator.runtime_config import GamePerformanceConfig, get_config_manager
from services.game_coordinator.stream_writes import serialized_write

tracer = trace.get_tracer(__name__)
logger = logging.getLogger(__name__)

# A dead player's colour in a (re)opened stream's config: off (UC-030 step 9, BR-006).
DEAD_PLAYER_COLOR = (0, 0, 0)

# Game constants (Phase 43: Now uses runtime config for dynamic adjustment)
# Phase 72: Increased from 30Hz to 60Hz for better responsiveness
UPDATE_FREQUENCY = 60  # Hz - default, overridden by runtime config
COUNTDOWN_BEEP_COUNT = 3  # Number of beeps (Red/Yellow/Green)

# Phase 70: Music tempo constants (from original JoustMania)
SLOW_MUSIC_SPEED = 1.0  # Normal playback
FAST_MUSIC_SPEED = 1.3  # 30% faster

# Death-threshold factor clamps (UC-040 BR-045, #2696). The combined
# sensitivity x difficulty factor divides the thresholds, so it must stay above 0:
# it is clamped to [COMBINED_FACTOR_MIN, COMBINED_FACTOR_MAX], except that the
# per-player sensitivity factor alone may lower the floor to its own value, down to
# SENSITIVITY_FACTOR_FLOOR (death threshold x5). Global difficulty cannot reach
# below COMBINED_FACTOR_MIN on its own. The flag service's registry range of
# interventions/player_sensitivity_factor is [SENSITIVITY_FACTOR_FLOOR, 2.0].
COMBINED_FACTOR_MIN = 0.5
COMBINED_FACTOR_MAX = 2.0
SENSITIVITY_FACTOR_FLOOR = 0.2

# the tempo INTENSITY ramp (UC-031 step 3, BR-005). Each game owns one
# intensity i in [0, 1] (0 = slow table / 1.0x, 1 = fast table / 1.3x) that moves
# toward its target at a bounded rate: a full swing takes ``tempo.ramp_up_seconds``
# going up and ``tempo.ramp_down_seconds`` going down (game flags, read at each
# ramp start, never per frame). Thresholds read i every frame; the music is
# derived from it (one ChangeTempo glide per segment), so it can only trail.
# Shortest glide sent to the audio service: a transition_duration of 0 means 1.0 s there.
MIN_GLIDE_SECONDS = 0.05
# Deadline for one ChangeTempo, so a wedged audio service never stalls the music loop.
AUDIO_TEMPO_RPC_TIMEOUT_SECONDS = 0.5
# Back-off between resends after a ChangeTempo error (doubles per failure).
AUDIO_TEMPO_RETRY_MIN_SECONDS = 0.5
AUDIO_TEMPO_RETRY_MAX_SECONDS = 5.0
# A persistent failing music-loop tick logs once, then at most this often.
MUSIC_TICK_LOG_INTERVAL_SECONDS = 5.0
# Track id the audio service returns for PlayMusic while play_audio is off (UC-031 A1).
MUTED_TRACK_ID = "muted"

# ``ramp_tempo`` — a scheduled tempo CURVE that fills
# the #1114 ``agent``-mode seam. The agent emits a single string directive
# ``"<target>:<seconds>:<curve>"`` (e.g. ``"1.3:8:linear"``); the game-side records
# a RampDescriptor and ``_check_music_speed`` interpolates the live tempo toward
# the target over the duration (the SAME single tempo-owner seam the override /
# ``agent`` mode uses — no competing tempo writer), holding at target when done.
#
# Bounds (validated at parse time so a malformed value degrades to a safe no-op,
# never dispatches garbage): target is clamped to the valid music-speed range
# [SLOW_MUSIC_SPEED, FAST_MUSIC_SPEED]; seconds must be > 0 and is capped at
# RAMP_TEMPO_MAX_SECONDS so a stray huge value can't freeze pacing for the whole
# round; curve is one of the closed vocabulary {linear, ease}.
RAMP_CURVE_LINEAR = "linear"
RAMP_CURVE_EASE = "ease"
_VALID_RAMP_CURVES = (RAMP_CURVE_LINEAR, RAMP_CURVE_EASE)
# Upper bound on a single ramp's duration (seconds). A Free-for-All game lasts
# 2 to 3 minutes, so a longer curve would hold the pacing for too much of it; a
# malformed huge value is capped here too.
RAMP_TEMPO_MAX_SECONDS = 15.0

# Per-step audio transition for an ``ease`` ramp (#1122 review): the ease curve
# applies a small tempo step every ~100ms loop tick, so each step's ChangeTempo
# glides over roughly one tick rather than a full ramp — otherwise every tick
# stacks an overlapping long transition chasing a stale target. A ``linear`` ramp
# is one intensity segment with one glide instead.
RAMP_TEMPO_STEP_TRANSITION = 0.12


def _shape_progress(progress: float, curve: str) -> float:
    """Map linear progress in (0, 1) through the ramp curve (linear or smoothstep ease)."""
    if curve == RAMP_CURVE_EASE:
        return progress * progress * (3.0 - 2.0 * progress)
    return progress


def tempo_from_intensity(intensity: float) -> float:
    """Music ratio for an intensity (UC-031 BR-005): 1.0 + 0.3 * i, i clamped to [0, 1]."""
    clamped = max(0.0, min(1.0, intensity))
    return SLOW_MUSIC_SPEED + (FAST_MUSIC_SPEED - SLOW_MUSIC_SPEED) * clamped


def intensity_from_tempo(tempo: float) -> float:
    """Inverse of :func:`tempo_from_intensity`, clamped to [0, 1]."""
    speed_range = FAST_MUSIC_SPEED - SLOW_MUSIC_SPEED
    if speed_range <= 0:
        return 0.0
    return max(0.0, min(1.0, (tempo - SLOW_MUSIC_SPEED) / speed_range))


# The controller manager caps a charge layer at 120 s (CHARGE_MAX_DURATION_MS);
# the coordinator re-sends the layer this long before it runs out (UC-048 BR-006).
SHIELD_CHARGE_LAYER_MAX_SECONDS = 120.0
SHIELD_CHARGE_RESEND_MARGIN_SECONDS = 10.0
# shield.charge_outcome values (telemetry.md §3.2) and player_shield_charge_ended reasons.
CHARGE_OUTCOME_ACTIVATED = "activated"
CHARGE_OUTCOME_EXPIRED = "expired"
CHARGE_OUTCOME_DEATH = "death"
CHARGE_OUTCOME_GAME_END = "game_end"


# A grant that could add less than this is refused as shield_pool_full (UC-048 A9).
SHIELD_HOLD_POOL_FULL_ROOM_SECONDS = 0.05
# shield.hold_outcome values (telemetry.md §3.2).
HOLD_OUTCOME_RELEASED = "released"
HOLD_OUTCOME_EMPTY = "empty"
HOLD_OUTCOME_BUDGET_EXHAUSTED = "budget_exhausted"
HOLD_OUTCOME_DEATH = "death"
HOLD_OUTCOME_GAME_END = "game_end"
SHIELD_REFUSED_POOL_FULL = "shield_pool_full"
SHIELD_REFUSED_BUDGET_EXHAUSTED = "shield_budget_exhausted"


@dataclass(frozen=True)
class ShieldHoldParams:
    """The hold-mode values a game latched at the start of its loop (UC-048 BR-008, BR-011)."""

    cap: float
    budget: float
    initial: float
    recharge: float
    min_burst: float


# shield_up plays on an activation at most this often per player (UC-048 BR-010).
SHIELD_HOLD_SOUND_MIN_INTERVAL_SECONDS = 2.0
# Cue of a hold command (proto ShieldHoldCue); a later send keeps the strongest pending one.
HOLD_CUE_NONE = 0
HOLD_CUE_FULL = 1
HOLD_CUE_TOP_UP = 2
HOLD_CUE_EMPTY = 3
_HOLD_CUE_PRIORITY = {HOLD_CUE_NONE: 0, HOLD_CUE_TOP_UP: 1, HOLD_CUE_FULL: 2, HOLD_CUE_EMPTY: 3}
# A living player's player_shield_energy is re-published after this long without one (UC-035 BR-008):
# a dashboard opened mid-game, a reopened stream or a dropped event gets the bar back.
HOLD_ENERGY_KEYFRAME_SECONDS = 2.0


@dataclass(frozen=True)
class ShieldHoldCues:
    """The cue values a hold game latched with its hold values (UC-048 BR-010, BR-011)."""

    rumble_intensity: int = 255
    pulse_ms: int = 150
    ending_lead_ms: int = 1000
    hold_rumble_intensity: int = 70
    empty_pulse_ms: int = 400
    # #2463: the rumble ramp top (at or below hold_rumble_intensity = flat) and the LED dimming floor.
    hold_rumble_max_intensity: int = 200
    hold_led_floor_pct: int = 20


SHIELD_REFUSED_CAP = "shield_cap_reached"
SHIELD_REFUSED_COOLDOWN = "shield_cooldown"


@dataclass(frozen=True)
class IntensityRamp:
    """One segment of the tempo intensity (#1833, UC-031 BR-005). Immutable; replaced per segment.

    ``value_at`` is a pure function of the clock, so thresholds read it every frame
    without awaiting anything, and a stalled music loop can never stall them.
    """

    start: float
    target: float
    start_time: float
    duration: float
    curve: str = RAMP_CURVE_LINEAR

    @classmethod
    def hold(cls, intensity: float, now: float = 0.0) -> "IntensityRamp":
        """A finished ramp that holds ``intensity`` (clamped to [0, 1])."""
        clamped = max(0.0, min(1.0, intensity))
        return cls(start=clamped, target=clamped, start_time=now, duration=0.0)

    @property
    def end_time(self) -> float:
        return self.start_time + self.duration

    def value_at(self, now: float) -> float:
        """Intensity at ``now``: start before the segment, target after it, clamped to [0, 1]."""
        if self.duration <= 0:
            value = self.target
        else:
            progress = (now - self.start_time) / self.duration
            if progress <= 0.0:
                value = self.start
            elif progress >= 1.0:
                value = self.target
            else:
                value = self.start + (self.target - self.start) * _shape_progress(progress, self.curve)
        return max(0.0, min(1.0, value))

    def is_complete(self, now: float) -> bool:
        """True once the segment has reached (and holds) its target."""
        return self.duration <= 0 or now >= self.end_time


@dataclass(frozen=True)
class RampDescriptor:
    """A scheduled tempo curve (#1117). Immutable; replaced wholesale on a new ramp.

    Attributes:
        start_tempo: Music speed when the ramp began (the interpolation origin).
        target_tempo: Clamped destination tempo, held once reached.
        start_time: ``game_clock.now()`` when the ramp began.
        duration: Ramp length in seconds (> 0, capped at RAMP_TEMPO_MAX_SECONDS).
        curve: Interpolation shape — ``linear`` or ``ease`` (smoothstep).
    """

    start_tempo: float
    target_tempo: float
    start_time: float
    duration: float
    curve: str

    def tempo_at(self, now: float) -> float:
        """Interpolated tempo at ``now``; clamped to the target once complete."""
        if self.duration <= 0:
            return self.target_tempo
        progress = (now - self.start_time) / self.duration
        if progress <= 0.0:
            return self.start_tempo
        if progress >= 1.0:
            return self.target_tempo
        # ease = smoothstep: ease in and out, monotone, hits 0/1 at the endpoints.
        return self.start_tempo + (self.target_tempo - self.start_tempo) * _shape_progress(progress, self.curve)

    def is_complete(self, now: float) -> bool:
        """True once the ramp has reached (and should hold at) its target."""
        return self.duration <= 0 or (now - self.start_time) >= self.duration


def parse_ramp_tempo_value(value: object) -> RampDescriptor | None:
    """Parse a ``ramp_tempo`` directive ``"<target>:<seconds>:<curve>"`` (#1117).

    Returns a partially-filled :class:`RampDescriptor` (``start_tempo`` /
    ``start_time`` are 0.0 placeholders — the handler fills them from the live
    game) on a VALID directive, or ``None`` for any malformed / neutral input so
    the caller degrades to a safe no-op (never dispatches garbage). Validation:

    * exactly three ``:``-delimited fields;
    * target is a finite number, clamped to ``[SLOW_MUSIC_SPEED, FAST_MUSIC_SPEED]``;
    * seconds is a finite number ``> 0``, capped at :data:`RAMP_TEMPO_MAX_SECONDS`;
    * curve is one of :data:`_VALID_RAMP_CURVES`.

    ``"none"`` / ``""`` (the neutral value) and any parse failure return ``None``.
    """
    if not isinstance(value, str):
        return None
    text = value.strip()
    if text == "" or text.lower() == "none":
        return None
    parts = text.split(":")
    if len(parts) != 3:
        return None
    target_raw, seconds_raw, curve_raw = (p.strip() for p in parts)
    curve = curve_raw.lower()
    if curve not in _VALID_RAMP_CURVES:
        return None
    try:
        target = float(target_raw)
        seconds = float(seconds_raw)
    except (TypeError, ValueError):
        return None
    if not math.isfinite(target) or not math.isfinite(seconds):
        return None
    if seconds <= 0.0:
        return None
    seconds = min(seconds, RAMP_TEMPO_MAX_SECONDS)
    target = max(SLOW_MUSIC_SPEED, min(FAST_MUSIC_SPEED, target))
    return RampDescriptor(
        start_tempo=0.0,  # filled by the handler from the live music_speed
        target_tempo=target,
        start_time=0.0,  # filled by the handler from game_clock.now()
        duration=seconds,
        curve=curve,
    )


# Span attribute keys (S1192 - avoid duplicate string literals)
GAME_MODE_ATTR = "game.mode"

# Log messages (S1192 - avoid duplicate strings)
_MSG_COUNTDOWN_INTERRUPTED = "Countdown interrupted - game no longer running"

# Threshold Scaling: LERP approach (matches original JoustMania)
# ===============================================================
# Uses linear interpolation between slow/fast threshold arrays based on music speed.
# This allows fine-tuned per-sensitivity-level behavior as music tempo changes.
#
# Formula: threshold = lerp(SLOW[sens], FAST[sens], intensity)
# Where intensity is the game's tempo intensity in [0, 1] (UC-031 BR-005);
# the music ratio is derived from it: 1.0 + 0.3 * intensity.
#
# Example at MEDIUM (sens=2), music at 1.15x (50% between slow/fast):
#   warning = lerp(1.6, 1.9, 0.5) = 1.75g
#   death   = lerp(1.8, 2.8, 0.5) = 2.3g


# Volume levels — module constants double as flagd defaults and as the
# revert target for the volume_override intervention (interventions.py).
GAME_VOLUME = 0.7
COUNTDOWN_MUSIC_VOLUME = 0.15  # Soft background music during countdown
# Upper bound on the wait for a spoken instruction clip (UC-020 A5), so a bad
# duration from the audio service can never stall a game start.
MAX_INSTRUCTIONS_WAIT_SECONDS = 60.0
# How often the instruction wait checks for a force-end.
INSTRUCTIONS_POLL_SECONDS = 0.1
# Slow breathing while the clip plays: calm enough not to read as the countdown.
INSTRUCTIONS_CUE_PULSE_HZ = 1
# Deadline for StopSound after a force-end, so a wedged audio service cannot stall teardown.
STOP_SOUND_TIMEOUT_SECONDS = 2.0


# Warning feedback duration (seconds) - flash + rumble time
# This is purely visual feedback, NOT protection (player can still die during warning)
WARNING_DURATION = 0.5

# UC-020 BR-002: death_grace_period_seconds has no effect in FFA (no respawn).
# Post-respawn/conversion invincibility window (no death or warning during this
# time). This is a RESPAWN-MODE concept ONLY: it protects a player who has just
# respawned, been revived, or been converted (e.g. zombie infection) from being
# instantly re-killed by the same motion spike. FFA (ffa.py) has NO respawn — a
# death is permanent elimination — so this knob and the ``game.death_grace_period_seconds``
# flag that overrides it have NO EFFECT on FFA gameplay. Do not treat
# ``death_grace_period_seconds`` as an FFA difficulty/pacing lever (see #1090).
# The unrelated STARTUP grace (residual-motion guard at game start) is a separate,
# fixed 0.3s window applied to every mode and is intentionally NOT derived from this.
DEATH_GRACE_PERIOD = 2.0  # seconds of invincibility after respawn (respawn modes only)

# Log at import time to verify correct version is deployed
logger.info(f"base.py loaded: WARNING_DURATION={WARNING_DURATION}s")


def player_identity_attributes(serial: str, name: str = "") -> dict[str, str]:
    """Span-event attributes naming the player an intervention was applied to (#1295).

    ``player.serial`` is the same key the player span carries (and the agent puts on
    its targeted ``agent.decision`` spans), so one Jaeger search finds both sides;
    ``player.name`` is the display name (e.g. "Calm Lion") and is omitted until the
    controller reported one.
    """
    attrs = {"player.serial": serial}
    if name:
        attrs["player.name"] = name
    return attrs


@dataclass
class Player:
    """Represents a player in the game."""

    serial: str
    name: str = ""  # Human-readable controller name (e.g., "Blue Phoenix")
    team: int = 0
    alive: bool = True
    color: tuple = (255, 255, 255)
    last_accel_mag: float = 0.0
    # Latest battery percentage (0-100) seen on this player's gameplay frames,
    # or None until the first frame carrying a battery reading arrives. Sourced
    # from GameplayData.battery and normalized via lib.controller_constants.
    # Read by the InterventionManager battery guard.
    battery_pct: float | None = None
    # Exponential moving average of acceleration (from original JoustMania)
    # EMA smooths sensor noise and prevents false positives from single-frame spikes
    smoothed_accel: float = 0.0
    span: trace.Span | None = None  # OpenTelemetry span for this player's lifecycle
    # Grace period: no death or warning checks until this timestamp
    # Used for respawn modes (e.g., after team swap or zombie respawn)
    grace_until: float = 0.0
    # Warning state: when > 0, player is in warning feedback (flash + rumble)
    # This is purely visual - player CAN still die during warning (matches original)
    warning_until: float = 0.0
    # Analytics tracker for this player (initialized when game starts)
    analytics: "PlayerAnalytics | None" = None
    # Per-player sensitivity multiplier (Phase 3: Per-Player Sensitivity Infrastructure)
    # 1.0 = default, >1.0 = more sensitive (easier to die), <1.0 = less sensitive (harder to die)
    # Thresholds are divided by this factor: higher factor = lower threshold = easier to trigger
    sensitivity_factor: float = 1.0
    # Per-player MULTIPLICATIVE handicap on the death/warning threshold (#1107,
    # #1103 MVP action 1). A SEPARATE agent-only knob that COMPOSES with (does not
    # replace) sensitivity_factor. Intent-framed and OPPOSITE in direction to
    # sensitivity_factor: >1.0 raises the effective threshold (harder to die ->
    # "help"); <1.0 lowers it (easier to die -> "rein in"). Clamped [0.5, 2.0] in
    # _compute_effective_thresholds. 1.0 = neutral (no effect). Shadow-game-only
    # initially via the interventions_allowed allow-list gate.
    handicap_factor: float = 1.0
    # Time-boxed toughness boost. When ``toughness_boost_until``
    # is in the future, ``toughness_boost_factor`` acts as a TEMPORARY handicap
    # override that COMPOSES with ``handicap_factor`` by taking the MAX of the two
    # (so a standing set_player_handicap is never weakened, only strengthened for
    # the window) — see ``_compute_effective_thresholds``. A large boost (~2.0)
    # makes the player MUCH harder to eliminate but NOT immune (vs grant_shield's
    # total grace_until immunity): the combined factor is still clamped [0.5, 2.0].
    # On expiry the boost simply stops applying (no reset task needed) and the
    # effective handicap falls back to the standing ``handicap_factor``. 0.0 / past
    # deadline = inactive (no effect). Shadow-game-only via the allow-list gate.
    toughness_boost_until: float = 0.0
    toughness_boost_factor: float = 1.0
    # Time-boxed SOFT-PENALTY tighten. The deliberate
    # MIRROR of toughness_boost: where toughness_boost STRENGTHENS protection (max,
    # factor >= 1.0), a tighten WEAKENS it for a short window (factor < 1.0),
    # making the player TEMPORARILY EASIER to eliminate — graduated pressure, not
    # guaranteed elimination. While ``soft_penalty_until`` is in the future,
    # ``soft_penalty_factor`` composes into the effective handicap by taking the
    # MIN (so it can cut through even a toughness boost — pressure overrides
    # protection), then the existing [0.5, 2.0] clamp keeps the threshold finite
    # (never instant-death). On expiry the factor simply stops applying (no reset
    # task needed) and the effective handicap falls back to the standing/shielded
    # value. 1.0 / past deadline = inactive (no effect). Shadow-game-only via the
    # allow-list gate. See ``_compute_effective_thresholds`` for the precedence.
    soft_penalty_until: float = 0.0
    soft_penalty_factor: float = 1.0
    # Time-boxed AUTO-RUBBERBAND boost. A SINGLE agent
    # decision (auto_rubberband=gentle|strong) is expanded by the coordinator
    # across players based on the LIVE skill gap: the trailing player(s) — those
    # closest to their death threshold — receive a temporary handicap BOOST that
    # COMPRESSES the gap to the leader. Like toughness_boost it STRENGTHENS (boost
    # >= 1.0, composed by MAX), but unlike it the boost is computed per-player
    # from the gap and is CAPPED so the boosted laggard can never overtake the
    # current leader (never inverts standings). While ``rubberband_until`` is in
    # the future, ``rubberband_boost`` composes into the effective handicap by the
    # MAX (alongside toughness_boost), still clamped [0.5, 2.0] so even a maxed
    # boost is "harder to die", never immune / never instant-death. On expiry the
    # boost silently stops applying (no reset task needed). 1.0 / past deadline =
    # inactive. Shadow-game-only via the allow-list gate.
    rubberband_until: float = 0.0
    rubberband_boost: float = 1.0
    # Shield charge (UC-048 A3-A5, BR-005): at most one per player.
    # ``shield_charge_seconds`` > 0 means a charge is held; it becomes the shield
    # duration on activation. The expiry is fixed at grant time (0 = never).
    # ``shield_charge_layer_until`` is when the last SHIELD_CHARGE LED command runs
    # out on the controller, so the frame path re-sends it in time (UC-048 BR-006).
    shield_charge_seconds: float = 0.0
    shield_charge_granted_at: float = 0.0
    shield_charge_expires_at: float = 0.0
    shield_charge_layer_until: float = 0.0
    _shield_charge_span: trace.Span | None = None
    # Trigger edge detection (UC-030 BR-005): the trigger of the previous frame,
    # and whether the trigger read released once in the game loop (so a press held
    # or latched from the lobby is never a press).
    trigger_prev: bool = False
    trigger_armed: bool = False
    # Shield hold (UC-030 A7, UC-048 BR-008): only used in a game latched to
    # hold. The energy is a function of time, so nothing ticks: ``hold_energy`` is
    # the stored value at ``hold_energy_at`` (0 = the recharge clock has not
    # started). While an activation runs (``hold_started_at`` > 0) the energy and
    # the usable energy at its start are kept, and ``hold_until`` is the protection
    # deadline: the energy's end while held, start + cost after the release.
    hold_energy: float = 0.0
    hold_energy_at: float = 0.0
    hold_budget_used: float = 0.0
    hold_started_at: float = 0.0
    hold_until: float = 0.0
    hold_pressed: bool = False
    hold_energy_start: float = 0.0
    hold_budget_start: float = 0.0
    hold_usable_start: float = 0.0
    hold_last_pressed_at: float = 0.0
    hold_min_burst_applied: bool = False
    # UC-030 BR-009: the first unprotected frame after an activation primes the EMA
    # with min(raw, warning threshold) instead of the raw sample.
    hold_prime_cap: bool = False
    # UC-048 BR-015 (#2464): after a shield ends, movement cannot kill until this
    # timestamp. Not part of effective_grace_until, so a re-hold or a charge press
    # inside the window starts at once. Cleared by an activation, the kill path, a revive.
    end_grace_until: float = 0.0
    # When the last shield protection ended (or ends, for a running instant or trigger
    # shield): feeds `shield.since_end_ms` on player_death (telemetry.md §4). 0 = none.
    shield_end_at: float = 0.0
    hold_budget_exhausted_noted: bool = False
    # Per-game totals for the agent's view and the retro (UC-042 BR-007): energy the
    # agent's grants added, activations started, activations that ran out.
    hold_granted_total: float = 0.0
    hold_activations: int = 0
    hold_empties: int = 0
    _shield_hold_span: trace.Span | None = None
    # Per-player countdown span (created before countdown, ended after)
    _countdown_span: trace.Span | None = None
    # Health tracking (#571: controller health on game.player spans)
    total_poll_drops: int = 0
    total_poll_errors: int = 0
    total_led_failures: int = 0
    _poll_degraded_span: trace.Span | None = None
    _led_degraded_span: trace.Span | None = None
    # Rolling window rate detection for poll drops
    _health_window_start: float | None = None  # monotonic time at window start
    _health_window_drops: int = 0  # cumulative drops at window start
    _health_drop_rate: float = 0.0  # current calculated drop rate (drops/sec)


@dataclass
class Phase:
    """Represents a game phase with a name and execution method."""

    name: str
    execute: callable


@dataclass
class _RunProgress:
    """How far run()'s teardown got: what its finally must still stop (#2357)."""

    music_stopped: bool = False
    shield_feedback_ended: bool = False


class PollDegradationError(Exception):
    """Synthetic exception for poll degradation span events.

    Not raised for control flow — only passed to span.record_exception()
    so failure analysis can group and alert on poll health issues
    by exception type (PollDegradationError) and message.
    """


class GameState(Enum):
    """Game lifecycle states."""

    IDLE = "idle"
    STARTING = "starting"
    RUNNING = "running"
    ENDING = "ending"
    ENDED = "ended"


# The mixins import the constants above back from this module, so
# these imports stay below them.
from services.game_coordinator.games.elimination import EliminationMixin  # noqa: E402
from services.game_coordinator.games.game_loop import GameLoopMixin  # noqa: E402
from services.game_coordinator.games.gameplay_stream import GameplayStreamMixin  # noqa: E402
from services.game_coordinator.games.motion import MotionMixin  # noqa: E402
from services.game_coordinator.games.outcome import GameOutcomeMixin  # noqa: E402
from services.game_coordinator.games.player_telemetry import PlayerTelemetryMixin  # noqa: E402
from services.game_coordinator.games.rubberband import RubberbandMixin  # noqa: E402
from services.game_coordinator.games.shields import ShieldMixin  # noqa: E402
from services.game_coordinator.games.tempo import TempoMixin  # noqa: E402


class BaseGameMode(
    ShieldMixin,
    TempoMixin,
    GameLoopMixin,
    MotionMixin,
    GameplayStreamMixin,
    EliminationMixin,
    GameOutcomeMixin,
    RubberbandMixin,
    PlayerTelemetryMixin,
    ABC,
):
    """
    Abstract base class for all game modes.

    Provides:
    - Template method run() for consistent game lifecycle orchestration
    - Concrete methods for shared operations (settings, countdown, controller processing)
    - Abstract methods for game-specific behavior (team assignment, win conditions, etc.)
    - Consistent OpenTelemetry span hierarchy across all game modes

    Subclasses must implement:
    - get_game_name() - return game mode identifier
    - _initialize_players_impl() - assign players to teams
    - _create_player_spans() - create lifecycle spans (flat vs hierarchical)
    - _check_win_condition() - determine if game should end
    - _kill_player_impl() - handle player death (stay dead vs respawn)
    - _get_additional_phases() - return extra phases (e.g., team_formation)
    - _end_game_impl() - cleanup and declare winner
    """

    # UC-048 BR-004: ``shield.activation = trigger`` is an allow-list. Only a mode
    # that sets this (Free-for-All) turns a grant_shield into a charge; every other
    # mode, and every shadow game whose pattern controllers do not press the
    # trigger (no ``pattern_trigger_policy``, UC-046 BR-018), always takes the
    # instant path.
    SHIELD_TRIGGER_ALLOWED: bool = False

    # The runtime config snapshot of the frame being processed: read once
    # per frame, not twice per player. The config is event-refreshed (flags.md), so
    # a refresh applies from the next frame. None outside a frame: a call outside
    # the frame loop reads the live snapshot.
    _frame_config: GamePerformanceConfig | None = None
    # The snapshots of the scopes still open, oldest first (immutable, so the class
    # default is never shared state); ``_frame_config`` is the newest of them.
    _frame_configs: tuple[GamePerformanceConfig, ...] = ()
    # The bound ``accel_distribution`` child of this game: the label lookup runs
    # once per game (``get_game_name()`` is constant per mode class).
    _accel_distribution_child: Any = None

    def __init__(
        self,
        controller_manager_client: Any,  # controller_manager_pb2_grpc.ControllerManagerServiceStub
        event_publisher: Callable[[str, dict[str, str]], Any],
        audio_client: Any | None = None,  # audio_pb2_grpc.AudioServiceStub
        game_id: str = "",
        initial_players: list | None = None,  # List of Player protobuf messages
        sensitivity: int = 2,  # 0-4, passed from StartGameConfig (default MEDIUM)
        rng_seed: int = 0,  # #1003 paired-CRN seed; 0 => entropy (today's behavior)
    ) -> None:
        """
        Initialize base game mode (Phase 33 - added type hints).

        Args:
            controller_manager_client: gRPC stub for ControllerManager service
            event_publisher: Async callback function to publish game events (event_type, data)
            audio_client: gRPC stub for Audio service (Phase 29)
            initial_players: Optional list of Player protobuf messages from StartGame RPC
            game_id: Unique identifier for this game instance
            sensitivity: Sensitivity level 0-4 (passed from StartGameConfig)
        """
        self.controller_client = controller_manager_client
        self.event_publisher = event_publisher
        self.audio_client = audio_client

        # Per-instance RNG (the load-bearing CRN isolation). Shadow games
        # run concurrently in ONE game_coordinator interpreter, so global
        # random.seed() would corrupt a concurrent game's in-flight sequence.
        # Every random draw in this game (and its subclasses) MUST go through
        # self._rng so two seed-matched games in an (experimental, control) pair
        # produce identical shuffles/samples/choices — the only difference being
        # the flag under test. rng_seed == 0 (real games, unbound shadow games, or
        # any pre-#1003 caller) maps to random.Random(None) == entropy: byte-for-
        # byte today's behavior. A CI grep gate forbids bare ``random.`` under
        # services/game_coordinator/games to keep this invariant from regressing.
        self._rng = random.Random(rng_seed if rng_seed else None)

        # Game ID - subclasses can override with mode-specific prefix
        if not game_id:
            mode_prefix = self.get_game_name().lower().replace(" ", "_")
            game_id = f"{mode_prefix}_{int(time.time())}"
        self.game_id = game_id

        self._init_session_state()

        # Game state
        self.state = GameState.IDLE
        self.players: dict[str, Player] = {}
        self.initial_players = initial_players  # Players from StartGame RPC
        self.start_time = None
        self.running = False

        # The state each concern owns, set by its own mixin.
        self._init_stream_state()
        self._init_outcome_state()
        self._init_shield_state()
        self._init_sensitivity_state(sensitivity)
        self._init_flag_state()
        self._init_tempo_state()
        # Backwards-compatible alias kept for any external reader.
        # The reverted global_sensitivity_override now restores the CURRENT admin
        # baseline, so this is no longer the restore target — but the
        # game-start snapshot is still useful telemetry, so expose it as a
        # property mirroring the baseline at construction time.
        self._configured_sensitivity = self._baseline_sensitivity
        self._init_elimination_state()
        self._init_span_state()

        # Tracked async tasks for automatic cleanup on game end
        self._tasks: set[asyncio.Task] = set()

        logger.info(f"{self.get_game_name()} game initialized: {self.game_id}")

    def _init_session_state(self) -> None:
        """Multi-session bookkeeping and the moment identity of the game (set at construction)."""
        # Multi-session bookkeeping. The servicer overrides these on the
        # game instance after construction based on the owning GameSession. They
        # default to the single-game/primary values so a standalone game (and
        # every existing test) behaves exactly as before:
        #   game_kind: "real" or "shadow" — labels lifecycle metrics.
        #   experiment_id / arm: experiment attribution within a shadow session;
        # empty for a non-experiment game. The servicer overrides
        #     these from the owning GameSession (set by #976's spawn binding).
        self.game_kind = "real"
        # UC-100 BR-002: the moment shape of every moment of the game (the warning, the
        # death), read once per game by the session (telemetry.moment_shape) and held for
        # the game; and the per-serial moment sequence behind moment.id, kept by the game,
        # not the Player (BR-003).
        self.moment_shape = MOMENT_SHAPE_SPAN_EVENT
        self._moment_sequence: dict[str, int] = {}
        self.experiment_id = ""
        self.arm = ""
        # UC-046 BR-018 / UC-048 BR-004: set by the session for a shadow game
        # whose archetype presses the trigger; such a game honours trigger and hold.
        self.pattern_trigger_policy = False
        # The flag transaction context _set_flag_eval_context set for this game's
        # run; intervention handlers resolve flags in it (flag_eval_context).
        self._flag_eval_context: EvaluationContext | None = None

    def _init_sensitivity_state(self, sensitivity: int) -> None:
        """The admin baseline sensitivity and the agent override layer (M9 ownership model)."""
        # Settings - sensitivity is now passed via StartGameConfig.
        #
        # Sensitivity arbitration (M9 ownership model):
        #   - ``baseline_sensitivity`` is the ADMIN's source of truth (pre-game
        #     lobby intent). The admin may update it mid-game via
        #     ``set_baseline_sensitivity`` (which invalidates any active agent
        #     override — a human baseline change wins).
        #   - ``sensitivity_override`` is the AGENT's bounded in-game override
        #     LAYER (``None`` = no override). The agent never persists into the
        #     baseline; clearing the override returns to the CURRENT baseline,
        #     not a stale game-start snapshot.
        #   - ``sensitivity`` (property below) is the EFFECTIVE level read by
        #     ``_compute_effective_thresholds`` and the metrics/spans: the
        #     override if active, otherwise the baseline.
        # Validate and convert to Sensitivity enum.
        if 0 <= sensitivity <= 4:
            self._baseline_sensitivity = Sensitivity(sensitivity)
        else:
            logger.warning(f"Sensitivity {sensitivity} out of range, using MEDIUM")
            self._baseline_sensitivity = Sensitivity.MEDIUM
        # No agent override at game start; effective == baseline.
        self._sensitivity_override: Sensitivity | None = None

    def _init_flag_state(self) -> None:
        """Read the init-latched flags once and derive the game's frozen settings from them."""
        # Init-latched flags: every read below happens once, here, and the raw
        # values are validated and frozen into ``init_flags`` (GameInitFlags.from_raw).
        # The attributes after it start from the snapshot. The readers are called as
        # ``read_*_flag`` / ``get_config_manager`` in this module, so tests patch them here.
        #
        # death/warning threshold tables, the ``game.thresholds`` object flag
        # (init-frozen; malformed/missing values fall back to the module constants).
        #
        # EMA filter weight (agent-domain `perception.ema_weight`).
        # READ ONCE HERE AT GAME INIT and frozen for the life of the game. It is
        # intentionally never re-evaluated mid-game: changing the smoothing
        # weight while running would invalidate the per-player movement variance
        # baseline that the agent's perception layer relies on (#722 §5 --
        # difficulty/filter changes "invalidate the variance baseline"). Falls
        # back to the hardcoded default (4.0) when flagd is unavailable.
        #
        # UC-020 BR-002 / #766 F2: ``game.death_grace_period_seconds``, the
        # post-respawn/conversion invincibility window for RESPAWN modes only
        # (zombie conversion + respawn, swapper team-swap; they set
        # ``player.grace_until = game_clock.now() + self.death_grace_period``). FFA has
        # no respawn, so it has NO effect on FFA. Init-frozen; malformed or
        # negative values fall back to DEATH_GRACE_PERIOD.
        #
        # ``game.windows`` music tempo-change windows (init-frozen calibration;
        # the ``pacing_profile`` presets are named variants).
        #
        # per-channel volumes from the user domain (init-frozen). Defaults
        # reproduce the original constants; malformed values fall back to them.
        self.init_flags = GameInitFlags.from_raw(
            thresholds=read_object_flag("game", "thresholds", {}, game_id=self.game_id),
            ema_weight=get_config_manager().read_ema_weight(),
            death_grace_period_seconds=read_float_flag(
                "game", "death_grace_period_seconds", DEATH_GRACE_PERIOD, game_id=self.game_id
            ),
            death_grace_default=DEATH_GRACE_PERIOD,
            windows=read_object_flag("game", "windows", {}, game_id=self.game_id),
            game_volume=_read_volume_flag("audio_volume.game", GAME_VOLUME),
            countdown_music_volume=_read_volume_flag("audio_volume.countdown_music", COUNTDOWN_MUSIC_VOLUME),
        )
        self.slow_warning = list(self.init_flags.slow_warning)
        self.slow_max = list(self.init_flags.slow_max)
        self.fast_warning = list(self.init_flags.fast_warning)
        self.fast_max = list(self.init_flags.fast_max)
        self._ema_weight = self.init_flags.ema_weight
        self.death_grace_period = self.init_flags.death_grace_period_seconds
        # F6: the live ``pacing_profile`` intervention swaps ``self.music_windows``
        # atomically; ``init_music_windows`` is the restore target, never reassigned.
        self.music_windows = self.init_flags.music_windows
        self.init_music_windows = self.music_windows
        self.game_volume = self.init_flags.game_volume
        self.countdown_music_volume = self.init_flags.countdown_music_volume

        # live global difficulty factor (analogue of per-player
        # ``sensitivity_factor``). Combined with the per-player factor in
        # ``_compute_effective_thresholds``; 1.0 is neutral (baseline behavior).
        # Set/cleared by the global_difficulty_factor intervention handler and
        # live-read every frame.
        self.global_difficulty_factor: float = 1.0

    # ========================================================================
    # Sensitivity arbitration (M9 ownership model)
    # ========================================================================
    # ``sensitivity`` is the EFFECTIVE level: the agent override if one is
    # active, otherwise the admin baseline. Every reader (the per-frame
    # ``_compute_effective_thresholds``, the game_sensitivity metric, span
    # attributes) sees the effective value with no change. Writers are split by
    # owner: the admin owns ``baseline_sensitivity`` (assigning ``sensitivity``
    # is the admin path and routes here); the agent owns the override layer via
    # ``apply_sensitivity_override`` / ``clear_sensitivity_override`` and never
    # touches the baseline.

    @property
    def sensitivity(self) -> Sensitivity:
        """Effective sensitivity: agent override if active, else admin baseline."""
        if self._sensitivity_override is not None:
            return self._sensitivity_override
        return self._baseline_sensitivity

    @sensitivity.setter
    def sensitivity(self, value: Sensitivity) -> None:
        """Admin path: set the baseline (clears any active agent override).

        Assigning ``game.sensitivity`` is treated as a HUMAN baseline change
        (the lobby/admin surface and existing test fixtures do this). Per the
        ownership model a human baseline change invalidates the agent's active
        override, so this delegates to :meth:`set_baseline_sensitivity`.
        """
        self.set_baseline_sensitivity(value)

    @property
    def baseline_sensitivity(self) -> Sensitivity:
        """The admin's source-of-truth sensitivity (pre-game lobby intent)."""
        return self._baseline_sensitivity

    @property
    def sensitivity_override(self) -> Sensitivity | None:
        """The agent's active override layer, or ``None`` if no override."""
        return self._sensitivity_override

    @property
    def configured_sensitivity(self) -> Sensitivity:
        """Game-start sensitivity snapshot (telemetry only; not the restore target)."""
        return self._configured_sensitivity

    def set_baseline_sensitivity(self, value: Sensitivity) -> None:
        """Update the admin baseline; invalidate any active agent override (#816).

        A human baseline change wins: the agent's in-game override is dropped so
        a later override clear cannot clobber fresh admin intent, and the
        effective sensitivity reflects the new baseline immediately.
        """
        had_override = self._sensitivity_override is not None
        self._baseline_sensitivity = value
        self._sensitivity_override = None
        if had_override:
            logger.info(f"sensitivity: admin baseline -> {value.name}; active agent override invalidated")
        else:
            logger.info(f"sensitivity: admin baseline -> {value.name}")

    def apply_sensitivity_override(self, value: Sensitivity) -> None:
        """Agent path: set the temporary override layer on top of the baseline."""
        self._sensitivity_override = value
        logger.info(f"sensitivity: agent override -> {value.name} (baseline {self._baseline_sensitivity.name})")

    def clear_sensitivity_override(self) -> None:
        """Agent path: drop the override; effective returns to the CURRENT baseline."""
        self._sensitivity_override = None
        logger.info(f"sensitivity: agent override cleared, restored to baseline {self._baseline_sensitivity.name}")

    # ========================================================================
    # Abstract Methods - Subclasses MUST implement these
    # ========================================================================

    @abstractmethod
    def get_game_name(self) -> str:
        """
        Return game mode identifier for logging and span naming.

        Returns:
            Game mode name (e.g., "FFA", "Teams", "Nonstop Joust")
        """
        pass

    @abstractmethod
    async def _initialize_players_impl(self, controllers: list):
        """
        Initialize players with game-specific logic (team assignment, etc.).

        Args:
            controllers: List of controller protobuf messages from GetReadyControllers

        Subclass responsibilities:
        - Create Player objects and add to self.players
        - Assign teams (FFA uses team=0 for all, Teams uses round-robin, etc.)
        - Set controller colors
        """
        pass

    @abstractmethod
    def _create_player_spans(self, game_context):
        """
        Create player/team lifecycle spans with game-specific hierarchy.

        Args:
            game_context: Parent span context for proper hierarchy

        FFA: Flat hierarchy - player spans directly under game.phase.gameplay
        Teams/Random Teams: Hierarchical - team spans → player spans
        Nonstop Joust: Flat hierarchy like FFA
        """
        pass

    @abstractmethod
    async def _check_win_condition(self) -> bool:
        """
        Check if game should end.

        Returns:
            True if game should end, False otherwise

        FFA: Last player standing (len(alive_players) <= 1)
        Teams: Last team standing (len(alive_teams) <= 1)
        Nonstop Joust: Time limit reached
        """
        pass

    @abstractmethod
    async def _kill_player_impl(self, serial: str, accel_mag: float):
        """
        Handle player death with game-specific logic.

        Args:
            serial: Controller serial number
            accel_mag: Acceleration magnitude that caused death

        FFA/Teams: Player stays dead, end their lifecycle span
        Nonstop Joust: Player respawns, add death event but keep span open
        """
        pass

    def get_instruction_clip(self) -> Sound | str | None:
        """Spoken instruction clip played before the countdown (UC-020 A5).

        A ``Sound`` plays in the ``menu_voice`` voice; a ``str`` is an asset path
        the audio service plays as-is (a line recorded in one voice only, #2151).
        None means the mode has no recording: nothing plays, no wait.
        """
        return None

    def _instructions_enabled(self) -> bool:
        """Read ``user.game_instructions`` at use time; any failure means off."""
        try:
            from openfeature.evaluation_context import EvaluationContext

            from lib.feature_flags import get_flag_client, init_flag_domain

            init_flag_domain("user")
            client = get_flag_client("user")
            value = client.get_boolean_value("game_instructions", False, EvaluationContext())
            return value is True
        except Exception as e:
            logger.warning(f"Failed to read game_instructions, treating as off: {e}")
            return False

    def _is_shadow_game(self) -> bool:
        """True for a shadow game: it sends no audio RPC (UC-031 BR-006, #1835)."""
        return self.game_kind == GAME_KIND_SHADOW

    async def _play_instructions(self, span: trace.Span) -> None:
        """Play the mode's instruction clip and wait for it to finish (UC-020 A5).

        Records ``game.instructions`` on ``span``: ``played``, ``off``,
        ``shadow`` (a shadow game has no audience), ``no_clip`` (the mode has
        no clip), ``no_audio`` (no audio client) or ``failed`` (PlaySound raised).
        Only ``played`` waits.
        """
        if self.game_kind == GAME_KIND_SHADOW:
            span.set_attribute("game.instructions", "shadow")
            return
        if not self._instructions_enabled():
            span.set_attribute("game.instructions", "off")
            return
        instruction_clip = self.get_instruction_clip()
        if instruction_clip is None:
            span.set_attribute("game.instructions", "no_clip")
            return
        clip = instruction_clip.value if isinstance(instruction_clip, Sound) else instruction_clip
        if not self.audio_client:
            span.set_attribute("game.instructions", "no_audio")
            return

        try:
            from proto import audio_pb2

            response = await self.audio_client.PlaySound(
                audio_pb2.PlaySoundRequest(file_path=clip, volume=1.0, priority=audio_pb2.HIGH)
            )
            duration = max(0.0, min(float(response.duration_seconds), MAX_INSTRUCTIONS_WAIT_SECONDS))
        except Exception as e:
            logger.warning(f"Failed to play instructions {clip}: {e}")
            span.set_attribute("game.instructions", "failed")
            return

        span.set_attribute("game.instructions", "played")
        span.add_event(
            "game.instructions_played",
            {"instructions.clip": clip, "instructions.duration_seconds": duration},
        )
        logger.info(f"Playing instructions {clip}, waiting {duration:.1f}s before the countdown")

        # Wait for the clip to finish; a force-end stops the wait at once.
        wait_seconds = duration * self._INSTRUCTIONS_WAIT_SCALE
        deadline = game_clock.now() + wait_seconds
        if wait_seconds > 0:
            await self._show_instructions_cue(wait_seconds)
        for _ in range(math.ceil(wait_seconds / INSTRUCTIONS_POLL_SECONDS)):
            remaining = deadline - game_clock.now()
            if not self.running or remaining <= 0:
                break
            await game_clock.sleep(min(INSTRUCTIONS_POLL_SECONDS, remaining))

        if not self.running:
            await self._stop_instruction_clip()

    async def _show_instructions_cue(self, wait_seconds: float) -> None:
        """Pulse every controller in its colour while the clip plays (UC-020 A5).

        In a loud venue the clip goes unheard, and unchanged LEDs read as a hung
        start. Best effort: a failure is logged and the stream opens after the clip.
        """
        from proto import controller_manager_pb2

        opened_here = self.gameplay_stream is None
        try:
            if opened_here:
                await self._start_gameplay_stream()
                self._start_early_stream_drain()
            for serial, player in self.players.items():
                r, g, b = player.color
                await serialized_write(
                    self.gameplay_stream,
                    controller_manager_pb2.GameplayStreamControl(
                        game_effect=controller_manager_pb2.GameEffectCommand(
                            serial=serial,
                            effect=controller_manager_pb2.GAME_EFFECT_PULSE,
                            color=controller_manager_pb2.RGB(r=r, g=g, b=b),
                            duration_ms=int(wait_seconds * 1000),
                            speed=INSTRUCTIONS_CUE_PULSE_HZ,
                        )
                    ),
                )
        except Exception as e:
            logger.warning(f"Failed to show the instructions cue on the controllers: {e}")
            if opened_here and self._early_drain_task is None:
                # The stream never got its drain: drop it so run() opens a fresh one after the clip.
                self._cancel_gameplay_call()
                self.gameplay_stream = None

    async def _stop_instruction_clip(self) -> None:
        """Cut the instruction clip short after a force-end (UC-020 A5, #1586).

        Best effort and bounded: a failure or timeout is logged and never blocks the end.
        """
        try:
            from proto import audio_pb2

            await asyncio.wait_for(
                self.audio_client.StopSound(audio_pb2.StopSoundRequest()), timeout=STOP_SOUND_TIMEOUT_SECONDS
            )
        except TimeoutError:
            logger.warning(f"StopSound timed out after {STOP_SOUND_TIMEOUT_SECONDS}s; the clip may play on")
        except Exception as e:
            logger.warning(f"Failed to stop the instruction clip after force-end: {e}")

    async def _run_streamed_intro_and_instructions(self, init_span: trace.Span) -> None:
        """Open the stream, run the intro phases on it, then the instruction clip.

        For the modes with their own ``run()`` (Werewolf, Zombies, Fight Club,
        Tournament): the intro goes over the stream, so it opens first and a drain
        discards the intro's frames and keeps disconnects (UC-022 A1, UC-023 step 1,
        UC-025 step 1 and A2 step 1). The clip plays after the intro while the
        drain still runs, as in the base ``run()`` (UC-020 A5, #2151); a force-end
        during the intro skips the clip.
        """
        await self._start_gameplay_stream()
        self._start_early_stream_drain()
        phases_completed = False
        try:
            for phase in self._get_additional_phases():
                if not self.running:
                    break
                with tracer.start_as_current_span(f"game.phase.{phase.name}"):
                    await phase.execute()
            if self.running:
                await self._play_instructions(init_span)
            phases_completed = True
        finally:
            await self._stop_early_stream_drain(reopen=phases_completed)

    @abstractmethod
    def _get_additional_phases(self) -> list:
        """
        Return extra phases to execute before countdown (e.g., team_formation).

        Returns:
            List of phase objects with name and execute() method

        FFA/Teams/Nonstop: return []
        Random Teams: return [TeamFormationPhase]
        """
        pass

    @abstractmethod
    async def _end_game_impl(self):
        """
        Handle game ending with game-specific logic.

        Responsibilities:
        - Close any remaining player/team lifecycle spans
        - Determine and declare winner
        - Set controller colors/effects for winner
        - Publish game_ended event
        """
        pass

    # ========================================================================
    # Concrete Methods - Shared implementation used by all subclasses
    # ========================================================================

    async def _initialize_players(self):
        """Initialize players from StartGame RPC payload."""
        try:
            # Players must be provided via StartGame RPC (from Menu → Supervisor)
            if not self.initial_players:
                raise RuntimeError("No players provided - StartGame must include player list")

            # Convert protobuf Player messages to controller-like objects for _initialize_players_impl
            # _initialize_players_impl expects controllers with .serial attribute
            class ControllerStub:
                def __init__(self, serial):
                    self.serial = serial

            controllers = [ControllerStub(p.serial) for p in self.initial_players]
            await self._initialize_players_impl(controllers)
            self._attach_player_analytics()

            # Set alive metric for all initialized players (Phase 75: filter dead from dashboard)
            for serial in self.players:
                metrics.player_alive.labels(serial=serial, game_id=self.game_id).set(1)
            self._set_players_alive_aggregate(len(self.players))

            logger.info(f"Initialized {len(self.players)} players from StartGame RPC")

            # Publish event
            await self.event_publisher(
                GameEvent.PLAYERS_INITIALIZED,
                {
                    "player_count": len(self.players),
                    "serials": list(self.players.keys()),
                },
            )

        except Exception as e:
            logger.error(f"Error initializing players: {e}", exc_info=True)
            raise

    _EARLY_DRAIN_STOP_TIMEOUT_S = 0.5

    _WARMUP_DURATION_S = 0.1  # enough to drain the buffer and get ~6 frames at 60 Hz
    _WARMUP_STALL_TIMEOUT_S = 0.5

    _RECONNECT_BACKOFF_S = 1.0
    # UC-030 BR-006: the controller manager sends a frame every tick, so a call this
    # long without one is wedged; only the wait counts, detection lands at 1x to 1.5x.
    _STREAM_FRAME_GAP_S: float = 1.0
    _STREAM_REOPEN_TIMEOUT_S: float = 2.0

    # Real waits a test can shorten: production keeps these values.
    # Poll step of the countdown beep wait (one step per 50 ms of beep interval).
    _COUNTDOWN_BEEP_POLL_S: float = 0.05
    # Factor on the wait for the instruction clip to finish (1.0 = the clip's length).
    _INSTRUCTIONS_WAIT_SCALE: float = 1.0
    # Errors of the gameplay call itself, raised by a read or by a write on it
    # (UC-030 BR-006): the game loop reconnects on these. Any other error while a
    # frame is processed ends the game with game_error (UC-020 BR-003).
    _STREAM_CALL_ERRORS = (grpc.aio.AioRpcError, asyncio.InvalidStateError)
    # A reopened stream counts as healthy, and the reconnect count starts again, after
    # this many frames (about 1 s at the default 60 Hz; UC-030 BR-006).
    _RECONNECT_HEALTHY_FRAMES = 60

    # The win hold; a class attribute so tests shorten it. Production keeps 1 s.
    _WIN_HOLD_S: float = 1.0

    # UC-002 BR-009: disconnects of this many players within this much game time are an
    # infrastructure fault (rust-hid restart, dongle loss, HID stall), not play.
    # Disconnects arrive on the 1 Hz discovery pass: the window spans two consecutive passes.
    _MASS_DISCONNECT_WINDOW_S: float = 1.5
    _MASS_DISCONNECT_MIN_PLAYERS: int = 2

    # Note: Don't set gameplay_stream = None here - it's needed by _end_game_impl
    # for sending winner effects. Stream cleanup happens after teardown phase.

    # ========================================================================
    # Task Tracking and Lifecycle Hooks
    # ========================================================================

    def _track_task(self, task: asyncio.Task) -> asyncio.Task:
        """
        Register an async task for automatic cleanup.

        Tracked tasks are cancelled when cleanup() is called, preventing
        dangling tasks after game end.

        Args:
            task: The asyncio.Task to track

        Returns:
            The same task (for chaining with asyncio.create_task)
        """
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)
        return task

    async def cleanup(self) -> None:
        """
        Clean up game resources. Called in finally block after game ends.

        Cancels any tracked async tasks registered via _track_task(), then closes
        the gameplay stream (UC-030 BR-006). Subclasses should override to add
        mode-specific cleanup (e.g., cancel timers, close connections) and call
        super().cleanup().
        """
        try:
            if self._tasks:
                # Snapshot tasks before cancelling (done callbacks remove from set)
                tasks_to_cancel = list(self._tasks)
                logger.info(f"Cleaning up {len(tasks_to_cancel)} tracked tasks")
                for task in tasks_to_cancel:
                    if not task.done():
                        task.cancel()
                # Wait for all tasks to complete cancellation
                await asyncio.gather(*tasks_to_cancel, return_exceptions=True)
                self._tasks.clear()
        finally:
            # Every mode's run() reaches here on every end (natural, force-end,
            # error): close the stream once no tracked task can use it.
            self._release_gameplay_stream()
        logger.info(f"{self.get_game_name()} cleanup complete")

    async def on_force_end(self) -> None:
        """
        Hook called when game is force-ended (not natural completion).

        Called before cleanup(). Use for mode-specific force-end logic
        like announcing forced end or saving partial results.
        Subclasses can override to add custom behavior.
        """
        logger.info(f"{self.get_game_name()} force-ended")

    # ========================================================================
    # Template Method - Orchestrates entire game lifecycle with spans
    # ========================================================================

    @property
    def flag_eval_context(self) -> EvaluationContext:
        """The flag evaluation context of this game, for reads made outside its task (#1970).

        Intervention handlers run on the game's loop but not in the game's task, so
        the manager sets this context around each one (UC-040 BR-018). After the
        run started it is exactly what :meth:`_set_flag_eval_context` set; before
        that (init phase) it is the session-kind context the game's own init reads
        saw (``game_kind`` plus the experiment identity).
        """
        if self._flag_eval_context is not None:
            return self._flag_eval_context
        return build_session_kind_context(self.game_kind, self.experiment_id or None, self.arm or None)

    def _set_flag_eval_context(self) -> None:
        """Set the per-game flag evaluation context (transaction context) for this game.

        The only setter: it also keeps the context as ``flag_eval_context`` so
        intervention handlers resolve flags in the same context (#1970).
        """
        # Set transaction context for flag evaluations during this game.
        # Map the session kind ("primary"/"shadow") to the eval-context
        # game_kind the agent's experiment writer scopes on ("real"/"shadow",
        # targeting.go #932): only a SHADOW session resolves experiments;
        # every primary (menu-driven, player-facing) game is protected as
        # "real". This runs inside the per-session game loop, so the override
        # is scoped to this session's async context (contextvars).
        # Experiment attribution rides the same transaction context
        # as game_kind so an experiment-scoped flag override resolves for
        # this session. Pass them only when set (a non-experiment game
        # leaves them absent, so its experiment targeting is false by
        # construction — real-by-default).
        # set_game_transaction_context maps the session kind (eval_game_kind).
        self._flag_eval_context = set_game_transaction_context(
            game_mode=self.get_game_name(),
            controller_count=len(self.players),
            sensitivity=self.sensitivity.value,
            game_kind=self.game_kind,
            experiment_id=self.experiment_id or None,
            arm=self.arm or None,
        )

    async def _cleanup_after_run(self) -> None:
        """Run-end cleanup shared by every mode's run() finally block.

        Closes open shield charge spans, runs cleanup(), ends any player span
        still open and clears this session's analytics, on every exit path
        (natural end, force-end, error).
        """
        # An error teardown skips _end_shield_feedback: end open charge spans.
        # Before cleanup, so a raising cleanup leaves no span open.
        self._close_shield_charge_spans()
        # Always call cleanup to cancel tracked tasks
        try:
            await self.cleanup()
        finally:
            # A grant arriving while cleanup awaits can open a span after the first close.
            self._close_shield_charge_spans()
            # An early return (force-end after the intro, countdown or warm-up) or an
            # error skips the end step that closes the player spans. Guarded
            # so a failure here neither masks the run's own error nor skips the
            # analytics clear below.
            try:
                self._end_open_player_spans_on_exit()
            except Exception:
                logger.exception(f"Ending the open player spans failed: {self.game_id}")
            try:
                self._close_grouping_spans()
            except Exception:
                logger.exception(f"Closing the grouping spans failed: {self.game_id}")
        # Ensure this session's analytics are always cleared, even on error
        # — per-session targeted cleanup, never a global wipe.
        self._clear_shield_metrics()
        metrics.clear_session_player_analytics(
            list(self.players.keys()),
            self.game_id,
            game_kind=self.game_kind,
        )

    async def _teardown_before_end(self, teardown_span: trace.Span, progress: "_RunProgress") -> None:
        """Teardown steps before ``_end_game_impl`` (the winner effects)."""
        teardown_span.add_event(
            "game.state_changed",
            {
                "game.state": "ENDING",
                "game.id": self.game_id,
            },
        )
        # Stop game music first (inside game.phase.teardown so StopMusic span is a child)
        await self._stop_game_music()
        progress.music_stopped = True

        # Shield LEDs end with the game, before the winner effects.
        await self._end_shield_feedback()
        progress.shield_feedback_ended = True

    # A flow that returns before its teardown when force-ended records the
    # force_ended outcome in run()'s finally (StreamedIntroGameMode).
    _RUN_RETURNS_EARLY_ON_FORCE_END: bool = False

    async def run(self):
        """
        Main entry point to run the game (Template Method, the only run()).

        Modes change the flow through the hooks, never by overriding run():

        - ``_run_span_scope``: the span the run executes in (base: none).
        - ``_run_until_teardown``: start, initialization and gameplay; returns
          False when a force-end skips the teardown.
        - ``_teardown_before_end`` / ``_teardown_after_outcome``: the teardown
          steps around ``_end_game_impl`` and the outcome record.
        - ``_on_run_error``: how an error is reported before it is re-raised.
        - ``_stop_feedback_and_force_end``: the finally's music / shield stop
          and the force-end hook, in the flow's order.

        The base flow: game.phase.initialization (setup: players, colors, stream,
        music), game.phase.gameplay (game.player spans with game.phase.countdown
        children), game.phase.teardown. Phase spans are children of the game span.

        A run that raises or is cancelled before the teardown step still stops
        the game music and ends the shield feedback in ``finally`` (UC-020
        failure postcondition, #2357).
        """
        progress = _RunProgress()
        # The session game root span carries the outcome.
        self._capture_game_root_span()
        with self._run_span_scope() as game_span:
            try:
                if await self._run_until_teardown():
                    self._latch_game_outcome()
                    with self._run_tracer().start_as_current_span("game.phase.teardown") as teardown_span:
                        await self._teardown_before_end(teardown_span, progress)
                        await self._end_game_impl()
                        # The finally adds no shield end after the winner effects.
                        progress.shield_feedback_ended = True
                        self._record_game_outcome(teardown_span)
                        self._teardown_after_outcome()
                        self._record_ground_truth()
                    if game_span is not None:
                        game_span.set_status(Status(StatusCode.OK))
            except Exception as e:
                self._run_failed = True
                await self._on_run_error(e, game_span)
                raise
            finally:
                if self._RUN_RETURNS_EARLY_ON_FORCE_END:
                    self._record_force_ended_outcome_if_unrecorded()
                force_ended = not self.running
                self.running = False
                await self._stop_feedback_and_force_end(force_ended, progress)
                await self._cleanup_after_run()
                logger.info(f"{self.get_game_name()} game finished: {self.game_id}")

    def _send_phase_start(self, span: trace.Span, phase_span: str) -> None:
        """Agent-only ``game.phase.start`` in ``span``'s context: the record's span id is the phase's id."""
        if not span.get_span_context().trace_flags.sampled:
            return
        emit_agent_event(
            "game.phase.start",
            span=span,
            attributes={"game.id": self.game_id, "game.kind": self.game_kind, "game.phase.span": phase_span},
            flush=True,
        )

    def _run_tracer(self) -> trace.Tracer:
        """The tracer of run()'s own spans (game.phase.teardown)."""
        return tracer

    @contextlib.contextmanager
    def _run_span_scope(self):
        """The span run() executes in; yields it, or None (base: the session's game span)."""
        yield None

    async def _run_until_teardown(self) -> bool:
        """Start, initialization and gameplay of the base flow; True: run the teardown."""
        # State transitions
        self.state = GameState.STARTING
        self.running = True  # Set early to allow force_end during countdown
        current_span = trace.get_current_span()
        current_span.add_event(
            "game.state_changed",
            {
                "game.state": "STARTING",
                GAME_MODE_ATTR: self.get_game_name(),
                "game.id": self.game_id,
            },
        )
        await self.event_publisher(GameEvent.GAME_STARTING, {"game_id": self.game_id})

        # Phase 1: Initialization (setup only — no countdown)
        with tracer.start_as_current_span("game.phase.initialization") as init_span:
            init_span.set_attribute("game.id", self.game_id)
            init_span.set_attribute(GAME_MODE_ATTR, self.get_game_name())

            self._emit_sensitivity_metric()
            logger.info(f"Sensitivity: {self.sensitivity.name} ({self.sensitivity.value})")

            # Initialize players
            await self._initialize_players()

            # Validate player count
            if len(self.players) < 2:
                raise ValueError(f"Need at least 2 players, got {len(self.players)}")

            init_span.set_attribute("player_count", len(self.players))

            self._set_flag_eval_context()

            # A mode whose phases send effects over the stream opens it first
            # (UC-021 A1, D-G10); a drain discards frames until the start.
            early_stream = self._opens_stream_before_additional_phases()
            if early_stream:
                await self._start_gameplay_stream()
                self._start_early_stream_drain()
            phases_completed = False
            try:
                # Additional phases (e.g., color_assignment, team_formation)
                # These are children of game.phase.initialization
                for phase in self._get_additional_phases():
                    with tracer.start_as_current_span(f"game.phase.{phase.name}"):
                        await phase.execute()

                # Spoken instructions, when the host switched them on (UC-020 A5):
                # the clip finishes before the music and the countdown start.
                await self._play_instructions(init_span)
                phases_completed = True
            finally:
                # Also stops the drain of a stream the instructions cue opened (a no-op without one).
                await self._stop_early_stream_drain(reopen=phases_completed)

            # A force-end during the instructions skips the start: no music,
            # game_started or countdown; teardown records force_ended.
            force_ended_before_start = not self.running
            if not force_ended_before_start:
                # Start gameplay stream before countdown (needed for countdown effects)
                if self.gameplay_stream is None:
                    await self._start_gameplay_stream()

                # Start game music BEFORE countdown at low volume.
                # OGG decode happens during countdown, so music is ready instantly after.
                await self._start_game_music(volume=self.countdown_music_volume)

        if force_ended_before_start:
            # run() latches the outcome before the teardown.
            logger.info(f"{self.get_game_name()} force-ended before the start: {self.game_id}")
        else:
            await self._run_gameplay_phase()
        return True

    def _teardown_after_outcome(self) -> None:
        """Teardown steps after ``_end_game_impl`` and the outcome record."""
        # Winner effects are sent: close the stream (UC-030 BR-006)
        self._release_gameplay_stream()

        # Clear THIS session's analytics so dashboards show no data when
        # the game is over, without clobbering other concurrent sessions.
        # Game-state gauges are per game_id, so each session
        # removes only its own series (FR-041).
        self._clear_shield_metrics()
        metrics.clear_session_player_analytics(
            list(self.players.keys()),
            self.game_id,
            game_kind=self.game_kind,
        )

    async def _on_run_error(self, error: Exception, game_span: trace.Span | None) -> None:  # noqa: ARG002
        """Report a run error before run() re-raises it: GAME_ERROR is published here (#2025 S2)."""
        logger.error(f"{self.get_game_name()} game error: {error}", exc_info=True)
        self.state = GameState.ENDED
        current_span = trace.get_current_span()
        current_span.add_event(
            "game.state_changed",
            {
                "game.state": "ENDED",
                "game.id": self.game_id,
                "game.error": str(error),
            },
        )
        await self.event_publisher(GameEvent.GAME_ERROR, {"game_id": self.game_id, "error": str(error)})
        self.game_error_published = True

    async def _stop_feedback_and_force_end(self, force_ended: bool, progress: "_RunProgress") -> None:
        """The finally's force-end hook, then the music / shield stop the teardown did not reach."""
        # Call force-end hook if game was externally stopped
        if force_ended:
            await self.on_force_end()
        await self._stop_run_feedback(progress.music_stopped, progress.shield_feedback_ended)

    async def _stop_run_feedback(self, music_stopped: bool, shield_feedback_ended: bool) -> None:
        """Stop the music and end the shield feedback the teardown step did not reach (#2357).

        Best-effort and guarded, so a failure here neither masks the run's own
        error nor skips ``_cleanup_after_run``.
        """
        if not music_stopped:
            try:
                await self._stop_game_music()
            except Exception:
                logger.exception(f"Stopping the game music after a failed run failed: {self.game_id}")
        if not shield_feedback_ended:
            try:
                await self._end_shield_feedback()
            except Exception:
                logger.exception(f"Ending the shield feedback after a failed run failed: {self.game_id}")

    # ========================================================================
    # Helper Methods - Span creation utilities
    # ========================================================================

    async def _play_sound(
        self,
        sound: str | Sound,
        priority: int = 2,
        parent_context: otel_context.Context | None = None,
        volume: float = 1.0,
    ):
        """
        Play sound via Audio service (Phase 29).

        Args:
            sound: Sound enum or string name (e.g., Sound.VOX_CONGRATULATIONS or "congratulations")
            priority: Audio priority (0=LOW, 1=MEDIUM, 2=HIGH, 3=CRITICAL)
            parent_context: Explicit parent context. When set, overrides
                game_cycle_context so the sound span is parented to a specific
                span (e.g., a player lifecycle span for death explosions).
            volume: Playback volume 0.0-1.0 (default 1.0).

        A shadow game plays nothing: it has no audience and the audio service is shared (#1835).
        """
        if not self.audio_client or self._is_shadow_game():
            return

        try:
            from proto import audio_pb2

            # Convert Sound enum to string value if needed
            sound_name = sound.value if isinstance(sound, Sound) else sound
            request = audio_pb2.PlaySoundRequest(file_path=sound_name, volume=volume, priority=priority)

            # Use explicit parent if provided, otherwise fall back to game.cycle
            ctx = parent_context or self.game_cycle_context
            if ctx:
                token = otel_context.attach(ctx)
                try:
                    await self.audio_client.PlaySound(request)
                finally:
                    otel_context.detach(token)
            else:
                await self.audio_client.PlaySound(request)
            logger.debug(f"Playing sound: {sound_name}")
        except Exception as e:
            logger.warning(f"Failed to play sound {sound_name}: {e}")

    # ========================================================================
    # Phase 70: Music Tempo Control (the tempo driver is TempoMixin, games/tempo.py)
    # ========================================================================

    def _lerp(self, a: float, b: float, t: float) -> float:
        """Linear interpolation between a and b by t (0.0 to 1.0)."""
        return a * (1 - t) + b * t

    # The kill path's gauges (games/elimination.py calls these): they record on this
    # module's ``metrics``, which the kill-path tests patch as ``games.base.metrics``.
    def _count_elimination(self, serial: str) -> None:
        """Count a player out: ``dead_count``, ``game_player_alive`` 0 and the elimination order.

        Called by the kill path for every death, and by a mode that takes a
        player out without a death (Tournament's match time limit, UC-025 A1).
        """
        # Phase 70: Track deaths for music tempo timing
        self.dead_count += 1

        # Mark player as dead in metrics - dashboard template variables filter
        # on game_player_alive==1 so dead players naturally disappear from panels.
        # Metric removal happens at game end via clear_session_player_analytics().
        metrics.player_alive.labels(serial=serial, game_id=self.game_id).set(0)

        if self._kill_records_elimination_order(serial):
            self._record_elimination_order(serial)

    def _record_elimination_order(self, serial: str) -> None:
        """Record the player's elimination order (1 = first out) for #730 / #722 §7.

        Makes session.elimination_sequence queryable per game.
        """
        self._elimination_count += 1
        metrics.player_elimination_order.labels(serial=serial, game_id=self.game_id).set(self._elimination_count)

    def _count_death(self) -> None:
        """UC-030 step 9: one ``game_player_deaths_total`` increment per death (respawn modes count each death).

        Aggregate {mode, game_kind} only; per-player order is ``game_player_elimination_order``.
        """
        metrics.player_deaths_total.labels(mode=self.get_game_name(), game_kind=self.game_kind).inc()

    def _game_state_labels(self) -> dict[str, str]:
        """Label set of this game's game-state gauges (FR-041)."""
        return {"game_kind": self.game_kind, "game_id": self.game_id}

    def _emit_sensitivity_metric(self) -> None:
        """Emit this game's sensitivity level for the dashboard and the agent."""
        metrics.game_sensitivity.labels(**self._game_state_labels()).set(self.sensitivity.value)
