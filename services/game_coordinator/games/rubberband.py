"""Agent effects on a player: grace, toughness, soft penalty, rubberband, revive.

Split out of ``BaseGameMode`` (#2633): the public effect methods the intervention
manager calls (their signatures are unchanged), the grace deadline and the
``intervention`` moment. Names that tests patch on ``games.base`` (``tracer``, ``time``,
``get_config_manager``, ``record_moment``, ``inject_trace_context``) are looked up on
the base module at call time (``_base.``), as in ``games/shields.py``.
"""

from collections.abc import Collection
from typing import Any

from lib.moments import MOMENT_ID_ATTR, moment_id
from services.game_coordinator import game_clock, metrics
from services.game_coordinator.games import base as _base
from services.game_coordinator.games.base import (
    Player,
    logger,
    player_identity_attributes,
)
from services.game_coordinator.games.intervention_moment import (
    INTERVENTION_ID_ATTR,
    INTERVENTION_MOMENT,
    INTERVENTION_TYPE_ATTR,
    take_intervention,
)
from services.game_coordinator.stream_writes import serialized_write


class RubberbandMixin:
    """Agent effects on a player: grace, toughness, soft penalty, rubberband, revive. Mixed into ``BaseGameMode``."""

    # Toughness boost defaults. Boost is clamped to the same
    # upper bound as handicap_factor so a toughness boost is "much harder to die"
    # but never immune; seconds are capped so a single arming can't last forever.
    TOUGHNESS_BOOST_DEFAULT_FACTOR = 2.0
    TOUGHNESS_BOOST_FACTOR_MIN = 1.0
    TOUGHNESS_BOOST_FACTOR_MAX = 2.0
    TOUGHNESS_BOOST_MAX_SECONDS = 30.0

    # Soft-penalty defaults. The tighten factor is the
    # MIRROR of the toughness boost: a factor BELOW 1.0 weakens the player's
    # threshold for a short window (easier to die). It is clamped to the same
    # lower handicap bound (0.5) so even a fully-tightened threshold stays finite
    # — never instant-death. Seconds are capped so a single arming can't last
    # forever.
    SOFT_PENALTY_DEFAULT_FACTOR = 0.6
    SOFT_PENALTY_FACTOR_MIN = 0.5
    SOFT_PENALTY_FACTOR_MAX = 1.0
    SOFT_PENALTY_MAX_SECONDS = 30.0

    # Auto-rubberband defaults. A SINGLE agent decision is
    # expanded across players by the coordinator from the live skill gap. The
    # boost is TIME-BOXED (re-applied each agent cycle while the decision holds)
    # and BOUNDED: ``RUBBERBAND_MAX_BOOST_GENTLE``/``_STRONG`` cap the per-player
    # boost added on top of the neutral 1.0, and the boost a laggard gets is
    # ADDITIONALLY capped so its boosted death threshold can never exceed the
    # current leader's (no inversion of standings). Seconds are short so the boost
    # naturally expires shortly after the decision is withdrawn.
    RUBBERBAND_BOOST_MIN = 1.0
    RUBBERBAND_BOOST_MAX = 2.0
    RUBBERBAND_DEFAULT_SECONDS = 5.0
    RUBBERBAND_MAX_SECONDS = 30.0
    # Per-strength cap on the boost ADDED above the neutral 1.0 (so gentle tops out
    # at 1.2x, strong at 1.4x — well inside the [0.5, 2.0] threshold clamp).
    RUBBERBAND_MAX_BOOST_GENTLE = 0.2
    RUBBERBAND_MAX_BOOST_STRONG = 0.4

    # Spawn grace applied to a revived player so they don't instantly re-die
    # before they can stop moving (N5). Mirrors Nonstop/Zombie respawn grace.
    REVIVE_SPAWN_GRACE = 2.0

    def record_intervention(
        self, serial: str, *, types: Collection[str] | None = None, timestamp_ns: int | None = None
    ) -> None:
        """Record the ``intervention`` moment of ``serial`` if an agent intervention is landing on it now.

        Called where a player lever records its own effect (UC-101 BR-001, BR-005). The
        intervention comes from the scope the intervention manager runs the handler in
        (``games/intervention_moment.py``): outside a scope, for a player the scope does not
        name, a second time for the same player, or for another lever type (``types``) this
        records nothing. Records the ground-truth entry (FR-222), then the moment in the
        game's shape with ``intervention.type``, ``intervention.id`` (left out without one,
        A3) and the moment identity (BR-002, BR-004). ``timestamp_ns`` is the apply time
        when the caller already read it (an elimination: the death's time, BR-006).

        Never raises: an applied lever stays applied whatever the moment does (NFR-061).
        """
        try:
            tag = take_intervention(serial, types)
            if tag is None:
                return
            entry = self._ground_truth.record_intervention(serial, tag.type, tag.id, at_ns=timestamp_ns)
            player = self.players.get(serial)
            if player is None or not player.span:
                return  # no moment to send; the ground truth names none (BR-009)
            identity = self._moment_identity(player, serial)
            attributes: dict[str, Any] = {INTERVENTION_TYPE_ATTR: tag.type}
            if tag.id:
                attributes[INTERVENTION_ID_ATTR] = tag.id
            attributes.update(identity)
            sent = _base.record_moment(
                INTERVENTION_MOMENT,
                shape=self.moment_shape,
                parent_span=player.span,
                attributes=attributes,
                tracer=_base.tracer,
                timestamp_ns=entry.applied_at_ns,
            )
            if sent:
                entry.moment_id = identity[MOMENT_ID_ATTR]
        except Exception as e:  # noqa: BLE001 - a moment never reaches into the intervention
            logger.debug(f"Recording the intervention moment of {serial} failed: {e}")

    def _moment_identity(self, player: "Player", serial: str) -> dict[str, str]:
        """Identity attributes of a new moment of ``serial`` (UC-100 BR-003, telemetry.md §3.2)."""
        sequence = self._moment_sequence.get(serial, 0) + 1
        self._moment_sequence[serial] = sequence
        identity = {
            MOMENT_ID_ATTR: moment_id(self.game_id, serial, sequence),
            "player.serial": serial,
            "game.id": self.game_id,
            "game.kind": self.game_kind,
        }
        if player.name:
            identity["player.name"] = player.name
        return identity

    @staticmethod
    def effective_grace_until(player: "Player") -> float:
        """
        Single source of truth for a player's effective death-immunity deadline (#817).

        Two independent actors grant temporary death-immunity through different
        fields, and #817 (epic #814) ratifies how they compose:

        - **Admin baseline** — ``invincible_until``: the pre-game
          ``invincibility_seconds`` flag, materialized at match/round start by
          Tournament/Fight Club (mode-specific; base ``Player`` has no such
          field, so we read it defensively). The human owns this baseline.
        - **Agent delta** — ``grace_until``: a temporary additive shield granted
          mid-game via ``grant_shield`` (also reused by respawn/spawn grace). The
          agent owns this bounded in-game delta and NEVER persists it to game.json.

        Composition rule (ratified, do not relitigate):

            effective_grace_until = max(grace_until, invincible_until)

        i.e. a player is death-immune while *either* window is open. The agent can
        only ever EXTEND protection (``grant_shield`` is extend-not-shorten on
        ``grace_until``); it can neither shorten the admin baseline nor be
        shortened by it. Symmetrically the admin baseline never silently cancels
        an active agent shield. ``max`` is the only rule that honors both
        ownership boundaries at once — overwrite or last-write-wins would let one
        actor silently void the other.

        Every death/immunity check (the base per-frame gate and the mode-specific
        ``_kill_player_impl`` / ``_process_controller_state`` checks in Tournament
        and Fight Club) routes through this helper so the rule lives in exactly
        one place.

        Amended by #2096 (UC-030 BR-008): a running shield hold adds its own
        deadline, ``hold_until``. Hold never writes ``grace_until``, so a release
        can never shorten another window; ``hold_until`` is 0 outside hold games.
        """
        return max(_base.BaseGameMode._other_grace_until(player), player.hold_until)

    @staticmethod
    def _other_grace_until(player: "Player") -> float:
        """The effective grace without the shield hold: ``max(grace_until, invincible_until)`` (#817).

        A hold activation waits for this window to close (UC-030 BR-008: holding
        during another grace does not drain).
        """
        # base Player has no invincible_until; only Tournament/Fight Club add it.
        return max(player.grace_until, getattr(player, "invincible_until", 0.0))

    def is_in_grace(self, player: "Player", now: float | None = None) -> bool:
        """True while the player's effective grace (admin ∪ agent) is still open (#817)."""
        if now is None:
            now = game_clock.now()
        return now < self.effective_grace_until(player)

    async def grant_toughness_boost(self, serial: str, seconds: float, boost: float | None = None) -> bool:
        """
        Grant a time-boxed toughness boost to a player (#1129, #1103 Phase 2).

        Unlike :meth:`grant_shield` (total immunity via ``grace_until`` that skips
        the death check entirely), a toughness boost only raises the player's
        effective death/warning threshold for ``seconds`` by applying a temporary
        ``handicap`` boost — making them MUCH harder to eliminate but NOT immune: a
        genuinely huge spike can still exceed the (still-finite) boosted threshold.
        It reuses the #1107 per-player handicap mechanism: while the deadline is in
        the future, ``_compute_effective_thresholds`` takes ``max(handicap_factor,
        toughness_boost_factor)`` (so it never weakens a standing set_player_handicap
        delta, only strengthens it for the window), still clamped [0.5, 2.0].

        Time-box semantics mirror ``grant_shield``: extend-not-shorten on the
        deadline. A new arming only ever lengthens an active window (and never
        lowers an active boost); a shorter/weaker request is a no-op extension. On
        expiry the boost silently drops out — no reset task needed — and the
        standing ``handicap_factor`` resumes.

        Args:
            serial: Controller serial number.
            seconds: Shield duration in seconds. Non-positive is a no-op (False);
                capped at ``TOUGHNESS_BOOST_MAX_SECONDS``.
            boost: Handicap boost while active; clamped
                [``TOUGHNESS_BOOST_FACTOR_MIN``, ``TOUGHNESS_BOOST_FACTOR_MAX``].
                Defaults to ``TOUGHNESS_BOOST_DEFAULT_FACTOR`` (~2.0).

        Returns:
            True if a toughness boost was armed/extended, False if the player is
            unknown/dead or the duration was non-positive.
        """
        if seconds <= 0:
            return False

        player = self.players.get(serial)
        if not player or not player.alive:
            return False

        seconds = min(seconds, self.TOUGHNESS_BOOST_MAX_SECONDS)
        if boost is None:
            boost = self.TOUGHNESS_BOOST_DEFAULT_FACTOR
        boost = max(self.TOUGHNESS_BOOST_FACTOR_MIN, min(self.TOUGHNESS_BOOST_FACTOR_MAX, boost))

        new_until = game_clock.now() + seconds
        # Extend-not-shorten / strengthen-not-weaken: keep whichever active window
        # is longer and whichever active boost is higher, so repeated arming never
        # reduces in-flight protection.
        changed = True
        was_protected = self._shield_protected(player, game_clock.now())
        if game_clock.now() < player.toughness_boost_until:
            changed = new_until > player.toughness_boost_until or boost > player.toughness_boost_factor
            new_until = max(new_until, player.toughness_boost_until)
            boost = max(boost, player.toughness_boost_factor)
        player.toughness_boost_until = new_until
        player.toughness_boost_factor = boost
        logger.info(f"Toughness boost armed for {serial}: boost={boost:.2f} for {seconds:.1f}s (until={new_until:.2f})")

        if player.span:
            player.span.add_event(
                "toughness_boost_armed",
                attributes={
                    "toughness_boost_seconds": seconds,
                    "toughness_boost_factor": boost,
                    **player_identity_attributes(serial, player.name),
                },
            )

        # UC-101 BR-005: an arming that changed nothing is no intervention moment.
        if changed:
            self.record_intervention(serial, types=("toughness_boost",))

        # Weaker perceptible feedback, only when the arming changed the window or
        # the boost (UC-040 A16, BR-009/BR-010).
        if changed:
            await self._send_shield_feedback(
                player, new_until - game_clock.now(), kind="toughness_boost", sound=not was_protected
            )

        return True

    async def warn_player(self, serial: str) -> bool:
        """Fire the visible warning feedback for a player as an agent cue (#1134).

        This is the ``soft_penalty`` ``"warn"`` action: a graduated, fully
        RECOVERABLE alternative to ``eliminate_player``. It reuses the existing
        ``_warn_player`` feedback (white flash + rumble) as an agent-driven
        "you're on notice" cue. It changes NO thresholds and grants NO protection
        — purely visual/haptic, the player can still die immediately. We pass the
        player's current effective warning threshold so the span event is
        consistent with a natural warning; ``accel_mag`` is 0.0 (agent-driven, no
        triggering motion).

        Returns True if the warning fired, False if the player is unknown/dead.
        """
        player = self.players.get(serial)
        if not player or not player.alive:
            return False
        effective_warn, _ = self._compute_effective_thresholds(player)
        await self._warn_player(serial, 0.0, effective_warn)
        # UC-101 BR-001: the intervention moment after the warning's death_warning moment,
        # with its own clock read.
        self.record_intervention(serial, types=("soft_penalty",))
        return True

    async def apply_soft_penalty(self, serial: str, seconds: float, factor: float | None = None) -> bool:
        """Apply a time-boxed soft-penalty TIGHTEN to a player (#1134, Phase 3).

        The deliberate MIRROR of :meth:`grant_toughness_boost`: where a toughness boost
        STRENGTHENS protection (boost >= 1.0, composed by ``max``), a
        tighten WEAKENS it for ``seconds`` (``factor`` < 1.0, composed by ``min``
        in :meth:`_compute_effective_thresholds`) — making the player TEMPORARILY
        EASIER to eliminate. It is graduated PRESSURE, not guaranteed elimination:
        the effective handicap is still clamped [0.5, 2.0], so even a maxed
        tighten only lowers the threshold to half — never instant-death.

        Because the tighten composes by ``min`` AFTER the toughness_boost ``max``,
        it can cut through an active toughness boost (pressure overrides
        protection). On expiry the factor silently stops applying — no reset task
        — and the standing/shielded handicap resumes.

        Time-box semantics mirror :meth:`grant_toughness_boost`: extend-not-shorten
        on the deadline and tighten-not-loosen on the factor, so repeated arming
        never reduces in-flight pressure (keeps the LOWER, stronger factor).

        Args:
            serial: Controller serial number.
            seconds: Penalty duration. Non-positive is a no-op (False); capped at
                ``SOFT_PENALTY_MAX_SECONDS``.
            factor: Handicap multiplier while active; clamped
                [``SOFT_PENALTY_FACTOR_MIN``, ``SOFT_PENALTY_FACTOR_MAX``].
                Defaults to ``SOFT_PENALTY_DEFAULT_FACTOR`` (0.6).

        Returns:
            True if a tighten was armed/extended, False if the player is
            unknown/dead or the duration was non-positive.
        """
        if seconds <= 0:
            return False

        player = self.players.get(serial)
        if not player or not player.alive:
            return False

        seconds = min(seconds, self.SOFT_PENALTY_MAX_SECONDS)
        if factor is None:
            factor = self.SOFT_PENALTY_DEFAULT_FACTOR
        factor = max(self.SOFT_PENALTY_FACTOR_MIN, min(self.SOFT_PENALTY_FACTOR_MAX, factor))

        new_until = game_clock.now() + seconds
        # Extend-not-shorten / tighten-not-loosen: keep whichever active window is
        # longer and whichever active factor is LOWER (stronger pressure), so
        # repeated arming never reduces in-flight pressure. ``changed``: the arming
        # lengthened the window or strengthened the factor (UC-101 BR-005).
        changed = True
        if game_clock.now() < player.soft_penalty_until:
            changed = new_until > player.soft_penalty_until or factor < player.soft_penalty_factor
            new_until = max(new_until, player.soft_penalty_until)
            factor = min(factor, player.soft_penalty_factor)
        player.soft_penalty_until = new_until
        player.soft_penalty_factor = factor
        logger.info(f"Soft penalty tighten {serial}: factor={factor:.2f} for {seconds:.1f}s (until={new_until:.2f})")

        if player.span:
            player.span.add_event(
                "soft_penalty_tighten",
                attributes={
                    "soft_penalty_seconds": seconds,
                    "soft_penalty_factor": factor,
                    **player_identity_attributes(serial, player.name),
                },
            )
        # UC-101 BR-001, BR-005: one intervention moment for a tighten that changed the
        # window or the factor; the warning cue below adds no intervention moment.
        if changed:
            self.record_intervention(serial, types=("soft_penalty",))

        # Visible warning feedback so the tighten is observable to the player
        # ("you're on notice"), reusing the same warn cue as the "warn" action.
        await self._warn_player(serial, 0.0, self._compute_effective_thresholds(player)[0])

        return True

    async def apply_auto_rubberband(self, strength: str, seconds: float | None = None) -> int:
        """Auto-compress the live skill gap by boosting the trailing player(s) (#1143).

        Unlike :meth:`set_player_handicap` / :meth:`grant_toughness_boost` (the
        agent hand-picks one serial + factor), ``auto_rubberband`` is a SINGLE
        decision (``"gentle"`` | ``"strong"``) that the COORDINATOR expands across
        players from the live skill gap. The standing proxy is each alive player's
        HEADROOM to their effective death threshold (``death - smoothed_accel``):
        a large headroom = comfortably ahead (the leader), a small headroom = on
        the edge (the laggard). Trailing players receive a temporary handicap
        BOOST — the same #1107 mechanism, composed by MAX in
        :meth:`_compute_effective_thresholds` — that raises their death threshold,
        compressing the gap to the leader.

        Safety (never inverts standings, never instant-death):

        - The boost added above 1.0 is capped per strength
          (``RUBBERBAND_MAX_BOOST_GENTLE`` 0.2 / ``RUBBERBAND_MAX_BOOST_STRONG``
          0.4), so a laggard's threshold rises by at most 20% / 40%.
        - It is ADDITIONALLY capped so the laggard's BOOSTED death threshold can
          never exceed the current leader's death threshold — compression only,
          never overtaking (no standings inversion).
        - The combined handicap is still clamped [0.5, 2.0] downstream, so even a
          maxed boost is "harder to die", never immune / never instant-death.

        Needs at least two alive players to have a gap to compress; with fewer it
        is a no-op. Time-box / strengthen semantics mirror
        :meth:`grant_toughness_boost` (extend-not-shorten, strengthen-not-weaken).

        Args:
            strength: ``"gentle"`` or ``"strong"``. Any other value is a no-op.
            seconds: Boost window; defaults to ``RUBBERBAND_DEFAULT_SECONDS``,
                capped at ``RUBBERBAND_MAX_SECONDS``.

        Returns:
            The number of players boosted (0 on a no-op).
        """
        strength = (strength or "").strip().lower()
        if strength == "gentle":
            max_boost = self.RUBBERBAND_MAX_BOOST_GENTLE
        elif strength == "strong":
            max_boost = self.RUBBERBAND_MAX_BOOST_STRONG
        else:
            return 0  # unknown strength: safe no-op

        if seconds is None:
            seconds = self.RUBBERBAND_DEFAULT_SECONDS
        if seconds <= 0:
            return 0
        seconds = min(seconds, self.RUBBERBAND_MAX_SECONDS)

        # Rank alive players by headroom to their effective death threshold. We
        # need at least two to have a gap to compress.
        alive = [p for p in self.players.values() if p.alive]
        if len(alive) < 2:
            return 0

        headrooms: dict[str, tuple[float, float]] = {}  # serial -> (headroom, death)
        for p in alive:
            _, death = self._compute_effective_thresholds(p)
            headrooms[p.serial] = (death - p.smoothed_accel, death)

        # Leader = most headroom (comfortably ahead); laggard ranking from least.
        max_leader_headroom = max(hd[0] for hd in headrooms.values())
        # The gap span across the field; if everyone is equal there is nothing to
        # compress (gap == 0) and we no-op rather than boost identically.
        min_headroom = min(hd[0] for hd in headrooms.values())
        gap = max_leader_headroom - min_headroom
        if gap <= 1e-9:
            return 0

        now = game_clock.now()
        boosted = 0
        for p in alive:
            headroom, death = headrooms[p.serial]
            if death <= 0:
                continue
            # How far behind the leader this player is, in [0, 1]: the laggard
            # (min headroom) is 1.0, the leader is 0.0. The boost scales with this
            # deficit and the strength cap, so the leader gets ~0 and the laggard
            # gets up to max_boost.
            deficit = (max_leader_headroom - headroom) / gap
            target_boost = 1.0 + max_boost * deficit
            # No-invert cap (standings = headroom to death, NOT the raw threshold).
            # The boosted player's headroom (death*boost - accel) must never exceed
            # the leader's headroom — compression toward the leader, never past it.
            # Solve death*boost - accel <= leader_headroom => boost <= cap.
            no_invert_cap = (max_leader_headroom + p.smoothed_accel) / death
            target_boost = min(target_boost, max(1.0, no_invert_cap))
            # Final clamp to the rubberband boost band (still re-clamped [0.5,2.0]
            # downstream when composed).
            target_boost = max(self.RUBBERBAND_BOOST_MIN, min(self.RUBBERBAND_BOOST_MAX, target_boost))
            if target_boost <= 1.0 + 1e-9:
                continue  # nothing to add for this player (e.g. the leader)

            new_until = now + seconds
            # Extend-not-shorten / strengthen-not-weaken (mirror toughness_boost).
            boost = target_boost
            if now < p.rubberband_until:
                new_until = max(new_until, p.rubberband_until)
                boost = max(boost, p.rubberband_boost)
            p.rubberband_until = new_until
            p.rubberband_boost = boost
            boosted += 1
            if p.span:
                p.span.add_event(
                    "auto_rubberband_boost",
                    attributes={
                        "rubberband_strength": strength,
                        "rubberband_boost": boost,
                        "rubberband_seconds": seconds,
                        **player_identity_attributes(p.serial, p.name),
                    },
                )

        if boosted:
            logger.info(f"Auto-rubberband ({strength}): boosted {boosted} trailing player(s) for {seconds:.1f}s")
        return boosted

    async def revive_player(self, serial: str, spawn_grace: float | None = None) -> bool:
        """
        Revive a dead player, re-entering them into the game (#730, N5).

        Whether reviving is allowed at all in a given mode is decided by the
        intervention manager's §9 capability matrix BEFORE this runs (it denies
        ``revive_player`` for permanent-elimination / bracket / hidden-role
        modes). This method only implements the actual re-entry once allowed.

        Re-entry restores the player to the alive state, clears stale
        warning/motion state, restores the controller LED to the player's color,
        and grants a brief spawn grace (via the shield grace mechanism) so the
        revived player can't instantly re-die. Modes with a native respawn path
        (Nonstop ``_respawn_player``) delegate to it so their mode-specific state
        (spawn protection, scoring) stays consistent; other modes use the generic
        re-entry below.

        Args:
            serial: Controller serial number.
            spawn_grace: Spawn-protection grace in seconds; defaults to
                ``REVIVE_SPAWN_GRACE``.

        Returns:
            True if the player was revived, False if unknown / already alive or
            the mode's native respawn refused (returned False).
        """
        player = self.players.get(serial)
        if player is None:
            logger.warning(f"revive_player: unknown serial {serial}, ignoring")
            return False
        if player.alive:
            logger.info(f"revive_player: {serial} already alive, no-op")
            return False

        grace = self.REVIVE_SPAWN_GRACE if spawn_grace is None else spawn_grace
        # UC-040 BR-035: spawn grace is not a shield; no end grace window carries over.
        player.end_grace_until = 0.0
        player.shield_end_at = 0.0

        # Prefer a mode-native respawn path so mode-specific state stays correct.
        native = getattr(self, "_respawn_player", None)
        if callable(native):
            # A native path may refuse (returns False, e.g. Zombies once decided);
            # None (NonStop) keeps the old "always revived" behaviour.
            if await native(serial) is False:
                return False
            # Ensure spawn grace even if the native path uses its own scheme.
            player.grace_until = max(player.grace_until, game_clock.now() + grace)
            self.record_intervention(serial, types=("revive_player",))  # UC-101 BR-001
            logger.info(f"revive_player: {serial} revived via native respawn")
            return True

        # Generic re-entry for modes without a native respawn path.
        player.alive = True
        player.warning_until = 0.0
        player.smoothed_accel = 0.0
        player.grace_until = game_clock.now() + grace

        metrics.player_alive.labels(serial=serial, game_id=self.game_id).set(1)
        alive_count = len([p for p in self.players.values() if p.alive])
        self._set_players_alive_aggregate(alive_count)

        if player.span:
            player.span.add_event(
                "player_revived",
                attributes={"spawn_grace": grace, **player_identity_attributes(serial, player.name)},
            )
        self.record_intervention(serial, types=("revive_player",))  # UC-101 BR-001

        if self.gameplay_stream:
            from proto import controller_manager_pb2

            trace_parent, trace_state = _base.inject_trace_context(player.span)
            # Restore the player's base LED color, then a respawn pulse.
            base_color_cmd = controller_manager_pb2.GameplayStreamControl(
                base_color=controller_manager_pb2.ControllerColorConfig(
                    serial=serial,
                    color=controller_manager_pb2.RGB(r=player.color[0], g=player.color[1], b=player.color[2]),
                )
            )
            await serialized_write(self.gameplay_stream, base_color_cmd)
            respawn_cmd = controller_manager_pb2.GameplayStreamControl(
                game_effect=controller_manager_pb2.GameEffectCommand(
                    serial=serial,
                    effect=controller_manager_pb2.GAME_EFFECT_PLAYER_RESPAWN,
                    trace_parent=trace_parent,
                    trace_state=trace_state,
                )
            )
            await serialized_write(self.gameplay_stream, respawn_cmd)

        await self.event_publisher("player_revived", {"serial": serial})
        logger.info(f"revive_player: {serial} revived (generic re-entry, grace {grace:.1f}s)")
        return True
