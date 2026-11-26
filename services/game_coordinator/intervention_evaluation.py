"""Per-spec intervention evaluation: change detection over the flag reads.

Split out of ``InterventionManager``. For one spec and one live session,
:class:`SpecEvaluator` decides what to apply: it reads the flag through the
manager's flag reader, compares the read with the session's baseline in
:class:`~services.game_coordinator.intervention_sessions.SessionState`, moves the
baseline, and hands each change to the manager's ``dispatch`` (the enforcement
chain + handler) or ``revert`` callback. Four shapes:

- state flags: one value per game, applied when it changes away from neutral;
- one-shot flags: per-game event lists, each new event applied once (FR-137);
- per-player levels: one value per serial, the changed serials applied together;
- per-player grants: one-shot per serial, each new grant applied on its own.

The module-level helpers map event payloads to the handlers' payload strings
(and back) and compare flag values.
"""

import logging
from collections.abc import Awaitable, Callable
from typing import TYPE_CHECKING, Protocol

from services.game_coordinator.intervention_dispatch import _ids_for
from services.game_coordinator.intervention_sessions import SessionState

if TYPE_CHECKING:
    from services.game_coordinator.intervention_sessions import SessionView
    from services.game_coordinator.intervention_target import InterventionTarget
    from services.game_coordinator.interventions import InterventionSpec

# The manager's logger: log routing is unchanged by the split.
logger = logging.getLogger("services.game_coordinator.interventions")


class _ReadFailed:
    """A per-player flag read that failed (flagd error, #1299/#1313).

    OpenFeature's value getters return the default on an error, which would
    look like "neutral" and move the change-detection baseline. A failed read
    is reported as ``_UNKNOWN`` instead: it never moves a baseline and never
    fires (fail closed); the next good read is compared as usual.
    """

    def __repr__(self) -> str:
        return "<read failed>"


_UNKNOWN = _ReadFailed()


class FlagReads(Protocol):
    """The flag reads the evaluator needs (``InterventionFlagReader`` in interventions.py)."""

    def _read_flag(self, spec: "InterventionSpec", session: "SessionView") -> object: ...

    def _read_event_list(
        self, spec: "InterventionSpec", session: "SessionView"
    ) -> "tuple[str, int, list[tuple[int, dict, str | None]]] | None": ...

    def _read_grant(
        self, spec: "InterventionSpec", game_id: str, serial: str
    ) -> "tuple[object, object, str | None] | _ReadFailed": ...

    def read_levels(self, spec: "InterventionSpec", game_id: str, serials, ids: "dict | None" = None) -> dict: ...


class Dispatch(Protocol):
    """Run the enforcement chain and the handler for one change (the manager's ``_enforce_and_dispatch``)."""

    def __call__(
        self,
        spec: "InterventionSpec",
        value: object,
        payload: str,
        target: str | None,
        session: "SessionView",
        *,
        intervention_id: str | None = None,
        intervention_ids: "dict[str, str] | None" = None,
    ) -> Awaitable[None]: ...


class Revert(Protocol):
    """Run the flag's revert handler, if any (the manager's ``_run_revert``)."""

    def __call__(
        self,
        spec: "InterventionSpec",
        value: object,
        session: "SessionView",
        *,
        intervention_ids: "dict[str, str] | None" = None,
    ) -> Awaitable[None]: ...


class SpecEvaluator:
    """Change-detects one spec for one session and dispatches what changed.

    ``allow_list_lags(spec)`` is the cross-set consistency gate (UC-053 A9): asked
    after the values are read and before any baseline moves; True defers the
    values (nothing applied, no baseline moved).
    """

    def __init__(
        self,
        state: SessionState,
        reads: FlagReads,
        *,
        allow_list_lags: Callable[["InterventionSpec"], bool],
        dispatch: Dispatch,
        revert: Revert,
    ):
        self._state = state
        self._reads = reads
        self._allow_list_lags = allow_list_lags
        self._dispatch = dispatch
        self._revert = revert

    async def evaluate(self, spec: "InterventionSpec", session: "SessionView") -> None:
        # Player-targeted state flags are driven entirely through flagd's
        # per-serial targeting if-ladder (grants: shield/toughness boost/soft
        # penalty, per serial; levels: sensitivity/handicap factor); the
        # agent writer leaves the flag's global defaultVariant on its neutral
        # value, so the global evaluated read NEVER changes and can't be used for
        # change detection. Resolve the per-serial values instead and key change
        # detection on them (handled separately below).
        if spec.per_serial_grant:
            await self.evaluate_targeted_grant(spec, session)
            return
        if spec.player_targeted and not spec.edge_triggered:
            await self.evaluate_targeted_state(spec, session)
            return

        if spec.edge_triggered:
            await self.evaluate_event_list(spec, session)
            return

        raw = self._reads._read_flag(spec, session)
        if self._allow_list_lags(spec):
            return  # deferred: the baseline stays, the agent stream's catch-up re-evaluates
        # State-shaped: only act on value change away from the none-value.
        with self._state.lock:
            bucket = self._state.existing_state_bucket(session.game_id)
            if bucket is None:
                return  # session retired mid-pass
            last = bucket.get(spec.flag_key)
            changed = raw != last
            bucket[spec.flag_key] = raw
        if not changed:
            return
        if _is_none_value(spec, raw):
            # Reverting to neutral is not an enforced intervention, but some
            # state flags need to actively restore a default on revert (e.g.
            # volume_override -> configured game volume). Such
            # flags register a revert handler; it runs without enforcement
            # (restoring a default is always safe / not rate-limited).
            # Only fire when the PREVIOUS value was a real (non-none)
            # intervention — a fresh none-to-none transition (unknown prior
            # baseline) must not spuriously restore.
            was_active = last is not None and not _is_none_value(spec, last)
            if was_active:
                await self._revert(spec, raw, session)
            return
        # Player-targeted state flags fan out across all active serials via flagd
        # targeting (keyed on ``targeting_key = serial``); there is no single
        # chain-level target, so the chain's single-target battery check is a no-op
        # for them (the battery guard runs per serial in the per-player reads).
        await self._dispatch(spec, raw, "", None, session)

    async def evaluate_event_list(self, spec: "InterventionSpec", session: "SessionView") -> None:
        """Apply every new event of a one-shot flag's per-game event list (FR-137).

        UC-040 BR-006: a one-shot intervention fires once, only in its game.

        The flag resolves, for this game's ``gameId``, to
        ``{"epoch","seq","events":[{"seq","payload"}, ...]}`` (the last K
        events). Rules (architecture note §6, re-review N2):

        - a failed read (error code or reason ERROR) does nothing: it neither
          applies nor moves the last applied ``(epoch, seq)``;
        - an empty epoch is the baseline value: nothing to apply or reset;
        - only a non-empty, different epoch (a flag-service restart) resets the
          last seq to 0;
        - a seq at or below the last applied one in the same epoch is ignored;
        - more than K events between two reads is logged and counted;
        - the last applied ``(epoch, seq)`` advances under the lock BEFORE the
          events are dispatched, so an overlapping pass cannot apply them twice.
        """
        value = self._reads._read_event_list(spec, session)
        if value is None:
            return
        epoch, seq, events = value
        if not epoch:
            return
        if self._allow_list_lags(spec):
            return  # deferred: (epoch, seq) stays, so nothing is skipped or replayed
        with self._state.lock:
            bucket = self._state.existing_event_bucket(session.game_id)
            if bucket is None:
                return  # session retired mid-pass: never fire into it
            last_epoch, last_seq = bucket.get(spec.flag_key, ("", 0))
            if epoch != last_epoch:
                last_seq = 0
            elif seq <= last_seq:
                return
            fresh = sorted((e for e in events if e[0] > last_seq), key=lambda e: e[0])
            gap = bool(fresh) and fresh[0][0] > last_seq + 1
            bucket[spec.flag_key] = (epoch, max([seq, last_seq] + [e[0] for e in fresh]))
        if gap:
            logger.warning(
                f"{spec.flag_key}: game {session.game_id}: events {last_seq + 1}..{fresh[0][0] - 1} "
                "fell out of the event list before they were read (more than K between two reads)"
            )
        for _, payload, decision_id in fresh:
            payload_str = _event_payload_string(spec, payload)
            if spec.payload_required and not payload_str.strip(" :"):
                continue
            target = _edge_target_serial(spec, payload_str)
            await self._dispatch(spec, payload_str, payload_str, target, session, intervention_id=decision_id)

    async def evaluate_targeted_state(self, spec: "InterventionSpec", session: "SessionView") -> None:
        """Change-detect and dispatch a player-targeted LEVEL flag.

        Levels (``player_sensitivity_factor``, ``player_handicap_factor``) are
        idempotent: the handler sets every player's factor, so re-applying an
        unchanged factor is a no-op. One-shot grants use
        :meth:`evaluate_targeted_grant` instead (UC-040 BR-012).

        The agent writer drives these flags via a per-serial targeting if-ladder
        and never moves the global ``defaultVariant`` off neutral, so a global
        read is useless for change detection. Instead resolve the flag per active
        serial and key change detection on that resolved map. The handler (PR C/D)
        re-resolves and applies; dispatch is idempotent so re-applying an
        unchanged map would be harmless, but we still gate on change to avoid
        spurious metrics/events and needless rate-limit consumption.

        With no live game there are no serials to target, so this is a no-op
        (and the baseline is reset to empty so a later game picks up the flag).

        Only the players whose level changed since the last read in this game
        are dispatched (as a ``{serial: level}`` value); a serial the game has
        not read yet counts as neutral. Levels are game-scoped in the flag
        service, so a new game never sees a level of an earlier game
        (UC-040 BR-017). Players whose level moved to neutral go through the
        flag's revert handler, which resets their factors without the
        permission chain (UC-040 A24, #1317).
        """
        game = session.game
        if game is None:
            with self._state.lock:
                bucket = self._state.existing_state_bucket(session.game_id)
                if bucket is not None:
                    bucket[spec.flag_key] = None
            return

        # Each alive, battery-eligible serial is read with evaluation details
        # (float getter, value_kind="float" for both levels): a flagd error
        # reads as ``_UNKNOWN`` instead of the neutral default, so it can never
        # look like "level reverted" nor move the baseline (fail closed, as for
        # grants).
        default = _spec_default(spec)
        ids: dict[str, str] = {}
        reads = self._reads.read_levels(spec, session.game_id, _active_player_serials(game), ids)
        if self._allow_list_lags(spec):
            return  # deferred: no serial's baseline moves

        # Per-serial baseline for this game: ``{serial: level}``. A serial with
        # no entry is neutral, so a first non-neutral level applies.
        changed: dict = {}
        with self._state.lock:
            bucket = self._state.existing_state_bucket(session.game_id)
            if bucket is None:
                return  # session retired mid-pass
            last = bucket.get(spec.flag_key)
            baseline = dict(last) if isinstance(last, dict) else {}
            for serial, value in reads.items():
                if value is _UNKNOWN:
                    continue  # never move the baseline on an error read
                previous = baseline.get(serial, default)
                baseline[serial] = value
                if _same_value(value, previous):
                    continue
                changed[serial] = value
            bucket[spec.flag_key] = baseline
        if not changed:
            return

        # Only the players whose level changed in this game are dispatched, so a
        # level left over from an earlier game is never applied (UC-040 BR-017).
        # A move to neutral is a revert: it bypasses the permission chain and the
        # backstop, so a blocked chain (kill switch, exhausted budget) can never
        # leave a factor stuck after the baseline moved (UC-040 A24).
        to_neutral = {s: v for s, v in changed.items() if _is_none_value(spec, v)}
        applied = {s: v for s, v in changed.items() if s not in to_neutral}
        # UC-101 BR-004: each changed player's intervention id goes with its level.
        if to_neutral:
            await self._revert(spec, to_neutral, session, intervention_ids=_ids_for(ids, to_neutral))
        if applied:
            # Battery gating already happened in read_levels; the chain-level
            # target is None (fan-out), so its single-target battery check is a
            # no-op here. The handler applies ``applied``.
            await self._dispatch(spec, applied, "", None, session, intervention_ids=_ids_for(ids, applied))

    async def evaluate_targeted_grant(self, spec: "InterventionSpec", session: "SessionView") -> None:
        """Change-detect a per-serial GRANT flag and dispatch only new grants.

        Grants (shield, toughness boost, soft penalty) are one-shot effects, so a
        serial whose grant did not change since this game last read it must not
        be granted again (UC-040 BR-012, #1313). Each live serial's
        ``(variant, value)`` is compared with the game's per-serial baseline;
        the flag service generates a fresh opaque ``r<epoch>_<n>`` variant per
        write, so even a same-value re-grant is a change. A serial the game has
        not read yet has no grant, so its first non-neutral grant fires. One
        intervention is dispatched per changed, non-neutral serial with
        ``target`` = that serial, so the battery guard and the backstop apply
        per grant. Fail closed: a failed read never moves that serial's
        baseline and never fires. Grants are game-scoped, so a new game never
        sees a grant of an earlier game. A serial pruned by the agent
        (UC-040 BR-016) resolves to the neutral variant, which is a change but
        never fires.
        """
        game = session.game
        if game is None:
            return
        reads = {
            serial: self._reads._read_grant(spec, session.game_id, serial) for serial in _active_player_serials(game)
        }
        if self._allow_list_lags(spec):
            return  # deferred: no serial's baseline moves
        fired: list[tuple[str, object, str | None]] = []
        with self._state.lock:
            bucket = self._state.existing_state_bucket(session.game_id)
            if bucket is None:
                return  # session retired mid-pass
            baseline = bucket.get(spec.flag_key)
            if not isinstance(baseline, dict):
                baseline = {}
                bucket[spec.flag_key] = baseline
            for serial, grant in reads.items():
                if grant is _UNKNOWN:
                    continue  # never move the baseline on an error read
                last = baseline.get(serial)
                # The grant identity is ``(variant, value)``: the intervention id
                # never takes part in change detection (NFR-061).
                identity = (grant[0], grant[1])
                baseline[serial] = identity
                if identity == last:
                    continue
                value = grant[1]
                if not _is_none_value(spec, value):
                    fired.append((serial, value, grant[2]))
        for serial, value, decision_id in fired:
            await self._dispatch(spec, value, "", serial, session, intervention_id=decision_id)


# ---------------------------------------------------------------------- #
# Module-level helpers (pure; easy to unit-test)
# ---------------------------------------------------------------------- #
def _event_payload_string(spec: "InterventionSpec", payload: dict) -> str:
    """Map an event payload (FR-137) to the payload string the handlers take:
    the serial (eliminate/revive), the sound id (audio_cue),
    ``"<serial>:<effect>"`` (controller_effect) or ``""`` (end_game; the game
    is the scope)."""
    serial = str(payload.get("serial") or "")
    if spec.flag_key == "audio_cue":
        return str(payload.get("sound") or "")
    if spec.flag_key == "controller_effect":
        return f"{serial}:{payload.get('effect') or ''}"
    if spec.flag_key == "end_game":
        return ""
    if spec.flag_key == "group_shield":
        # "<serial,serial,...>|<seconds>" (UC-048 A13): _event_target_serials and
        # _event_group_seconds read it back.
        return f"{payload.get('serials') or ''}|{payload.get('seconds') or ''}"
    return serial


def _event_target_serials(spec: "InterventionSpec", payload: str) -> list[str]:
    """The target serials of a group event's payload string, in payload order (UC-048 A13).

    Empty for every other flag. Entries are stripped; empty entries are kept, so
    the handler refuses a malformed set instead of shrinking it.
    """
    if spec.flag_key != "group_shield":
        return []
    serials = payload.split("|", 1)[0]
    if not serials.strip():
        return []
    return [serial.strip() for serial in serials.split(",")]


def _event_group_seconds(payload: str) -> float | None:
    """The seconds of a group event's payload string, or None when missing or not a number."""
    if "|" not in payload:
        return None
    raw = payload.split("|", 1)[1].strip()
    try:
        return float(raw)
    except ValueError:
        return None


def _edge_target_serial(spec: "InterventionSpec", payload: str) -> str | None:
    """Resolve the target serial for an edge-triggered intervention.

    - eliminate_player / revive_player: payload IS the serial.
    - controller_effect: payload is ``"<serial>:<effect>"``; empty serial =
      broadcast (-> None).
    - audio_cue / end_game: not player-targeted (-> None).
    """
    if not spec.player_targeted:
        return None
    serial = payload.split(":", 1)[0] if ":" in payload else payload
    serial = serial.strip()
    return serial or None


def _is_none_value(spec: "InterventionSpec", value: object) -> bool:
    """True if a state-shaped flag value equals its neutral/none value."""
    none_val = spec.none_value
    if isinstance(none_val, (int, float)) and isinstance(value, (int, float)):
        return abs(float(value) - float(none_val)) < 1e-9
    return value == none_val


def _same_value(a: object, b: object) -> bool:
    """Equal values, comparing numbers with a float tolerance."""
    if isinstance(a, (int, float)) and isinstance(b, (int, float)):
        return abs(float(a) - float(b)) < 1e-9
    return a == b


def _spec_default(spec: "InterventionSpec") -> object:
    if spec.edge_triggered:
        return ""
    if spec.value_kind in ("int", "float"):
        return spec.none_value
    # State-shaped string flag: fall back to its string none_value (e.g. "none").
    return spec.none_value if isinstance(spec.none_value, str) else ""


def _active_player_serials(game: "InterventionTarget | None") -> list[str]:
    """Serials of the live game's players that are still alive.

    Empty list when there is no game / no players.
    """
    if game is None:
        return []
    return [serial for serial, player in game.players.items() if player.alive]
