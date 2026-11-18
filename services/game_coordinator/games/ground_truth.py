"""Ground truth of a challenge game (UC-100 step 6, BR-006, FR-219, #2658; UC-101 step 7, FR-222, #2665).

The game records, per player serial, the time of each warning and of each death, and
each applied agent intervention on one player (its type, id, player and apply time).
At a normal game end the base ``run()`` puts one ``challenge.ground_truth`` span
event per player on the game root span, then one ``challenge.intervention_ground_truth``
per applied intervention, with the time of that player's first death at or after the
apply time (telemetry.md §3.2). A force-ended game or one whose ``run()`` raised
records none (UC-100 A5, UC-101 A7).

The times come from the clock the moment records use: wall-clock epoch
nanoseconds, ``time.time_ns()``. Each ``record_*`` returns its time, and the game
passes it to ``record_moment`` as the record's own timestamp, so a ground-truth time
equals its record's timestamp in every shape. ``game_clock`` is not wall time and is
not used.
Kept per game and serial, never on the ``Player`` object, so a recreated player
record keeps its times.
"""

from collections.abc import Mapping
from dataclasses import dataclass
from time import time_ns

from opentelemetry import trace

GROUND_TRUTH_EVENT = "challenge.ground_truth"
WARNING_TIMES_ATTR = "challenge.warning_times_ns"
DEATH_TIMES_ATTR = "challenge.death_times_ns"

INTERVENTION_GROUND_TRUTH_EVENT = "challenge.intervention_ground_truth"
APPLIED_AT_ATTR = "challenge.applied_at_ns"
FIRST_DEATH_AT_ATTR = "challenge.first_death_at_ns"


@dataclass
class AppliedIntervention:
    """One applied intervention on one player (UC-101 BR-006).

    ``moment_id`` is the ``moment.id`` of its intervention moment, set once the moment
    was sent; empty when none was (no player span, a failed record), so the presenter
    never asks about a moment that was not sent (BR-009).
    """

    serial: str
    type: str
    intervention_id: str | None
    applied_at_ns: int
    moment_id: str = ""


class GroundTruth:
    """Warning and death times per player serial, and the applied interventions, for one game."""

    def __init__(self) -> None:
        self._warnings: dict[str, list[int]] = {}
        self._deaths: dict[str, list[int]] = {}
        self._interventions: list[AppliedIntervention] = []

    def record_warning(self, serial: str) -> int:
        """A warning of ``serial`` happens now; returns its time, the ``death_warning`` record's timestamp."""
        stamp = time_ns()
        self._warnings.setdefault(serial, []).append(stamp)
        return stamp

    def record_death(self, serial: str) -> int:
        """A death of ``serial`` happens now; returns its time, the ``player_death`` record's timestamp."""
        stamp = time_ns()
        self._deaths.setdefault(serial, []).append(stamp)
        return stamp

    def record_intervention(
        self, serial: str, intervention_type: str, intervention_id: str | None, at_ns: int | None = None
    ) -> AppliedIntervention:
        """An intervention lands on ``serial`` now (or at ``at_ns``: an elimination's death time, BR-006).

        Returns its entry; ``applied_at_ns`` is the ``intervention`` record's timestamp.
        """
        entry = AppliedIntervention(
            serial=serial,
            type=intervention_type,
            intervention_id=intervention_id,
            applied_at_ns=time_ns() if at_ns is None else at_ns,
        )
        self._interventions.append(entry)
        return entry

    def intervention_attributes(self, entry: AppliedIntervention, name: str = "") -> dict:
        """The ``challenge.intervention_ground_truth`` attributes of one applied intervention.

        ``challenge.first_death_at_ns`` is the player's first death at or after the apply
        time, left out when the player did not die from then until the game ended
        (UC-101 BR-006); ``intervention.id`` and ``moment.id`` are left out when empty.
        """
        attrs: dict = {"intervention.type": entry.type}
        if entry.intervention_id:
            attrs["intervention.id"] = entry.intervention_id
        attrs["player.serial"] = entry.serial
        if name:
            attrs["player.name"] = name
        if entry.moment_id:
            attrs["moment.id"] = entry.moment_id
        attrs[APPLIED_AT_ATTR] = entry.applied_at_ns
        later = [death for death in self._deaths.get(entry.serial, ()) if death >= entry.applied_at_ns]
        if later:
            attrs[FIRST_DEATH_AT_ATTR] = min(later)
        return attrs

    def event_attributes(self, serial: str, name: str = "") -> dict:
        """The ``challenge.ground_truth`` attributes of one player.

        Each time array is left out when the player has no such moment.
        """
        attrs: dict = {"player.serial": serial}
        if name:
            attrs["player.name"] = name
        if self._warnings.get(serial):
            attrs[WARNING_TIMES_ATTR] = list(self._warnings[serial])
        if self._deaths.get(serial):
            attrs[DEATH_TIMES_ATTR] = list(self._deaths[serial])
        return attrs

    def record_on(self, span: trace.Span | None, names: Mapping[str, str]) -> None:
        """One ``challenge.ground_truth`` event per player on ``span`` (the game root), then
        one ``challenge.intervention_ground_truth`` per applied intervention, in apply order.

        ``names`` maps every player serial of the game to its display name ("" when
        unknown); a serial with a recorded moment but no longer in the game is
        recorded too.
        """
        if span is None:
            return
        serials = dict.fromkeys([*names, *self._warnings, *self._deaths])
        for serial in serials:
            span.add_event(GROUND_TRUTH_EVENT, attributes=self.event_attributes(serial, names.get(serial, "")))
        for entry in self._interventions:
            span.add_event(
                INTERVENTION_GROUND_TRUTH_EVENT,
                attributes=self.intervention_attributes(entry, names.get(entry.serial, "")),
            )
