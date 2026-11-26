"""Cross-set consistency gate for intervention reads (UC-053 A9, #1552, #2634).

Split out of ``InterventionManager``. The flag service stamps every flag of the
``interventions`` set and ``agent/interventions_allowed`` with the snapshot it
rendered them from. The two sets arrive on separate in-process sync streams that
reconnect in no fixed order after a flagd restart, so the coordinator defers
every intervention while the allow-list it holds is older than the interventions
set it holds; the catching-up ``agent`` stream fires a configuration change that
re-evaluates them. The snapshot probes themselves are flag reads and stay with
the manager's flag reader in ``interventions.py``.
"""

import asyncio
import logging
from collections.abc import Callable
from typing import TYPE_CHECKING, Protocol

if TYPE_CHECKING:
    from services.game_coordinator.interventions import InterventionSpec

# The manager's logger: log routing is unchanged by the split (#2634).
logger = logging.getLogger("services.game_coordinator.interventions")

# A deferral that lasts longer than this is logged as a WARNING (rate limited to
# one per DEFERRAL_WARN_INTERVAL_S) and re-checked every DEFERRAL_RECHECK_S even
# when no flag changes: a steady-state deferral lasts milliseconds, a permanent
# one (the ``agent`` set not served or its stream stuck) stops every intervention.
DEFERRAL_WARN_AFTER_S = 5.0
DEFERRAL_WARN_INTERVAL_S = 30.0
DEFERRAL_RECHECK_S = 5.0


class SnapshotProbes(Protocol):
    """The snapshot reads the gate needs (``InterventionFlagReader`` in interventions.py)."""

    @property
    def has_interventions_client(self) -> bool: ...

    @property
    def has_agent_client(self) -> bool: ...

    def interventions_snapshot(self) -> "tuple[str, int] | None": ...

    def allow_list_snapshot(self) -> "tuple[str, int] | None": ...


class AllowListGate:
    """Defers interventions while the allow-list held is older than the interventions set held.

    One deferral episode runs from the first deferred read to the next pass that
    is not deferred (or that finds no live session). ``spawn_evaluation(loop)``
    starts a re-evaluation on ``loop``; the gate schedules one every
    ``recheck_s()`` seconds (default ``DEFERRAL_RECHECK_S``, read when the
    re-check is scheduled) while an episode is open, so a stuck deferral stays
    visible without flag changes.
    """

    def __init__(
        self,
        probes: SnapshotProbes,
        *,
        time_fn: Callable[[], float],
        spawn_evaluation: Callable[[asyncio.AbstractEventLoop], None],
        recheck_s: Callable[[], float] = lambda: DEFERRAL_RECHECK_S,
    ):
        self._probes = probes
        self._time_fn = time_fn
        self._spawn_evaluation = spawn_evaluation
        self._recheck_s = recheck_s
        # The allow-list stamp last read in this pass (the allow-list only moves
        # forward, so a cached stamp that covers the probe needs no re-read).
        self.pass_allow_snapshot: tuple[str, int] | None = None
        self.since: float | None = None
        self.last_warned: float | None = None
        self.recheck: asyncio.TimerHandle | None = None  # while a re-check is pending

    def begin_pass(self) -> None:
        """Forget the allow-list stamp cached by the previous pass."""
        self.pass_allow_snapshot = None

    def lags(self, spec: "InterventionSpec") -> bool:
        """True while the allow-list held is older than the interventions set
        held (UC-053 A9, #1552): the values just read are then deferred.

        Called after the values are read and before any baseline moves. The
        probe is read after the values, and the allow-list after the probe (and
        again by the enforcement chain after this check), so when this returns False
        the allow-list that gates a value is at least as new as that value. Fail
        closed: a stamped interventions set is deferred against an allow-list
        with no stamp, another epoch or an older version, and a probe or
        allow-list read that raises defers too. An unstamped interventions set
        (file-sourced flagd) is not gated.
        """
        if not self._probes.has_interventions_client:
            return False
        allow = None
        try:
            held = self._probes.interventions_snapshot()
        except Exception as e:
            self.note(spec, None, None, reason=f"snapshot probe failed: {e}")
            return True
        if held is None:
            self.end()
            return False
        cached = self.pass_allow_snapshot
        if cached is not None and cached[0] == held[0] and cached[1] >= held[1]:
            self.end()
            return False
        if self._probes.has_agent_client:
            try:
                allow = self._probes.allow_list_snapshot()
            except Exception as e:
                logger.debug(f"InterventionManager: interventions_allowed snapshot read failed: {e}")
        self.pass_allow_snapshot = allow
        if allow is not None and allow[0] == held[0] and allow[1] >= held[1]:
            self.end()
            return False
        self.note(spec, held, allow)
        return True

    def note(
        self,
        spec: "InterventionSpec",
        held: "tuple[str, int] | None",
        allow: "tuple[str, int] | None",
        reason: str = "",
    ) -> None:
        """Record a deferral: warns (rate limited) once it lasts longer than
        DEFERRAL_WARN_AFTER_S and keeps a re-check scheduled so a stuck deferral
        stays visible without changes."""
        now = self._time_fn()
        if self.since is None:
            self.since = now
        age = now - self.since
        detail = reason or f"allow-list snapshot {allow} is older than interventions snapshot {held}"
        if age > DEFERRAL_WARN_AFTER_S and (
            self.last_warned is None or now - self.last_warned >= DEFERRAL_WARN_INTERVAL_S
        ):
            self.last_warned = now
            logger.warning(
                f"InterventionManager: interventions deferred for {age:.1f}s ({spec.flag_key}): {detail}; "
                "check that the flag service serves the agent set and the coordinator's agent stream is connected"
            )
        else:
            logger.debug(f"InterventionManager: {spec.flag_key} deferred: {detail}")
        self.schedule_recheck()

    def end(self) -> None:
        """Close the deferral episode (if one is open) and cancel its re-check."""
        if self.since is None:
            return
        self.since = None
        self.last_warned = None
        self.cancel_recheck()

    def cancel_recheck(self) -> None:
        if self.recheck is not None:
            self.recheck.cancel()
            self.recheck = None

    def schedule_recheck(self) -> None:
        if self.recheck is not None:
            return
        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            return

        def _recheck() -> None:
            self.recheck = None
            self._spawn_evaluation(loop)

        self.recheck = loop.call_later(self._recheck_s(), _recheck)
