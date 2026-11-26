package promote

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/joustmania/agent/experiment"
)

// ---- RECORDING FAKES for the three promotion seams (#936 safety rail). The
// Promoter owns only the dispatch logic; opening an issue and the real-default
// write are faked here so every mode is exercised against EXACTLY what it "would"
// do — ZERO network, ZERO real-default mutation. None of these fakes touch
// github.com; they only record. ----

// recordingGitHub records the issue it was asked to open and returns a canned
// URL (or a configured error). It is the unit-test stand-in for the GitHubClient
// wire boundary.
type recordingGitHub struct {
	issues   []IssueReq
	issueURL string
	issueErr error
}

func (g *recordingGitHub) OpenIssue(_ context.Context, req IssueReq) (string, error) {
	g.issues = append(g.issues, req)
	return g.issueURL, g.issueErr
}

// recordingRealDefault records the real-default writes it was asked to make. This
// is the highest-privilege seam; the fake NEVER writes a real file — it only
// records that a real-default change "would" have happened.
type recordingRealDefault struct {
	calls []struct {
		flag  string
		value any
	}
	err error
}

func (w *recordingRealDefault) SetRealDefault(_ context.Context, flag string, value any) error {
	w.calls = append(w.calls, struct {
		flag  string
		value any
	}{flag, value})
	return w.err
}

// recordingReverter records real-default reverts.
type recordingReverter struct {
	calls int
	err   error
}

func (r *recordingReverter) RevertRealDefault(context.Context, string) error {
	r.calls++
	return r.err
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

var promoteProposal = experiment.Proposal{
	FlagKey:           "death_grace_period_ms",
	ExperimentalValue: 500,
	Rationale:         "longer grace may extend sessions",
}

var promoteVerdict = experiment.Decision{
	Outcome:       experiment.OutcomePromote,
	FitnessBefore: 0.50,
	FitnessAfter:  0.72,
	Games:         5,
}

func testEvidence() Evidence {
	return Evidence{
		FitnessBefore:    0.50,
		FitnessAfter:     0.72,
		FitnessDelta:     0.22,
		SyntheticGames:   5,
		Reasoning:        "longer grace reduces early eliminations",
		FlagState:        "death_grace_period_ms: default=300, experimental=500",
		RealDefaultValue: 500,
		Objective:        "balanced",
	}
}

// promoterWithRecorder wires a Promoter over the given seams with an in-memory span
// recorder, mirroring the validate_test.go recorder pattern.
func promoterWithRecorder(t *testing.T, gh GitHubClient, realDef RealDefaultWriter, rev RealDefaultReverter) (*Promoter, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(rec))
	p := NewPromoter(gh, realDef, rev, tp.Tracer("test"), discardLogger())
	// The substrate validity bar (UC-043 A23) admits testEvidence's objective.
	p.SetSubstrateBar(map[string]bool{"balanced": true})
	return p, rec
}

func spanByName(t *testing.T, rec *tracetest.SpanRecorder, name string) trace.ReadOnlySpan {
	t.Helper()
	for _, s := range rec.Ended() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("span %q not found", name)
	return nil
}

func stringAttr(s trace.ReadOnlySpan, key string) string {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return a.Value.AsString()
		}
	}
	return ""
}

// --- mode=issue: opens an issue with hypothesis + flag, applies NOTHING else. ---
// UC: UC-043 main BR-003
func TestPromoteIssueMode(t *testing.T) {
	gh := &recordingGitHub{issueURL: "https://github.com/WatchMeJoustMyFlags/JoustMania/issues/42"}
	realDef := &recordingRealDefault{}
	p, rec := promoterWithRecorder(t, gh, realDef, nil)

	res := p.Promote(context.Background(), promoteProposal, promoteVerdict,
		Config{Mode: ModeIssue}, testEvidence())

	if res.Outcome != OutcomeApplied {
		t.Fatalf("outcome = %q, want applied", res.Outcome)
	}
	if len(gh.issues) != 1 {
		t.Fatalf("OpenIssue calls = %d, want 1", len(gh.issues))
	}
	// The issue body must name the hypothesis and the implicated flag.
	body := gh.issues[0].Body
	if !strings.Contains(body, promoteProposal.FlagKey) {
		t.Errorf("issue body missing flag key:\n%s", body)
	}
	if !strings.Contains(body, "longer grace reduces early eliminations") {
		t.Errorf("issue body missing reasoning:\n%s", body)
	}
	// Nothing else applied: no real-default change.
	if len(realDef.calls) != 0 {
		t.Errorf("issue mode applied more than an issue: realDef=%d", len(realDef.calls))
	}
	// The body carries the structured evidence.
	for _, want := range []string{
		"Fitness before/after", "0.5000", "0.7200",
		"death_grace_period_ms: default=300, experimental=500",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("issue body missing %q:\n%s", want, body)
		}
	}
	span := spanByName(t, rec, SpanPromote)
	if got := stringAttr(span, AttrGate); got != "issue" {
		t.Errorf("gate attr = %q, want issue", got)
	}
	if got := stringAttr(span, AttrOutcome); got != "applied" {
		t.Errorf("outcome attr = %q, want applied", got)
	}
	if got := stringAttr(span, AttrURL); got != gh.issueURL {
		t.Errorf("url attr = %q, want %q", got, gh.issueURL)
	}
}

// --- the retired `pr` mode (and any unknown value) counts as issue. ---
// UC: UC-043 main BR-003
func TestPromoteRetiredPRModeCountsAsIssue(t *testing.T) {
	gh := &recordingGitHub{issueURL: "https://github.com/WatchMeJoustMyFlags/JoustMania/issues/43"}
	realDef := &recordingRealDefault{}
	p, rec := promoterWithRecorder(t, gh, realDef, nil)

	res := p.Promote(context.Background(), promoteProposal, promoteVerdict,
		Config{Mode: Mode("pr")}, testEvidence())

	if res.Mode != ModeIssue || res.Outcome != OutcomeApplied {
		t.Fatalf("result = %+v, want issue/applied", res)
	}
	if len(gh.issues) != 1 || len(realDef.calls) != 0 {
		t.Errorf("issues=%d realDef=%d, want 1/0", len(gh.issues), len(realDef.calls))
	}
	if got := stringAttr(spanByName(t, rec, SpanPromote), AttrGate); got != "issue" {
		t.Errorf("gate attr = %q, want issue", got)
	}
}

// --- mode=autonomous: applies to the REAL default (separate writer) and the
// promotion is applied. The agent never watches real games or reverts on its own:
// a person reverts, or the kill switch (UC-043 step 7, A1). ---
// UC: UC-043 main
func TestPromoteAutonomousApplied(t *testing.T) {
	t.Run("UC-043/main step 7 autonomous writes the real default and never reverts it on its own", func(t *testing.T) {
		realDef := &recordingRealDefault{}
		rev := &recordingReverter{}
		p, rec := promoterWithRecorder(t, &recordingGitHub{}, realDef, rev)

		res := p.Promote(context.Background(), promoteProposal, promoteVerdict,
			Config{Mode: ModeAutonomous, Enabled: true}, testEvidence())

		if res.Outcome != OutcomeApplied || res.Reason != "" {
			t.Fatalf("result = %q (%s), want applied with no reason", res.Outcome, res.Reason)
		}
		if len(realDef.calls) != 1 {
			t.Fatalf("SetRealDefault calls = %d, want 1", len(realDef.calls))
		}
		if realDef.calls[0].flag != promoteProposal.FlagKey || realDef.calls[0].value != 500 {
			t.Errorf("real-default call = %+v, want flag=%s value=500", realDef.calls[0], promoteProposal.FlagKey)
		}
		if rev.calls != 0 {
			t.Errorf("RevertRealDefault calls = %d, want 0: only a person or the kill switch reverts", rev.calls)
		}
		span := spanByName(t, rec, SpanPromote)
		if got := stringAttr(span, AttrGate); got != "autonomous" {
			t.Errorf("gate attr = %q, want autonomous", got)
		}
	})
	t.Run("UC-043/main step 7 an immediate re-promotion of the same flag is written", func(t *testing.T) {
		realDef := &recordingRealDefault{}
		p, _ := promoterWithRecorder(t, &recordingGitHub{}, realDef, &recordingReverter{})
		cfg := Config{Mode: ModeAutonomous, Enabled: true}
		p.Promote(context.Background(), promoteProposal, promoteVerdict, cfg, testEvidence())
		if got := p.RevertOnKillSwitch(context.Background()); len(got) != 1 {
			t.Fatalf("kill switch = %+v, want one revert", got)
		}
		again := p.Promote(context.Background(), promoteProposal, promoteVerdict, cfg, testEvidence())
		if again.Outcome != OutcomeApplied || len(realDef.calls) != 2 {
			t.Fatalf("re-promotion = %q (%s), writes=%d, want applied and written: no post-revert cooldown",
				again.Outcome, again.Reason, len(realDef.calls))
		}
	})
}

// A failed real-default write is a recorded no-op, never retried.
// UC: UC-043 A9
func TestPromoteAutonomousFailedWriteIsDiscarded(t *testing.T) {
	t.Run("UC-043/A9 a failed autonomous write is discarded with the reason", func(t *testing.T) {
		realDef := &recordingRealDefault{err: errors.New("flag service unavailable")}
		p, _ := promoterWithRecorder(t, &recordingGitHub{}, realDef, &recordingReverter{})

		res := p.Promote(context.Background(), promoteProposal, promoteVerdict,
			Config{Mode: ModeAutonomous, Enabled: true}, testEvidence())

		if res.Outcome != OutcomeDiscarded || !strings.Contains(res.Reason, "flag service unavailable") {
			t.Fatalf("result = %q (%s), want discarded with the write failure", res.Outcome, res.Reason)
		}
		if len(realDef.calls) != 1 {
			t.Fatalf("SetRealDefault calls = %d, want 1 (no retry)", len(realDef.calls))
		}
	})
}

// --- only a PROMOTE verdict is ever acted on. ---
func TestPromoteNonPromoteVerdictIsNoop(t *testing.T) {
	gh := &recordingGitHub{}
	realDef := &recordingRealDefault{}
	p, _ := promoterWithRecorder(t, gh, realDef, nil)

	for _, verdict := range []experiment.Outcome{experiment.OutcomeDiscard, experiment.OutcomeRevert} {
		res := p.Promote(context.Background(), promoteProposal,
			experiment.Decision{Outcome: verdict},
			Config{Mode: ModeAutonomous, Enabled: true}, testEvidence())
		if res.Outcome != OutcomeDiscarded {
			t.Errorf("verdict %q: outcome = %q, want discarded", verdict, res.Outcome)
		}
	}
	if len(gh.issues)+len(realDef.calls) != 0 {
		t.Errorf("a non-promote verdict acted: issues=%d realDef=%d",
			len(gh.issues), len(realDef.calls))
	}
}

// --- a client error degrades to a recorded no-op, never a different action. ---
// UC: UC-043 A9
func TestPromoteClientErrorIsRecordedNoop(t *testing.T) {
	gh := &recordingGitHub{issueErr: errors.New("rate limited")}
	p, _ := promoterWithRecorder(t, gh, nil, nil)
	res := p.Promote(context.Background(), promoteProposal, promoteVerdict,
		Config{Mode: ModeIssue}, testEvidence())
	if res.Outcome != OutcomeDiscarded {
		t.Fatalf("outcome = %q, want discarded on client error", res.Outcome)
	}
	if !strings.Contains(res.Reason, "rate limited") {
		t.Errorf("reason = %q, want it to carry the client error", res.Reason)
	}
	if len(gh.issues) != 1 {
		t.Errorf("the attempt should still be recorded: %d", len(gh.issues))
	}
}
