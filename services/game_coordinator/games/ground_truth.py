"""Ground truth of a challenge game (UC-100 step 6, BR-006, FR-219, #2658).

The game records, per player serial, the time of each warning and of each death.
At a normal game end the base ``run()`` puts one ``challenge.ground_truth`` span
event per player on the game root span (telemetry.md §3.2). A force-ended game or
one whose ``run()`` raised records none (UC-100 A5).

The times come from the clock the moment records use: wall-clock epoch
nanoseconds, ``time.time_ns()``. Each ``record_*`` returns its time, and the game
passes it to ``record_moment`` as the record's own timestamp, so a ground-truth time
equals its record's timestamp in every shape. ``game_clock`` is not wall time and is
not used.
Kept per game and serial, never on the ``Player`` object, so a recreated player
record keeps its times.
"""

from collections.abc import Mapping
from time import time_ns

from opentelemetry import trace

GROUND_TRUTH_EVENT = "challenge.ground_truth"
WARNING_TIMES_ATTR = "challenge.warning_times_ns"
DEATH_TIMES_ATTR = "challenge.death_times_ns"


class GroundTruth:
    """Warning and death times per player serial, for one game."""

    def __init__(self) -> None:
        self._warnings: dict[str, list[int]] = {}
        self._deaths: dict[str, list[int]] = {}

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
        """One ``challenge.ground_truth`` event per player on ``span`` (the game root).

        ``names`` maps every player serial of the game to its display name ("" when
        unknown); a serial with a recorded moment but no longer in the game is
        recorded too.
        """
        if span is None:
            return
        serials = dict.fromkeys([*names, *self._warnings, *self._deaths])
        for serial in serials:
            span.add_event(GROUND_TRUTH_EVENT, attributes=self.event_attributes(serial, names.get(serial, "")))
