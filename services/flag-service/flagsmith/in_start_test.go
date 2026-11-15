package flagsmith_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
)

// The restart rule end to end: the real start sequence, the real in side and
// the real out side (review of #2597). Every value below was last set by an
// API key (audit rows with author null), so the restart rule clears the admin
// setting but must never loosen a kill switch.
//
// UC: UC-054 A3 BR-005
func TestRestartKillSwitchRealStart(t *testing.T) {
	t.Run("UC-054/A3 BR-005 FR-215 an API-key-tightened persisted kill switch stays tightened across restart", func(t *testing.T) {
		h := newHarness(t, harnessOpts{
			realStart: true,
			before: func(hh *harness) {
				hh.fake.Handle("GET "+flagsmithtest.APIPath+"projects/{pid}/audit/", func(w http.ResponseWriter, _ *http.Request) {
					flagsmithtest.WriteJSON(w, http.StatusOK, map[string]any{"count": 2, "next": nil, "previous": nil, "results": []any{
						map[string]any{"log": "Flag state updated for feature: interventions_allowed", "is_system_event": false, "author": nil},
						map[string]any{"log": "Flag state updated for feature: decision.capture_content", "is_system_event": false, "author": nil},
					}})
				})
			},
			doc: document(time.Date(2026, 11, 15, 11, 59, 0, 0, time.UTC), []map[string]any{
				fsState("interventions_allowed", true, "none"),
				fsState("decision.capture_content", true, "off"),
			}),
		})
		h.online()

		// The start round itself applies the tighter kill switch: no edge-lag
		// skip and no wait for the first poll.
		if v, _ := h.local("agent", "interventions_allowed", flagsmith.ScopeStack); v.Variant != "none" {
			t.Fatalf("interventions_allowed after the start round = %+v, want none (the more restrictive API-key value)", v)
		}

		h.round()
		h.round()
		if v, _ := h.local("agent", "interventions_allowed", flagsmith.ScopeStack); v.Variant != "none" {
			t.Fatalf("interventions_allowed after two polls = %+v, want none: the restart clear loosened a kill switch", v)
		}
		// The admin setting an API key set is cleared, not applied (BR-006).
		if c, _ := h.local("agent", "decision.capture_content", flagsmith.ScopeStack); c.Present {
			t.Fatalf("decision.capture_content applied locally %+v, want the API-key value cleared", c)
		}
	})
}
