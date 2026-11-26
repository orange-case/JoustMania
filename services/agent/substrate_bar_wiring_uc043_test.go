package main

import (
	"context"
	"strings"
	"testing"

	"github.com/joustmania/agent/experiment"
	"github.com/joustmania/agent/promote"
)

// substrate_bar_wiring_uc043_test.go: buildPromoter sets the substrate validity bar
// from the operator's env ceiling (UC-043 A23, FR-152 a, g, #2541): unset validates
// nothing, an entry counts only for the running release, and without a validated
// objective no real-default writer is wired, so autonomous stays a recorded no-op.

// UC: UC-043 A23
// UC: UC-043 A4
func TestUC043_A23_BuildPromoterSetsTheBarFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name, env, version string
		balanced           bool
	}{
		{"unset env validates nothing", "", "0.14.0", false},
		{"a blank list validates nothing", " , ", "0.14.0", false},
		{"an entry for the running release counts", "balanced@0.14.0", "0.14.0", true},
		{"an entry for another release does not", "balanced@0.13.0", "0.14.0", false},
		{"a moving tag never counts", "balanced@latest", "latest", false},
	} {
		t.Run("UC-043/A23 T8 "+tc.name, func(t *testing.T) {
			t.Setenv("AGENT_AUTONOMOUS_VALIDATED_OBJECTIVES", tc.env)
			t.Setenv("SERVICE_VERSION", tc.version)
			t.Setenv("FLAG_SERVICE_URL", "http://127.0.0.1:1") // never dialled below
			p := buildPromoter(testLogger())
			if got := p.SubstrateValidated("balanced"); got != tc.balanced {
				t.Fatalf("balanced validated = %v, want %v", got, tc.balanced)
			}
			if p.SubstrateValidated("endurance") {
				t.Fatal("endurance validated without an entry")
			}
			if tc.balanced {
				return
			}
			res := p.Promote(context.Background(),
				experiment.Proposal{FlagKey: "death_grace_period_seconds", ExperimentalValue: 3.0},
				experiment.Decision{Outcome: experiment.OutcomePromote},
				promote.Config{Mode: promote.ModeAutonomous, Enabled: true},
				promote.Evidence{Objective: "balanced", RealDefaultValue: 3.0})
			if res.Outcome != promote.OutcomeDiscarded || !strings.Contains(res.Reason, "no real-default writer wired") {
				t.Fatalf("autonomous without a validated objective = %q (%s), want a discarded no-op: no writer wired", res.Outcome, res.Reason)
			}
		})
	}
}
