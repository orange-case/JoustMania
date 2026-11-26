package experiment

// registry_rehydrate.go holds the Registry's REHYDRATE phase (#2635 split of
// registry.go): rebuilding the in-memory view from the durable journals on startup.

import (
	"context"
	"errors"
	"fmt"

	"github.com/joustmania/agent/experiment/journal"
	"github.com/joustmania/agent/patterngame"
)

// Rehydrate rebuilds the live registry from the durable journal on startup (the
// #831 fix, design §7.2): it scans the journal root for experiment dirs, Loads
// each, and reconstructs an entry from its compact view (status + arms). It does
// NOT re-load the in-flight game map — a restart loses no DURABLE state (concluded
// games are folded into the journal summary), and any game that was in-flight at
// crash time is no longer running, so the registry starts each rehydrated
// experiment with an empty in-flight set and refills capacity on the next
// AllocateAndSpawn. Terminal experiments are skipped (their work is done).
//
// CRN cursors (#1003): like armCursor, pairCursor restarts at 0 on rehydrate —
// the in-flight set is empty after a crash, so the registry resumes allocating
// pairs from index 0. A pairIndex may therefore repeat across a restart boundary
// (same seed, but a genuinely NEW pair of games); the paired-difference verdict
// (#1004) matches the two games of a pair by the (pair_index + the game_ids
// assigned to it) recorded contiguously in the journal, so a post-restart repeat
// is a distinct pair, not an aliasing hazard. This mirrors the accepted armCursor
// restart semantics and keeps Rehydrate free of an event replay.
func (r *Registry) Rehydrate(ids []string) error {
	r.mu.Lock()
	var lost, unstarted, unfinished []string
	var errs []error
	defer func() {
		r.mu.Unlock()
		// UC-046 BR-015: a pattern experiment whose pin cannot be restored is discarded.
		for _, id := range lost {
			r.teardown(context.Background(), id, r.flagKeyOf(id), StatusDiscarded, patterngame.DiscardPinLost)
		}
		// UC-043 A8 (#1700): a restored PROPOSED experiment never started (its start
		// failed, or the agent stopped between declare and start) and nothing starts
		// it again, so abort it rather than let it hold its flag and a budget slot.
		for _, id := range unstarted {
			r.teardown(context.Background(), id, r.flagKeyOf(id), StatusAborted, AbortReasonStartFailed)
		}
		// UC-043 A15 (#2042): a restored CONCLUDED or PROMOTING experiment is ended
		// without a promotion; nothing else would ever finish it.
		for _, id := range unfinished {
			r.finishRestored(id)
		}
	}()
	for _, id := range ids {
		// #1731: an id this registry already holds was declared by this process
		// (Declare registers it under r.mu together with its journal), never a
		// leftover of a restart: leave it to its declarer.
		if _, known := r.entries[id]; known {
			continue
		}
		jrnl, err := journal.Load(r.root, id)
		if err != nil {
			// UC-043 A15 (#2042): one bad journal does not stop the restore of the
			// others; every failure is logged and returned.
			err = fmt.Errorf("registry: rehydrate %q: %w", id, err)
			r.log.Error("experiment.rehydrate_failed", "experiment_id", id, "error", err)
			errs = append(errs, err)
			continue
		}
		view := jrnl.CompactView()
		st := Status(view.Status)
		if st == "" {
			st = StatusRunning
		}
		if st.IsTerminal() {
			r.warnLegacyRevert(id, jrnl)
			continue
		}
		e := &entry{
			id:        id,
			journal:   jrnl,
			status:    st,
			flagKey:   view.Intent.FlagKey,
			arms:      append([]string(nil), view.Intent.Arms...),
			games:     make(map[string]string),
			gamePairs: make(map[string]int),
		}
		if events, evErr := jrnl.Events(); evErr == nil {
			e.recommendOwner = lastForeignOwner(events) // UC-043 A19
		}
		// #1188 RESTART DISCONTINUITY: a rehydrated experiment gets a BRAND-NEW root
		// span with a fresh trace_id — the SpanContext is NOT persisted across the
		// restart (the pre-restart trace is already closed and un-appendable). The new
		// span outlives this rehydrated entry exactly like a freshly-Started one and is
		// ended in teardown. Rehydrate is the only place a RUNNING entry is created
		// WITHOUT going through Start, so the span must be started here too. A rehydrated
		// PROPOSED entry also gets one; if Start later runs, startSpanLocked is a no-op
		// (idempotent), so no double span.
		r.startSpanLocked(e, jrnl.Intent())
		r.entries[id] = e
		if st == StatusProposed {
			unstarted = append(unstarted, id)
			continue
		}
		if st == StatusConcluded || st == StatusPromoting {
			unfinished = append(unfinished, id)
			continue
		}
		if r.patterns != nil && !r.restorePatternLocked(e) {
			lost = append(lost, id)
			continue
		}
		// #2059: once per restored experiment that stays live (the torn-down ones
		// above emit their terminal snapshot instead).
		r.emitProgressLocked(e, ProgressTriggerRehydrate, "", e.spanCtx)
	}
	return errors.Join(errs...)
}

// finishRestored ends an experiment restored as CONCLUDED or PROMOTING (UC-043
// A15, #2042) without ever calling the promoter: its in-flight games are lost and
// its evidence predates the restart. It needs only the local journal and the
// targeting strip (teardown), so it is safe inside Rehydrate.
//   - a promotion event after the last concluded decision (any status): the
//     promotion ran, so end done / discarded from it (BR-006: once);
//   - promoting without one: unknown outcome, abort promotion_interrupted + WARN;
//   - concluded without one: discarded, "concluded before restart: <outcome>; not promoted".
func (r *Registry) finishRestored(id string) {
	r.mu.Lock()
	e := r.entries[id]
	if e == nil || (e.status != StatusConcluded && e.status != StatusPromoting) {
		r.mu.Unlock()
		return
	}
	jrnl, flagKey, st := e.journal, e.flagKey, e.status
	r.mu.Unlock()

	ctx := context.Background()
	if res, ok := r.journaledPromotion(id, jrnl); ok {
		r.warnLegacyRevert(id, jrnl)
		final := StatusDiscarded
		if promotionActed(res) {
			final = StatusDone
		}
		note := "promotion " + res.Outcome
		if res.Reason != "" {
			note += ": " + res.Reason
		}
		r.teardown(ctx, id, flagKey, final, note+"; finished after an agent restart")
		return
	}
	if st == StatusPromoting {
		r.log.Warn("experiment.promotion_interrupted", "experiment_id", id, "flag_key", flagKey)
		r.teardown(ctx, id, flagKey, StatusAborted, AbortReasonPromotionInterrupted)
		return
	}
	outcome := "unknown"
	if v := jrnl.CompactView().Verdict; v != nil && v.Outcome != "" {
		outcome = v.Outcome
	}
	r.teardown(ctx, id, flagKey, StatusDiscarded, "concluded before restart: "+outcome+"; not promoted")
}

// journaledPromotion returns the first promotion event after the experiment's
// last concluded decision: the result of its one promotion (later ones are
// kill-switch or re-watch reverts of it). An unreadable log reports none.
func (r *Registry) journaledPromotion(id string, jrnl *journal.Journal) (PromotionResult, bool) {
	evs, err := jrnl.Events()
	if err != nil {
		r.log.Warn("experiment.rehydrate_events_unreadable", "experiment_id", id, "error", err)
		return PromotionResult{}, false
	}
	start := -1
	for i, ev := range evs {
		if ev.Kind == journal.KindDecision && ev.Status == string(StatusConcluded) {
			start = i
		}
	}
	if start < 0 {
		return PromotionResult{}, false
	}
	for _, ev := range evs[start+1:] {
		if ev.Kind != journal.KindPromotion {
			continue
		}
		if ev.Promotion != nil {
			return *ev.Promotion, true
		}
		return PromotionResult{Outcome: ev.Status, Reason: ev.Note}, true
	}
	return PromotionResult{}, false
}
