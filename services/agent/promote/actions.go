package promote

import (
	"context"
	"errors"
	"fmt"

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

// promoteAutonomous applies the validated change to the REAL default. The agent
// does not watch real games afterwards and never reverts on its own: people watch
// every game and revert by hand, and the kill switch reverts every promotion
// (UC-043 step 7, A1). The guards, in order:
//
//   - KILL-SWITCH: a disabled agent (cfg.Enabled == false) NEVER mutates the real
//     default — degrades to a recorded no-op.
//   - SEPARATE WRITER: the real-default change goes through the injected
//     RealDefaultWriter — explicitly NOT the shadow experiment.Writer. A nil
//     RealDefaultWriter (no validated objective) → a recorded no-op.
//   - SUBSTRATE BAR (UC-043 A23): only an attested objective may change a real
//     default; writeTracked checks it again as the backstop.
func (pr *Promoter) promoteAutonomous(ctx context.Context, p experiment.Proposal, cfg Config, ev Evidence, res *Result) {
	if p.FlagKey == experiment.HoldActivationKey {
		res.Outcome = OutcomeDiscarded
		res.Reason = ReasonNotRealPromotable
		return
	}
	if !cfg.Enabled {
		res.Outcome = OutcomeDiscarded
		res.Reason = "autonomous requested but agent disabled (kill-switch); real default not changed"
		return
	}
	if pr.realDef == nil {
		res.Outcome = OutcomeDiscarded
		res.Reason = "autonomous requested but no real-default writer wired (no validated objective); real default not changed"
		return
	}
	if err := pr.substrateRefusal(ev.Objective); err != nil {
		res.Outcome = OutcomeDiscarded
		res.Reason = err.Error()
		return
	}

	// The one sanctioned real-player-affecting write, through the gated seam. It is
	// serialised with the kill switch (#1704): a kill-switch tick since this
	// promotion's config was resolved refuses the write, and a tick that starts
	// after it finds the written overlay.
	entry, err := pr.writeTracked(ctx, p, ev, cfg)
	if errors.Is(err, ErrRealOverlayOwnedByAnotherWriter) {
		// UC-043 A19 (#2260): another writer owns the real overlay; the promotion
		// is not made and not retried.
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
	// A kill-switch tick between the write and here reverted it: this result
	// reports that revert, once (#1704).
	if pr.endSync(entry) {
		res.Outcome = OutcomeReverted
		res.Reason = KillSwitchReason
	}
}
