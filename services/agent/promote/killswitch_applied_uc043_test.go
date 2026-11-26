package promote

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// killswitch_applied_uc043_test.go covers the kill-switch revert of an applied
// autonomous promotion (UC-043 A1, UC-045 step 5, #1704, #1753): the kill switch
// reverts it at once, that revert is its last write, and a failed revert is
// reported once and retried on every tick.

// listingReverter is a recordingReverter that also lists promotion overlays the
// way the flag-service-backed reverter does after an agent restart.
type listingReverter struct {
	recordingReverter
	flags []string
}

func (l *listingReverter) PromotedFlags(context.Context) ([]string, error) { return l.flags, nil }

type flagServiceError string

func (e flagServiceError) Error() string { return string(e) }

const errFlagService = flagServiceError("flag service unavailable")

// UC: UC-043 A1
// UC: UC-045 main
func TestUC043_A1_KillSwitchRevertsAppliedPromotion(t *testing.T) {
	proposal := promoteProposal
	proposal.ExperimentID = "exp-1704"
	on := Config{Mode: ModeAutonomous, Enabled: true}
	off := Config{Mode: ModeAutonomous, Enabled: false}

	t.Run("UC-043/A1 applied promotion: one revert, then no writes", func(t *testing.T) {
		realDef := &recordingRealDefault{}
		rev := &recordingReverter{}
		p, _ := promoterWithRecorder(t, &recordingGitHub{}, realDef, rev)

		if res := p.Promote(context.Background(), proposal, promoteVerdict, on, testEvidence()); res.Outcome != OutcomeApplied {
			t.Fatalf("precondition: outcome = %q, want applied", res.Outcome)
		}

		got := p.RevertOnKillSwitch(context.Background())

		if len(got) != 1 {
			t.Fatalf("kill switch handled %d promotions, want 1", len(got))
		}
		if got[0].Outcome != OutcomeReverted || got[0].Reason != KillSwitchReason || got[0].ExperimentID != "exp-1704" {
			t.Fatalf("kill-switch result = %+v, want reverted / %s / exp-1704", got[0], KillSwitchReason)
		}
		if rev.calls != 1 {
			t.Fatalf("revert writes after the kill switch = %d, want 1", rev.calls)
		}
		// A promotion while off and further kill-switch ticks write nothing.
		p.Promote(context.Background(), proposal, promoteVerdict, off, testEvidence())
		if again := p.RevertOnKillSwitch(context.Background()); len(again) != 0 {
			t.Fatalf("second kill-switch tick handled %d promotions, want 0", len(again))
		}
		if rev.calls != 1 || len(realDef.calls) != 1 {
			t.Fatalf("writes after the kill-switch revert: reverts=%d sets=%d, want 1 and 1", rev.calls, len(realDef.calls))
		}
	})

	t.Run("UC-043/A1 a failed revert is retried on the next tick", func(t *testing.T) {
		rev := &recordingReverter{}
		p, _ := promoterWithRecorder(t, &recordingGitHub{}, &recordingRealDefault{}, rev)
		p.Promote(context.Background(), proposal, promoteVerdict, on, testEvidence())
		rev.err = errFlagService

		first := p.RevertOnKillSwitch(context.Background())
		rev.err = nil
		second := p.RevertOnKillSwitch(context.Background())

		if len(first) != 1 || first[0].Outcome != OutcomeApplied {
			t.Fatalf("failed revert result = %+v, want outcome applied", first)
		}
		if len(second) != 1 || second[0].Outcome != OutcomeReverted || rev.calls != 2 {
			t.Fatalf("retry = %+v reverts=%d, want reverted after 2 attempts", second, rev.calls)
		}
	})
}

// UC: UC-043 A1
// UC: UC-045 main
func TestUC043_A1_FailedKillSwitchRevertJournaledOnce(t *testing.T) {
	proposal := promoteProposal
	proposal.ExperimentID = "exp-1704"
	on := Config{Mode: ModeAutonomous, Enabled: true}

	failingPromoter := func(t *testing.T) (*Promoter, *recordingReverter) {
		t.Helper()
		rev := &recordingReverter{}
		p, _ := promoterWithRecorder(t, &recordingGitHub{}, &recordingRealDefault{}, rev)
		if res := p.Promote(context.Background(), proposal, promoteVerdict, on, testEvidence()); res.Outcome != OutcomeApplied {
			t.Fatalf("precondition: outcome = %q, want applied", res.Outcome)
		}
		rev.err = errFlagService
		return p, rev
	}

	t.Run("UC-043/A1 a failing revert is reported once across ticks and the retry that succeeds reports the final reverted", func(t *testing.T) {
		p, rev := failingPromoter(t)

		var reported []int
		var first []KillSwitchRevert
		for tick := 0; tick < 3; tick++ {
			got := p.RevertOnKillSwitch(context.Background())
			if tick == 0 {
				first = got
			}
			reported = append(reported, len(got))
		}
		if reported[0] != 1 || reported[1] != 0 || reported[2] != 0 {
			t.Fatalf("failure reports per tick = %v, want [1 0 0]", reported)
		}
		if first[0].Outcome != OutcomeApplied || first[0].ExperimentID != "exp-1704" {
			t.Fatalf("failure report = %+v, want applied / exp-1704", first[0])
		}
		// Fail closed: every tick tried the revert again.
		if rev.calls != 3 {
			t.Fatalf("revert attempts = %d, want 3 (one per tick)", rev.calls)
		}

		rev.err = nil
		final := p.RevertOnKillSwitch(context.Background())
		if len(final) != 1 || final[0].Outcome != OutcomeReverted || final[0].Reason != KillSwitchReason || final[0].ExperimentID != "exp-1704" {
			t.Fatalf("retry that succeeds = %+v, want one reverted / %s / exp-1704", final, KillSwitchReason)
		}
		if after := p.RevertOnKillSwitch(context.Background()); len(after) != 0 {
			t.Fatalf("tick after the revert reported %+v, want nothing", after)
		}
		if rev.calls != 4 {
			t.Fatalf("revert attempts = %d, want 4", rev.calls)
		}
	})

	t.Run("UC-043/A1 a new promotion of the flag starts a new count", func(t *testing.T) {
		p, rev := failingPromoter(t)
		if got := p.RevertOnKillSwitch(context.Background()); len(got) != 1 {
			t.Fatalf("first failure reported %d items, want 1", len(got))
		}
		rev.err = nil
		again := on
		again.killEpoch, again.epochKnown = p.KillSwitchEpoch(), true
		if res := p.Promote(context.Background(), proposal, promoteVerdict, again, testEvidence()); res.Outcome != OutcomeApplied {
			t.Fatalf("re-promotion outcome = %q (%s), want applied", res.Outcome, res.Reason)
		}
		rev.err = errFlagService
		if got := p.RevertOnKillSwitch(context.Background()); len(got) != 1 || got[0].Outcome != OutcomeApplied {
			t.Fatalf("the new promotion's failed revert reported %+v, want one applied", got)
		}
	})

	t.Run("UC-043/A1 an untracked failure is returned on every tick without an experiment id", func(t *testing.T) {
		// A promotion from before an agent restart has no experiment id, so the
		// loop logs it and never journals it; the Promoter keeps no marker for it.
		rev := &listingReverter{flags: []string{"death_grace_period_ms"}}
		rev.err = errFlagService
		p, _ := promoterWithRecorder(t, &recordingGitHub{}, &recordingRealDefault{}, rev)
		for tick := 0; tick < 2; tick++ {
			got := p.RevertOnKillSwitch(context.Background())
			if len(got) != 1 || got[0].ExperimentID != "" || got[0].Outcome != OutcomeApplied {
				t.Fatalf("tick %d: untracked failure = %+v, want one applied without an experiment id", tick, got)
			}
		}
	})

	t.Run("UC-043/A1 concurrent ticks report one failure", func(t *testing.T) {
		p, rev := failingPromoter(t)
		var wg sync.WaitGroup
		var total atomic.Int32
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				total.Add(int32(len(p.RevertOnKillSwitch(context.Background()))))
			}()
		}
		wg.Wait()
		if total.Load() != 1 {
			t.Fatalf("concurrent ticks reported %d failures, want 1", total.Load())
		}
		if rev.calls != 8 {
			t.Fatalf("revert attempts = %d, want 8", rev.calls)
		}
	})
}
