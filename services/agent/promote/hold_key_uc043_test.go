package promote

import (
	"context"
	"testing"

	"github.com/joustmania/agent/experiment"
)

// A hold sub-parameter is written and reverted at once: the game latches it at
// its start (UC-048 BR-011), so a real write never reaches a running game.

const holdKey = "shield.energy_budget_seconds"

var autoOn = Config{Mode: ModeAutonomous, Target: TargetLocal, Enabled: true}

func holdProposal(id string) experiment.Proposal {
	return experiment.Proposal{FlagKey: holdKey, ExperimentID: id, ExperimentalValue: 20.0}
}

func holdEvidence() Evidence {
	ev := testEvidence()
	ev.RealDefaultValue = 20.0
	return ev
}

// UC: UC-043 A17
// UC: UC-048 BR-011
func TestHoldKeyPromotion_WritesAndRevertsAtOnce(t *testing.T) {
	t.Run("UC-043/A17 a hold-key promotion writes the real value at once", func(t *testing.T) {
		realDef := &recordingRealDefault{}
		p, _ := promoterWithRecorder(t, &recordingGitHub{}, &recordingGit{}, realDef, fakeWatcher{}, &recordingReverter{})
		res := p.Promote(context.Background(), holdProposal("exp-h1"), promoteVerdict, autoOn, holdEvidence())
		if res.Outcome != OutcomeApplied {
			t.Fatalf("outcome = %q (%s), want applied", res.Outcome, res.Reason)
		}
		if len(realDef.calls) != 1 {
			t.Fatalf("SetRealDefault calls = %d, want 1", len(realDef.calls))
		}
	})
	t.Run("UC-043/A17 shield.activation is never written to real games", func(t *testing.T) {
		realDef := &recordingRealDefault{}
		p, _ := promoterWithRecorder(t, &recordingGitHub{}, &recordingGit{}, realDef, fakeWatcher{}, &recordingReverter{})
		prop := experiment.Proposal{FlagKey: experiment.HoldActivationKey, ExperimentID: "exp-h3", ExperimentalValue: experiment.HoldValue}
		res := p.Promote(context.Background(), prop, promoteVerdict, autoOn, holdEvidence())
		if res.Outcome != OutcomeDiscarded || res.Reason != ReasonNotRealPromotable {
			t.Fatalf("outcome = %q (%s), want discarded (%s)", res.Outcome, res.Reason, ReasonNotRealPromotable)
		}
		if len(realDef.calls) != 0 {
			t.Fatalf("SetRealDefault calls = %d, want 0", len(realDef.calls))
		}
	})
	t.Run("UC-043/A17 a degraded hold-key promotion is reverted at once", func(t *testing.T) {
		rev := &recordingReverter{}
		p, _ := promoterWithRecorder(t, &recordingGitHub{}, &recordingGit{}, &recordingRealDefault{}, fakeWatcher{degraded: true}, rev)
		res := p.Promote(context.Background(), holdProposal("exp-h2"), promoteVerdict, autoOn, holdEvidence())
		if res.Outcome != OutcomeReverted {
			t.Fatalf("outcome = %q (%s), want reverted", res.Outcome, res.Reason)
		}
		if rev.calls != 1 {
			t.Fatalf("RevertRealDefault calls = %d, want 1", rev.calls)
		}
	})
}
