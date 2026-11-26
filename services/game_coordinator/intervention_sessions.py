"""Live sessions and their per-session intervention state (#838).

Split out of ``InterventionManager``: the ``SessionView`` the servicer hands the
manager per live game, and the per-game change-detection buckets and game-loop
dispatchers the manager keeps for each of them.
"""

import asyncio
import threading
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import TYPE_CHECKING

from services.game_coordinator.game_loop_dispatch import GameLoopDispatcher

if TYPE_CHECKING:
    from services.game_coordinator.intervention_target import InterventionTarget

# Sentinel game_id for the synthesized primary-only session when the manager is
# wired with the legacy ``get_game`` callback (no session registry). An empty
# game_id adds NO ``gameId`` attribute to the EvaluationContext, so un-targeted
# flags resolve exactly as before (today's primary-only semantics).
_PRIMARY_SENTINEL_GAME_ID = ""


@dataclass(frozen=True)
class SessionView:
    """A live session the manager evaluates interventions against (#838).

    The servicer supplies one of these per live game (primary + every shadow)
    via ``get_sessions``. Each carries the game instance, its ``game_id`` (used
    as the ``gameId`` EvaluationContext attribute so flagd targeting rules can
    scope an intervention to one game), and the publish callable for THAT
    session's EventBus (so effects/blocked events land on the right stream).

    Backward compatibility (#838): a manager wired with the legacy ``get_game``
    callback synthesizes a single primary view with ``game_id=""`` (no ``gameId``
    in context) and the primary publisher — so un-targeted interventions behave
    exactly as today and only touch the primary game.

    Attributes:
        game_id: The session's game id. ``""`` means the synthesized primary
            session (legacy path); no ``gameId`` is added to the context.
        game: The live BaseGameMode instance (or ``None``).
        publish: Async ``publish(event_type, data)`` for this session's bus.
        is_real: Whether this is the real (primary) game. Defaults to False so a
            caller that does not say is treated as a shadow (fail closed).
        loop: The asyncio loop the session's game runs on (its own thread). The
            game's gRPC clients and gameplay stream belong to it, so handler side
            effects run there (UC-040 BR-018, #1250). ``None`` (legacy / tests)
            runs effects inline on the caller's loop.
    """

    game_id: str
    game: "InterventionTarget | None"
    publish: "Callable[[str, dict], Awaitable[None]]"
    # True only for the real (primary) game; unknown kind fails closed (shadow).
    is_real: bool = False
    loop: asyncio.AbstractEventLoop | None = None


class SessionState:
    """Per-game intervention state, keyed by ``game_id``, under one lock.

    - ``last_state_value``: ``game_id -> flag_key -> value``, the last applied
      state value (per-serial maps for levels and grants) for change detection.
      The legacy single-primary path uses the ``_PRIMARY_SENTINEL_GAME_ID`` ("")
      bucket.
    - ``last_event``: ``game_id -> flag_key -> (epoch, seq)``, the last applied
      event of each one-shot flag's per-game event list (FR-137).
    - ``dispatchers``: ``game_id -> GameLoopDispatcher``, running handler side
      effects on the owning game's loop in decision order (UC-040 BR-018).

    The buckets are created once per session at the start of its pass
    (:meth:`begin`); inside the pass they are only read with the ``existing_*``
    accessors, so a session retired while a handler is awaited (:meth:`forget`)
    sees no bucket and is never fired into.
    """

    def __init__(self) -> None:
        self.lock = threading.RLock()
        self.last_state_value: dict[str, dict[str, object]] = {}
        self.last_event: dict[str, dict[str, tuple[str, int]]] = {}
        self.dispatchers: dict[str, GameLoopDispatcher] = {}

    def begin(self, game_id: str) -> None:
        """Create the session's buckets (once, at the start of its pass)."""
        with self.lock:
            self.state_bucket(game_id)
            self.last_event.setdefault(game_id, {})

    def state_bucket(self, game_id: str) -> dict[str, object]:
        return self.last_state_value.setdefault(game_id, {})

    def existing_state_bucket(self, game_id: str) -> dict[str, object] | None:
        """Mid-pass accessor: never (re)creates a bucket. ``None`` means the session
        was forgotten (retired) while evaluate_all was suspended in it."""
        return self.last_state_value.get(game_id)

    def existing_event_bucket(self, game_id: str) -> dict[str, tuple[str, int]] | None:
        """Mid-pass accessor for the session's last applied events; ``None`` once retired."""
        return self.last_event.get(game_id)

    def forget(self, game_id: str) -> None:
        """Drop all per-game state for a retired session (#1299)."""
        with self.lock:
            self.last_state_value.pop(game_id, None)
            self.dispatchers.pop(game_id, None)
            self.last_event.pop(game_id, None)

    def prune(self, live_ids: set[str]) -> None:
        """Drop state for sessions that are no longer live.

        Prevents unbounded growth as shadow games come and go, and ensures a new
        game reusing a (vanishingly unlikely) recycled id starts from a clean
        baseline. The sentinel primary bucket is always retained (dispatchers are not:
        they are bound to a game's loop).
        """
        with self.lock:
            keep = live_ids | {_PRIMARY_SENTINEL_GAME_ID}
            for stale in [gid for gid in self.last_state_value if gid not in keep]:
                self.last_state_value.pop(stale, None)
            for stale in [gid for gid in self.dispatchers if gid not in live_ids]:
                self.dispatchers.pop(stale, None)
            for stale in [gid for gid in self.last_event if gid not in keep]:
                self.last_event.pop(stale, None)

    def dispatcher_for(self, session: SessionView, timeout_s: float) -> GameLoopDispatcher:
        """Return the session's dispatcher, bound to its current game loop.

        A session id whose loop changed (a new game reusing the id, or the legacy
        sentinel across games) gets a fresh dispatcher for the new loop.
        """
        loop = session.loop
        with self.lock:
            dispatcher = self.dispatchers.get(session.game_id)
            if dispatcher is None or dispatcher.loop is not loop:
                dispatcher = GameLoopDispatcher(loop, timeout_s=timeout_s)
                self.dispatchers[session.game_id] = dispatcher
            return dispatcher
