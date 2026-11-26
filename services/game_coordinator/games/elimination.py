"""The kill and elimination path of a game (UC-030 step 9).

Tests patch ``tracer``, ``time``, ``get_config_manager``, ``record_moment`` and
``inject_trace_context`` on ``games.base``: read them as ``_base.<name>``.
The kill path's gauges stay on ``BaseGameMode``: tests patch ``games.base.metrics``.
"""

from opentelemetry import trace

from lib.colors import Colors
from lib.types import Sound
from services.game_coordinator import game_clock
from services.game_coordinator.games import base as _base
from services.game_coordinator.games.base import (
    CHARGE_OUTCOME_DEATH,
    HOLD_OUTCOME_DEATH,
    Player,
    logger,
)
from services.game_coordinator.stream_writes import serialized_write

# Tie cue (UC-020 BR-010)
TIE_CUE_DELAY_S = 0.3
TIE_FLASH_MS = 2000
TIE_FLASH_HZ = 4


class EliminationMixin:
    """The kill and elimination path of a game (UC-030 step 9). Mixed into ``BaseGameMode``."""

    def _init_elimination_state(self) -> None:
        """Death count, elimination order and the kills in flight."""
        self.dead_count = 0  # Track deaths for tempo timing
        # Elimination ordering for game_player_elimination_order metric.
        # Incremented on each kill; the value at kill time is the player's order
        # index (1 = first eliminated). In respawn modes this counts each death.
        self._elimination_count = 0
        # Serials whose _kill_player is mid-flight. _kill_player awaits (death
        # sound) before the mode impl flips player.alive, so a concurrent kill
        # of the same serial (e.g. motion death racing a disconnect) would
        # otherwise pass the alive check and count the death twice.
        self._kills_in_progress: set[str] = set()

    def _player_death_event_attributes(self, serial: str) -> dict:  # noqa: ARG002
        """Mode-specific attributes for the base kill path's ``player_death`` moment.

        Called by ``_kill_player`` BEFORE ``_kill_player_impl`` (which may end the
        player span), so values describe the state *after* this death as if the
        player were already out. They ride on every record of the moment, in every
        shape (UC-100 BR-003). They come after the base values (``death.reason``,
        ``alive_remaining``, ...) and override them on a key clash; only the identity
        keys (``moment.id``, ``player.serial``, ``player.name``, ``game.id``,
        ``game.kind``) come last and win. Default: no extra attributes (#1327).
        """
        return {}

    def _sends_death_effect(self, serial: str) -> bool:  # noqa: ARG002
        """Whether the base kill path sends ``GAME_EFFECT_PLAYER_DEATH`` for this kill.

        Called by ``_kill_player`` AFTER ``_kill_player_impl``, synchronously (no
        await between the impl and the write). Default: always (#1342). Zombies
        overrides it so a converted human, still in the game, keeps its zombie
        colour instead of a trailing death effect.
        """
        return True

    def _accepts_kill(self, serial: str) -> bool:  # noqa: ARG002
        """Whether the mode takes this kill at all (UC-030 step 9, #1383).

        Called by ``_kill_player`` synchronously, with its other guards, and again
        by ``_run_kill`` after its awaits (shield charge end, explosion), right
        before the bookkeeping: a declined kill gets no ``dead_count``, ``player_alive``
        gauge, elimination order, ``player_deaths_total``, ``player_death`` moment
        or death effect (nor an explosion, nor the end of a held shield charge, when
        declined at the first check), and
        ``_kill_player`` returns False. True means
        the kill passed the gate, not that the mode's handler applied it. Default:
        until the game's outcome is latched; after that no kill is taken, so a kill
        during the post-win hold or teardown counts nothing and cannot change a
        decided result (UC-030 step 9, UC-020 BR-003, UC-026 BR-003; #2019). Modes
        with their own gate (Swapper, Fight Club, Tournament) decline after their
        own latch.
        """
        return self._latched_outcome is None

    def _kill_gate_open(self, serial: str) -> bool:
        """The kill path's gate: closed for every mode once the game is force-ended, else the mode's gate.

        A force-end (UC-027) can land while the game is still in a frame: the frame's
        later kills, and a kill awaiting its explosion, then count nothing, in every
        mode (UC-030 step 9, UC-020 BR-003; #2033). The session applies it on this
        game's loop (#2149), so it never lands between this check and the bookkeeping
        after it. Checked only when a kill is attempted, so it costs nothing per frame.
        """
        return self.force_end_reason is None and self._accepts_kill(serial)

    async def _kill_player(self, serial: str, accel_mag: float, reason: str = "motion") -> bool:
        """
        Kill a player (template method calling subclass implementation).

        Args:
            serial: Controller serial number
            accel_mag: Acceleration magnitude that caused death (0.0 for
                non-motion deaths such as agent interventions)
            reason: What caused the death. ``"motion"`` for natural threshold
                deaths; ``"agent_intervention"`` (#730) when the death is forced
                by an ``eliminate_player`` agent intervention. Recorded on the
                player_death moment so forced deaths are distinguishable from
                natural ones while still emitting identical spans/events/metrics.

        Returns:
            False when the kill was ignored (unknown or dead player, a kill of the
            same player in flight), the game was force-ended (#2033) or the kill was
            declined by ``_accepts_kill``; True otherwise.
        """
        player = self.players.get(serial)
        if not player or not player.alive or serial in self._kills_in_progress:
            return False
        if not self._kill_gate_open(serial):
            return False

        self._kills_in_progress.add(serial)
        try:
            return await self._run_kill(serial, player, accel_mag, reason)
        finally:
            self._kills_in_progress.discard(serial)

    def _kill_records_elimination_order(self, serial: str) -> bool:  # noqa: ARG002
        """Whether ``_count_elimination`` records the player's elimination order.

        Default: always, every death takes the player out. Modes where a death can
        keep the player in the game override it (UC-041 BR-008): Fight Club (a round
        loss, #2517), NonStop (a respawn death), Swapper (a swap) and Zombies (every
        death, #2524); they record the order when a player leaves the game for good.
        """
        return True

    def _record_death_moment(
        self,
        serial: str,
        player: Player,
        accel_mag: float,
        reason: str,
        alive_remaining: int,
        since_shield_end: dict,
        death_ns: int,
    ) -> None:
        """Record the ONE ``player_death`` moment of a death in the game's moment shape (UC-100 BR-007)."""
        if player.span:
            _base.record_moment(
                "player_death",
                shape=self.moment_shape,
                parent_span=player.span,
                attributes={
                    "accel_magnitude": accel_mag,
                    "sensitivity": self.sensitivity.name,
                    "sensitivity_factor": player.sensitivity_factor,
                    "music_speed": self.music_speed,
                    "alive_remaining": alive_remaining,
                    "death.reason": reason,
                    **since_shield_end,
                    **self._player_death_event_attributes(serial),
                    **self._moment_identity(player, serial),
                },
                timestamp_ns=death_ns,
                tracer=_base.tracer,
            )

    async def _send_death_effect(self, serial: str, trace_parent: str, trace_state: str) -> None:
        """Send the death effect (red + vibrate, no restore) on the gameplay stream, if the mode sends one."""
        # Send death effect via stream (red + vibrate, no restore)
        # Use pre-extracted trace context so effect appears under player_lifecycle
        # in traces, even though _kill_player_impl may have closed the span.
        from proto import controller_manager_pb2

        if not self._sends_death_effect(serial):
            return
        if not self.gameplay_stream:
            return
        effect_cmd = controller_manager_pb2.GameplayStreamControl(
            game_effect=controller_manager_pb2.GameEffectCommand(
                serial=serial,
                effect=controller_manager_pb2.GAME_EFFECT_PLAYER_DEATH,
                trace_parent=trace_parent,
                trace_state=trace_state,
            )
        )
        try:
            await serialized_write(self.gameplay_stream, effect_cmd)
        except self._STREAM_CALL_ERRORS:
            # The call died under the write (a reopen follows): the next stream sends it (UC-030 BR-006, #2471).
            self._undelivered_deaths.add(serial)
            raise

    async def _run_kill(self, serial: str, player: Player, accel_mag: float, reason: str) -> bool:
        """Body of _kill_player; runs at most once at a time per serial.

        Returns False when the kill gate closed while the kill path awaited (the
        shield charge end, the explosion): the kill is then declined before any
        bookkeeping (UC-030 step 9, #1988).
        """
        # A held shield charge ends with the death, before the player span closes
        # (UC-048 A5; a disconnect and eliminate_player come through here too).
        if player.shield_charge_seconds > 0:
            await self._discard_shield_charge(player, CHARGE_OUTCOME_DEATH)
        # UC-048 A11: the death drops a hold pool; eliminate_player ignores the hold.
        if self._shield_hold is not None:
            self._drop_shield_hold(player, HOLD_OUTCOME_DEATH, game_clock.now())

        # Play death explosion sound under the player's span
        player_ctx = trace.set_span_in_context(player.span) if player.span else None
        await self._play_sound(Sound.SFX_EXPLOSION, priority=2, parent_context=player_ctx)

        # The gate again, synchronous with the bookkeeping and the mode's handler: a
        # mode's state may have changed during the awaits above (Swapper latched its
        # win, the game was force-ended), and a declined kill counts no death.
        if not self._kill_gate_open(serial):
            return False
        # Again after the awaits: energy granted while the kill awaited is dropped too.
        if self._shield_hold is not None:
            self._drop_shield_hold(player, HOLD_OUTCOME_DEATH, game_clock.now())
        # UC-048 BR-015: the death ends a shield end grace window; a respawn never inherits it.
        since_shield_end = self._shield_since_end_attributes(player, game_clock.now())
        player.end_grace_until = 0.0
        player.shield_end_at = 0.0
        player.hold_prime_cap = False

        alive_count_before = len([p for p in self.players.values() if p.alive])
        logger.info(f"Player died: {serial}, {alive_count_before - 1} players remaining")

        self._count_elimination(serial)

        self._count_death()
        # UC-100 BR-006: the death's time, also the timestamp of its player_death record.
        death_ns = self._ground_truth.record_death(serial)

        # Extract trace context BEFORE _kill_player_impl() which may close the span.
        # This ensures the death effect and sound are parented to the player_lifecycle
        # span even after the subclass impl ends it.
        trace_parent, trace_state = _base.inject_trace_context(player.span)

        # The ONE player_death moment per death, for every mode, in the game's moment
        # shape (UC-100 BR-007; a span event on the player span by default): modes add
        # their own attributes through _player_death_event_attributes() and must not
        # emit a second player_death from _kill_player_impl(). record_moment never
        # raises, so the kill below is never half-applied.
        self._record_death_moment(serial, player, accel_mag, reason, alive_count_before - 1, since_shield_end, death_ns)
        # An eliminate_player intervention lands as this death: its moment comes right
        # after player_death, applied at the death's time (UC-101 BR-001, BR-006).
        self.record_intervention(serial, types=("eliminate_player",), timestamp_ns=death_ns)

        # Finalize health attributes BEFORE _kill_player_impl() which may close the span.
        # Without this, dead players lose their health.total_poll_drops etc. attributes.
        self._finalize_player_health(player)

        # Call subclass-specific death handling
        await self._kill_player_impl(serial, accel_mag)

        # The aggregate counts the state AFTER the mode's handler: it flips alive to
        # False for a death, keeps it True for a Zombies conversion.
        # A kill finishing after the game stopped must not overwrite the end-of-game 0.
        if self.running:
            self._set_players_alive_aggregate(len([p for p in self.players.values() if p.alive]))

        await self._send_death_effect(serial, trace_parent, trace_state)
        return True

    async def _wait_for_rainbow_effect(self) -> bool:
        """
        Wait for the winner rainbow effect to complete.

        Uses runtime config for duration. Interruptible by force_end.

        Returns:
            True if wait completed normally, False if interrupted
        """
        config = _base.get_config_manager().get_config()
        rainbow_duration_s = config.winner_rainbow_duration_ms / 1000.0
        iterations = int(rainbow_duration_s * 10)  # 0.1s increments

        logger.debug(f"Waiting {rainbow_duration_s}s for rainbow effect")
        for i in range(iterations):
            if not self.running:
                logger.info(f"Rainbow wait interrupted at {i * 0.1:.1f}s/{rainbow_duration_s}s")
                return False
            await game_clock.sleep(0.1)

        return True

    async def _wait_while_running(self, seconds: float) -> None:
        """Sleep ``seconds`` of game time in 0.1 s steps; a force-end cuts it short."""
        for _ in range(round(seconds / 0.1)):
            if not self.running:
                return
            await game_clock.sleep(0.1)

    async def _show_tie_cue(self, sound: Sound = Sound.SFX_DEATH) -> None:
        """The tie cue (UC-020 BR-010): white flash on every player and the tie sound, then wait it out.

        Sent after a latched tie even when a force-end landed after the latch (BR-007);
        the force-end only cuts the waits short.
        """
        from proto import controller_manager_pb2

        # The controller manager skips a flash on a running death effect, and the tied
        # players' death effect (1 s) can outlast the 1 s post-win hold by the write latency.
        await self._wait_while_running(TIE_CUE_DELAY_S)
        r, g, b = Colors.White.value
        for serial in self.players:
            await self._write_end_effect(
                controller_manager_pb2.GameplayStreamControl(
                    game_effect=controller_manager_pb2.GameEffectCommand(
                        serial=serial,
                        effect=controller_manager_pb2.GAME_EFFECT_FLASH,
                        color=controller_manager_pb2.RGB(r=r, g=g, b=b),
                        duration_ms=TIE_FLASH_MS,
                        speed=TIE_FLASH_HZ,
                    )
                )
            )
        await self._play_sound(sound, priority=2)
        await self._wait_while_running(TIE_FLASH_MS / 1000.0)
