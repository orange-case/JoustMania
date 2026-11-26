package promote

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/joustmania/agent/experiment"
)

// killswitch.go is the kill-switch revert of autonomous promotions (UC-043 A1,
// UC-045 step 5, #1704): while `enabled` is off, every experiment tick removes each
// real-default overlay a promotion wrote and reports what it reverted so the
// caller journals it. It is the agent's only revert: otherwise a person reverts.
//
// STATELESS: the overlays to revert are read from the flag service (the real
// overlays whose writer is "promotion"), not only from memory, so a promotion
// written before an agent restart is reverted too. The in-memory list only adds
// what the flag service cannot tell: the experiment id (for the journal).
//
// SERIALISED WITH THE WRITE: the autonomous write and the kill-switch revert hold
// one mutex (killMu), and each kill-switch tick bumps an epoch. A promotion whose
// config was resolved before a tick refuses to write after it, and a tick that
// runs after a write finds the overlay. So once a tick has run, no promotion that
// predates it writes a real default again, and the tick's revert is the last
// real-default write of every promotion it reverted. Lock order: killMu, then mu
// (or the writer's lock); nothing under killMu journals.

// KillSwitchReason is the promotion reason a kill-switch revert journals.
const KillSwitchReason = "kill_switch"

// KillSwitchRevert is what RevertOnKillSwitch did for one promoted flag: the
// experiment (empty when this agent run did not write it, e.g. before a restart),
// the flag, and the outcome (reverted, or applied when the revert failed) with its
// reason. A tracked promotion's failed revert is returned once; later failures of
// it are retried without being returned (#1753).
type KillSwitchRevert struct {
	ExperimentID string
	FlagKey      string
	Outcome      Outcome
	Reason       string
}

// PromotionOverlayLister lists the flags that carry a real-default overlay written
// by a promotion. The production RealDefaultReverter implements it (a flag-service
// read); a reverter without it limits the kill switch to the promotions in memory.
type PromotionOverlayLister interface {
	PromotedFlags(ctx context.Context) ([]string, error)
}

// errKillSwitched refuses a write whose config predates a kill-switch tick.
var errKillSwitched = errors.New("autonomous write refused: the kill switch reverted promotions after this promotion's config was read (#1704)")

// KillSwitchEpoch returns the number of kill-switch ticks so far. A caller that
// resolves a promotion Config reads it first (ExperimentPromoter does), so a tick
// between that read and the write refuses the write.
func (pr *Promoter) KillSwitchEpoch() uint64 {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.killEpoch
}

// writeTracked performs the real-default write under killMu, refuses it when a
// kill-switch tick ran since cfg was resolved, and tracks the applied promotion
// (marked as still inside its Promote call until endSync). It writes
// ev.RealDefaultValue, and only for an objective that meets the substrate validity
// bar (UC-043 A23): this is the one place every real-default write passes, so the
// bar is checked here again as the backstop, unwrapped so the reason reads
// "substrate_not_validated objective=<obj>". Reverts never come here.
func (pr *Promoter) writeTracked(ctx context.Context, p experiment.Proposal, ev Evidence, cfg Config) (*appliedPromotion, error) {
	pr.killMu.Lock()
	defer pr.killMu.Unlock()
	if pr.KillSwitchEpoch() != cfg.killEpoch {
		return nil, errKillSwitched
	}
	if err := pr.substrateRefusal(ev.Objective); err != nil {
		return nil, err
	}
	if err := pr.realDef.SetRealDefault(ctx, p.FlagKey, ev.RealDefaultValue); err != nil {
		return nil, fmt.Errorf("set real default: %w", err)
	}
	entry := &appliedPromotion{proposal: p, syncPending: true}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.applied == nil {
		pr.applied = map[string]*appliedPromotion{}
	}
	pr.applied[p.FlagKey] = entry
	return entry, nil
}

// endSync marks the promotion's Promote call as returned: from now on a
// kill-switch revert of it is journaled by the kill-switch caller. It reports
// whether the kill switch reverted it during the call (the call's result must
// then say so, since the kill switch left it out of its own report).
func (pr *Promoter) endSync(entry *appliedPromotion) bool {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	entry.syncPending = false
	return entry.killReverted
}

// RevertOnKillSwitch reverts every promotion-written real default (UC-043 A1,
// UC-045 step 5, #1704). The caller invokes it on each experiment tick while
// `enabled` is off (or fails closed). It returns one KillSwitchRevert per flag it
// handled that the caller must journal; a promotion still inside its Promote call
// is left out, because that call's own result carries the revert (one promotion
// event per revert). A failed revert is tried again on every tick (fail closed),
// but a tracked promotion's failure is returned only once, with outcome applied:
// the next item returned for it is the reverted outcome of the retry that succeeds
// (UC-043 A1, #1753). The dedup filters only the report, after the attempt and
// the retrack. An untracked flag (no experiment id, nothing to journal) has its
// failure returned on every tick.
func (pr *Promoter) RevertOnKillSwitch(ctx context.Context) []KillSwitchRevert {
	pr.killMu.Lock()
	defer pr.killMu.Unlock()

	pr.mu.Lock()
	pr.killEpoch++
	tracked := pr.applied
	pr.applied = map[string]*appliedPromotion{}
	pr.mu.Unlock()

	flags := map[string]struct{}{}
	for flag := range tracked {
		flags[flag] = struct{}{}
	}
	if lister, ok := pr.reverter.(PromotionOverlayLister); ok {
		listed, err := lister.PromotedFlags(ctx)
		if err != nil {
			pr.log.Warn("kill switch: cannot list promotion overlays; reverting the tracked promotions only", "error", err)
		}
		for _, flag := range listed {
			flags[flag] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(flags))
	for flag := range flags {
		ordered = append(ordered, flag)
	}
	sort.Strings(ordered)

	out := make([]KillSwitchRevert, 0, len(ordered))
	for _, flag := range ordered {
		entry := tracked[flag]
		rv := KillSwitchRevert{FlagKey: flag, Outcome: OutcomeApplied}
		report := true
		if entry != nil {
			rv.ExperimentID = entry.proposal.ExperimentID
		}
		switch {
		case pr.reverter == nil:
			rv.Reason = KillSwitchReason + "; no real-default reverter wired"
		default:
			err := pr.reverter.RevertRealDefault(ctx, flag)
			superseded := errors.Is(err, ErrSupersededByAnotherWriter)
			if err != nil && !superseded {
				rv.Reason = fmt.Sprintf("%s; revert failed: %v", KillSwitchReason, err)
				if entry != nil {
					pr.retrack(entry)
					report = !pr.markKillFailReported(entry)
				}
			} else {
				rv.Outcome = OutcomeReverted
				rv.Reason = KillSwitchReason
				if superseded {
					rv.Reason += SupersededReasonSuffix
				}
			}
		}
		pr.log.Warn("autonomous promotion: kill switch", "flag", flag,
			"experiment_id", rv.ExperimentID, "outcome", string(rv.Outcome), "reason", rv.Reason)
		if entry != nil && rv.Outcome == OutcomeReverted && pr.handOverToSync(entry) {
			continue // its Promote call reports the revert
		}
		if !report {
			continue // this promotion's failure was reported already (#1753)
		}
		out = append(out, rv)
	}
	return out
}

// handOverToSync records a kill-switch revert on an entry whose Promote call is
// still running, and reports whether it was running (that call then reports the
// revert, endSync).
func (pr *Promoter) handOverToSync(entry *appliedPromotion) bool {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if !entry.syncPending {
		return false
	}
	entry.killReverted = true
	return true
}

// retrack puts back an entry whose kill-switch revert failed, unless a newer
// promotion of the flag took its place meanwhile, so the next tick retries it
// with its experiment id.
func (pr *Promoter) retrack(entry *appliedPromotion) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if _, ok := pr.applied[entry.proposal.FlagKey]; !ok {
		pr.applied[entry.proposal.FlagKey] = entry
	}
}

// markKillFailReported marks a failed kill-switch revert of entry as reported and
// returns whether an earlier tick reported it already (#1753).
func (pr *Promoter) markKillFailReported(entry *appliedPromotion) (alreadyReported bool) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	alreadyReported = entry.failReported
	entry.failReported = true
	return alreadyReported
}
