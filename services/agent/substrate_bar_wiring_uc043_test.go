package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/joustmania/agent/experiment"
	"github.com/joustmania/agent/flagservice"
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

// UC: UC-043 A1
func TestUC043_A1_BuildPromoterKillSwitchWithoutValidatedObjectives(t *testing.T) {
	t.Run("UC-043/A1 with no validated objective the kill switch still reverts a promotion overlay", func(t *testing.T) {
		const flag = "death_grace_period_seconds"
		var mu sync.Mutex
		overlay := true
		var deletes []string
		view := func() flagservice.FlagView {
			v := flagservice.FlagView{Flag: flag, EffectiveDefault: "default"}
			if overlay {
				v.RealOverlay = &flagservice.ValueView{Variant: "long", Value: json.RawMessage("3"), Writer: "promotion"}
			}
			return v
		}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /game", func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(flagservice.SetView{FlagSet: "game", Flags: []flagservice.FlagView{view()}})
		})
		mux.HandleFunc("GET /game/{flag}", func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(view())
		})
		mux.HandleFunc("DELETE /game/{flag}", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			deletes = append(deletes, r.PathValue("flag")+"?"+r.URL.RawQuery)
			overlay = false
			w.WriteHeader(http.StatusNoContent)
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		t.Setenv("FLAG_SERVICE_URL", srv.URL)
		t.Setenv("AGENT_AUTONOMOUS_VALIDATED_OBJECTIVES", "")
		t.Setenv("SERVICE_VERSION", "0.14.0")

		got := buildPromoter(testLogger()).RevertOnKillSwitch(context.Background())

		if len(got) != 1 || got[0].FlagKey != flag || got[0].Outcome != promote.OutcomeReverted || got[0].Reason != promote.KillSwitchReason {
			t.Fatalf("kill switch = %+v, want one reverted / kill_switch for %s", got, flag)
		}
		mu.Lock()
		defer mu.Unlock()
		if overlay || len(deletes) != 1 || !strings.Contains(deletes[0], "if_writer=promotion") {
			t.Fatalf("overlay present=%v, deletes=%v, want one if_writer=promotion delete", overlay, deletes)
		}
	})
}
