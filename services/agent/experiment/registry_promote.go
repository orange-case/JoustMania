package experiment

// registry_promote.go holds the Registry's PROMOTE phase (#2635 split of registry.go):
// the promotion of a CONCLUDED experiment and the recording of its result.

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/trace"

	"github.com/joustmania/agent/experiment/journal"
)

// promote routes a concluded+promote experiment to the Promoter seam
// (#961/#980), transitioning it to PROMOTING, and journals what the promotion
// achieved (#1701). It returns the result; conclude sets the terminal status. A nil
// Promoter is a promotion that could not act (discarded). A failed promotion never
// blocks teardown.
func (r *Registry) promote(ctx context.Context, id string, jrnl *journal.Journal) PromotionResult {
	span, journalErr, refused, proceed := r.beginPromotion(id)
	if !proceed {
		return refused
	}
	res := r.runPromoter(ctx, id, jrnl, span, journalErr)
	r.journalPromotionResult(id, res)
	return res
}

// refusePromotionLocked is promote's validation step, called under r.mu: only a
// registered, CONCLUDED experiment is promoted.
func refusePromotionLocked(e *entry) (PromotionResult, bool) {
	if e == nil {
		return PromotionResult{Outcome: PromotionDiscarded, Reason: "experiment no longer registered"}, true
	}
	if e.status.IsTerminal() {
		// AbortAll (kill switch, experiments_enabled off) won the race after the
		// conclusion: the promotion does not act.
		return PromotionResult{Outcome: PromotionDiscarded, Reason: "experiment " + string(e.status) + " before promotion"}, true
	}
	if e.status != StatusConcluded {
		// UC-043 BR-006 / BR-007 (#2042): only a concluded experiment is promoted, so
		// one already promoting is never promoted a second time.
		return PromotionResult{Outcome: PromotionDiscarded, Reason: "experiment " + string(e.status) + ", not concluded"}, true
	}
	return PromotionResult{}, false
}

// beginPromotion is promote's state-transition step, one r.mu section: the
// experiment moves CONCLUDED → PROMOTING with its write-ahead decision journaled.
// proceed is false when the promotion must not act; refused is then its result.
func (r *Registry) beginPromotion(id string) (span trace.Span, journalErr error, refused PromotionResult, proceed bool) {
	r.mu.Lock()
	e := r.entries[id]
	if res, refuse := refusePromotionLocked(e); refuse {
		r.mu.Unlock()
		return nil, nil, res, false
	}
	if r.shuttingDown() {
		// UC-043 A20 (#2332): a conclusion that lands after the graceful stop began
		// keeps its sample and verdict but does not promote: no issue, PR or real
		// default write while the agent goes down. Journaled `discarded: shutdown`, so the
		// experiment ends discarded and a restart finds the outcome (A15 step 3).
		res := PromotionResult{Outcome: PromotionDiscarded, Reason: PromotionReasonShutdown}
		promotion := res
		r.appendEvent(e.journal, journal.Event{
			Kind:      journal.KindPromotion,
			Status:    res.Outcome,
			Note:      res.Reason,
			Promotion: &promotion,
		})
		r.mu.Unlock()
		r.log.Info("experiment.promotion_skipped_at_shutdown", "experiment_id", id)
		return nil, nil, res, false
	}
	span, journalErr = r.markPromotingLocked(e)
	r.mu.Unlock()
	return span, journalErr, PromotionResult{}, true
}

// markPromotingLocked sets PROMOTING and journals the write-ahead decision, under
// r.mu. It returns the experiment's root span (the promote span's parent) and the
// journal error that refuses the promotion, if any.
func (r *Registry) markPromotingLocked(e *entry) (trace.Span, error) {
	e.status = StatusPromoting
	// A later outcome (re-watch, kill switch) journaled before this promotion's own
	// event waits until it, so the journal keeps their order.
	e.promotionInFlight = true
	// UC-043 BR-007 (#2042): write-ahead. The promoter acts only once the promoting
	// decision is durable, so a restart never finds a promotion that acted without
	// one (A15). A summary rewrite that fails after the line is durable still counts.
	journalErr := r.appendEventErr(e.journal, journal.Event{
		Kind:   journal.KindDecision,
		Status: string(StatusPromoting),
		Note:   "routing to promoter",
	})
	if errors.Is(journalErr, journal.ErrSummaryWrite) {
		journalErr = nil
	}
	r.emitProgressLocked(e, ProgressTriggerPromoting, "", e.spanCtx) // #2059
	// #1188: re-parent the code_improvement.promote span as the experiment's TERMINAL
	// child. The promote span is started from the ctx passed to Promoter.Promote, so
	// installing the still-live experiment ROOT span on the ctx (an in-process
	// parent-child, the span lives in THIS process) makes the promotion the experiment's
	// terminal child with no change to the promote package. The span is captured under
	// the lock (it is ended later, in teardown, after this returns). A nil span (tracer
	// unwired) leaves ctx unchanged — graceful.
	return e.span, journalErr
}

// runPromoter is promote's action step, outside r.mu: the promoter runs under the
// experiment's root span, unless the write-ahead decision could not be journaled.
func (r *Registry) runPromoter(ctx context.Context, id string, jrnl *journal.Journal, span trace.Span, journalErr error) PromotionResult {
	if span != nil {
		ctx = trace.ContextWithSpan(ctx, span)
	}

	res := PromotionResult{Outcome: PromotionDiscarded, Reason: "no promoter wired"}
	switch {
	case journalErr != nil:
		// Refused: nothing acted. The result is still journaled below, so a restart
		// finds a promotion event and does not report promotion_interrupted.
		res = PromotionResult{Outcome: PromotionDiscarded, Reason: "promotion not journaled: " + journalErr.Error()}
	case r.promo != nil:
		res = r.promo.Promote(ctx, jrnl.CompactView())
	}
	if !promotionActed(res) {
		r.log.Warn("experiment.promote_failed", "experiment_id", id,
			"outcome", res.Outcome, "reason", res.Reason)
	}
	return res
}

// journalPromotionResult is promote's journal step, one r.mu section. Journal the
// result whatever the experiment's status is now: an AbortAll that raced the
// promotion does not undo an issue, PR or real default it produced.
func (r *Registry) journalPromotionResult(id string, res PromotionResult) {
	r.mu.Lock()
	if e := r.entries[id]; e != nil {
		promotion := res
		r.appendEvent(e.journal, journal.Event{
			Kind:      journal.KindPromotion,
			Status:    res.Outcome,
			Note:      res.Reason,
			Promotion: &promotion,
		})
		if res.ForeignOwner != "" {
			e.recommendOwner = res.ForeignOwner
		}
		r.flushLaterPromotionsLocked(e)
	}
	r.mu.Unlock()
}

// flushLaterPromotionsLocked journals the later promotion outcomes RecordPromotion
// held back while e's promotion ran, after its own event. Assumes r.mu is held.
func (r *Registry) flushLaterPromotionsLocked(e *entry) {
	e.promotionInFlight = false
	for _, res := range e.laterPromotions {
		promotion := res
		r.noteRecommendationLocked(e, res)
		r.appendEvent(e.journal, journal.Event{
			Kind:      journal.KindPromotion,
			Status:    res.Outcome,
			Note:      res.Reason,
			Promotion: &promotion,
		})
	}
	e.laterPromotions = nil
}

// warnLegacyRevert logs one WARN when the experiment's last promotion event is a
// hold-key revert an older agent queued (revert_pending): nothing runs it now
// (UC-043 A15 step 3). Reads only the journal, so r.mu may or may not be held.
func (r *Registry) warnLegacyRevert(id string, jrnl *journal.Journal) {
	events, err := jrnl.Events()
	if err != nil {
		return
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != journal.KindPromotion || events[i].Promotion == nil {
			continue
		}
		if events[i].Promotion.RevertPending {
			r.log.Warn("experiment: a queued revert from an older agent was not run; check the flag on the flag service (GET /{flagSetId}/{flagName}): a real overlay of a hold sub-parameter is cleared by the kill switch, and shield.activation has no real scope any more, so its value comes from the stack overlay (UC-043 A15)",
				"experiment_id", id, "flag", jrnl.Intent().FlagKey)
		}
		return
	}
}

// RecordPromotion journals a later outcome of an experiment's promotion (UC-043
// A1, #1704: the kill switch reverted its autonomous real default). It appends a
// promotion event whatever the experiment's status is, and never changes that
// status. An unknown experiment id is ignored (logged).
func (r *Registry) RecordPromotion(id string, res PromotionResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[id]
	if e == nil {
		// A done experiment is not held in memory after a restart (Rehydrate skips
		// it), yet the kill switch journals on it (UC-043 A1).
		if jrnl, err := journal.Load(r.root, id); err == nil {
			promotion := res
			r.appendEvent(jrnl, journal.Event{Kind: journal.KindPromotion, Status: res.Outcome, Note: res.Reason, Promotion: &promotion})
			return
		}
		r.log.Warn("experiment.promotion_unrecorded", "experiment_id", id, "outcome", res.Outcome, "reason", res.Reason)
		return
	}
	if e.promotionInFlight {
		// Its own promotion event is not journaled yet: keep the order (UC-043 A17).
		e.laterPromotions = append(e.laterPromotions, res)
		return
	}
	promotion := res
	r.appendEvent(e.journal, journal.Event{
		Kind:      journal.KindPromotion,
		Status:    res.Outcome,
		Note:      res.Reason,
		Promotion: &promotion,
	})
	r.noteRecommendationLocked(e, res)
}

// noteRecommendationLocked records a refused promotion's foreign owner (UC-043 A19,
// #2276). Assumes r.mu is held.
func (r *Registry) noteRecommendationLocked(e *entry, res PromotionResult) {
	if res.ForeignOwner != "" {
		e.recommendOwner = res.ForeignOwner
	}
}
