"""
Intervention Manager core for JoustMania (#730, PR A).

The game-side application point for agent interventions. The agent ACTs by
writing flag config to the flagd ``interventions`` domain; this manager
subscribes to ``PROVIDER_CONFIGURATION_CHANGED`` and re-evaluates all
intervention flags on every change, runs each candidate through the policy
enforcement chain, and dispatches surviving interventions to a registered
handler.

Design (from docs/research/722-intervention-surface.md §5/§6/§8/§9):

- Two flag shapes:
    * State-shaped flags (music_tempo_override, global_sensitivity_override,
      player_sensitivity_factor, shield_seconds, volume_override) — the game
      converges on the flag value; re-applied whenever the value changes.
    * Edge-triggered (one-shot) flags (eliminate_player, revive_player,
      audio_cue, controller_effect, end_game) — per-game EVENT LISTS
      ``{"epoch","seq","events":[{"seq","payload"}]}`` served by the flag
      service on ``gameId`` (FR-137, UC-053 A4 / BR-006). Each event with a seq
      above the game's last applied one is applied exactly once (idempotent
      across flagd reconnects / re-reads). The list belongs to the game's scope,
      which the flag service drops when the game ends, so a new game starts
      with an empty list and can never replay an earlier game's event.
- Game-scoped values (state, per-player levels and grants) die with the game's
  scope the same way, so a new game never sees a value left over from an
  earlier game.
- Enforcement chain, evaluated *before* any handler runs (defense in depth; the
  agent also self-checks in #726):
    1. allowed-membership   — type must be in ``interventions_allowed`` (agent
       domain, default ``[]``), and nothing while agent ``enabled`` is off
    2. weighted rate limit  — sliding 60s window, weighted cost per class. This
       is the GENEROUS GLOBAL BACKSTOP (#919), budget from
       ``policy.coordinator_backstop_per_minute``; the per-game budget is owned
       authoritatively by the agent (see the defense-in-depth contract block
       below).
    3. battery guard        — player-targeted interventions blocked when the
       target's battery pct < ``policy.battery_threshold``
    4. mode-capability matrix — declarative per-mode gating (§9)
- Every outcome (applied stub or blocked) increments
  ``game_interventions_total{type, objective, blocked}`` and publishes
  ``GameEvent.AGENT_INTERVENTION`` on the EventBus.

PR A scope: handlers are no-op stubs that log + record metrics. The real
effects land in PRs C/D/E by registering handlers against the existing
registry — see ``register_handler`` and the handler contract documented there.
NO game effects are applied in this PR.
"""

import asyncio
import contextlib
import logging
import threading
import time
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import TYPE_CHECKING

from openfeature.evaluation_context import EvaluationContext
from openfeature.flag_evaluation import Reason
from openfeature.provider import ProviderEvent

from lib.flag_eval_visibility import record_flag_fallback
from services.game_coordinator import intervention_effects
from services.game_coordinator.game_loop_dispatch import (
    DEFAULT_DISPATCH_TIMEOUT_S,
    GameLoopDispatcher,
    GameLoopUnavailableError,
)
from services.game_coordinator.games.intervention_moment import (
    decision_id_of,
    event_decision_id,
    run_with_intervention_scope,
)

# The units the manager coordinates. Names that lived in this module
# before the split are re-exported here (``as`` form), so imports and patches of
# ``services.game_coordinator.interventions.<name>`` keep resolving.
from services.game_coordinator.intervention_deferral import (
    DEFERRAL_RECHECK_S as DEFERRAL_RECHECK_S,
)
from services.game_coordinator.intervention_deferral import (
    DEFERRAL_WARN_AFTER_S as DEFERRAL_WARN_AFTER_S,
)
from services.game_coordinator.intervention_deferral import (
    DEFERRAL_WARN_INTERVAL_S as DEFERRAL_WARN_INTERVAL_S,
)
from services.game_coordinator.intervention_deferral import AllowListGate
from services.game_coordinator.intervention_dispatch import (
    _ids_for as _ids_for,
)
from services.game_coordinator.intervention_dispatch import (
    _intervention_scope as _intervention_scope,
)
from services.game_coordinator.intervention_dispatch import (
    count_dispatch_failure,
    record_outcome,
)
from services.game_coordinator.intervention_dispatch import (
    run_in_game_flag_context as run_in_game_flag_context,
)
from services.game_coordinator.intervention_effects import (
    _CONTROLLER_EFFECT_NAMES as _CONTROLLER_EFFECT_NAMES,
)
from services.game_coordinator.intervention_effects import (
    _coerce_float as _coerce_float,
)
from services.game_coordinator.intervention_effects import (
    _effect_targets as _effect_targets,
)
from services.game_coordinator.intervention_effects import (
    _parse_effect_payload as _parse_effect_payload,
)
from services.game_coordinator.intervention_effects import (
    _resolve_effect as _resolve_effect,
)
from services.game_coordinator.intervention_errors import InterventionNotApplied
from services.game_coordinator.intervention_evaluation import (
    _UNKNOWN as _UNKNOWN,
)
from services.game_coordinator.intervention_evaluation import (
    SpecEvaluator,
)
from services.game_coordinator.intervention_evaluation import (
    _active_player_serials as _active_player_serials,
)
from services.game_coordinator.intervention_evaluation import (
    _edge_target_serial as _edge_target_serial,
)
from services.game_coordinator.intervention_evaluation import (
    _event_group_seconds as _event_group_seconds,
)
from services.game_coordinator.intervention_evaluation import (
    _event_payload_string as _event_payload_string,
)
from services.game_coordinator.intervention_evaluation import (
    _event_target_serials as _event_target_serials,
)
from services.game_coordinator.intervention_evaluation import (
    _is_none_value as _is_none_value,
)
from services.game_coordinator.intervention_evaluation import (
    _ReadFailed as _ReadFailed,
)
from services.game_coordinator.intervention_evaluation import (
    _same_value as _same_value,
)
from services.game_coordinator.intervention_evaluation import (
    _spec_default as _spec_default,
)
from services.game_coordinator.intervention_policy import (
    BLOCK_ALREADY_CHARGED as BLOCK_ALREADY_CHARGED,
)
from services.game_coordinator.intervention_policy import (
    BLOCK_ELIMINATE_REFUSED as BLOCK_ELIMINATE_REFUSED,
)
from services.game_coordinator.intervention_policy import (
    BLOCK_GAME_ENDING as BLOCK_GAME_ENDING,
)
from services.game_coordinator.intervention_policy import (
    BLOCK_LOW_BATTERY as BLOCK_LOW_BATTERY,
)
from services.game_coordinator.intervention_policy import (
    BLOCK_MODE_UNSUPPORTED as BLOCK_MODE_UNSUPPORTED,
)
from services.game_coordinator.intervention_policy import (
    BLOCK_NO_GAME as BLOCK_NO_GAME,
)
from services.game_coordinator.intervention_policy import (
    BLOCK_NOT_ALLOWED as BLOCK_NOT_ALLOWED,
)
from services.game_coordinator.intervention_policy import (
    BLOCK_RATE_LIMITED as BLOCK_RATE_LIMITED,
)
from services.game_coordinator.intervention_policy import (
    BLOCK_REVIVE_REFUSED as BLOCK_REVIVE_REFUSED,
)
from services.game_coordinator.intervention_policy import (
    BLOCK_SHIELD_BUDGET_EXHAUSTED as BLOCK_SHIELD_BUDGET_EXHAUSTED,
)
from services.game_coordinator.intervention_policy import (
    BLOCK_SHIELD_CAP_REACHED as BLOCK_SHIELD_CAP_REACHED,
)
from services.game_coordinator.intervention_policy import (
    BLOCK_SHIELD_COOLDOWN as BLOCK_SHIELD_COOLDOWN,
)
from services.game_coordinator.intervention_policy import (
    BLOCK_SHIELD_POOL_FULL as BLOCK_SHIELD_POOL_FULL,
)
from services.game_coordinator.intervention_policy import (
    RATE_LIMIT_WINDOW_SECONDS as RATE_LIMIT_WINDOW_SECONDS,
)
from services.game_coordinator.intervention_policy import (
    EnforcementChain,
)
from services.game_coordinator.intervention_policy import (
    _game_mode_name as _game_mode_name,
)
from services.game_coordinator.intervention_policy import (
    _RateLimiter as _RateLimiter,
)
from services.game_coordinator.intervention_sessions import (
    _PRIMARY_SENTINEL_GAME_ID as _PRIMARY_SENTINEL_GAME_ID,
)
from services.game_coordinator.intervention_sessions import (
    SessionState,
)
from services.game_coordinator.intervention_sessions import (
    SessionView as SessionView,
)

if TYPE_CHECKING:
    from services.game_coordinator.intervention_target import InterventionTarget

logger = logging.getLogger(__name__)


# Rate-limit weighting classes (§5). Cost per applied intervention.
WEIGHT_SOFT = 0.5  # psychological, self-reverting
WEIGHT_MEDIUM = 1.0  # tunable, reversible
WEIGHT_HARD = 2.0  # state-changing, player-visible as unfair if wrong

# ---------------------------------------------------------------------------- #
# Defense-in-depth budget contract
# ---------------------------------------------------------------------------- #
# Intervention rate limiting is enforced in TWO layers, in two languages. They
# have DISTINCT, non-overlapping roles — they are NOT two copies of the same
# governor, and they intentionally read DIFFERENT flags so they cannot drift:
#
#   1. Go agent (AUTHORITATIVE, per-game). services/agent/decision/loopset.go
#      gives every game_id its OWN decision Loop, each with its OWN weighted
#      sliding-window budget read from ``policy.max_interventions_per_minute``
#      (services/agent/decision/ratelimit.go). This is the real per-game
#      governor: it decides how many interventions a single game may receive per
#      minute, and one game exhausting its budget never starves another. The
#      agent only emits an intervention (writes the flag) once its per-game
#      limiter admits it.
#
#   2. Coordinator ``_RateLimiter`` (BACKSTOP, process-global). The limiter in
#      ``intervention_policy.py`` is a single GLOBAL safety net shared across every concurrent
#      game in this coordinator process. Its job is NOT to govern normal
#      per-game pacing — that is the agent's. Its job is to cap a *runaway or
#      buggy* agent that floods flag writes far beyond any sane per-game rate
#      (e.g. a crash-loop appending events, or a misconfigured agent ignoring
#      its own limiter). It is therefore deliberately set to a GENEROUS budget
#      that normal multi-session traffic — several concurrent games each at
#      their own authoritative per-game budget — will never reach. It reads its
#      budget from a SEPARATE flag, ``policy.coordinator_backstop_per_minute``,
#      so it can be tuned independently and so the two layers never share a
#      value that could silently drift.
#
# The drift-prevention rule (closes bug classes #800/#848): the per-game pacing
# decision lives in exactly ONE place — the agent. The coordinator does not
# re-derive or mirror the per-game budget; it only owns the generous global cap.
# To change how aggressively a single game may be nudged, change
# ``policy.max_interventions_per_minute`` (agent). Touch the coordinator backstop
# (``policy.coordinator_backstop_per_minute``) only to move the global flood
# ceiling.

# Generous default global-backstop budget (weighted interventions / minute)
# applied across ALL concurrent games in this coordinator process. Picked so it
# never fires for legitimate traffic: even a busy fleet of several concurrent
# games each spending its full authoritative per-game budget (default 2/min,
# aggressive 4/min) stays well under this; the backstop only trips on an agent
# emitting flag writes at an order-of-magnitude-higher, clearly-runaway rate.
# Overridable via the ``policy.coordinator_backstop_per_minute`` flag.
DEFAULT_COORDINATOR_BACKSTOP_BUDGET = 60.0


# Cross-set consistency (UC-053 A9). The flag service stamps every flag of
# the ``interventions`` set and ``agent/interventions_allowed`` with the snapshot
# it rendered them from. The two sets arrive on separate in-process sync streams
# that reconnect in no fixed order after a flagd restart, so the coordinator
# defers every intervention while the allow-list it holds is older than the
# interventions set it holds; the catching-up ``agent`` stream fires a
# configuration change that re-evaluates them.
SNAPSHOT_EPOCH_KEY = "snapshotEpoch"
SNAPSHOT_VERSION_KEY = "snapshotVersion"


def _hookless_provider(client: object, method: str) -> object | None:
    """The client's provider when it has ``method``, else None.

    The snapshot probes resolve through the provider directly, so the OpenFeature
    metrics and tracing hooks do not count them (they would otherwise dominate the
    per-flag evaluation panels). A stand-in client without a provider is read
    through its own ``get_*_details``. A flag the provider does not know is
    unstamped (the client would return its default without metadata); any other
    exception propagates to the caller, which fails closed.
    """
    provider = getattr(client, "provider", None)
    return provider if provider is not None and callable(getattr(provider, method, None)) else None


def _snapshot_of(details: object) -> "tuple[str, int] | None":
    """``(epoch, version)`` from a read's flag metadata, or None when unstamped."""
    meta = getattr(details, "flag_metadata", None) or {}
    epoch = meta.get(SNAPSHOT_EPOCH_KEY)
    version = meta.get(SNAPSHOT_VERSION_KEY)
    if not isinstance(epoch, str) or not epoch or version is None:
        return None
    try:
        return epoch, int(version)
    except (TypeError, ValueError):
        return None


@dataclass(frozen=True)
class InterventionSpec:
    """Declarative description of one intervention flag.

    The registry maps flag key -> InterventionSpec. PRs C/D/E append entries (or
    attach real handlers) without restructuring this module.

    Attributes:
        flag_key: Flag identifier in the ``interventions`` flagd domain.
        type_id: Stable intervention identifier used as the ``type`` metric
            label and matched against ``interventions_allowed`` (§6). NOTE this
            differs from ``flag_key`` (e.g. flag ``audio_cue`` -> type
            ``play_audio_cue``).
        weight: Rate-limit cost class (WEIGHT_SOFT / MEDIUM / HARD).
        edge_triggered: True for one-shot flags, read as per-game event lists
            (FR-137, UC-053 A4 / BR-006); False for state-shaped flags.
        player_targeted: True if the intervention targets a specific player
            (subject to the battery guard); the payload string's first
            ``:``-delimited field is the target serial.
        none_value: The flag value that means "no intervention" for
            state-shaped flags (e.g. ``0`` for shield_seconds, ``-1`` for
            overrides). Ignored for edge-triggered flags.
        value_kind: How to read the flag: ``"string"``, ``"int"`` or
            ``"float"``.
    """

    flag_key: str
    type_id: str
    weight: float
    edge_triggered: bool
    player_targeted: bool
    value_kind: str = "string"
    none_value: object = None
    # Edge-triggered only: whether a non-empty payload is required to fire.
    # True for serial/sound/effect-carrying flags; False for ``end_game``, whose
    # event alone is the command. An event list is per game, so ``end_game``
    # ends exactly the game whose list carries the event (UC-040 BR-011).
    payload_required: bool = True
    # Player-targeted state only: a one-shot, time-boxed GRANT (shield, toughness boost,
    # soft penalty), not a level. Change-detected per serial on the
    # resolved (variant, value) and dispatched only for the serials whose grant
    # changed (UC-040 BR-012).
    per_serial_grant: bool = False


# Registry of all intervention flags (§8). Ordered to keep diffs stable.
# PRs C/D/E attach real handlers via register_handler(); they do NOT edit this
# table. Adding a brand-new intervention appends one row here.
INTERVENTION_SPECS: tuple[InterventionSpec, ...] = (
    # --- State-shaped flags ---
    InterventionSpec(
        flag_key="music_tempo_override",
        type_id="adjust_music_tempo",
        weight=WEIGHT_MEDIUM,
        edge_triggered=False,
        player_targeted=False,
        value_kind="float",
        none_value=0,
    ),
    InterventionSpec(
        flag_key="global_sensitivity_override",
        type_id="adjust_global_sensitivity",
        weight=WEIGHT_HARD,
        edge_triggered=False,
        player_targeted=False,
        value_kind="int",
        none_value=-1,
    ),
    InterventionSpec(
        flag_key="player_sensitivity_factor",
        type_id="adjust_player_sensitivity",
        weight=WEIGHT_MEDIUM,
        edge_triggered=False,
        player_targeted=True,
        value_kind="float",
        # 1.0 is the neutral default; a value != 1.0 is an intervention.
        none_value=1.0,
    ),
    InterventionSpec(
        flag_key="player_handicap_factor",
        type_id="set_player_handicap",
        weight=WEIGHT_MEDIUM,
        edge_triggered=False,
        player_targeted=True,
        value_kind="float",
        # 1.0 is the neutral default; a value != 1.0 is an intervention (#1107,
        # #1103 MVP action 1). MULTIPLICATIVE per-player handicap that COMPOSES
        # with player_sensitivity_factor (>1 harder to die / "help", <1 easier /
        # "rein in"), clamped [0.5, 2.0] in base.py. SHADOW-game-only initially:
        # the agent's interventions_allowed allow-list (the load-bearing gate)
        # only includes this type in the new `shadow_experimental` variant.
        none_value=1.0,
    ),
    InterventionSpec(
        flag_key="shield_seconds",
        type_id="grant_shield",
        weight=WEIGHT_MEDIUM,
        edge_triggered=False,
        player_targeted=True,
        value_kind="float",
        none_value=0,
        per_serial_grant=True,
    ),
    InterventionSpec(
        flag_key="toughness_boost_seconds",
        type_id="toughness_boost",
        weight=WEIGHT_MEDIUM,
        edge_triggered=False,
        player_targeted=True,
        # STRING value "<seconds>" or "<seconds>:<boost>":
        # a TIME-BOXED handicap boost (much harder to die, but NOT immune — vs
        # grant_shield's total grace_until immunity). The handler parses both
        # forms; malformed values are a safe no-op. "0" (and "none"/empty) means
        # no toughness boost. SHADOW-game-only initially: allow-listed ONLY via the
        # `shadow_experimental` interventions_allowed variant (the load-bearing
        # gate), never the real-facing ambient/standard/full variants.
        value_kind="string",
        none_value="0",
        per_serial_grant=True,
    ),
    InterventionSpec(
        flag_key="soft_penalty_action",
        type_id="soft_penalty",
        weight=WEIGHT_MEDIUM,
        edge_triggered=False,
        player_targeted=True,
        # STRING value "warn" (default) | "tighten" | "tighten:<seconds>:<factor>":
        # a graduated, RECOVERABLE alternative to the permanent eliminate_player.
        # "warn" fires the visible warning feedback
        # (no threshold change). "tighten" applies a short EXPIRING handicap
        # REDUCTION (factor < 1.0, the MIRROR of toughness_boost) making the player
        # temporarily EASIER to die — still clamped [0.5, 2.0], never instant-
        # death. The handler parses all forms; malformed -> safe default ("warn").
        # "none"/empty means no penalty. SHADOW-game-only initially: allow-listed
        # ONLY via the `shadow_experimental` interventions_allowed variant (the
        # load-bearing gate), never the real-facing ambient/standard/full variants.
        value_kind="string",
        none_value="none",
        per_serial_grant=True,
    ),
    # #1143 — auto_rubberband: a SINGLE global decision the
    # COORDINATOR expands across players from the live skill gap. STRING value
    # "off" (default / neutral) | "gentle" | "strong" (compression strength). The
    # handler ranks alive players by headroom to their death threshold and applies
    # a bounded, time-boxed handicap BOOST to the trailing player(s) — compressing
    # the gap without inverting standings or causing instant death (bounded in the
    # method + the [0.5,2.0] clamp). Reuses the #1107 handicap mechanism, composes
    # by MAX (alongside toughness_boost). NOT player_targeted — the global value is
    # read once and expanded. SHADOW-game-only initially: allow-listed ONLY via the
    # `shadow_experimental` interventions_allowed variant (the load-bearing gate),
    # never the real-facing ambient/standard/full variants.
    InterventionSpec(
        flag_key="auto_rubberband",
        type_id="auto_rubberband",
        weight=WEIGHT_MEDIUM,
        edge_triggered=False,
        player_targeted=False,
        value_kind="string",
        # "off" (and "none"/empty) means no rubberbanding (the default-safe value).
        none_value="off",
    ),
    InterventionSpec(
        flag_key="volume_override",
        type_id="adjust_volume",
        weight=WEIGHT_SOFT,
        edge_triggered=False,
        player_targeted=False,
        value_kind="float",
        none_value=-1,
    ),
    # #766 F6 — two new state-shaped difficulty/pacing levers (weight Medium).
    InterventionSpec(
        flag_key="global_difficulty_factor",
        type_id="adjust_global_difficulty",
        weight=WEIGHT_MEDIUM,
        edge_triggered=False,
        player_targeted=False,
        value_kind="float",
        # 1.0 is the neutral default; a value != 1.0 is an intervention.
        none_value=1.0,
    ),
    InterventionSpec(
        flag_key="pacing_profile",
        type_id="set_pacing_profile",
        weight=WEIGHT_MEDIUM,
        edge_triggered=False,
        player_targeted=False,
        value_kind="string",
        # "none" (and "default") mean the neutral init-resolved windows.
        none_value="none",
    ),
    # #1117 — ramp_tempo: a scheduled tempo CURVE that fills
    # the #1114 ``agent``-mode tempo seam. Session-scoped STRING state flag whose
    # value is "<target>:<seconds>:<curve>" (e.g. "1.3:8:linear"); the handler
    # records a RampDescriptor and the game's 100ms _check_music_speed loop
    # interpolates toward target via the single tempo-owner seam (no second
    # writer). SHADOW-game-only initially: allow-listed ONLY via the
    # `shadow_experimental` interventions_allowed variant (the load-bearing gate),
    # never the real-facing ambient/standard/full variants.
    InterventionSpec(
        flag_key="tempo_ramp",
        type_id="ramp_tempo",
        weight=WEIGHT_MEDIUM,
        edge_triggered=False,
        player_targeted=False,
        value_kind="string",
        # "none" means no ramp (the default-safe neutral value).
        none_value="none",
    ),
    # --- Edge-triggered (one-shot) flags ---
    InterventionSpec(
        flag_key="eliminate_player",
        type_id="eliminate_player",
        weight=WEIGHT_HARD,
        edge_triggered=True,
        player_targeted=True,
    ),
    InterventionSpec(
        flag_key="revive_player",
        type_id="revive_player",
        weight=WEIGHT_HARD,
        edge_triggered=True,
        player_targeted=True,
    ),
    InterventionSpec(
        flag_key="audio_cue",
        type_id="play_audio_cue",
        weight=WEIGHT_SOFT,
        edge_triggered=True,
        player_targeted=False,
    ),
    InterventionSpec(
        flag_key="controller_effect",
        type_id="send_controller_effect",
        weight=WEIGHT_SOFT,
        edge_triggered=True,
        # serial may be empty (broadcast); guard treats missing target as
        # "don't block", so broadcast effects are not battery-gated.
        player_targeted=True,
    ),
    InterventionSpec(
        flag_key="end_game",
        type_id="end_game",
        weight=WEIGHT_HARD,
        edge_triggered=True,
        player_targeted=False,
        # the event alone ends the game whose list carries it
        payload_required=False,
    ),
    # #2467 PR 3 (UC-048 A13, A14, BR-014): one event shields 2 to 6 players, all or
    # nothing. Payload {"serials": "A,B,C", "seconds": "5"}. Not player_targeted: the
    # handler checks the battery of every serial itself (lifecycle_handlers.py).
    # Allowed in every mode, like grant_shield (no MODE_CAPABILITY_DENY row).
    InterventionSpec(
        flag_key="group_shield",
        type_id="grant_group_shield",
        weight=WEIGHT_MEDIUM,
        edge_triggered=True,
        player_targeted=False,
    ),
)

# Map game mode name (BaseGameMode.get_game_name()) -> set of type_ids that are
# DISALLOWED in that mode (§9). Absence from this table = allowed everywhere.
# Only modes with a restriction appear. ``send_controller_effect`` is restricted
# in hidden-role modes because LED effects can leak roles; ``revive_player`` is
# gated where reviving breaks the mode; ``adjust_global_sensitivity`` is gated
# where it interacts with per-role threshold overrides. ``adjust_global_difficulty``
# inherits the ``adjust_global_sensitivity`` row (same role-threshold
# interaction); ``set_pacing_profile`` inherits ``adjust_music_tempo`` (allowed
# everywhere — no deny rows).
MODE_CAPABILITY_DENY: dict[str, frozenset[str]] = {
    # Hidden-role modes: LED effects leak roles; revive breaks hidden roles.
    "Werewolf": frozenset(
        {"send_controller_effect", "revive_player", "adjust_global_sensitivity", "adjust_global_difficulty"}
    ),
    "Traitor": frozenset(
        {"send_controller_effect", "revive_player", "adjust_global_sensitivity", "adjust_global_difficulty"}
    ),
    # Bracket / queue modes: revive breaks the bracket/queue.
    "Tournament": frozenset({"revive_player"}),
    "Fight Club": frozenset({"revive_player"}),
    # Permanent-elimination modes: revive is opt-in only (off by default in M2).
    "FFA": frozenset({"revive_player"}),
    "Teams": frozenset({"revive_player"}),
    "Random Teams": frozenset({"revive_player"}),
    # Zombie role thresholds are asymmetric; global override/difficulty interacts.
    "Zombie": frozenset({"adjust_global_sensitivity", "adjust_global_difficulty"}),
    # Swapper: revive leaves team state ambiguous.
    "Swapper": frozenset({"revive_player"}),
}


# A handler receives the InterventionContext and applies the effect. In PR A all
# handlers are the no-op stub. Handlers are async to match game_coordinator
# patterns and because PRs C/D/E will await RPCs / game-state mutations.
Handler = Callable[["InterventionContext"], Awaitable[None]]


@dataclass
class InterventionContext:
    """Everything a handler needs to apply one intervention.

    Passed to the registered handler after the enforcement chain passes. PRs
    C/D/E read ``payload``/``target_serial``/``value`` to do the real work.

    Attributes:
        spec: The InterventionSpec for this flag.
        value: The evaluated flag value, typed per ``spec.value_kind`` for
            state-shaped flags. For edge-triggered flags, the event's payload
            string (same as ``payload``).
        payload: For edge-triggered flags, the event payload as a string
            (``"<serial>"``, ``"<sound_id>"``, ``"<serial>:<effect>"`` or ``""``
            for end_game; see ``_event_payload_string``). Empty string for
            state-shaped flags.
        target_serial: Resolved target controller serial, or ``None`` if the
            intervention is not player-targeted / is a broadcast.
        game: The live game instance (BaseGameMode) or ``None`` if no game is
            running.
        objective: Dominant session objective label for metrics/events.
        game_id: The owning session's game id (#838). Handlers that re-resolve
            per-player targets (e.g. player_sensitivity_factor) MUST pass this
            into ``resolve_player_targets(game_id=...)`` so per-game targeting
            composes. ``""`` for the synthesized primary session (no gameId).
    """

    spec: InterventionSpec
    value: object
    payload: str
    target_serial: str | None
    game: "InterventionTarget | None"
    objective: str
    game_id: str = ""


class InterventionFlagReader:
    """Every flag read of the intervention path: target resolution and policy values.

    Owns the three flag clients (``interventions``, ``agent``, ``user`` domains).
    It resolves intervention flags per session (``gameId``) and per player
    (``targetingKey``), reads the event lists and per-serial grants and levels,
    and reads the policy values the enforcement chain and the manager use
    (allow-list, battery threshold, backstop budget, objectives, cue toggle) and
    the snapshot stamps of the consistency gate. Reads stay in this module: the
    flagd contract test (``tests/contract/test_flagd_resolution.py``) pins each
    read site by file and function.
    """

    def __init__(self, battery_provider: Callable[[str], float | None] | None = None):
        self._interventions_client = None  # interventions domain
        self._agent_client = None  # agent domain (backstop reads)
        self._user_client = None  # user domain (on-device cue gate)
        self._battery_provider = battery_provider

    def connect(self) -> None:
        """Initialize the three flag domains and their clients. Raises on failure."""
        from lib.feature_flags import get_flag_client, init_flag_domain

        init_flag_domain("interventions")
        self._interventions_client = get_flag_client("interventions")

        init_flag_domain("agent")
        self._agent_client = get_flag_client("agent")

        # User domain holds the operator-facing cue mute toggle.
        init_flag_domain("user")
        self._user_client = get_flag_client("user")

    @property
    def has_interventions_client(self) -> bool:
        return self._interventions_client is not None

    @property
    def has_agent_client(self) -> bool:
        return self._agent_client is not None

    # ------------------------------------------------------------------ #
    # Intervention flags, per session and per player
    # ------------------------------------------------------------------ #
    @staticmethod
    def session_context(game_id: str, *, targeting_key: str | None = None) -> EvaluationContext:
        """Build the per-session EvaluationContext (#838).

        Adds ``gameId`` so flagd targeting rules can scope an intervention to one
        game. An empty ``game_id`` (the synthesized primary session in the legacy
        ``get_game`` path) adds NO ``gameId`` attribute, so un-targeted flags
        resolve exactly as before. ``targeting_key`` (the player serial) is set
        for per-player resolution so per-player and per-game rules compose.
        """
        attributes = {"gameId": game_id} if game_id else {}
        return EvaluationContext(targeting_key=targeting_key, attributes=attributes)

    def _read_flag(self, spec: InterventionSpec, session: "SessionView") -> object:
        client = self._interventions_client
        if client is None:
            return _spec_default(spec)
        ctx = self.session_context(session.game_id)
        if spec.value_kind == "int":
            return client.get_integer_value(spec.flag_key, spec.none_value, ctx)
        if spec.value_kind == "float":
            return client.get_float_value(spec.flag_key, float(spec.none_value), ctx)
        # State-shaped string flags carry a string none_value (e.g. pacing_profile
        # "none").
        string_default = spec.none_value if isinstance(spec.none_value, str) else ""
        return client.get_string_value(spec.flag_key, string_default, ctx)

    def _read_event_list(
        self, spec: InterventionSpec, session: "SessionView"
    ) -> "tuple[str, int, list[tuple[int, dict, str | None]]] | None":
        """Read an event list as ``(epoch, seq, [(seq, payload, decision_id), ...])``,
        or None on a failed or malformed read (fail closed: nothing applies).

        ``decision_id`` is the entry's intervention id, None without one; it never
        makes a read fail (UC-101 BR-003, NFR-061)."""
        client = self._interventions_client
        if client is None:
            return None
        ctx = self.session_context(session.game_id)
        try:
            details = client.get_object_details(spec.flag_key, {}, ctx)
        except Exception as e:
            record_flag_fallback(spec.flag_key, type(e).__name__, str(e))
            return None
        if details.error_code is not None or details.reason == Reason.ERROR:
            error_code = getattr(details.error_code, "value", None) or str(details.error_code or "ERROR")
            record_flag_fallback(spec.flag_key, error_code, details.error_message or "")
            return None
        value = details.value
        if not isinstance(value, dict):
            return None
        try:
            epoch = str(value.get("epoch") or "")
            seq = int(value.get("seq") or 0)
            events = [
                (
                    int(e["seq"]),
                    e.get("payload") if isinstance(e.get("payload"), dict) else {},
                    event_decision_id(e),
                )
                for e in value.get("events") or []
                if isinstance(e, dict) and "seq" in e
            ]
        except (TypeError, ValueError):
            return None
        return epoch, seq, events

    def read_levels(self, spec: InterventionSpec, game_id: str, serials, ids: "dict | None" = None) -> dict:
        """Read a per-player LEVEL flag for each battery-eligible serial as its
        float value, or ``_UNKNOWN`` on a flagd error. Battery-gated serials are
        left out (their level is neither applied nor baselined). ``ids``, when
        given, gets each serial's intervention id served with its value (UC-101
        BR-004); the id never takes part in change detection (NFR-061)."""
        threshold = self.battery_threshold()
        reads: dict = {}
        for serial in serials:
            pct = self.battery_pct(serial)
            if pct is not None and pct < threshold:
                continue  # battery guard: skip low-battery controllers
            grant = self._read_grant(spec, game_id, serial)
            if grant is _UNKNOWN:
                reads[serial] = grant
                continue
            reads[serial] = grant[1]
            if ids is not None and grant[2]:
                ids[serial] = grant[2]
        return reads

    def _read_grant(
        self, spec: InterventionSpec, game_id: str, serial: str
    ) -> "tuple[object, object, str | None] | _ReadFailed":
        """Read one serial's grant as ``(variant, value, decision_id)`` or ``_UNKNOWN``.

        Read with evaluation details: the value getters never raise and return
        the default on error, which would look like "no grant" and move the
        baseline (#1299). The variant is part of the grant identity because the
        flag service generates a fresh opaque ``r<epoch>_<n>`` variant per write.
        ``decision_id`` is the intervention id served with the resolved variant
        (``decisionId.<variant>`` metadata, UC-101 BR-003), None without one; it is
        informational only and never makes a read fail (NFR-061).
        """
        client = self._interventions_client
        if client is None:
            return _UNKNOWN
        ctx = self.session_context(game_id, targeting_key=serial)
        default = _spec_default(spec)
        try:
            if spec.value_kind == "string":
                details = client.get_string_details(spec.flag_key, str(default), ctx)
            else:
                details = client.get_float_details(spec.flag_key, float(default), ctx)
        except Exception as e:
            record_flag_fallback(spec.flag_key, type(e).__name__, str(e))
            return _UNKNOWN
        if details.error_code is not None or details.reason == Reason.ERROR:
            error_code = getattr(details.error_code, "value", None) or str(details.error_code or "ERROR")
            record_flag_fallback(spec.flag_key, error_code, details.error_message or "")
            return _UNKNOWN
        value = details.value
        if spec.value_kind == "string":
            if not isinstance(value, str):
                return _UNKNOWN
        else:
            value = _coerce_float(value)
            if value is None:
                return _UNKNOWN
        return (details.variant, value, decision_id_of(details))

    def resolve_player_targets(
        self,
        flag_key: str,
        default,
        game: "InterventionTarget | None",
        *,
        value_kind: str = "float",
        battery_gate: bool = True,
        game_id: str = "",
    ) -> dict:
        """Evaluate a per-player targeted flag for every active player serial.

        For each active serial in ``game.players`` the flag is evaluated with an
        ``EvaluationContext(targeting_key=serial, attributes={gameId})`` so
        flagd's per-serial AND per-game targeting rules resolve and compose in a
        single rule (#838: a rule may match both ``targetingKey == serial`` and
        ``gameId == <id>``). Serials with no targeting match receive ``default``.
        When ``battery_gate`` is True, any serial whose battery pct is below
        ``policy.battery_threshold`` is dropped from the result (the per-serial
        equivalent of the chain's battery guard, which is a no-op for fan-out
        flags: they have no single chain-level target).

        Reusable across player-targeted state interventions: PR C uses it for
        ``player_sensitivity_factor``; PR D uses it for ``shield_seconds``.

        Args:
            flag_key: Flag to evaluate in the ``interventions`` domain.
            default: Neutral value returned for serials with no targeting match
                (e.g. 1.0 for sensitivity factor, 0 for shield seconds).
            game: Live game whose ``players`` dict supplies the active serials.
            value_kind: ``"float"``, ``"int"`` or ``"string"`` (flag value type).
                ``"string"`` (#1129 toughness_boost) returns raw per-serial strings
                for the handler to parse; float/int return coerced numbers.
            battery_gate: If True, drop serials below the battery threshold.
            game_id: Owning session's game id, added as ``gameId`` to the context
                (#838). Empty string adds no ``gameId`` (legacy primary path).

        Returns:
            ``{serial: value}`` for every active serial that should be acted on.
            Battery-gated serials are omitted entirely.
        """
        result: dict = {}
        client = self._interventions_client
        serials = _active_player_serials(game)
        threshold = self.battery_threshold() if battery_gate else None

        for serial in serials:
            if threshold is not None:
                pct = self.battery_pct(serial)
                if pct is not None and pct < threshold:
                    continue  # battery guard: skip low-battery controllers

            if client is None:
                value = default
            else:
                ctx = self.session_context(game_id, targeting_key=serial)
                try:
                    if value_kind == "int":
                        value = float(client.get_integer_value(flag_key, int(default), ctx))
                    elif value_kind == "string":
                        # Raw string returned for the handler to parse.
                        value = str(client.get_string_value(flag_key, str(default), ctx))
                    else:
                        value = float(client.get_float_value(flag_key, float(default), ctx))
                except Exception as e:  # one bad serial must not drop the rest
                    logger.debug(f"InterventionManager: per-serial read failed for {serial}: {e}")
                    value = default
            result[serial] = value

        return result

    # ------------------------------------------------------------------ #
    # Cross-set consistency stamps (UC-053 A9)
    # ------------------------------------------------------------------ #
    def interventions_snapshot(self) -> "tuple[str, int] | None":
        """The interventions set's stamp, probed on ``end_game`` (in every rendering of the set)."""
        from openfeature.exception import FlagNotFoundError

        provider = _hookless_provider(self._interventions_client, "resolve_object_details")
        try:
            if provider is not None:
                details = provider.resolve_object_details("end_game", {}, EvaluationContext())
            else:
                details = self._interventions_client.get_object_details("end_game", {}, EvaluationContext())
        except FlagNotFoundError:
            return None
        return _snapshot_of(details)

    def allow_list_snapshot(self) -> "tuple[str, int] | None":
        """The stamp of ``agent/interventions_allowed``."""
        from openfeature.exception import FlagNotFoundError

        provider = _hookless_provider(self._agent_client, "resolve_string_details")
        try:
            if provider is not None:
                details = provider.resolve_string_details("interventions_allowed", "", EvaluationContext())
            else:
                details = self._agent_client.get_string_details("interventions_allowed", "", EvaluationContext())
        except FlagNotFoundError:
            return None
        return _snapshot_of(details)

    # ------------------------------------------------------------------ #
    # Policy values
    # ------------------------------------------------------------------ #
    def allowed_types(self) -> set[str]:
        # interventions_allowed is a STRING flag whose value is a comma-separated
        # list of intervention ids (`none` == ""). It used to be a LIST/object
        # flag, but the agent's flagd RPC resolver silently TYPE_MISMATCHes
        # get_object_value on a top-level LIST flag and falls back to its empty
        # default, blocking EVERY intervention even with a non-`none` variant
        # active. Both sides now read a string and parse it identically;
        # the coordinator's in-process resolver reads the string cleanly too.
        if self._agent_client is None:
            return set()
        try:
            # The human stop writes `enabled` alone (UC-040 BR-004): a stopped agent gets nothing applied.
            if self._agent_client.get_boolean_value("enabled", False, EvaluationContext()) is not True:
                return set()
            value = self._agent_client.get_string_value("interventions_allowed", "", EvaluationContext())
            if isinstance(value, str):
                return {token.strip() for token in value.split(",") if token.strip()}
        except Exception as e:
            logger.debug(f"InterventionManager: interventions_allowed read failed: {e}")
        return set()

    def battery_threshold(self) -> float:
        if self._agent_client is None:
            return 20.0
        try:
            return float(self._agent_client.get_integer_value("policy.battery_threshold", 20, EvaluationContext()))
        except Exception:
            return 20.0

    def battery_pct(self, serial: str) -> float | None:
        if self._battery_provider is None:
            return None
        try:
            return self._battery_provider(serial)
        except Exception:
            return None

    def backstop_budget(self) -> float:
        """The GLOBAL BACKSTOP budget (#919): ``policy.coordinator_backstop_per_minute``.

        NOT ``policy.max_interventions_per_minute``: that is the agent's
        authoritative per-game budget, which the coordinator must not mirror (that
        is exactly the drift the contract forbids). Falls back to
        ``DEFAULT_COORDINATOR_BACKSTOP_BUDGET`` when the flag is unreachable.
        """
        budget = DEFAULT_COORDINATOR_BACKSTOP_BUDGET
        if self._agent_client is not None:
            try:
                budget = float(
                    self._agent_client.get_integer_value(
                        "policy.coordinator_backstop_per_minute",
                        int(DEFAULT_COORDINATOR_BACKSTOP_BUDGET),
                        EvaluationContext(),
                    )
                )
            except Exception:
                budget = DEFAULT_COORDINATOR_BACKSTOP_BUDGET
        return budget

    def dominant_objective(self) -> str:
        """Return the highest-weight objective from the agent ``objectives``
        flag (weighted dict). Defaults to ``balanced``."""
        if self._agent_client is None:
            return "balanced"
        try:
            weights = self._agent_client.get_object_value("objectives", {}, EvaluationContext())
            if isinstance(weights, dict) and weights:
                return max(weights, key=lambda k: weights.get(k, 0.0))
        except Exception:
            pass
        return "balanced"

    def cue_enabled(self) -> bool:
        """Whether the on-device agent-intervention cue is enabled (#818).

        Reads the ``agent_intervention_cue`` toggle from the user domain. Defaults
        to ON (fail-open to visibility) when the client is unavailable or the read
        fails — the whole point of #818 is that the operator can SEE agent action.
        """
        if self._user_client is None:
            return True
        try:
            return bool(self._user_client.get_boolean_value("agent_intervention_cue", True, EvaluationContext()))
        except Exception as e:
            logger.debug(f"agent cue gate read failed, defaulting to enabled: {e}")
            return True


class _Forward:
    """An attribute kept on another object (``<owner>.<name>``), readable and settable here.

    Keeps the manager's flag clients and deferral state addressable as before the
    split: ``mgr._agent_client = stub`` reaches the flag reader.
    """

    def __init__(self, owner: str, name: str):
        self._owner = owner
        self._name = name

    def __get__(self, obj, objtype=None):
        if obj is None:
            return self
        return getattr(getattr(obj, self._owner), self._name)

    def __set__(self, obj, value) -> None:
        setattr(getattr(obj, self._owner), self._name, value)


class InterventionManager:
    """Coordinates the intervention path: flag change -> evaluation -> enforcement -> handler -> outcome.

    Lifecycle mirrors RuntimeConfigManager: ``start()`` initializes flag
    domains, registers the change handler, and does an initial evaluation;
    ``stop()`` removes the handler. ``get_game`` is a callable returning the
    live game (or None).

    Split by responsibility: ``InterventionFlagReader`` (every flag read:
    targets, policy values), ``SpecEvaluator`` (change detection),
    ``EnforcementChain`` + ``_RateLimiter`` (policy, backstop), ``AllowListGate``
    (UC-053 A9), ``SessionState`` (per-game buckets, dispatchers) and
    ``intervention_dispatch`` (flag context, scope, outcome record). The manager
    owns the handler registry and runs each change through chain, handler,
    record and cue.
    """

    # Flag clients and deferral state live on their units.
    _interventions_client = _Forward("_reads", "_interventions_client")
    _agent_client = _Forward("_reads", "_agent_client")
    _user_client = _Forward("_reads", "_user_client")
    _battery_provider = _Forward("_reads", "_battery_provider")
    _deferral_since = _Forward("_gate", "since")
    _deferral_recheck = _Forward("_gate", "recheck")

    def __init__(
        self,
        event_publisher: Callable[[str, dict], Awaitable[None]],
        get_game: Callable[[], object | None] | None = None,
        battery_provider: Callable[[str], float | None] | None = None,
        time_fn: Callable[[], float] = time.monotonic,
        end_game_fn: Callable[[str, str], Awaitable[tuple[bool, str]]] | None = None,
        get_sessions: Callable[[], list["SessionView"]] | None = None,
    ):
        """
        Args:
            event_publisher: Async ``publish(event_type, data)`` (EventBus) for
                the PRIMARY session — used as the publisher for the synthesized
                primary view in the legacy ``get_game`` path.
            get_game: Legacy seam. Returns the live PRIMARY BaseGameMode instance
                or None. When supplied without ``get_sessions``, the manager
                evaluates against a single primary-only session (today's
                semantics) — un-targeted interventions touch only the primary
                game. Mutually exclusive with ``get_sessions`` in practice
                (``get_sessions`` wins when both are given).
            battery_provider: ``(serial) -> battery_pct | None``. None means the
                battery is unknown and the guard does not block. Injected for
                testability (the real source is the controller manager's
                ``controller_battery_pct`` metric).
            time_fn: Monotonic clock (injectable for tests).
            end_game_fn: Async ``end_game(game_id, reason) -> (success, error)``
                used by the ``end_game`` intervention handler to share the
                servicer's force-end path (#730, PR E). It must end exactly the
                session ``game_id`` (``""`` = the legacy single primary), never
                "the current game" (UC-040 BR-011, #1310). None disables the
                handler's effect.
            get_sessions: Per-session seam (#838). Returns one ``SessionView``
                per LIVE session (primary + every shadow). The manager evaluates
                every intervention flag against each session with a per-session
                ``{gameId, targetingKey=serial}`` context and applies/publishes
                on THAT session. A flag with no ``gameId`` targeting rule (the
                baseline default, or a flag nothing has targeted) resolves to the
                same value for every live session; one-shot flags are per-game event lists, so each
                session reads only its own events (FR-137, BR-011).
        """
        self._publish = event_publisher
        # Single-flight evaluate_all (re-review N1): one flag-service write
        # wakes every flagd domain watcher, so PROVIDER_CONFIGURATION_CHANGED
        # fires once per domain; overlapping passes coalesce into one follow-up.
        self._eval_state_lock = threading.Lock()
        self._eval_running = False
        self._eval_dirty = False
        self._get_game = get_game
        self._get_sessions = get_sessions
        self._time_fn = time_fn
        self._end_game_fn = end_game_fn
        # Strong references to the tasks this manager creates: the loop keeps only a weak one.
        self._background_tasks: set[asyncio.Task] = set()

        self._reads = InterventionFlagReader(battery_provider)
        # Per-game state: change-detection buckets (``game_id -> flag_key -> value``),
        # last applied events and game-loop dispatchers, under one lock. Aliased
        # here because tests and the pass read them by these names.
        self._sessions = SessionState()
        self._lock = self._sessions.lock
        self._last_state_value = self._sessions.last_state_value
        self._last_event = self._sessions.last_event
        self._dispatchers = self._sessions.dispatchers
        self._dispatch_timeout_s = DEFAULT_DISPATCH_TIMEOUT_S
        # The re-check delay is read from this module when scheduled, so a patched value applies.
        self._gate = AllowListGate(
            self._reads,
            time_fn=time_fn,
            spawn_evaluation=self._spawn_evaluate_all,
            recheck_s=lambda: DEFERRAL_RECHECK_S,
        )

        # Global BACKSTOP limiter: generous, process-global, NOT the
        # per-game governor (that is the agent's). See the defense-in-depth
        # contract block at the top of this module.
        self._rate_limiter = _RateLimiter(budget=DEFAULT_COORDINATOR_BACKSTOP_BUDGET, time_fn=time_fn)
        self._chain = EnforcementChain(
            self._rate_limiter,
            allowed_types=self._reads.allowed_types,
            battery_pct=self._reads.battery_pct,
            battery_threshold=self._reads.battery_threshold,
            mode_deny=MODE_CAPABILITY_DENY,
        )
        # The evaluator calls back through the manager's methods, so a patched
        # ``_allow_list_lags`` / ``_enforce_and_dispatch`` takes effect.
        self._evaluator = SpecEvaluator(
            self._sessions,
            self._reads,
            allow_list_lags=lambda spec: self._allow_list_lags(spec),
            dispatch=lambda *args, **kwargs: self._enforce_and_dispatch(*args, **kwargs),
            revert=lambda *args, **kwargs: self._run_revert(*args, **kwargs),
        )

        # Handler registry: flag_key -> Handler. Defaults to the no-op stub for
        # every spec. PRs C/D/E replace entries via register_handler().
        self._handlers: dict[str, Handler] = {spec.flag_key: self._noop_handler for spec in INTERVENTION_SPECS}
        # Optional per-flag handlers run when a state-shaped flag reverts to its
        # neutral value (e.g. volume_override -> default game volume); absence means
        # "do nothing on revert". A revert is routed HERE, never to the apply-handler,
        # so a flag that mutates live game state on apply MUST register one to undo
        # it (e.g. handle_music_tempo_override_revert in difficulty_handlers.py).
        self._revert_handlers: dict[str, Handler] = {}

        # Flags whose handler must run on the coordinator's main loop instead of
        # the game's loop (UC-040 BR-018): end_game force-ends the session, which
        # joins the game's thread, so running it on that thread would deadlock.
        self._main_loop_flags: set[str] = set()

        self._started = False

        # The servicer's loop, captured at start(): PROVIDER_CONFIGURATION_CHANGED
        # fires on a background thread, and evaluate_all() (loop-bound gRPC calls)
        # MUST run on this loop, or its futures fail "attached to a different loop".
        self._loop: object | None = None

    # ------------------------------------------------------------------ #
    # Handler registry (the contract PRs C/D/E follow)
    # ------------------------------------------------------------------ #
    def register_handler(self, flag_key: str, handler: Handler, *, on_game_loop: bool = True) -> None:
        """Register the real handler for a flag (PRs C/D/E).

        ``on_game_loop`` (default True): the handler runs on the owning game's
        event loop (UC-040 BR-018). Pass False only for a handler that must run on
        the coordinator's main loop (``end_game``: it joins the game thread).

        Handler contract:
        - signature: ``async def handler(ctx: InterventionContext) -> None``
        - called ONLY after the enforcement chain passes; the handler must NOT
          re-check policy.
        - read ``ctx.value`` (typed for state flags / raw string for edge
          flags), ``ctx.payload``, ``ctx.target_serial``, ``ctx.game``.
        - the manager records the metric and publishes the EventBus event around
          the handler call; the handler does not.
        - raising propagates as a blocked-with-error outcome; handlers should be
          defensive.
        - a flag the handler reads (directly or through the game) sees only the
          API-level evaluation context plus what the read passes itself, NOT the
          game's transaction context (game_mode, controller_count, sensitivity,
          experiment; ``game_kind`` is always ``real``): the effect runs in a copy
          of the main loop's contextvars (``game_loop_dispatch``, #1872). Declare
          the flag in ``handler_flag_reads.py``; ``tests/test_handler_flag_reads.py``
          fails for an undeclared read.
        """
        if flag_key not in self._handlers:
            raise KeyError(f"Unknown intervention flag key: {flag_key}")
        with self._lock:
            self._handlers[flag_key] = handler
            if on_game_loop:
                self._main_loop_flags.discard(flag_key)
            else:
                self._main_loop_flags.add(flag_key)
            logger.info(f"Registered intervention handler for '{flag_key}'")

    async def _noop_handler(self, ctx: InterventionContext) -> None:
        """PR A stub: log only, apply no game effect."""
        logger.info(
            f"[intervention stub] {ctx.spec.type_id} "
            f"(flag={ctx.spec.flag_key}, target={ctx.target_serial}, "
            f"value={ctx.value!r}, payload={ctx.payload!r}) — no-op (PR A)"
        )

    # ------------------------------------------------------------------ #
    # Ambient + session intervention handlers (PR E): the games' own audio and
    # effect paths (intervention_effects); the chain has already passed.
    # ------------------------------------------------------------------ #
    def register_ambient_handlers(self) -> None:
        """Register the PR E ambient/session handlers (one line per flag).

        Additive: PR C registers its own difficulty handlers separately. Call
        once after construction (the servicer does this on startup).
        """
        self.register_handler("audio_cue", self._handle_audio_cue)
        self.register_handler("volume_override", self._handle_volume_override)
        self.register_handler("controller_effect", self._handle_controller_effect)
        # end_game stays on the main loop: force-end joins the game's thread.
        self.register_handler("end_game", self._handle_end_game, on_game_loop=False)
        # volume_override restores the configured game volume when reverted.
        self._revert_handlers["volume_override"] = self._handle_volume_revert

    async def _handle_audio_cue(self, ctx: InterventionContext) -> None:
        return await intervention_effects.handle_audio_cue(ctx)

    async def _handle_volume_override(self, ctx: InterventionContext) -> None:
        return await intervention_effects.handle_volume_override(ctx)

    async def _handle_volume_revert(self, ctx: InterventionContext) -> None:
        return await intervention_effects.handle_volume_revert(ctx)

    async def _handle_controller_effect(self, ctx: InterventionContext) -> None:
        return await intervention_effects.handle_controller_effect(ctx)

    async def _handle_end_game(self, ctx: InterventionContext) -> None:
        # Read _end_game_fn at call time: it may be assigned after construction.
        return await intervention_effects.handle_end_game(ctx, self._end_game_fn)

    # ------------------------------------------------------------------ #
    # Lifecycle
    # ------------------------------------------------------------------ #
    def start(self) -> None:
        """Initialize flag clients, register change handler, initial evaluate."""
        if self._started:
            return

        # Capture the servicer's running loop so the (background-thread) flag
        # change handler can schedule evaluate_all() back onto it.
        try:
            self._loop = asyncio.get_running_loop()
        except RuntimeError:
            self._loop = None

        try:
            from openfeature import api

            self._reads.connect()

            api.add_handler(ProviderEvent.PROVIDER_CONFIGURATION_CHANGED, self._on_flags_changed)
            logger.info("InterventionManager: registered PROVIDER_CONFIGURATION_CHANGED handler")

            self._started = True
        except ImportError:
            logger.warning("InterventionManager: OpenFeature unavailable, interventions disabled")
            return
        except Exception as e:
            logger.error(f"InterventionManager: failed to initialize: {e}")
            return

        self._refresh_budget()

    def stop(self) -> None:
        """Remove the change handler."""
        if not self._started:
            return
        try:
            from openfeature import api

            api.remove_handler(ProviderEvent.PROVIDER_CONFIGURATION_CHANGED, self._on_flags_changed)
        except Exception as e:
            logger.debug(f"InterventionManager: handler removal failed: {e}")
        self._started = False
        self._gate.cancel_recheck()

    def forget_session(self, game_id: str) -> None:
        """Drop all per-game state for a retired session (#1299).

        Called by the servicer when it retires a session, including one whose
        game was never built. A pass suspended in that session sees no bucket
        afterwards and never fires into it.
        """
        self._sessions.forget(game_id)

    # ------------------------------------------------------------------ #
    # Session resolution + per-session change-detection buckets
    # ------------------------------------------------------------------ #
    def _live_sessions(self) -> list["SessionView"]:
        """Resolve the live sessions to evaluate (#838): ``get_sessions``, else the
        legacy ``get_game`` as one primary-only view (empty game_id, primary publisher)."""
        if self._get_sessions is not None:
            try:
                return list(self._get_sessions() or [])
            except Exception as e:
                logger.error(f"InterventionManager: get_sessions failed: {e}")
                return []
        # Legacy path: a single synthesized primary session.
        game = self._get_game() if self._get_game is not None else None
        return [SessionView(game_id=_PRIMARY_SENTINEL_GAME_ID, game=game, publish=self._publish, is_real=True)]

    def _state_bucket(self, game_id: str) -> dict[str, object]:
        return self._sessions.state_bucket(game_id)

    # ------------------------------------------------------------------ #
    # Flag change handling
    # ------------------------------------------------------------------ #
    def _on_flags_changed(self, _event_details) -> None:
        """Event handler (background thread): re-evaluate all intervention flags.

        Schedules evaluate_all() onto the servicer's loop captured at start()
        (run_coroutine_threadsafe: loop-bound gRPC clients); without one, on the
        running loop, else on a fresh loop (direct/test invocations).
        """
        loop = self._loop
        if loop is not None and loop.is_running():
            # Cross-thread-safe: bind the coroutine to the servicer's loop.
            asyncio.run_coroutine_threadsafe(self.evaluate_all(), loop)
            return

        try:
            running = asyncio.get_running_loop()
        except RuntimeError:
            running = None

        if running is not None:
            self._spawn_evaluate_all(running)
        else:
            asyncio.run(self.evaluate_all())

    def _spawn_evaluate_all(self, loop: asyncio.AbstractEventLoop) -> None:
        """Run evaluate_all() as a task that stays referenced until it is done (#2357, RUF006)."""
        task = loop.create_task(self.evaluate_all())
        self._background_tasks.add(task)
        task.add_done_callback(self._background_tasks.discard)

    async def evaluate_all(self) -> None:
        """Single-flight wrapper around :meth:`_evaluate_all_once` (re-review N1).

        A call while a pass runs only marks the pass dirty and returns; the
        running pass then loops once more, so N overlapping change events cost
        at most two passes and two passes never interleave.
        """
        with self._eval_state_lock:
            if self._eval_running:
                self._eval_dirty = True
                return
            self._eval_running = True
            self._eval_dirty = False
        try:
            while True:
                await self._evaluate_all_once()
                with self._eval_state_lock:
                    if not self._eval_dirty:
                        self._eval_running = False
                        return
                    self._eval_dirty = False
        except BaseException:
            with self._eval_state_lock:
                self._eval_running = False
            raise

    async def _evaluate_all_once(self) -> None:
        """Re-evaluate every intervention flag for every live session (#838).

        Refreshes the backstop budget first, then walks the registry in order per
        session, with that session's ``{gameId, targetingKey}`` context. A flag with
        no ``gameId`` targeting rule (the baseline default; agent writes are
        game-scoped, FR-136) resolves to the same value for every live session.
        """
        self._refresh_budget()
        self._gate.begin_pass()
        sessions = self._live_sessions()
        if not sessions:
            # No game left to defer for: close the episode so the next game's
            # brief deferral is a new episode with a fresh age, not a stale WARNING.
            self._gate.end()
        self._sessions.prune({s.game_id for s in sessions})
        for session in sessions:
            # Create the session's buckets once, at the start of its iteration.
            # Inside the pass they are only read with .get(): if the servicer
            # retires the session while a handler is awaited (forget_session),
            # the remaining specs see no bucket and skip instead of firing into
            # a retired game.
            self._sessions.begin(session.game_id)
            for spec in INTERVENTION_SPECS:
                try:
                    await self._evaluate_one(spec, session)
                except Exception as e:  # one bad flag must not stop the rest
                    logger.error(
                        f"InterventionManager: error evaluating {spec.flag_key} "
                        f"for game {session.game_id or '<primary>'}: {e}"
                    )

    async def _evaluate_one(self, spec: InterventionSpec, session: "SessionView") -> None:
        await self._evaluator.evaluate(spec, session)

    async def _evaluate_event_list(self, spec: InterventionSpec, session: "SessionView") -> None:
        await self._evaluator.evaluate_event_list(spec, session)

    def _allow_list_lags(self, spec: InterventionSpec) -> bool:
        """The cross-set consistency gate (UC-053 A9): True defers the values just read."""
        return self._gate.lags(spec)

    def _schedule_deferral_recheck(self) -> None:
        self._gate.schedule_recheck()

    # ------------------------------------------------------------------ #
    # Enforcement chain -> handler -> outcome -> cue
    # ------------------------------------------------------------------ #
    async def _enforce_and_dispatch(
        self,
        spec: InterventionSpec,
        value: object,
        payload: str,
        target: str | None,
        session: "SessionView",
        *,
        intervention_id: str | None = None,
        intervention_ids: "dict[str, str] | None" = None,
    ) -> None:
        """Run the enforcement chain, then the handler, and record the outcome.

        ``intervention_id`` (one target, an event) or ``intervention_ids`` (a level's
        changed players) are the ids served with the value at the read that found the
        change. A player lever's handler runs in an intervention scope carrying them,
        so the game records the ``intervention`` moment where the effect lands
        (UC-101 BR-004, BR-005). The ids are informational: the chain, the outcome
        record and the ``agent_intervention`` event never see them (BR-008, NFR-061).
        """
        objective = self._dominant_objective()
        game = session.game

        # The enforcement chain (UC-040 BR-004): allow-list, mode matrix, battery guard, backstop.
        block_reason, reservation = self._chain.check(spec, target, game)
        if block_reason is not None:
            await record_outcome(spec, target, objective, session, blocked=True, block_reason=block_reason)
            return

        ctx = InterventionContext(
            spec=spec,
            value=value,
            payload=payload,
            target_serial=target,
            game=game,
            objective=objective,
            game_id=session.game_id,
        )
        handler = self._handlers.get(spec.flag_key, self._noop_handler)
        try:
            if spec.flag_key in self._main_loop_flags:
                await handler(ctx)
            else:
                scope = _intervention_scope(spec, value, target, intervention_id, intervention_ids)
                await self._run_on_game_loop(session, spec, run_with_intervention_scope, scope, handler, ctx)
        except GameLoopUnavailableError as e:
            # UC-040 BR-018, already logged + counted. game_loop_late: the effect
            # started and may still apply, so it is not reported as blocked;
            # otherwise it was never applied (fail closed). No cue either way.
            await record_outcome(spec, target, objective, session, blocked=not e.applied_unknown, block_reason=e.reason)
            return
        except InterventionNotApplied as e:
            # UC-048 A4: nothing applied, so the backstop reservation is refunded.
            self._rate_limiter.refund(reservation)
            logger.info(f"InterventionManager: {spec.flag_key} for {target or '<game>'} not applied: {e.reason}")
            await record_outcome(spec, target, objective, session, blocked=True, block_reason=e.reason)
            return
        except Exception as e:
            logger.error(f"InterventionManager: handler for {spec.flag_key} raised: {e}")
            await record_outcome(spec, target, objective, session, blocked=True, block_reason="handler_error")
            return

        await record_outcome(spec, target, objective, session, blocked=False, block_reason="")

        # On-device visibility cue: surface HARD-class agent actions at the
        # controller so an operator can tell agent activity from a malfunction.
        # Gated behind a user.json mute toggle so it can be silenced for demos.
        #
        # Not for main-loop flags: end_game has just ended the game, so its loop
        # is gone and there is no controller left to cue. The mute toggle is read
        # here, on the main loop, so a muted cue is never dispatched.
        if spec.weight >= WEIGHT_HARD and spec.flag_key not in self._main_loop_flags and self._cue_enabled():
            # Best-effort cue: a game loop that just ended is not a failure.
            with contextlib.suppress(GameLoopUnavailableError):
                await self._run_on_game_loop(session, spec, self._emit_agent_cue, spec, target, game, quiet=True)

    async def _run_revert(
        self,
        spec: InterventionSpec,
        value: object,
        session: "SessionView",
        *,
        intervention_ids: "dict[str, str] | None" = None,
    ) -> None:
        """Run ``spec``'s revert handler for ``session``, if one is registered.

        Reverts restore a default; they bypass the permission chain and the
        backstop, and a failing revert handler is logged, never raised. Like an
        apply, the revert runs on the owning game's loop through the session's
        dispatcher, so it queues behind any effect already queued there
        (UC-040 BR-018, #1250). A player lever's revert (a level back to 1.0) runs
        in an intervention scope, so it records the intervention moment (UC-101
        BR-001), with the id served with the neutral value when there is one (A3).
        """
        revert = self._revert_handlers.get(spec.flag_key)
        if revert is None:
            return
        ctx = InterventionContext(
            spec=spec,
            value=value,
            payload="",
            target_serial=None,
            game=session.game,
            objective=self._dominant_objective(),
            game_id=session.game_id,
        )
        # Only a revert of named players (a level's changed serials) records moments.
        scope = _intervention_scope(spec, value, None, None, intervention_ids) if isinstance(value, dict) else None
        try:
            await self._run_on_game_loop(session, spec, run_with_intervention_scope, scope, revert, ctx)
        except GameLoopUnavailableError:
            pass  # already logged + counted once by _run_on_game_loop
        except Exception as e:
            logger.error(f"InterventionManager: revert handler for {spec.flag_key} raised: {e}")

    # ------------------------------------------------------------------ #
    # Game-loop dispatch (UC-040 BR-018)
    # ------------------------------------------------------------------ #
    def _dispatcher_for(self, session: "SessionView") -> GameLoopDispatcher:
        """Return the session's dispatcher, bound to its current game loop."""
        return self._sessions.dispatcher_for(session, self._dispatch_timeout_s)

    async def _run_on_game_loop(
        self, session: "SessionView", spec: InterventionSpec, fn, *args, quiet: bool = False
    ) -> None:
        """Run one side effect ``fn(*args)`` on the session's game loop.

        Raises ``GameLoopUnavailableError`` when the effect did not complete on
        the game's loop: not applied (closed / stopping loop, or not started in
        time) or ``game_loop_late`` (started, outcome unknown). It is logged and
        counted once here; ``quiet`` (the best-effort agent cue) logs at debug
        and does not count. Errors raised by ``fn`` itself propagate unchanged.
        """
        try:
            await self._dispatcher_for(session).run(run_in_game_flag_context, session.game, fn, *args)
        except GameLoopUnavailableError as e:
            if quiet:
                game_label = session.game_id or "<primary>"
                logger.debug(f"InterventionManager: {spec.flag_key} cue for game {game_label} skipped ({e})")
                raise
            logger.warning(
                f"InterventionManager: {spec.flag_key} effect for game {session.game_id or '<primary>'} "
                f"not applied ({e})"
            )
            count_dispatch_failure(spec, e.reason)
            raise

    async def _emit_agent_cue(
        self, spec: InterventionSpec, target: str | None, game: "InterventionTarget | None"
    ) -> None:
        # Pass the bound gate, not a bool: the cue re-reads it on the game loop.
        return await intervention_effects.emit_agent_cue(spec, target, game, cue_enabled=self._cue_enabled)

    # ------------------------------------------------------------------ #
    # Policy values (read by the flag reader) and per-player targeting
    # ------------------------------------------------------------------ #
    def _cue_enabled(self) -> bool:
        return self._reads.cue_enabled()

    def _allowed_types(self) -> set[str]:
        return self._reads.allowed_types()

    def _battery_threshold(self) -> float:
        return self._reads.battery_threshold()

    def _battery_pct(self, serial: str) -> float | None:
        return self._reads.battery_pct(serial)

    def _dominant_objective(self) -> str:
        return self._reads.dominant_objective()

    def _refresh_budget(self) -> None:
        """Refresh the GLOBAL BACKSTOP budget (#919) from ``policy.coordinator_backstop_per_minute``."""
        self._rate_limiter.set_budget(self._reads.backstop_budget())

    def resolve_player_targets(
        self,
        flag_key: str,
        default,
        game: "InterventionTarget | None",
        *,
        value_kind: str = "float",
        battery_gate: bool = True,
        game_id: str = "",
    ) -> dict:
        """Evaluate a per-player targeted flag for every active player serial (``{serial: value}``).

        Reusable by the per-player handlers; see :meth:`InterventionFlagReader.resolve_player_targets`.
        """
        return self._reads.resolve_player_targets(
            flag_key, default, game, value_kind=value_kind, battery_gate=battery_gate, game_id=game_id
        )
