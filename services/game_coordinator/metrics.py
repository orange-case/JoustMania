"""
OTEL Push Metrics for Game Coordinator (Issue #103).

Tracks game state, player performance, audio playback, and game quality metrics.
Uses OTLP push at 100ms intervals for real-time dashboard updates.
"""

import threading
from contextlib import suppress

from lib.otel_metrics import Counter, Gauge, Histogram

# Game state metrics
#
# Multi-session: lifecycle metrics carry a ``game_kind`` label
# ("real" / "shadow") so shadow games are observable in their own series
# instead of being invisible or clobbering the primary game's series. The
# primary (menu-driven) session reports game_kind="real"; concurrent
# agent/shadow sessions report game_kind="shadow".
#
# Experiment attribution: the per-live-game GAUGES below carry
# experiment_id/arm. This is safe precisely because they are gauges keyed on the
# already-unbounded game_id and SET per live game — the experiment labels add no
# permanent series; they come and go with the game's game_id and are cleared at
# game end. The cumulative COUNTERS (games_started_total, games_completed_total)
# deliberately do NOT carry experiment_id/arm: a label on a cumulative counter
# creates one permanent series per experiment value, forever (TSDB blow-up), which
# violates the §4.2 cardinality discipline. Per-experiment counts come from the
# span attributes (experiment.id/experiment.arm) plus #979's dedicated game-end
# metric. The per-frame metrics (game_player_accel_magnitude, etc.) carry none of
# this. The gauge labels are "" for a non-experiment game.
active_game = Gauge(
    "game_active",
    "Whether a game is currently running (0=no, 1=yes)",
    ["game_kind", "game_id", "experiment_id", "arm"],
)

# Per-game mode marker (FR-041): value 1 while the game is live, one series per
# game (mode, game_kind, game_id). Set at StartGame, removed at retire
# (GameSession.clear_metrics).
current_game_mode = Gauge(
    "game_current_mode",
    "Mode of a live game (always 1 while the game is live)",
    ["mode", "game_kind", "game_id"],
)

game_duration_seconds = Gauge(
    "game_duration_seconds",
    "Duration of current game in seconds",
    ["game_kind", "game_id", "experiment_id", "arm"],
)

games_started_total = Counter(
    "games_started_total",
    "Total number of games started",
    ["mode", "game_kind"],
)

# outcome: the game's game.outcome (winner, tie, completed, force_ended; #1328) or
# error (the session published game_error), bounded to those five values by
# game_session (GAME_OUTCOMES, completed_game_outcome; #1427).
games_completed_total = Counter(
    "games_completed_total",
    "Total number of games completed, by outcome",
    ["mode", "game_kind", "outcome"],
)

# Shadow game governance.
# Shadow starts rejected by the resource gate while a real (menu-origin) game is
# live and policy=block.
shadow_games_blocked_total = Counter(
    "game_shadow_games_blocked_total",
    "Total shadow game starts rejected because a real game is running (policy=block)",
)

# Shadow games force-ended because a real game started and preempted them.
shadow_games_preempted_total = Counter(
    "game_shadow_games_preempted_total",
    "Total shadow games force-ended (preempted) when a real game started",
)

# Player metrics
active_players = Gauge(
    "game_active_players",
    "Number of players currently in game",
    ["game_kind", "game_id", "experiment_id", "arm"],
)

players_alive = Gauge("game_players_alive", "Number of players currently alive")

# Last writer of the unlabeled players_alive aggregate: (game_id, value). A
# finishing session resets the gauge only while its own game is still the last
# writer: a force-ended session whose thread ends after the next game started
# must not zero that game's count. One module lock makes the write and
# the compare-and-zero atomic; it is held only for the gauge set (writers run
# per init, death or revive, never per frame), never across an await or IO.
_players_alive_lock = threading.Lock()
_players_alive_last: tuple[str, int] | None = None


def set_players_alive(game_id: str, alive_count: int) -> None:
    """Set the players_alive aggregate on behalf of ``game_id`` (primary games only)."""
    global _players_alive_last
    with _players_alive_lock:
        _players_alive_last = (game_id, alive_count)
        players_alive.set(alive_count)


def reset_players_alive_if_owner(game_id: str) -> bool:
    """Zero the players_alive aggregate when ``game_id`` wrote it last (#2244).

    Returns True when the gauge was reset.
    """
    global _players_alive_last
    with _players_alive_lock:
        last = _players_alive_last
        if last is None or last[0] != game_id:
            return False
        _players_alive_last = (game_id, 0)
        players_alive.set(0)
        return True


# Aggregate death count (UC-030 step 9). A cumulative counter, so it carries no
# serial / game_id: those would leave one permanent series per (serial, game) with
# nothing to remove them. Per-game, per-player death order is
# game_player_elimination_order{serial,game_id} (a gauge removed at the session's retire).
player_deaths_total = Counter("game_player_deaths_total", "Total player deaths", ["mode", "game_kind"])

# Hold-mode shield state for the agent (UC-040 A36, A37; UC-042 BR-007). Set at
# every hold transition and with the 2 s energy keyframe, removed at game end.
# Per player (serial, game_id), like game_player_alive:
player_shield_energy_seconds = Gauge(
    "game_player_shield_energy_seconds",
    "Hold-mode shield energy of a player (seconds)",
    ["serial", "game_id"],
)
player_shield_budget_remaining_seconds = Gauge(
    "game_player_shield_budget_remaining_seconds",
    "Hold-mode shield budget a player has left in this game (seconds)",
    ["serial", "game_id"],
)
player_shield_active = Gauge(
    "game_player_shield_active",
    "Whether a player's hold-mode shield is active (1) or not (0)",
    ["serial", "game_id"],
)
# stat: granted_seconds, recharged_seconds, used_seconds, activations, empties (this game).
PLAYER_SHIELD_STATS = ("granted_seconds", "recharged_seconds", "used_seconds", "activations", "empties")
player_shield_stats = Gauge(
    "game_player_shield_stats",
    "Hold-mode shield statistics of a player in this game (seconds or counts by stat)",
    ["serial", "game_id", "stat"],
)
# The agent's shield gate (UC-040 A31, A33): what the coordinator's grant_shield
# refusal reads, per player. stat: grants (agent shields applied in this game),
# charged (1 while a trigger-mode charge is held), cooldown_until (wall-clock epoch
# seconds the cooldown after the last agent shield ends; 0 = none).
PLAYER_SHIELD_GATE_STATS = ("grants", "charged", "cooldown_until")
player_shield_gate = Gauge(
    "game_player_shield_gate",
    "Agent shield gate of a player in this game (count, flag or epoch seconds by stat)",
    ["serial", "game_id", "stat"],
)
# Per game: the shield activation mode the game latched (instant, trigger or hold), value 1,
# and in a hold game every latched hold value (setting = the flag key).
shield_activation_mode = Gauge(
    "game_shield_activation_mode",
    "Shield activation mode a live game latched (always 1)",
    ["mode", "game_kind", "game_id", "experiment_id", "arm"],
)
shield_hold_setting = Gauge(
    "game_shield_hold_setting",
    "Latched hold-mode value of a live game (setting = flag key)",
    ["setting", "game_kind", "game_id", "experiment_id", "arm"],
)


def clear_player_shield_gauges(serial: str, game_id: str) -> None:
    """Remove one player's hold-mode shield gauges (idempotent)."""
    for gauge in (player_shield_energy_seconds, player_shield_budget_remaining_seconds, player_shield_active):
        with suppress(KeyError, ValueError):
            gauge.remove(serial, game_id)
    for total in PLAYER_SHIELD_STATS:
        with suppress(KeyError, ValueError):
            player_shield_stats.remove(serial, game_id, total)
    for stat in PLAYER_SHIELD_GATE_STATS:
        with suppress(KeyError, ValueError):
            player_shield_gate.remove(serial, game_id, stat)


# System metrics
process_cpu_seconds_total = Counter("process_cpu_seconds_total", "Total user and system CPU time spent in seconds")

process_resident_memory_bytes = Gauge("process_resident_memory_bytes", "Resident memory size in bytes")

process_threads = Gauge("process_threads", "Number of active threads")

# Game-state gauges (FR-041). One series per live game, labelled
# (game_kind, game_id), so a concurrent real game and shadow game never overwrite
# each other. Each game removes its own series when it ends
# (clear_game_state_gauges); there is no "0 = no game" sentinel any more.
#
# The effective thresholds are the game-wide values for the game's sensitivity at
# the current tempo. They do NOT include per-player factors (sensitivity_factor,
# handicap, soft_penalty, shields), which _compute_effective_thresholds applies
# per player per frame.
GAME_STATE_LABELS = ["game_kind", "game_id"]

# Music tempo metric (Phase 70)
music_tempo = Gauge(
    "game_music_tempo",
    "Current music playback speed of a live game (1.0=slow, 1.3=fast)",
    GAME_STATE_LABELS,
)

# Sensitivity and threshold metrics (Phase 80)
game_sensitivity = Gauge(
    "game_sensitivity",
    "Sensitivity level of a live game (0=ultra_slow, 1=slow, 2=medium, 3=fast, 4=ultra_fast)",
    GAME_STATE_LABELS,
)

effective_warning_threshold = Gauge(
    "game_effective_warning_threshold",
    "Game-wide effective warning threshold after the tempo LERP (g-force), before per-player factors",
    GAME_STATE_LABELS,
)

effective_death_threshold = Gauge(
    "game_effective_death_threshold",
    "Game-wide effective death threshold after the tempo LERP (g-force), before per-player factors",
    GAME_STATE_LABELS,
)

# Runtime metrics (Phase 43)
actual_update_frequency_hz = Gauge(
    "game_actual_update_frequency_hz",
    "Actual measured update frequency of a live game loop",
    GAME_STATE_LABELS,
)

# Frame consistency metrics. Per game like the gauges above:
# a concurrent real and shadow loop would otherwise overwrite each other.
game_loop_frame_consistency_percent = Gauge(
    "game_loop_frame_consistency_percent",
    "Percentage of a live game loop's frames within target timing (within 50% of target frame time)",
    GAME_STATE_LABELS,
)

game_loop_jitter_ms = Gauge(
    "game_loop_jitter_ms",
    "Standard deviation of a live game loop's recent frame times in milliseconds",
    GAME_STATE_LABELS,
)

# Agent group shields applied in a live game (UC-040 A47, BR-033).
shield_group_grants = Gauge(
    "game_shield_group_grants",
    "Agent group shields applied in a live game",
    GAME_STATE_LABELS,
)

GAME_STATE_GAUGES = (
    shield_group_grants,
    music_tempo,
    game_sensitivity,
    effective_warning_threshold,
    effective_death_threshold,
    actual_update_frequency_hz,
    game_loop_frame_consistency_percent,
    game_loop_jitter_ms,
)

# Feature flag metrics (Phase 44). Evaluations are counted by the standard OpenFeature
# OTel metrics hook (feature_flag_evaluation_*_total, lib/openfeature_otel_hooks.py),
# which replaced the subset counter game_flag_evaluations_total.
flag_configuration_changes_total = Counter(
    "game_flag_configuration_changes_total",
    "Total number of PROVIDER_CONFIGURATION_CHANGED events received",
)

# Process-wide on purpose: the configured rate comes from the
# RuntimeConfigManager singleton, read in an empty evaluation context, so every
# game runs at the same configured rate and there is nothing per game to label.
# A running game keeps the rate it read when its loop started (base.py), so after
# a flag change this gauge leads the games already running.
# The per-game measured rate is game_actual_update_frequency_hz.
current_update_frequency_hz = Gauge(
    "game_current_update_frequency_hz",
    "Current configured update frequency from feature flags (Hz)",
)

game_loop_frames_dropped_total = Counter(
    "game_loop_frames_dropped_total",
    "Total number of frames that exceeded 2x the target frame time",
    ["mode"],
)

# Controller analytics metrics (Phase XX - Analytics)
# Real-time gauges (updated every ~1 second during gameplay)
player_accel_magnitude = Gauge(
    "game_player_accel_magnitude",
    "Current acceleration magnitude for player (g-force)",
    ["serial", "game_id"],
)

player_movement_zone = Gauge(
    "game_player_movement_zone",
    "Current movement zone (0=still, 1=active, 2=warning, 3=danger)",
    ["serial", "game_id"],
)

player_peak_accel = Gauge(
    "game_player_peak_accel",
    "Peak acceleration magnitude for player in current game (g-force)",
    ["serial", "game_id"],
)

player_playstyle = Gauge(
    "game_player_playstyle",
    "Player playstyle classification (0=calm, 1=balanced, 2=active, 3=aggressive)",
    ["serial", "game_id"],
)

player_alive = Gauge(
    "game_player_alive",
    "Player alive status (1=alive, 0=dead)",
    ["serial", "game_id"],
)

# Intervention observability metrics (closes gaps from #722 §7).
# Continuous per-player signals the intervention rules engine and the
# agent fitness functions read as PromQL instant queries.
player_movement_variance = Gauge(
    "game_player_movement_variance",
    "Rolling-window variance of acceleration magnitude (g^2). "
    "Near zero indicates a player gaming the EMA by holding still.",
    ["serial", "game_id"],
)

player_skill_level = Gauge(
    "game_player_skill_level",
    "Derived player skill level in [0.0, 1.0] (blend of playstyle control and "
    "motion smoothness). Complements game_player_playstyle.",
    ["serial", "game_id"],
)

# Whole-game RETAINED movement-variance aggregate. Unlike
# game_player_movement_variance (a rolling-window value frozen at the last live
# frame before elimination), this is the cumulative Welford variance over the
# ENTIRE game and stays meaningful at conclusion — emitted while alive and
# retained by the agent into the OnGameEnd snapshot, exactly like
# game_player_skill_level. The agent's balanced-fitness spike-survival sub-check
# reads this aggregate post-game instead of the frozen last sample.
player_movement_variance_aggregate = Gauge(
    "game_player_movement_variance_aggregate",
    "Whole-game cumulative variance of acceleration magnitude (g^2, Welford). "
    "Retained whole-game aggregate; meaningful at game conclusion (#1024).",
    ["serial", "game_id"],
)

player_elimination_order = Gauge(
    "game_player_elimination_order",
    "Order in which a player was eliminated (1=first out). Set on elimination; "
    "makes session.elimination_sequence queryable per game.",
    ["serial", "game_id"],
)

# Counters (incremented on events)
near_death_events_total = Counter(
    "game_near_death_events_total",
    "Total near-death events (raw > threshold but EMA saved player)",
    ["serial", "game_mode"],
)

player_warnings_total = Counter(
    "game_player_warnings_total",
    "Total warning events triggered",
    ["serial", "game_mode"],
)

# Histogram for distribution analysis
accel_distribution = Histogram(
    "game_accel_distribution",
    "Distribution of acceleration magnitudes during gameplay",
    ["game_mode"],
    buckets=[0.5, 1.0, 1.1, 1.3, 1.5, 1.8, 2.0, 2.5, 3.0, 4.0, 5.0],
)

# Per-game summary metrics (set at end of game)
game_analytics_samples_total = Counter(
    "game_analytics_samples_total",
    "Total analytics samples recorded",
    ["game_mode"],
)


def clear_player_analytics(serial: str, game_id: str = "") -> None:
    """
    Clear analytics metrics for a specific player.

    Only used for targeted cleanup (e.g., controller disconnect during game).
    Normal player death sets player_alive=0 instead, letting dashboard queries
    filter dead players via game_player_alive==1 in template variables.
    Cleanup at game end uses clear_session_player_analytics().

    The per-player gauges carry both a ``serial`` and a ``game_id`` label (#845),
    so removal must supply the same tuple the emitter set; ``game_id`` is the
    ending session's id (mirrors how peak_accel / elimination_order are scoped).
    """
    with suppress(KeyError, ValueError):
        player_accel_magnitude.remove(serial, game_id)

    with suppress(KeyError, ValueError):
        player_movement_zone.remove(serial, game_id)

    with suppress(KeyError, ValueError):
        player_playstyle.remove(serial, game_id)

    with suppress(KeyError, ValueError):
        player_movement_variance.remove(serial, game_id)

    with suppress(KeyError, ValueError):
        player_skill_level.remove(serial, game_id)

    with suppress(KeyError, ValueError):
        player_movement_variance_aggregate.remove(serial, game_id)

    with suppress(KeyError, ValueError):
        player_alive.remove(serial, game_id)

    # peak_accel also carries serial + game_id labels. elimination_order is left to
    # clear_elimination_order at the session's delayed retire.
    if game_id:
        with suppress(KeyError, ValueError):
            player_peak_accel.remove(serial, game_id)

        clear_player_shield_gauges(serial, game_id)


def clear_elimination_order(serials, game_id: str) -> None:
    """Remove one game's ``game_player_elimination_order`` series (UC-041 BR-008).

    Called with the other per-game series at the session's delayed retire, not at
    teardown: a kill right before the end must still be exported, since the gauge is
    the agent's only death channel.
    """
    for serial in serials:
        with suppress(KeyError, ValueError):
            player_elimination_order.remove(serial, game_id)


def clear_game_state_gauges(game_kind: str, game_id: str) -> None:
    """Remove one game's game-state gauge series (FR-041).

    Idempotent: a series that was never set (or was already removed) is ignored.
    """
    for gauge in GAME_STATE_GAUGES:
        with suppress(KeyError, ValueError):
            gauge.remove(game_kind, game_id)


def clear_session_player_analytics(
    serials: list[str],
    game_id: str = "",
    game_kind: str = "real",
) -> None:
    """
    Clear analytics metrics for a single game session at game end (#775).

    Multi-session safe: removes only the gauges belonging to ``serials`` (the ending session's players)
    rather than globally wiping every player gauge. With two concurrent games
    this prevents the first game to end from erasing the live game's dashboard.

    The game-state gauges (GAME_STATE_GAUGES: music_tempo, game_sensitivity,
    effective_*_threshold, actual_update_frequency_hz and the loop-health
    gauges) are per game (game_kind, game_id), so the ending
    session removes only its own series and never touches a concurrent game's.

    Args:
        serials: Controller serials of the ending session's players.
        game_id: The ending session's game_id (for serial+game_id gauges).
        game_kind: The ending session's game_kind ("real" / "shadow").
    """
    for serial in serials:
        clear_player_analytics(serial, game_id)

    clear_game_state_gauges(game_kind, game_id)


# Agent intervention audit metric.
# Incremented by the flag-application layer in a later PR (PR C) for every agent
# intervention attempt — applied or blocked. Also feeds the weighted rate limiter
# and the #731 fitness functions. Defined here now so the metric name
# exists in the registry; zero usages in this PR is intentional.
#   type:         intervention identifier (e.g. 'adjust_player_sensitivity', 'grant_shield')
#   objective:    active session objective ('endurance'/'balanced'/'accelerate'/'chaos')
#   blocked:      'true' if rejected by policy/permission enforcement, else 'false'
#   block_reason: why a blocked attempt was rejected ('not_allowed', 'rate_limited',
#                 'low_battery', 'mode_unsupported', 'no_game', 'handler_error',
#                 'already_charged' (UC-040 A31), 'shield_cap_reached', 'shield_cooldown'
#                 (UC-040 A34), 'shield_pool_full', 'shield_budget_exhausted'
#                 (UC-040 A37), 'game_ending' (UC-040 A42), 'revive_refused', 'experiment_override',
#                 'eliminate_refused' (UC-040 A22)); empty
#                 string for applied (blocked='false') attempts. Cardinality is bounded
#                 to this fixed reason set, so it is safe as a label.
interventions_total = Counter(
    "game_interventions_total",
    "Total agent intervention attempts (applied or blocked by policy)",
    ["type", "objective", "blocked", "block_reason"],
)

# An intervention side effect that could not run on its game's own event loop
# (closed / stopping loop, or timeout): not applied (UC-040 BR-018).
intervention_dispatch_failures_total = Counter(
    "game_intervention_dispatch_failures_total",
    "Intervention side effects not applied because the owning game's event loop was unavailable",
    ["type", "reason"],
)
