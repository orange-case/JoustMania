package actions

import (
	"context"
	"testing"

	"github.com/joustmania/agent/decision"
)

// UC: UC-101 main step 3 A3 A4 BR-003
// The writer sends the decision's intervention id as the flag-service decision_id
// on every write shape (#2665, FR-221): a game state PUT, a per-player PUT, a
// one-shot POST and a group shield POST (one id for the whole group, UC-101 A4).
// A decision without an id is written unchanged, without one (NFR-061).
func TestWriter_SendsTheInterventionIDAsDecisionID(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    decision.Decision
	}{
		{"UC-101/BR-003 game state PUT", decision.Decision{Intervention: decision.InterventionAdjustMusicTempo}},
		{"UC-101/BR-003 per-player PUT", decision.Decision{Intervention: decision.InterventionGrantShield, TargetSerial: "A"}},
		{"UC-101/BR-003 one-shot POST", decision.Decision{Intervention: decision.InterventionRevivePlayer, TargetSerial: "A"}},
		{"UC-101/A4 group shield POST carries one id", decision.Decision{
			Intervention: decision.InterventionGrantGroupShield, TargetSerials: []string{"A", "B"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, id := range []string{"iv-2665", ""} {
				w, svc := newTestWriter(t)
				d := tc.d
				d.GameID = "game_1"
				d.InterventionID = id
				if err := w.Apply(context.Background(), d); err != nil {
					t.Fatalf("Apply: %v", err)
				}
				calls := svc.all()
				if len(calls) != 1 {
					t.Fatalf("writes = %d, want 1", len(calls))
				}
				got := calls[0].write.DecisionID
				if calls[0].method == "POST" {
					got = calls[0].event.DecisionID
				}
				if got != id {
					t.Errorf("decision_id = %q, want %q", got, id)
				}
			}
		})
	}
}
