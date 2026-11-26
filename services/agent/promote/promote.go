// Package promote implements M7-8 (#936): the PROMOTION flow for the agent's
// shadow-scoped flag experiments. It takes a VALIDATED (PROMOTE) experiment plus
// its fitness Evidence and ACTS according to one live flag:
//
//		code_improvement.mode = issue | autonomous   (the "gate")
//
//	  - issue       — open a GitHub issue describing the hypothesis + implicated
//	                  flag; NO code/flag applied.
//	  - autonomous  — apply the validated change to the REAL default. The agent
//	                  never reverts it on its own: a person does (pad, flag
//	                  service, Flagsmith), or the kill switch (UC-043 step 7, A1).
//
// THE SAFETY RAIL (agreed, non-negotiable — see docs/research/m7-4-testing-strategy.md
// §3.3). Promotion is the one consequential, REAL-affecting piece of the track,
// so the design makes a real action impossible to reach by accident or from a
// test:
//
//   - The seams are interfaces (GitHubClient, RealDefaultWriter). Unit tests use
//     RECORDING FAKES that assert "would open an issue with body X" / "would write
//     real default Z" — ZERO network, ZERO real-default mutation.
//   - The REAL GitHub client and the real-default writer are constructed ONLY in
//     main.go: the GitHub client behind GITHUB_TOKEN + GITHUB_REPOSITORY, the
//     real-default writer only when AGENT_AUTONOMOUS_VALIDATED_OBJECTIVES validates
//     at least one objective. There is NO test-reachable constructor for a real
//     GitHub client; a test that wants one cannot get one.
//   - The SAFE flag DEFAULT is baked in: mode defaults to issue (NEVER
//     autonomous). autonomous is additionally behind the agent `enabled`
//     kill-switch — the caller passes Config.Enabled, and a disabled agent degrades
//     autonomous to a recorded no-op.
//   - Real GitHub is default-off, fail-closed: without the token the real client
//     is not built; main.go passes the inert recording no-op fake, and an issue
//     promotion degrades to a RECORDED no-op (logged + span reason), never a
//     silent or accidental real action.
//   - autonomous changing the real defaultVariant is a SEPARATE, higher-privilege
//     path than the shadow Writer (which is structurally shadow-only). It is the
//     injected RealDefaultWriter, mutated explicitly + gated — the shadow Writer is
//     NEVER reused to sneak a real-default change.
package promote

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/joustmania/agent/experiment"
)

// Mode is the resolved code_improvement.mode — the "gate" that selects promotion
// behaviour. The set is closed so dashboards can enumerate every gate.
type Mode string

const (
	// ModeIssue opens a GitHub issue describing the hypothesis + implicated flag.
	// NO code or flag change is applied. This is the SAFE default mode.
	ModeIssue Mode = "issue"
	// ModeAutonomous applies the validated change to the REAL default. The
	// highest-privilege path; default-OFF and additionally behind the agent
	// kill-switch (Config.Enabled).
	ModeAutonomous Mode = "autonomous"
)

// Outcome is what the promotion ACHIEVED (#936 AC: code_improvement.outcome). The
// set is closed; distinct from experiment.Outcome (the M7-7 verdict).
type Outcome string

const (
	// OutcomeApplied — the requested action happened: an issue was opened, or the
	// autonomous real-default change was applied.
	OutcomeApplied Outcome = "applied"
	// OutcomeDiscarded — the promotion did NOT act: the verdict was not PROMOTE, the
	// agent was disabled (autonomous), or the issue degraded to an inert
	// recorded no-op because the real path was not enabled. The Reason explains which.
	OutcomeDiscarded Outcome = "discarded"
	// OutcomeReverted — autonomous applied to the real default and the kill switch
	// rolled it back (UC-043 A1).
	OutcomeReverted Outcome = "reverted"
)

// Config is the live, per-promotion flag configuration the Promoter acts under
// (#936 flags), resolved fresh each promotion so flipping the mode flag takes
// effect on the NEXT promotion with no restart — the same never-cached contract
// as the #847/#935 gate flags. The caller resolves these via flags.Promotion(ctx)
// (see flags/flags.go) + the agent kill-switch and passes the value in, so this
// package stays free of an OpenFeature dependency (the Gate/Writer/Validator all
// follow this "package owns logic, caller owns flagd" split).
type Config struct {
	// Mode is code_improvement.mode (default issue). Selects issue/autonomous.
	Mode Mode
	// Enabled is the agent `enabled` kill-switch (flags.Snapshot.Enabled). It gates
	// ONLY the autonomous mode: a real-default mutation must never happen while the
	// agent is disabled. issue does not consume it (opening an issue is not a
	// live real-player change). A disabled agent degrades autonomous to a recorded
	// no-op (OutcomeDiscarded + reason), never a silent real change.
	Enabled bool

	// killEpoch is Promoter.KillSwitchEpoch read before Enabled was resolved
	// (epochKnown); an autonomous write is refused when a kill-switch tick ran since
	// (#1704). Unset, Promote reads the epoch on entry.
	killEpoch  uint64
	epochKnown bool
}

// Evidence is the fitness + provenance the promotion records into an issue body
// (fitness before/after, synthetic results, reasoning summary, flag state). It
// is assembled by the caller from the M7-7 Decision + the live flag snapshot and threaded into the body renderer.
type Evidence struct {
	// FitnessBefore / FitnessAfter / FitnessDelta are the M7-7 baseline-vs-synthetic
	// fitness numbers (experiment.Decision). Recorded in the body AND on the span.
	FitnessBefore float64
	FitnessAfter  float64
	FitnessDelta  float64
	// SyntheticGames is how many synthetic/shadow games the experiment was validated
	// against (experiment.Decision.Games) — the sample size behind FitnessAfter.
	SyntheticGames int
	// Reasoning is the coding-agent/proposer reasoning summary — the agent's
	// hypothesis for why the change helps. Sourced from Proposal.Rationale (the
	// proposer side, #931, is a parallel PR; here we render whatever rationale the
	// validated proposal carried).
	Reasoning string
	// FlagState is a human-readable snapshot of the relevant flag state at improvement
	// time (e.g. "death_grace_period_ms: default=300ms, experimental=500ms;
	// objectives=balanced_focused"). The caller assembles it from the live flag
	// snapshot so a reviewer sees the control-plane context the experiment ran under.
	FlagState string
	// RealDefaultValue is the value the autonomous path would write as the REAL
	// defaultVariant for the flag (typically the experiment's experimental value once
	// validated). Used only by ModeAutonomous; ignored by issue.
	RealDefaultValue any
	// Objective is the experiment's objective (journal.Intent.Objective). The
	// autonomous write is made only when it meets the substrate validity bar
	// (UC-043 A23, substrate.go); "" never does.
	Objective string
}

// IssueReq is the GitHub issue a ModeIssue promotion opens.
type IssueReq struct {
	// Title is the issue title (e.g. "Flag experiment: death_grace_period_ms").
	Title string
	// Body is the structured hypothesis + implicated-flag description.
	Body string
	// Labels tag the issue for triage (e.g. agent, code-improvement).
	Labels []string
}

// Result is the recorded outcome of one Promote, returned to the caller for the
// span/log and any follow-up. Never nil-meaningful: even a no-op returns a Result
// with OutcomeDiscarded + a Reason, so a promotion is always attributable.
type Result struct {
	// Mode is the resolved flag the promotion acted under (echoed for the caller's
	// log without re-reading flagd).
	Mode Mode
	// Outcome is applied | discarded | reverted.
	Outcome Outcome
	// URL is the opened issue URL (empty for a no-op).
	URL string
	// Reason explains a no-op / degrade / revert (empty on a clean applied). Its
	// presence on an issue Result is the SAFETY RAIL's visible proof that the
	// real path was unreachable and the promotion degraded to an inert recorded no-op.
	Reason string
	// ForeignOwner: the writer that owns the real overlay when the promotion was
	// refused with 412 writer_mismatch (UC-043 A19, #2276); empty otherwise.
	ForeignOwner string
}

// GitHubClient is the GitHub API wire boundary (the SAFETY RAIL's network seam).
// Unit tests inject a recording fake; the REAL client is built ONLY in main.go
// behind the token (NewGitHubClientFromEnv) and is never reachable from a test
// constructor.
type GitHubClient interface {
	// OpenIssue opens a GitHub issue and returns its URL.
	OpenIssue(ctx context.Context, req IssueReq) (string, error)
}

// RealDefaultWriter performs the autonomous mode's REAL defaultVariant mutation —
// the SEPARATE, higher-privilege path the safety rail isolates from the shadow
// Writer. It is explicitly NOT experiment.Writer (which is structurally shadow-
// scoped and can never change a real resolution). A nil RealDefaultWriter means
// "no real-default mechanism wired", so autonomous degrades to a recorded no-op.
// main.go wires it only when an objective is validated (UC-043 A23).
type RealDefaultWriter interface {
	// SetRealDefault changes the REAL defaultVariant for flagKey to value. This is the
	// one sanctioned real-player-affecting write; it is gated by the validated
	// objectives at construction AND the kill-switch at call time.
	SetRealDefault(ctx context.Context, flagKey string, value any) error
}

// RealDefaultReverter removes a promotion's real-default overlay; only the kill
// switch calls it (UC-043 A1). nil means no revert mechanism wired (a kill-switch
// revert is then logged, not made). Distinct from experiment.Reverter (the shadow
// rollback): this reverts the REAL default.
type RealDefaultReverter interface {
	// RevertRealDefault restores the prior real defaultVariant for flagKey.
	RevertRealDefault(ctx context.Context, flagKey string) error
}

// Promoter orchestrates a promotion per the live mode. It owns ONLY the
// dispatch + outcome logic; every consequential effect (open issue, write real
// default, kill-switch revert) is an injected seam, so the whole flow is
// unit-testable with recording fakes and the real path is built only in main.go.
type Promoter struct {
	github   GitHubClient        // wire boundary; recording fake in tests, real client in main.go
	realDef  RealDefaultWriter   // autonomous real-default mutation (nil = not wired)
	reverter RealDefaultReverter // kill-switch rollback (nil = not wired)
	tracer   trace.Tracer
	log      *slog.Logger

	mu sync.Mutex

	// --- kill-switch revert (#1704) ---
	// applied holds every autonomous promotion this agent run wrote and the kill
	// switch has not reverted, by flag (the newest write wins). Guarded by mu.
	applied map[string]*appliedPromotion
	// killEpoch counts kill-switch ticks (guarded by mu); killMu serialises the
	// autonomous write with RevertOnKillSwitch (killswitch.go, #1704). Lock order:
	// killMu before mu.
	killEpoch uint64
	killMu    sync.Mutex

	// --- substrate validity bar (UC-043 A23, substrate.go) ---
	// substrate is the set of objectives attested as validated for the running
	// release; set once (substrateSet) by SetSubstrateBar. Unset refuses every
	// autonomous write. Guarded by mu.
	substrate    map[string]bool
	substrateSet bool
}

// appliedPromotion is one autonomous real default this agent run wrote and the
// kill switch has not reverted (#1704). syncPending is true while its Promote call
// runs; killReverted is set when the kill switch reverted it during that call;
// failReported is set once a failed kill-switch revert of it was reported, so
// later failures of the same promotion are retried without another report
// (UC-043 A1, #1753). Guarded by Promoter.mu.
type appliedPromotion struct {
	proposal     experiment.Proposal
	syncPending  bool
	killReverted bool
	failReported bool
}

// NewPromoter builds a Promoter over the injected seams. github defaults to the
// inert recording no-op when nil (so a Promoter is ALWAYS safe to construct in a
// test or when the real path is disabled — it can only no-op). realDef/reverter
// may be nil (autonomous degrades accordingly). tracer nil uses the global
// tracer; log nil uses slog.Default().
func NewPromoter(github GitHubClient, realDef RealDefaultWriter, reverter RealDefaultReverter, tracer trace.Tracer, log *slog.Logger) *Promoter {
	if github == nil {
		// Default to the inert recording no-op: a Promoter built without an explicit
		// client can NEVER reach github.com. The real client is opt-in (main.go only).
		github = NewNoopGitHubClient(log)
	}
	if tracer == nil {
		tracer = otel.Tracer(instrumentationName)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Promoter{
		github:   github,
		realDef:  realDef,
		reverter: reverter,
		tracer:   tracer,
		log:      log,
		applied:  map[string]*appliedPromotion{},
	}
}

// Promote acts on a validated experiment per the live cfg. p is the PROMOTE'd
// Proposal, dec is the M7-7 verdict, ev is the assembled fitness Evidence. It
// emits the code_improvement.promote span carrying gate (mode) + outcome
// (+ the fitness evidence + any degrade reason), and returns the recorded Result.
//
// UC-043 BR-003: issue mode never changes a running system; a human acts on it.
// FAIL-SAFE / RECORDED: a promotion NEVER silently acts or silently no-ops. If the
// verdict was not PROMOTE, or GitHub is unreachable (real client absent),
// or autonomous is requested while disabled, the promotion degrades to
// OutcomeDiscarded with a Reason on the span — visible, never silent. The real,
// consequential paths run only through the injected seams main.go gates.
func (pr *Promoter) Promote(ctx context.Context, p experiment.Proposal, dec experiment.Decision, cfg Config, ev Evidence) Result {
	mode := normalizeMode(cfg.Mode)
	if !cfg.epochKnown {
		cfg.killEpoch, cfg.epochKnown = pr.KillSwitchEpoch(), true
	}

	ctx, span := pr.tracer.Start(ctx, SpanPromote, trace.WithAttributes(
		attribute.String(AttrGate, string(mode)),
		attribute.String(AttrFlagKey, p.FlagKey),
		attribute.Float64(AttrFitnessBefore, ev.FitnessBefore),
		attribute.Float64(AttrFitnessAfter, ev.FitnessAfter),
		attribute.Float64(AttrFitnessDelta, ev.FitnessDelta),
	))
	defer span.End()

	res := Result{Mode: mode}

	// GUARD 1: only a validated PROMOTE verdict is ever promoted (#936 depends on
	// M7-7: "only validated changes are promoted"). Anything else is a recorded
	// no-op — the agent does not act on a discard/revert verdict.
	if dec.Outcome != experiment.OutcomePromote {
		res.Outcome = OutcomeDiscarded
		res.Reason = fmt.Sprintf("verdict %q is not promote; nothing promoted", dec.Outcome)
		return pr.record(span, res)
	}

	switch mode {
	case ModeIssue:
		pr.promoteIssue(ctx, p, ev, &res)
	case ModeAutonomous:
		pr.promoteAutonomous(ctx, p, cfg, ev, &res)
	default:
		// Unreachable after normalizeMode, but fail safe: an unknown mode acts as the
		// safest mode (no real change), recorded.
		res.Outcome = OutcomeDiscarded
		res.Reason = fmt.Sprintf("unknown mode %q; treated as no-op", mode)
	}

	return pr.record(span, res)
}

// record stamps the outcome/url/reason on the span and logs the promotion, then
// returns the Result. A non-applied github-target outcome marks the span with an
// error status so a degraded/unreachable promotion is visible in Jaeger (it is a
// recorded no-op, not a failure of the agent, so the message is the reason).
func (pr *Promoter) record(span trace.Span, res Result) Result {
	span.SetAttributes(
		attribute.String(AttrOutcome, string(res.Outcome)),
		attribute.String(AttrURL, res.URL),
		attribute.String(AttrReason, res.Reason),
	)
	if res.Reason != "" {
		span.SetStatus(codes.Error, res.Reason)
	}
	pr.log.Info("code_improvement.promoted",
		"gate", string(res.Mode),
		"outcome", string(res.Outcome),
		"url", res.URL,
		"reason", res.Reason,
	)
	return res
}

// normalizeMode coerces an unknown/empty mode to the SAFE default (issue) — the
// safety rail's "default mode is never autonomous". A garbled flag value can never
// silently become a real-default mutation.
func normalizeMode(m Mode) Mode {
	switch m {
	case ModeIssue, ModeAutonomous:
		return m
	default:
		return ModeIssue
	}
}
