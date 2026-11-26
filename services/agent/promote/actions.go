package promote

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/joustmania/agent/experiment"
)

// actions.go holds the per-mode promotion logic. Each helper mutates the passed
// *Result (the span attributes are stamped by record()). Every consequential
// effect routes through an injected seam, so these methods are exercised in unit
// tests with recording fakes — ZERO network, ZERO real-default
// mutation (the SAFETY RAIL, see promote.go).

// promoteIssue opens a GitHub issue describing the hypothesis + implicated flag.
// NO code/flag is applied (#936: "issue — open an issue describing the hypothesis
// and the implicated file; no code applied"). The issue is ALWAYS opened through
// the GitHubClient seam: with the real client disabled (the default), that seam is
// the inert recording no-op, so this degrades to a RECORDED no-op (empty URL +
// reason), never a silent or accidental real action.
func (pr *Promoter) promoteIssue(ctx context.Context, p experiment.Proposal, ev Evidence, res *Result) {
	req := IssueReq{
		Title:  fmt.Sprintf("Flag experiment: %s", p.FlagKey),
		Body:   renderIssueBody(p, ev),
		Labels: []string{"agent", "code-improvement"},
	}
	url, err := pr.github.OpenIssue(ctx, req)
	if err != nil {
		// Fail-safe: a failed/declined open (incl. the no-op fake's recorded decline)
		// is a recorded no-op, never a crash and never a different action.
		res.Outcome = OutcomeDiscarded
		res.Reason = fmt.Sprintf("open issue: %v", err)
		return
	}
	res.Outcome = OutcomeApplied
	res.URL = url
}

// ReasonNotRealPromotable discards an autonomous promotion of shield.activation:
// instant and trigger games read it per grant, so a real write could change a
// running game; it has no real scope (UC-043 A17, flags.md §13).
const ReasonNotRealPromotable = "not_real_promotable"

// promoteAutonomous applies the validated change to the REAL default, watches real
// games, and reverts on degradation (#936: "autonomous — apply, watch, revert per
// M7-7"). This is the consequential real-player-affecting path, so it is the most
// heavily guarded:
//
//   - ENABLED: unless `enabled` is act (cfg.Enabled), the agent NEVER mutates the
//     real default — degrades to a recorded no-op. autonomous is default-OFF AND
//     behind `enabled` = act (the safety rail).
//   - SEPARATE WRITER: the real-default change goes through the injected
//     RealDefaultWriter — explicitly NOT the shadow experiment.Writer (which is
//     structurally shadow-only). A nil RealDefaultWriter (real path not wired) → a
//     recorded no-op; the shadow Writer is never reused to sneak a real change.
//   - WATCH + REVERT: after applying, the Watcher observes real-game fitness for a
//     window; on degradation vs the pre-promotion baseline the RealDefaultReverter
//     rolls the real default back (the OutcomeRevert path, reusing M7-7's
//     watch/revert seams).
//   - FAIL CLOSED WITHOUT A WATCH (#1016): a nil Watcher is a HARD STOP, NOT an
//     apply-without-watch. An unguarded autonomous promotion (apply to real players
//     with no auto-revert) is the unacceptable state, so we REFUSE to apply when no
//     Watcher is wired — checked BEFORE the real-default write, so nothing is ever
//     written without a connected safety net.
func (pr *Promoter) promoteAutonomous(ctx context.Context, p experiment.Proposal, cfg Config, ev Evidence, res *Result) {
	if p.FlagKey == experiment.HoldActivationKey {
		res.Outcome = OutcomeDiscarded
		res.Reason = ReasonNotRealPromotable
		return
	}
	if !cfg.Enabled {
		res.Outcome = OutcomeDiscarded
		res.Reason = "autonomous requested but agent `enabled` is not act; real default not changed"
		return
	}
	if pr.realDef == nil {
		res.Outcome = OutcomeDiscarded
		res.Reason = "autonomous requested but no real-default writer wired; real default not changed"
		return
	}
	// FAIL CLOSED (#1016): no safety net → refuse to apply. Checked BEFORE the write
	// so an autonomous promotion can NEVER touch the real default without a
	// watch→auto-revert loop connected behind it.
	if pr.watcher == nil {
		res.Outcome = OutcomeDiscarded
		res.Reason = "autonomous requested but no watch/auto-revert wired; refusing to apply to real default without a safety net (#1016)"
		return
	}

	// SUBSTRATE VALIDITY BAR (UC-043 A23, #2541): only an objective an operator
	// attested as validated for the running release may change a real default.
	// Checked before the cooldown; writeTracked checks it again as the backstop.
	if err := pr.substrateRefusal(ev.Objective); err != nil {
		res.Outcome = OutcomeDiscarded
		res.Reason = err.Error()
		return
	}

	// COOLDOWN (#1065 item 3): if this flag was reverted recently, refuse to re-apply
	// until the cooldown elapses. This is checked BEFORE the write so a thrash loop
	// (err→revert→re-promote→err→revert) cannot run hot — a reverted flag is parked
	// for the cooldown rather than immediately re-promoted into the same failure.
	if remaining, cooling := pr.cooldownRemaining(p.FlagKey); cooling {
		res.Outcome = OutcomeDiscarded
		res.Reason = fmt.Sprintf("autonomous re-promotion of %q suppressed: %s remaining in post-revert cooldown (#1065)", p.FlagKey, remaining.Round(time.Second))
		return
	}

	// The one sanctioned real-player-affecting write, through the gated seam. It is
	// serialised with the kill switch (#1704): a kill-switch tick since this
	// promotion's config was resolved refuses the write, and a tick that starts
	// after it finds the written overlay.
	entry, err := pr.writeTracked(ctx, p, ev, cfg)
	if errors.Is(err, ErrRealOverlayOwnedByAnotherWriter) {
		// UC-043 A19 (#2260): another writer owns the real overlay; the promotion
		// is not made and not retried. No cooldown: nothing was reverted.
		res.Outcome = OutcomeDiscarded
		res.Reason = foreignReason(err)
		res.ForeignOwner = foreignOwner(err)
		return
	}
	if err != nil {
		res.Outcome = OutcomeDiscarded
		res.Reason = err.Error()
		return
	}
	res.Outcome = OutcomeApplied
	pr.applyAndWatch(ctx, p, ev, entry, res)
}

// applyAndWatch watches an applied real-default write and acts on the verdict
// (revert, re-watch or keep). It owns the entry's endSync: when the kill switch
// reverted the write while this ran, res reports that revert (#1704).
func (pr *Promoter) applyAndWatch(ctx context.Context, p experiment.Proposal, ev Evidence, entry *appliedPromotion, res *Result) {
	defer func() {
		// The kill switch reverted it while this call ran: report that here, once.
		if pr.endSync(entry) && res.Outcome == OutcomeApplied {
			res.Outcome = OutcomeReverted
			res.Reason = KillSwitchReason
		}
	}()
	// The watch counts only real games that started after the write (#1702).
	appliedAt := pr.clock()

	// Watch real-game fitness for the promoted flag's objective and act on the verdict.
	// Use the tri-state Observe when the watcher supports it so an INSUFFICIENT-games
	// result schedules a re-watch (#1065 item 4) instead of silently keeping an
	// unverified change; otherwise fall back to the boolean Degraded.
	verdict, err := pr.observe(ctx, p.FlagKey, ev.FitnessBefore, appliedAt)
	switch {
	case err != nil:
		// Cannot CONFIRM healthy — fail safe by reverting rather than leaving a
		// real-player change unverified.
		pr.revertReal(ctx, p, entry, res, err, false)
	case verdict == VerdictDegraded:
		pr.revertReal(ctx, p, entry, res, nil, false)
	case verdict == VerdictInsufficient:
		// Apply stands, but unverified. Schedule a background re-watch (if wired) that
		// reverts later if degradation is confirmed as more real games complete.
		pr.scheduleRewatch(ctx, p, ev, appliedAt, entry)
	default: // VerdictHealthy — confirmed keep, nothing to do.
	}
}

// observe runs the watcher and returns a tri-state verdict. When the wired Watcher
// implements the richer Observer (the production fitnessWatcher does), its Observe is
// used so INSUFFICIENT evidence is distinguishable from a confirmed healthy keep.
// Otherwise it adapts the boolean Degraded (degraded→VerdictDegraded, else
// VerdictHealthy — a boolean-only watcher cannot signal insufficiency, so no re-watch
// is scheduled, matching the prior behavior).
func (pr *Promoter) observe(ctx context.Context, flagKey string, baseline float64, since time.Time) (WatchVerdict, error) {
	if obs, ok := pr.watcher.(Observer); ok {
		return obs.Observe(ctx, flagKey, baseline, since)
	}
	degraded, err := pr.watcher.Degraded(ctx, flagKey, baseline, since)
	if err != nil {
		return VerdictInsufficient, err
	}
	if degraded {
		return VerdictDegraded, nil
	}
	return VerdictHealthy, nil
}

// Observer is the tri-state watch interface (#1065 item 4). The production
// fitnessWatcher implements it; the Promoter type-asserts for it so a boolean-only
// Watcher still works (it just cannot trigger a re-watch).
type Observer interface {
	Observe(ctx context.Context, flagKey string, baseline float64, since time.Time) (WatchVerdict, error)
}

// scheduleRewatch hands an insufficient-games apply to the background re-watcher
// (#1065 item 4) so it is re-observed as more real games complete. nil rewatcher →
// the apply stands unverified (prior behavior). The revert closure performs a gated
// fail-safe revert with its own Result and journals it on the experiment through
// the re-watch journal (journalRewatch, UC-043 step 7, #1883).
func (pr *Promoter) scheduleRewatch(ctx context.Context, p experiment.Proposal, ev Evidence, since time.Time, entry *appliedPromotion) {
	if pr.rewatcher == nil {
		pr.log.Info("autonomous watch: insufficient real games and no re-watcher wired; apply stands unverified",
			"flag", p.FlagKey)
		return
	}
	pr.log.Info("autonomous watch: insufficient real games; scheduling background re-watch (#1065)",
		"flag", p.FlagKey)
	revert := func(rctx context.Context) {
		rres := Result{Outcome: OutcomeApplied}
		claim := pr.revertReal(rctx, p, entry, &rres, nil, true)
		// Log and journal after revertReal released killMu: the journal sink takes
		// the registry lock and must never run under killMu (#1893).
		if claim == claimOwned {
			pr.log.Warn("autonomous re-watch confirmed degradation; reverted real default (#1065)",
				"flag", p.FlagKey, "outcome", string(rres.Outcome), "reason", rres.Reason)
			pr.journalRewatch(p.ExperimentID, rres)
		}
	}
	// The re-watch outlives this call; the kill switch cancels it (#1704).
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if !pr.setCancel(entry, cancel) {
		// The kill switch already reverted this promotion: nothing to watch.
		cancel()
		return
	}
	pr.rewatcher.Schedule(wctx, p.FlagKey, ev.FitnessBefore, since, pr.watcher, revert)
}

// cooldownRemaining reports the time left in flagKey's post-revert cooldown, and
// whether it is still cooling. Outside the cooldown (or never reverted) it returns
// (0, false). Guarded by mu.
func (pr *Promoter) cooldownRemaining(flagKey string) (time.Duration, bool) {
	cooldown := pr.effectiveCooldown()
	if cooldown <= 0 {
		return 0, false
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	last, ok := pr.lastRevert[flagKey]
	if !ok {
		return 0, false
	}
	elapsed := pr.clock().Sub(last)
	if elapsed >= cooldown {
		return 0, false
	}
	return cooldown - elapsed, true
}

// markReverted stamps flagKey's last-revert time so the cooldown gate can suppress an
// immediate re-promotion. Guarded by mu.
func (pr *Promoter) markReverted(flagKey string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.lastRevert == nil {
		pr.lastRevert = map[string]time.Time{}
	}
	pr.lastRevert[flagKey] = pr.clock()
}

// clock returns the (possibly faked) time source; defaults to time.Now if unset.
func (pr *Promoter) clock() time.Time {
	if pr.now != nil {
		return pr.now()
	}
	return time.Now()
}

// watchRevert is the revert of a watch or re-watch. A hold key reverts at once:
// a running game keeps its latched values (UC-048 BR-011).
func (pr *Promoter) watchRevert(ctx context.Context, flagKey string) error {
	err := pr.reverter.RevertRealDefault(ctx, flagKey)
	if errors.Is(err, ErrSupersededByAnotherWriter) {
		return nil // dropped: the other writer owns the overlay, the revert is done
	}
	return err
}

// revertReal rolls the autonomous real-default change back on a degradation (or an
// unconfirmable watch). A nil RealDefaultReverter (no rollback wired) records the
// degradation but cannot undo it — logged + reasoned so it is visible, never
// silent. On a successful revert the outcome becomes OutcomeReverted.
//
// entry is the tracked applied promotion (#1704): the revert runs only when this
// call claims it; when the kill switch already reverted it, nothing is written and
// the result records the kill-switch revert. It returns the claim, so the
// re-watch journals only a revert it owned (#1883).
//
// It holds killMu from the claim through the revert attempt and its retrack, so a
// kill-switch tick never sees a claimed overlay before its revert has finished: it
// finds either the retracked entry (with its experiment id) or no overlay (#1893).
// The production writer already holds its own lock across the same flag-service
// read and delete, so a tick waited for an in-flight revert before as well; the
// lock adds no latency there. Never call revertReal while holding killMu
// (writeTracked, RevertOnKillSwitch): Go mutexes are not re-entrant.
// rewatch marks a re-watch revert (its journal reason prefix).
func (pr *Promoter) revertReal(ctx context.Context, p experiment.Proposal, entry *appliedPromotion, res *Result, watchErr error, rewatch bool) claimResult {
	pr.killMu.Lock()
	defer pr.killMu.Unlock()
	claim := pr.claimApplied(entry)
	switch claim {
	case claimKilled:
		// The kill switch reverted it while this promotion was still running; the
		// kill switch left the journaling to this result (#1704).
		res.Outcome = OutcomeReverted
		res.Reason = KillSwitchReason
		return claim
	case claimSuperseded:
		// A newer promotion of the flag replaced this one: its default is not ours to
		// revert, so nothing is written and the outcome stays applied.
		res.Reason = "superseded by a newer promotion of the flag; not reverted"
		return claim
	}
	reason := "real-game fitness degraded after autonomous change"
	if watchErr != nil {
		reason = fmt.Sprintf("could not confirm real-game fitness healthy (%v); reverting fail-safe", watchErr)
	}
	if pr.reverter == nil {
		// Applied but cannot roll back — record the degradation so it is actionable.
		res.Reason = reason + "; no real-default reverter wired"
		return claim
	}
	if err := pr.watchRevert(ctx, p.FlagKey); err != nil {
		res.Reason = fmt.Sprintf("%s; revert failed: %v", reason, err)
		// Still applied: the kill switch may revert it later (#1704).
		if entry != nil {
			pr.retrack(entry)
		}
		return claim
	}
	// Start the post-revert cooldown so a re-promotion of this flag cannot immediately
	// re-trigger the same failure (#1065 item 3). Marked only on a SUCCESSFUL revert —
	// a failed revert (above) leaves no cooldown, surfacing the unrolled-back state.
	pr.markReverted(p.FlagKey)
	res.Outcome = OutcomeReverted
	res.Reason = reason
	return claim
}
