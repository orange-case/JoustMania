"""The intervention moment's tag: which intervention a lever effect belongs to (#2665, UC-101, FR-220, NFR-061).

The intervention manager reads the intervention id the flag service serves with a value
(``decision_id_of``, BR-003) and opens an ``InterventionScope`` around the handler call
(``run_with_intervention_scope``). The game method that records the lever's own effect
(``shield_granted``, ``toughness_boost_armed``, ``player_revived``, ...) takes the tag for
its player from the scope (``take_intervention``) and records the ``intervention`` moment
(``BaseGameMode.record_intervention``, BR-004, BR-005). The tag travels in a
``ContextVar``, so no game method's signature changes and a mode's override of a lever
method keeps working.

Rules:
- one tag per player per scope: the first recording point of that player takes it, a
  second one gets none (at most one moment per player per applied intervention);
- only the players the dispatch names: the target serial, the changed serials of a level,
  or any player for a group shield (whose serials the game resolves, A4);
- the scope closes when the handler returns or raises, so a task the handler spawned
  never records later;
- nothing here raises into an intervention: a missing, empty or unreadable id only leaves
  the moment without one (NFR-061).
"""

from __future__ import annotations

import contextlib
import threading
from collections.abc import Awaitable, Callable, Iterable, Mapping
from contextvars import ContextVar
from dataclasses import dataclass
from typing import Any

INTERVENTION_MOMENT = "intervention"
INTERVENTION_TYPE_ATTR = "intervention.type"
INTERVENTION_ID_ATTR = "intervention.id"

# Flag metadata key prefix of a value's intervention id: ``decisionId.<variant>``
# (flag-service ``MetaDecisionIDPrefix``, services/flag-service/state/render.go; flags.md §5).
DECISION_ID_METADATA_PREFIX = "decisionId."
# An event list entry's intervention id (flags.md §5, services.md §7.2).
EVENT_DECISION_ID_KEY = "decision_id"

# The player levers that record the moment (UC-101 BR-001): the spec's ``type_id``.
PLAYER_LEVER_TYPES = frozenset(
    {
        "grant_shield",
        "grant_group_shield",
        "toughness_boost",
        "adjust_player_sensitivity",
        "set_player_handicap",
        "revive_player",
        "soft_penalty",
        "eliminate_player",
    }
)


@dataclass(frozen=True)
class InterventionTag:
    """The intervention one lever effect belongs to: its type and its id (``None`` without one, A3)."""

    type: str
    id: str | None


def decision_id_of(details: Any) -> str | None:
    """The intervention id served with a resolved value, or None (UC-101 BR-003, BR-004, NFR-061).

    The id of the value that resolved is the flag metadata ``decisionId.<variant>`` of the
    resolved ``details.variant``; another variant's id never counts. Anything missing,
    empty, not a string or raising reads as no id.
    """
    try:
        variant = details.variant
        if not isinstance(variant, str) or not variant:
            return None
        metadata = details.flag_metadata
        if not metadata:
            return None
        value = metadata.get(DECISION_ID_METADATA_PREFIX + variant)
    except Exception:  # noqa: BLE001 - an unreadable id is no id (NFR-061)
        return None
    return value if isinstance(value, str) and value else None


def event_decision_id(entry: Any) -> str | None:
    """The intervention id of one event list entry (``decision_id``), or None (NFR-061)."""
    try:
        value = entry.get(EVENT_DECISION_ID_KEY)
    except Exception:  # noqa: BLE001 - an unreadable id is no id (NFR-061)
        return None
    return value if isinstance(value, str) and value else None


class InterventionScope:
    """The tags one handler call may record, one per player (see the module docstring).

    ``serials`` are the players the dispatch names (``None``: any player, a group shield);
    ``ids`` the per-player intervention ids (a level), ``default_id`` the id of every other
    named player (a grant, an event).
    """

    def __init__(
        self,
        type_id: str,
        *,
        serials: Iterable[str] | None,
        ids: Mapping[str, str] | None = None,
        default_id: str | None = None,
    ) -> None:
        self.type_id = type_id
        self._serials = frozenset(serials) if serials is not None else None
        self._ids = dict(ids or {})
        self._default_id = default_id
        self._taken: set[str] = set()
        self._closed = False
        self._lock = threading.Lock()

    def take(self, serial: str, types: Iterable[str] | None = None) -> InterventionTag | None:
        """The tag of ``serial``, once; None when not named, already taken, closed or of another type."""
        if types is not None and self.type_id not in types:
            return None
        with self._lock:
            if self._closed or serial in self._taken:
                return None
            if self._serials is not None and serial not in self._serials:
                return None
            self._taken.add(serial)
        return InterventionTag(self.type_id, self._ids.get(serial) or self._default_id)

    def close(self) -> None:
        with self._lock:
            self._closed = True


_current_scope: ContextVar[InterventionScope | None] = ContextVar("joustmania_intervention_scope", default=None)


def take_intervention(serial: str, types: Iterable[str] | None = None) -> InterventionTag | None:
    """The tag of ``serial`` in the current scope, once (None outside a scope); never raises."""
    try:
        scope = _current_scope.get()
        return scope.take(serial, types) if scope is not None else None
    except Exception:  # noqa: BLE001 - a moment never reaches into the intervention (NFR-061)
        return None


async def run_with_intervention_scope(
    scope: InterventionScope | None, fn: Callable[..., Awaitable[Any]], *args: Any
) -> Any:
    """Run ``await fn(*args)`` with ``scope`` current; the scope closes when ``fn`` returns or raises."""
    if scope is None:
        return await fn(*args)
    token = _current_scope.set(scope)
    try:
        return await fn(*args)
    finally:
        scope.close()
        with contextlib.suppress(ValueError):  # reset in another context: the var dies with it
            _current_scope.reset(token)
