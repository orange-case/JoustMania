package experiment

// registry_conclude.go holds the Registry's CONCLUDE phase:
// folding a concluded or released game into the journal and the verdict-driven
// status transition.

import (
	"context"

	"github.com/joustmania/agent/experiment/journal"
)

// ConcludeGame records that a shadow game ended (design §7.1, §8.1): it appends a
// game_concluded journal event with the game's fitness sample (folding it into
// the matching arm's Welford accumulator), then calls the CohortVerdict seam to
// recompute the interim verdict, records it, and transitions the experiment to
// CONCLUDED when the verdict warrants (promote/discard, or an under-powered
// timeout the verdict signals). It looks the game up in the (game_id →
// experiment_id, arm) map the registry owns. An unknown game_id is ignored (it
// was not one of ours). On CONCLUDED the experiment is routed to promotion (if the
// verdict promotes) and torn down. Returns the resulting status.
func (r *Registry) ConcludeGame(ctx context.Context, gameID string, fitness float64, durationS float64) (Status, error) {
	r.mu.Lock()
	id, arm, e := r.findGameLocked(gameID)
	if e == nil {
		r.mu.Unlock()
		return "", nil // not one of ours; ignore.
	}
	jrnl := r.foldConcludedGameLocked(e, gameID, arm, fitness, durationS)
	r.mu.Unlock()

	verdict, haveVerdict := r.recordInterimVerdict(jrnl)
	r.emitConcludedProgress(id)

	r.log.Info("experiment.game_concluded",
		"experiment_id", id, "game_id", gameID, "arm", arm, "fitness", fitness)

	// Decide whether to conclude the experiment. A promote/discard verdict
	// concludes it; inconclusive keeps it running. (An under-powered timeout is the
	// verdict seam's call — it returns promote/discard once it is willing to.)
	if haveVerdict && verdict.Outcome != OutcomeInconclusive {
		return r.conclude(ctx, id, verdict)
	}

	// UC-046 A6: a pattern experiment whose archetypes are all used concludes once
	// its last game has ended.
	if st, done := r.concludeIfPatternExhausted(ctx, id); done {
		return st, nil
	}
	return StatusRunning, nil
}

// foldConcludedGameLocked unbinds the concluded game from e and appends its
// game_concluded event (folding the fitness sample into the arm), and returns e's
// journal. The caller holds r.mu; it does not lock.
func (r *Registry) foldConcludedGameLocked(e *entry, gameID, arm string, fitness, durationS float64) *journal.Journal {
	delete(e.games, gameID)
	// Recover the game's Common-Random-Numbers pair index (#1003) recorded at bind so
	// the game_concluded event carries it — the journal's paired-difference fold
	// (#1004) matches the two games of a pair by it. A bound game always has an entry;
	// a missing one (legacy/rehydrated in-flight, which Rehydrate never restores)
	// simply leaves PairIndex nil and the conclusion folds only into the two-arm path.
	var pairIdx *int
	if pi, ok := e.gamePairs[gameID]; ok {
		p := pi
		pairIdx = &p
		delete(e.gamePairs, gameID)
	}
	// Drop any release tombstone for this id: a bound game that concludes normally
	// can never have one (bindGame consumes it before binding), but clear it
	// defensively so an out-of-order unfound-release cannot linger.
	delete(r.releaseTombstones, gameID)
	f := fitness
	r.appendEvent(e.journal, journal.Event{
		Kind:      journal.KindGameConcluded,
		GameID:    gameID,
		Arm:       arm,
		PairIndex: pairIdx,
		Fitness:   &f,
		DurationS: durationS,
	})
	return e.journal
}

// recordInterimVerdict recomputes the interim verdict from the seam (outside the
// registry lock — the journal has its own lock). The verdict seam consumes the full
// Summary (raw per-arm Welford: Count/Mean/M2) so #979 can compute an unbiased /
// Welch estimator — CompactView would drop the raw M2. It records the verdict as an
// interim_verdict event.
func (r *Registry) recordInterimVerdict(jrnl *journal.Journal) (journal.Verdict, bool) {
	var verdict journal.Verdict
	haveVerdict := false
	if r.verdict != nil {
		verdict, haveVerdict = r.verdict.Evaluate(jrnl.Summary())
		if haveVerdict {
			r.appendEvent(jrnl, journal.Event{
				Kind:    journal.KindInterimVerdict,
				Verdict: &verdict,
			})
		}
	}
	return verdict, haveVerdict
}

// emitConcludedProgress emits the game_concluded progress snapshot (#2059): after
// the fold AND the interim verdict, so the snapshot counts exactly the folded games
// and its verdict.* is this game's. An experiment torn down in between (AbortAll)
// already emitted its terminal snapshot. It takes and releases r.mu.
func (r *Registry) emitConcludedProgress(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.entries[id]; e != nil && !e.status.IsTerminal() {
		r.emitProgressLocked(e, ProgressTriggerGameConcluded, "", e.spanCtx)
	}
}

// ReleaseGame frees the in-flight slot held by a shadow game that ended WITHOUT
// producing a usable fitness sample (#1014): a game that terminated in
// game_error, was force-ended on timeout, or was aborted. Unlike ConcludeGame it
// does NOT fold a fitness sample, recompute the verdict, or advance the
// termination budget — a transient game failure must not pollute the cohort with
// a fabricated 0-fitness sample (epic #982: fitness is observed, never assumed).
// It simply drops the (game_id → arm) binding so the slot returns to the cap,
// records a non-counting release event for the audit trail, and lets the next
// AllocateAndSpawn refill it. This is the BACKSTOP that guarantees the
// effective_concurrency=1 loop can never deadlock on an errored game even if the
// telemetry-driven ConcludeGame path never fires for it.
//
// It is IDEMPOTENT and SAFE to call alongside ConcludeGame: an unknown game_id
// (already concluded/released, or not one of ours) is ignored — and never records a
// lingering tombstone unless a bind is genuinely pending (spawnsInFlight > 0), so the
// conclude-then-release and double-release orderings leave no orphan. Returns whether
// a slot was actually freed.
func (r *Registry) ReleaseGame(gameID, reason string) bool {
	r.mu.Lock()
	id, arm, e := r.findGameLocked(gameID)
	if e == nil {
		// The game_id is not bound. This is EITHER the bind-vs-fast-terminal race
		// (#1014) — the terminal callback fired before AllocateAndSpawn could bind the
		// real game_id, so only the reservation placeholder is in e.games — OR a
		// genuinely stale call (already concluded/released, or not one of ours). We tell
		// them apart by spawnsInFlight: the race can ONLY happen while a spawn is in
		// flight (a bind is still coming). So tombstone ONLY then; bindGame will detect
		// it and free the reservation WITHOUT recording the id (closing the leak), and
		// spawnResolvedLocked sweeps any survivor when the last spawn resolves. When NO
		// spawn is in flight an unfound release is provably stale — recording a tombstone
		// would just leak an entry no bind will ever consume, so we skip it.
		if r.spawnsInFlight > 0 {
			if _, dup := r.releaseTombstones[gameID]; !dup {
				r.releaseTombstones[gameID] = reason
			}
		}
		r.mu.Unlock()
		return false // no bound slot freed (the bind, if any, frees it).
	}
	delete(e.games, gameID)
	// Recover the pair index so the journal can drop a parked pending half whose
	// partner is now gone (#1004 carried-over item 1): a released game that was the
	// first-concluded half of an open pair must NEVER fold a fabricated difference.
	var pairIdx *int
	if pi, ok := e.gamePairs[gameID]; ok {
		p := pi
		pairIdx = &p
		delete(e.gamePairs, gameID)
	}
	// A tombstone can never coexist with a bound game (bindGame consumes it before
	// binding), but drop defensively so a prior unfound-release for a since-rebound id
	// cannot linger.
	delete(r.releaseTombstones, gameID)
	r.appendEvent(e.journal, journal.Event{
		Kind:      journal.KindGameReleased,
		GameID:    gameID,
		Arm:       arm,
		PairIndex: pairIdx,
		Note:      reason,
	})
	// UC-046 BR-015: a pattern arm released without a sample reruns its pair.
	if pairIdx != nil {
		r.rerunPairLocked(e, *pairIdx, reason)
	}
	r.emitProgressLocked(e, ProgressTriggerGameReleased, "", e.spanCtx) // #2059
	r.mu.Unlock()

	r.log.Info("experiment.game_released",
		"experiment_id", id, "game_id", gameID, "arm", arm, "reason", reason)
	// UC-046 A6: a released last game of an exhausted pattern experiment concludes it.
	r.concludeIfPatternExhausted(context.Background(), id)
	return true
}

// conclude transitions an experiment to CONCLUDED on a final verdict, routes a
// promote verdict to the Promoter (PROMOTING → DONE when the promotion acted,
// PROMOTING → DISCARDED when it did not, #1701), discards otherwise, and tears down
// the targeting + frees capacity. It is the verdict→terminal path.
//
// It concludes an experiment at most once (UC-043 BR-006, #2034): the game-end
// handler and the allocation pass can both decide to conclude the same
// experiment, so only a live (proposed / running) experiment is concluded; a
// later caller gets the current status (concluded, promoting or terminal) back.
func (r *Registry) conclude(ctx context.Context, id string, verdict journal.Verdict) (Status, error) {
	jrnl, flagKey, current, won := r.markConcluded(id, &verdict)
	if !won {
		return current, nil
	}

	r.log.Info("experiment.concluded", "experiment_id", id, "outcome", verdict.Outcome)

	var promotion *PromotionResult
	if verdict.Outcome == CohortOutcomePromote {
		res := r.promote(ctx, id, jrnl)
		promotion = &res
	}

	final := r.concludedFinalStatus(id, promotion)
	r.teardown(ctx, id, flagKey, final, concludeNote(verdict, promotion))
	// teardown never overwrites a terminal status, so report the one that holds.
	if st, ok := r.Status(id); ok {
		return st, nil
	}
	return final, nil
}

// markConcluded is conclude's state-transition step, one r.mu section: a live
// experiment becomes CONCLUDED and its final verdict (with the pattern timeout
// summary appended to verdict.Reason) is journaled. won is false when another
// caller already ended it; current is then the status it holds.
func (r *Registry) markConcluded(id string, verdict *journal.Verdict) (jrnl *journal.Journal, flagKey string, current Status, won bool) {
	r.mu.Lock()
	e := r.entries[id]
	if e == nil || !e.status.IsLive() {
		// Copy the status under the lock: the winning caller keeps writing it
		// (promotion, teardown) after we unlock.
		if e != nil {
			current = e.status
		}
		r.mu.Unlock()
		return nil, "", current, false
	}
	e.status = StatusConcluded
	// UC-046 BR-013: report pattern timeouts per arm with the conclusion.
	if summary := e.timeoutSummaryLocked(); summary != "" {
		if verdict.Reason != "" {
			verdict.Reason += "; "
		}
		verdict.Reason += summary
	}
	// The final verdict rides on the concluded decision (#2042): the journal
	// summary, the promoter and a restart (UC-043 A15) read the verdict the
	// experiment concluded on, also after a give-up or pattern-exhausted conclusion.
	finalVerdict := *verdict
	r.appendEvent(e.journal, journal.Event{
		Kind:    journal.KindDecision,
		Status:  string(StatusConcluded),
		Note:    "target N reached / verdict " + verdict.Outcome,
		Verdict: &finalVerdict,
	})
	r.emitProgressLocked(e, ProgressTriggerConcluded, "", e.spanCtx) // #2059
	jrnl = e.journal
	flagKey = e.flagKey
	r.mu.Unlock()
	return jrnl, flagKey, StatusConcluded, true
}

// concludedFinalStatus is conclude's terminal-status step, one r.mu section.
// Terminal: DONE when the promotion acted, DISCARDED otherwise (#1701). A
// terminal status set while the promotion ran (AbortAll: kill switch or
// experiments_enabled off) is kept, never overwritten.
func (r *Registry) concludedFinalStatus(id string, promotion *PromotionResult) Status {
	r.mu.Lock()
	e := r.entries[id]
	final := StatusDiscarded
	switch {
	case e != nil && e.status.IsTerminal():
		final = e.status
	case promotion != nil && promotionActed(*promotion):
		final = StatusDone
	}
	r.mu.Unlock()
	return final
}

// concludeNote is the teardown note conclude journals: the verdict outcome and
// reason, plus the promotion outcome when one ran.
func concludeNote(verdict journal.Verdict, promotion *PromotionResult) string {
	note := "experiment concluded: " + verdict.Outcome
	if verdict.Reason != "" {
		note += " (" + verdict.Reason + ")"
	}
	if promotion != nil {
		note += "; promotion " + promotion.Outcome
		if promotion.Reason != "" {
			note += ": " + promotion.Reason
		}
	}
	return note
}
