"""Intervention effect emission helpers.

Split out of ``InterventionManager``: how a handler call runs on the owning
game's loop (its flag context and its UC-101 intervention scope), and how each
outcome is recorded (``game_interventions_total`` + the ``agent_intervention``
event on the session's bus). The manager keeps the order of these steps.
"""

import logging
from collections.abc import Awaitable, Callable
from typing import TYPE_CHECKING, Any

from openfeature import api as openfeature_api
from openfeature.evaluation_context import EvaluationContext

from services.game_coordinator.games.intervention_moment import PLAYER_LEVER_TYPES, InterventionScope

if TYPE_CHECKING:
    from services.game_coordinator.intervention_sessions import SessionView
    from services.game_coordinator.intervention_target import InterventionTarget
    from services.game_coordinator.interventions import InterventionSpec

# The manager's logger: log routing is unchanged by the split.
logger = logging.getLogger("services.game_coordinator.interventions")


def _intervention_scope(
    spec: "InterventionSpec",
    value: object,
    target: str | None,
    intervention_id: str | None,
    intervention_ids: "dict[str, str] | None",
) -> InterventionScope | None:
    """The intervention scope a handler call runs in, or None for a lever that records no moment.

    Only the player levers of UC-101 BR-001 get one. The players it names: the target
    serial; a level's changed serials (the ``{serial: level}`` value); any player when
    neither is known (a group shield, whose serials the game resolves, A4).
    """
    if spec.type_id not in PLAYER_LEVER_TYPES:
        return None
    if target:
        serials: frozenset[str] | None = frozenset({target})
    elif isinstance(value, dict):
        serials = frozenset(str(serial) for serial in value)
    else:
        serials = None
    return InterventionScope(spec.type_id, serials=serials, ids=intervention_ids, default_id=intervention_id)


def _ids_for(ids: "dict[str, str]", serials) -> dict[str, str]:
    """The intervention ids of ``serials`` (those with one)."""
    return {serial: ids[serial] for serial in serials if ids.get(serial)}


async def run_in_game_flag_context(
    game: "InterventionTarget | None", fn: Callable[..., Awaitable[Any]], *args: Any
) -> Any:
    """Run ``await fn(*args)`` with ``game``'s flag transaction context (#1970, UC-040 BR-018).

    Called on the game's loop by ``GameLoopDispatcher.run``, so a dispatched
    handler's flag reads resolve as the game's own reads do (an experiment arm's
    value included). The context is read from the game here, on the game loop,
    and the previous transaction context is restored afterwards, so on the
    inline path it never leaks into the caller's task. No game, or a context that
    is not an ``EvaluationContext``, runs ``fn`` unchanged.
    """
    eval_context = game.flag_eval_context if game is not None else None
    if not isinstance(eval_context, EvaluationContext):
        return await fn(*args)
    previous = openfeature_api.get_transaction_context()
    openfeature_api.set_transaction_context(eval_context)
    try:
        return await fn(*args)
    finally:
        openfeature_api.set_transaction_context(previous)


async def record_outcome(
    spec: "InterventionSpec",
    target: str | None,
    objective: str,
    session: "SessionView",
    *,
    blocked: bool,
    block_reason: str,
) -> None:
    """Count one intervention outcome and publish it on the session's bus.

    Every outcome (applied or blocked) increments
    ``game_interventions_total{type, objective, blocked, block_reason}`` and
    publishes ``GameEvent.AGENT_INTERVENTION``. A failing metric never stops the event.
    """
    try:
        from services.game_coordinator import metrics

        metrics.interventions_total.labels(
            type=spec.type_id,
            objective=objective,
            blocked=str(blocked).lower(),
            block_reason=block_reason,
        ).inc()
    except Exception as e:
        logger.debug(f"InterventionManager: metric record failed: {e}")

    from lib.types import GameEvent

    # Publish on THIS session's bus so the effect/blocked event lands on the
    # stream the owning game's subscribers (and the agent runner) are reading.
    # The session's game_id is stamped onto the event by the bus.
    await session.publish(
        GameEvent.AGENT_INTERVENTION,
        {
            "type": spec.type_id,
            "target": target or "",
            "blocked": str(blocked).lower(),
            "block_reason": block_reason,
        },
    )


def count_dispatch_failure(spec: "InterventionSpec", reason: str) -> None:
    """Count an effect that did not complete on its game's loop (UC-040 BR-018)."""
    try:
        from services.game_coordinator import metrics

        metrics.intervention_dispatch_failures_total.labels(type=spec.type_id, reason=reason).inc()
    except Exception as metric_error:
        logger.debug(f"InterventionManager: dispatch failure metric failed: {metric_error}")
