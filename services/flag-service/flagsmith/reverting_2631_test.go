package flagsmith_test

import (
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
		"PATCH " + backendPath: func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			rf := refuse
			mu.Unlock()
			if rf {
				flagsmithtest.WriteJSON(w, http.StatusConflict, map[string]string{"detail": "change requests enabled"})
				return
			}
			m.set("decision.backend", "openai") // Flagsmith holds the accepted value
			flagsmithtest.WriteJSON(w, http.StatusOK, map[string]string{})
		},
	}, nil)
	h.online()
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev"})
	h.waitFor("the push refused", func() bool {
		_, side := h.link.Links().Refusal(backendKey)
		return side == flagsmith.SideRemote
	})
	// Flagsmith's value is not a variant here: the revert to it is refused
	// locally and the refused jev stays in force.
	m.set("decision.backend", 42)
	h.roundAndSettle()
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
