package flagsmith_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/state"
)

// UC: UC-055 A4
// A revert of a refused stack value that does not happen (Flagsmith's own
// value is refused here) leaves nothing behind (#2631 N1): a later, ordinary
// Flagsmith change applied to the flag does not hold the old refused value
// again, so the refusal ends and a re-save of that value is sent.
func TestRevertNotDoneLeavesNoHeldValue(t *testing.T) {
	m := newFSModel()
	m.values["decision.backend"] = "claude"
	backendPath := futureAPI + strconv.Itoa(inoutIDs["decision.backend"]) + "/"
	var mu sync.Mutex
	refuse := true
	h := inoutHarness(t, m, map[string]http.HandlerFunc{
		"PATCH " + backendPath: func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			rf := refuse
			mu.Unlock()
			if rf {
				// Flagsmith's value turns into one that is not a variant
				// here, before the refusal is answered: the round the
				// refusal wakes sees it (no race with that round), the
				// revert to it is refused locally and jev stays in force.
				m.set("decision.backend", 42)
				flagsmithtest.WriteJSON(w, http.StatusConflict, map[string]string{"detail": "change requests enabled"})
				return
			}
			// Flagsmith holds the value it accepted.
			var body struct {
				Value any `json:"feature_state_value"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.set("decision.backend", body.Value)
			flagsmithtest.WriteJSON(w, http.StatusOK, map[string]string{})
		},
	}, nil)
	h.online()
	n := h.pulls()
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev"})
	h.waitFor("the push refused and the revert round run", func() bool {
		v, ok := h.link.Links().PushRefused(backendKey)
		return ok && v.Variant == "jev" && h.pulls() > n
	})
	if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "jev" {
		t.Fatalf("setup: decision.backend = %+v, want jev kept (Flagsmith's value refused here)", v)
	}
	// Change requests are switched off: a different value is accepted.
	mu.Lock()
	refuse = false
	mu.Unlock()
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "openai"})
	h.waitFor("the push confirmed", func() bool {
		r, ok := h.link.Links().Remote(backendKey)
		return ok && r.Variant == "openai"
	})
	h.roundAndSettle()
	// An ordinary Flagsmith change, not a revert.
	m.set("decision.backend", "claude")
	h.roundAndSettle()
	if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "claude" {
		t.Fatalf("setup: decision.backend = %+v, want Flagsmith's claude applied", v)
	}
	t.Run("UC-055/A4 a later Flagsmith change does not hold the old refused value", func(t *testing.T) {
		if v, ok := h.link.Links().PushRefused(backendKey); ok {
			t.Fatalf("held refused value %+v, want none: the revert of it never happened", v)
		}
		if _, _, r := h.link.Links().Counts(); r != 0 {
			t.Fatalf("refused %d, want 0", r)
		}
	})
	t.Run("UC-055/A4 a re-save of the once-refused value is sent", func(t *testing.T) {
		before := h.requests(http.MethodPatch, backendPath)
		h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev"})
		h.waitFor("the re-save sent", func() bool { return h.requests(http.MethodPatch, backendPath) == before+1 })
	})
}

// UC: UC-055 A4
// A revert skipped by the write budget leaves nothing behind (#2631 N1): 60
// game:serial changes sort before a refused stack value, so the round that
// would revert it runs out of writes. The operator then saves the value
// Flagsmith holds (the refusal ends, nothing is sent); a later, ordinary
// Flagsmith change to the once-refused value is applied as a change, not held
// as a refusal.
func TestRevertSkippedByWriteBudgetLeavesNoHeldValue(t *testing.T) {
	// user/game_instructions: a stack key, not a kill switch, in a set that
	// sorts after interventions; it has no feature id, so its push is refused
	// without a request (and wakes a revert round).
	k := flagsmith.Key{Set: "user", Flag: "game_instructions", Scope: flagsmith.ScopeStack}
	m := newFSModel()
	m.values["game_instructions"] = "on"
	h := inoutHarness(t, m, nil, nil)
	h.online()
	if v, _ := h.local("user", "game_instructions", flagsmith.ScopeStack); v.Variant != "on" {
		t.Fatalf("setup: game_instructions = %+v after the start round, want on", v)
	}
	var serials []string
	for i := range 30 {
		serials = append(serials, fmt.Sprintf("MOCK%04d", 100+i))
	}
	// The registration wakes a round; wait for it, so no other round runs.
	n := h.pulls()
	if _, ferr := h.st.RegisterGame(state.Game{ID: "g1", CoordinatorInstance: "c1", Kind: "shadow", Serials: serials}); ferr != nil {
		t.Fatal(ferr)
	}
	h.waitFor("the round woken by the registration", func() bool { return h.pulls() > n })
	// 60 game:serial values in Flagsmith, applied by the next round first.
	m.mu.Lock()
	for _, s := range serials {
		m.ids["game:g1:"+s] = map[string]any{"player_sensitivity_factor": 1.5, "player_handicap_factor": 1.5}
	}
	m.mu.Unlock()
	m.publish()

	n = h.pulls()
	h.put(state.WriteRequest{Set: "user", Flag: "game_instructions", Variant: "off"})
	h.waitFor("the push refused and the woken round run", func() bool {
		v, ok := h.link.Links().PushRefused(k)
		return ok && v.Variant == "off" && h.pulls() > n
	})
	if got := serialValues(h, serials); got != 50 {
		t.Fatalf("setup: %d game:serial values after the woken round, want 50 (the write budget)", got)
	}
	if v, _ := h.local("user", "game_instructions", flagsmith.ScopeStack); v.Variant != "off" {
		t.Fatalf("setup: game_instructions = %+v, want off (the revert skipped by the write budget)", v)
	}
	// The operator saves what Flagsmith holds: the refusal ends, nothing is
	// sent, and the next round finds the key in sync.
	h.put(state.WriteRequest{Set: "user", Flag: "game_instructions", Variant: "on"})
	if _, ok := h.link.Links().PushRefused(k); ok {
		t.Fatal("setup: the refusal did not end with the save of Flagsmith's value")
	}
	h.roundAndSettle()
	if got := serialValues(h, serials); got != 60 {
		t.Fatalf("setup: %d game:serial values after the next round, want 60", got)
	}
	// An ordinary Flagsmith change to the value refused before.
	m.set("game_instructions", "off")
	h.roundAndSettle()
	if v, _ := h.local("user", "game_instructions", flagsmith.ScopeStack); v.Variant != "off" {
		t.Fatalf("setup: game_instructions = %+v, want Flagsmith's off applied", v)
	}
	t.Run("UC-055/A4 a Flagsmith change after a revert skipped by the write budget is not held as refused", func(t *testing.T) {
		if v, ok := h.link.Links().PushRefused(k); ok {
			t.Fatalf("held refused value %+v, want none: the revert of it never happened", v)
		}
		if _, _, r := h.link.Links().Counts(); r != 0 {
			t.Fatalf("refused %d, want 0", r)
		}
	})
}
