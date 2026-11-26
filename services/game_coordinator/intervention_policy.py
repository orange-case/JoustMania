"""Intervention enforcement chain and global backstop rate limiter.

Split out of ``InterventionManager``: the policy checks every intervention passes
before its handler runs (UC-040 BR-004). The flag reads they need (allow-list,
battery threshold, backstop budget) stay with the manager's flag reader in
``interventions.py`` and are passed in as callables, so this module reads no flag.
See the "Defense-in-depth budget contract" block in ``interventions.py`` for the
role of the backstop limiter.
"""

import threading
import time
from collections import deque
from collections.abc import Callable, Mapping
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from services.game_coordinator.intervention_target import InterventionTarget
    from services.game_coordinator.interventions import InterventionSpec

# Sliding rate-limit window in seconds (§5: "max_interventions_per_minute").
RATE_LIMIT_WINDOW_SECONDS = 60.0

# Reason codes for blocked interventions (event payload ``block_reason``).
BLOCK_NOT_ALLOWED = "not_allowed"
BLOCK_RATE_LIMITED = "rate_limited"
BLOCK_LOW_BATTERY = "low_battery"
BLOCK_MODE_UNSUPPORTED = "mode_unsupported"
BLOCK_NO_GAME = "no_game"
# The handler applied nothing on purpose (InterventionNotApplied): a grant_shield to
# a player who already holds a shield charge (UC-048 A4, BR-005).
BLOCK_ALREADY_CHARGED = "already_charged"
# A grant_shield refused by the shield limits per player per game (UC-048 A7,
# UC-048 BR-007): raised by games/base.py grant_shield as InterventionNotApplied.
BLOCK_SHIELD_CAP_REACHED = "shield_cap_reached"
BLOCK_SHIELD_COOLDOWN = "shield_cooldown"
# A grant_shield in a hold game that could add nothing (UC-048 A9).
BLOCK_SHIELD_POOL_FULL = "shield_pool_full"
BLOCK_SHIELD_BUDGET_EXHAUSTED = "shield_budget_exhausted"
# A grant_shield that lands once the game is ending (outcome latched, force-ended,
# ENDING / ENDED): no layer, no cue over the winner effect (UC-048 A12 step 3).
BLOCK_GAME_ENDING = "game_ending"
# A revive_player the mode refused (decided game, unknown or already-alive player; #1948).
BLOCK_REVIVE_REFUSED = "revive_refused"
# An eliminate_player that killed nobody: unknown or already-dead player, a kill of
# the same player in flight, or the mode declined the kill (_accepts_kill; #1383).
BLOCK_ELIMINATE_REFUSED = "eliminate_refused"


class _RateLimiter:
    """Weighted sliding-window rate limiter — the GLOBAL BACKSTOP (#919).

    This is the process-global defense-in-depth backstop described in the
    "Defense-in-depth budget contract" block in ``interventions.py``. It is
    NOT the per-game governor — the Go agent owns that
    (``policy.max_interventions_per_minute``, per-game Loop). This limiter is one
    generous global cap shared across all concurrent games, instantiated with
    ``DEFAULT_COORDINATOR_BACKSTOP_BUDGET`` and tuned via the separate
    ``policy.coordinator_backstop_per_minute`` flag. It only trips when a runaway
    agent floods flag writes well beyond all sane per-game traffic combined.

    Tracks (timestamp, cost) of applied interventions over the last
    ``RATE_LIMIT_WINDOW_SECONDS``. ``check(cost)`` returns True (and reserves the
    cost) when ``current_weight + cost <= budget``; otherwise returns False and
    reserves nothing.
    """

    def __init__(self, budget: float, time_fn: Callable[[], float] = time.monotonic):
        self._budget = budget
        self._time_fn = time_fn
        # (timestamp, cost, reservation token); tokens are unique per reservation.
        self._events: deque[tuple[float, float, int]] = deque()
        self._next_token = 0
        self._lock = threading.Lock()

    def set_budget(self, budget: float) -> None:
        with self._lock:
            self._budget = budget

    def _evict(self, now: float) -> None:
        cutoff = now - RATE_LIMIT_WINDOW_SECONDS
        while self._events and self._events[0][0] <= cutoff:
            self._events.popleft()

    def current_weight(self) -> float:
        with self._lock:
            self._evict(self._time_fn())
            return sum(cost for _, cost, _ in self._events)

    def reserve(self, cost: float) -> int | None:
        """Reserve ``cost`` if budget allows. Returns the reservation token, or None when over budget."""
        with self._lock:
            now = self._time_fn()
            self._evict(now)
            used = sum(c for _, c, _ in self._events)
            if used + cost > self._budget:
                return None
            self._next_token += 1
            self._events.append((now, cost, self._next_token))
            return self._next_token

    def check(self, cost: float) -> bool:
        """Reserve ``cost`` if budget allows. Returns True if reserved."""
        return self.reserve(cost) is not None

    def refund(self, token: int | None) -> None:
        """Give back exactly the reservation ``token`` (an intervention that applied nothing, UC-048 A4).

        Unknown, already-refunded or evicted tokens are ignored, so a refund never frees another write's slot.
        """
        if token is None:
            return
        with self._lock:
            for i, (_, _, event_token) in enumerate(self._events):
                if event_token == token:
                    del self._events[i]
                    return


def _game_mode_name(game: "InterventionTarget | None") -> str:
    """Best-effort mode name from a live game via get_game_name().

    Fails open: no game or a raising getter gives ``""`` (no mode denial).
    """
    if game is None:
        return ""
    try:
        return game.get_game_name()
    except Exception:
        return ""


class EnforcementChain:
    """The coordinator's enforcement chain (UC-040 BR-004), run before any handler.

    The policy inputs are callables read at check time, so a flag change between
    two checks takes effect on the next one: ``allowed_types`` (the allow-list,
    empty while the agent is disabled), ``battery_pct`` (per serial, None =
    unknown) and ``battery_threshold``. ``mode_deny`` is the per-mode capability
    matrix (``MODE_CAPABILITY_DENY``).
    """

    def __init__(
        self,
        rate_limiter: _RateLimiter,
        *,
        allowed_types: Callable[[], set[str]],
        battery_pct: Callable[[str], float | None],
        battery_threshold: Callable[[], float],
        mode_deny: Mapping[str, frozenset[str]],
    ):
        self._rate_limiter = rate_limiter
        self._allowed_types = allowed_types
        self._battery_pct = battery_pct
        self._battery_threshold = battery_threshold
        self._mode_deny = mode_deny

    def check(
        self, spec: "InterventionSpec", target: str | None, game: "InterventionTarget | None"
    ) -> tuple[str | None, int | None]:
        """Run the enforcement chain. Returns ``(block_reason, reservation)``: a reason when blocked, else
        None plus the backstop reservation token to refund if the handler applies nothing (#1868).

        Order: allowed-membership -> mode matrix -> battery guard -> rate limit.
        The rate-limit reservation happens last among the cheap checks so a
        blocked intervention never consumes budget.

        UC-040 BR-004: the coordinator-side allow-list check (the agent has its own
        in the permission chain). UC-040 BR-004: the coordinator's half of the
        double enforcement, plus the global backstop.
        """
        # (a) allowed-membership
        if spec.type_id not in self._allowed_types():
            return BLOCK_NOT_ALLOWED, None

        # (d) mode-capability matrix (cheap, no budget side effect) — checked
        # before the rate limiter so unsupported types don't consume budget.
        if game is not None:
            mode_name = _game_mode_name(game)
            if spec.type_id in self._mode_deny.get(mode_name, frozenset()):
                return BLOCK_MODE_UNSUPPORTED, None

        # (c) battery guard (player-targeted only)
        if spec.player_targeted and target:
            pct = self._battery_pct(target)
            if pct is not None and pct < self._battery_threshold():
                return BLOCK_LOW_BATTERY, None

        # (b) weighted rate limit — the GENEROUS GLOBAL BACKSTOP, NOT the
        # per-game governor (that is the agent's). Reserves budget; do last so
        # the reservation only happens for otherwise-allowed interventions. A
        # block here means a process-wide flood across ALL games, not a single
        # game pacing itself out.
        reservation = self._rate_limiter.reserve(spec.weight)
        if reservation is None:
            return BLOCK_RATE_LIMITED, None

        return None, reservation
