package flagsmith_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/state"
)

// Flagsmith wins at a restart (#2621), end to end: the real start sequence,
// the real in side and the real out side. Every Flagsmith value below was last
// set by an API key (the audit log says so), the way the link's own pushes,
// Terraform or the Flagsmith MCP server leave it; the restart applies each one
// like a person's: no clear, no kill-switch tightening, no local value kept.
//
// UC: UC-054 A3 BR-005 BR-006
func TestFlagsmithWinsAtRestart(t *testing.T) {
	t.Run("UC-054/A3 BR-006 FR-215 values an API key set come back after a restart: admin setting persisted, kill switch loosened, game override applied", func(t *testing.T) {
		h := newHarness(t, harnessOpts{
			realStart: true,
			before: func(hh *harness) {
				// The audit log names an API key for every value (the old
				// restart rule read it to clear them).
				hh.fake.Handle("GET "+flagsmithtest.APIPath+"projects/{pid}/audit/", func(w http.ResponseWriter, _ *http.Request) {
					flagsmithtest.WriteJSON(w, http.StatusOK, map[string]any{"count": 4, "next": nil, "previous": nil, "results": []any{
						map[string]any{"log": "Flag state updated for feature: interventions_allowed", "is_system_event": false, "author": nil},
						map[string]any{"log": "Flag state updated for feature: decision.backend", "is_system_event": false, "author": nil},
						map[string]any{"log": "Flag state updated for feature: decision.capture_content", "is_system_event": false, "author": nil},
						map[string]any{"log": "Feature override created for feature 'global_sensitivity_override' and identity 'game:g7'", "is_system_event": false, "author": nil},
					}})
				})
				// The persisted local value of the kill switch is the more
				// restrictive one: before #2621 it won at the restart.
				if _, ferr := hh.st.Put(state.WriteRequest{Set: "agent", Flag: "interventions_allowed", Variant: "none", Writer: "operator", Persist: true}); ferr != nil {
					t.Fatal(ferr)
				}
				if _, ferr := hh.st.RegisterGame(state.Game{ID: "g7", CoordinatorInstance: "c1", Kind: "shadow", Serials: []string{"MOCK0001"}}); ferr != nil {
					t.Fatal(ferr)
				}
			},
			doc: document(time.Date(2026, 11, 15, 11, 59, 0, 0, time.UTC),
				[]map[string]any{
					fsState("interventions_allowed", true, "full"),
					fsState("decision.backend", true, "claude"),
					fsState("decision.capture_content", true, "off"),
				},
				override{"game:g7", []map[string]any{fsState("global_sensitivity_override", true, 2)}},
			),
		})
		h.online()

		// The start round applies every value: no edge-lag skip, no wait.
		if v, persisted := h.local("agent", "interventions_allowed", flagsmith.ScopeStack); v.Variant != "full" || !persisted {
			t.Fatalf("UC-054/BR-005 interventions_allowed after the restart = %+v persisted %v, want full persisted (Flagsmith wins, no restart tightening)", v, persisted)
		}
		if v, persisted := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "claude" || !persisted {
			t.Fatalf("UC-054/BR-006 decision.backend after the restart = %+v persisted %v, want claude persisted like the menu admin's value", v, persisted)
		}
		if v, _ := h.local("agent", "decision.capture_content", flagsmith.ScopeStack); v.Variant != "off" {
			t.Fatalf("UC-054/BR-006 decision.capture_content after the restart = %+v, want off (an API key's value is not cleared)", v)
		}
		if v, _ := h.local("interventions", "global_sensitivity_override", "game:g7"); string(v.Value) != "2" {
			t.Fatalf("UC-054/BR-006 the game override an API key set = %+v, want 2 applied once its game is registered", v)
		}
		if n := len(h.fake.RequestsTo(http.MethodGet, flagsmithtest.APIPath+"projects/"+strconv.Itoa(h.fake.ProjectID)+"/audit/")); n != 0 {
			t.Fatalf("UC-054/A3 the start read the audit log %d times, want 0 (no authorship at restart)", n)
		}

		// Later polls keep the values: nothing is cleared or pushed over them.
		h.round()
		h.round()
		if v, _ := h.local("agent", "interventions_allowed", flagsmith.ScopeStack); v.Variant != "full" {
			t.Fatalf("UC-054/BR-005 interventions_allowed after two polls = %+v, want full", v)
		}
		if st := h.link.Status(); st.Pending != 0 || st.OutOfSync != 0 {
			t.Fatalf("UC-054/A3 status %+v after the restart, want nothing pending or out of sync", st)
		}
	})
}
