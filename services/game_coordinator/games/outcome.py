"""The outcome of a game: force-end, latch and record (UC-020 BR-003).

Tests patch ``tracer``, ``time``, ``get_config_manager``, ``record_moment`` and
``inject_trace_context`` on ``games.base``: read them as ``_base.<name>``.
"""

from opentelemetry import trace

from services.game_coordinator.games.base import (
    GameState,
    logger,
)
from services.game_coordinator.games.ground_truth import GroundTruth


class GameOutcomeMixin:
    """The outcome of a game: force-end, latch and record (UC-020 BR-003, #1328). Mixed into ``BaseGameMode``."""

    def _init_outcome_state(self) -> None:
        """Force-end reason, the latched outcome and the root span the outcome lands on."""
        # Set by force_end(): the reason the game was stopped from outside, or
        # None for a game that ended on its own win condition.
        self.force_end_reason: str | None = None
        # The outcome, frozen once the game loop ends (win condition met, or the
        # loop stopped). A force-end that lands later (e.g. during the winner
        # rainbow) must not turn a real win into force_ended.
        self._latched_outcome: dict | None = None
        self._outcome_recorded = False
        # Set when run() exits by an exception: the run-end cleanup labels the
        # player spans it ends ``error`` (UC-030 BR-007).
        self._run_failed = False
        # Set once the base run() published game_error, so its session does not
        # publish it a second time.
        self.game_error_published = False
        # The game root span (current when run() starts); the outcome lands on it.
        self._game_root_span: trace.Span | None = None
        # Warning and death times per serial, recorded on the root at a normal end (UC-100 BR-006).
        self._ground_truth = GroundTruth()

    def force_end(self, reason: str = ""):
        """Force the game to end (called externally).

        Records the reason so the game's outcome is ``force_ended`` (UC-020 BR-003,
        #1328), never a winner or a set of victories. ``GameSession.force_end`` calls
        it on the game's own loop while that loop runs (#2149).
        """
        logger.info(f"Force ending game... (reason: {reason or 'unspecified'})")
        self.force_end_reason = reason or "unspecified"
        self.running = False

    @property
    def force_ended(self) -> bool:
        """True once the game was force-ended (#1328)."""
        return self.force_end_reason is not None

    def _natural_game_outcome(self) -> dict:
        """Outcome attributes of a game that ended on its own (not force-ended).

        Modes that classify their end (JoustFFA: ``winner`` / ``tie``) override this.
        Default ``completed``: the mode's own winner events carry the detail.
        """
        return {"game.outcome": "completed"}

    def game_ending(self) -> bool:
        """True once the game is ending: outcome latched, force-ended, or ENDING / ENDED.

        From then on the winner rainbow owns the winner's LED: no agent cue is sent
        (UC-040 step 13, UC-020 BR-004, #2533) and no ``grant_shield`` applies (UC-048 A12 step 3).
        """
        return (
            self._latched_outcome is not None or self.force_ended or self.state in (GameState.ENDING, GameState.ENDED)
        )

    def _latch_game_outcome(self) -> None:
        """Freeze the outcome when the game loop ends (first call wins, #1328).

        Called when the win condition is met and again when the loop exits, so a
        force-end that arrives afterwards (winner rainbow, teardown) cannot
        rewrite a ``winner`` / ``tie`` / ``completed`` outcome.
        """
        if self._latched_outcome is None:
            self._latched_outcome = self._game_outcome_attributes()

    def _game_outcome_attributes(self) -> dict:
        """The game's end-of-game outcome (UC-020 BR-003, #1328).

        The latched outcome wins once the loop ended. Before that, ``force_ended``
        (with ``game.end_reason``) takes precedence over any natural outcome: a
        game force-ended while its loop ran did not reach its win condition.
        """
        if self._latched_outcome is not None:
            return dict(self._latched_outcome)
        if self.force_ended:
            return {"game.outcome": "force_ended", "game.end_reason": self.force_end_reason}
        return self._natural_game_outcome()

    def game_outcome(self) -> str:
        """The ``game.outcome`` value: ``winner``, ``tie``, ``completed`` or ``force_ended``.

        The same value the game root span records; the session labels
        ``games_completed_total{outcome}`` with it (#1427).
        """
        return self._game_outcome_attributes()["game.outcome"]

    def _capture_game_root_span(self) -> None:
        """Remember the current span (the session's game root) for the outcome.

        Called at the start of ``run()`` by the base and by modes that override it.
        """
        self._game_root_span = trace.get_current_span()

    def _record_game_outcome(self, teardown_span: trace.Span | None) -> None:
        """Put the outcome on the game root span and a ``game.outcome`` teardown event.

        Records once per game; later calls are no-ops. ``teardown_span`` is None
        when a mode's run() returns before its teardown (force-ended during the
        intro or countdown).
        """
        if self._outcome_recorded:
            return
        self._outcome_recorded = True
        self._latch_game_outcome()
        attrs = self._game_outcome_attributes()
        if self._game_root_span is not None:
            self._game_root_span.set_attributes(attrs)
        if teardown_span is not None:
            teardown_span.add_event("game.outcome", attributes=attrs)

    def _record_ground_truth(self) -> None:
        """One ``challenge.ground_truth`` event per player on the game root (UC-100 step 6, BR-006).

        Only at a normal end: none for a force-ended game (checked here) or a run that failed (the
        only caller sits after the teardown in run()'s try, so a run that raised never gets here)
        (UC-100 A5). Telemetry never turns a finished game into a game error: a failure to record
        is logged and swallowed.
        """
        if self._game_outcome_attributes()["game.outcome"] == "force_ended":
            return
        try:
            names = {serial: player.name or "" for serial, player in self.players.items()}
            self._ground_truth.record_on(self._game_root_span, names)
        except Exception:
            logger.exception("Recording the ground truth failed; the game ends normally (UC-100 BR-006)")

    def _record_force_ended_outcome_if_unrecorded(self) -> None:
        """For run() overrides: a game force-ended before its teardown still records force_ended."""
        if self.force_ended:
            self._record_game_outcome(None)
