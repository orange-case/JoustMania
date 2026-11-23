package flagsmith_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/state"
)

// UC: UC-055 main step 5
// A pull round reads the local state before its fetch: the operator
// goes back to Flagsmith's claude while the round fetches a document that
// already shows the openai in flight. The round adopts the openai it read
// locally before; it must not mark the key in sync, nor queue openai again.
func TestRoundDoesNotActOnLocalValueChangedDuringFetch(t *testing.T) {
	m := newFSModel()
	m.values["decision.backend"] = "claude"
	backendPath := futureAPI + strconv.Itoa(inoutIDs["decision.backend"]) + "/"
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var mu sync.Mutex
	shown := "claude"
	var duringFetch func()
	h := inoutHarness(t, m, map[string]http.HandlerFunc{
		"PATCH " + backendPath: func(w http.ResponseWriter, _ *http.Request) {
			entered <- struct{}{}
			<-release
			flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
		},
		"GET " + flagsmithtest.APIPath + "environment-document/": func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			v, f := shown, duringFetch
			duringFetch = nil
			mu.Unlock()
			if f != nil {
				f()
			}
			flagsmithtest.WriteJSON(w, http.StatusOK, document(m.h.clock.Now().Add(-time.Minute),
				[]map[string]any{fsState("decision.backend", true, v)}))
		},
	}, nil)
	h.online()
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "openai"})
	<-entered // openai in flight
	var putErr error
	mu.Lock()
	shown = "openai" // Flagsmith shows openai before its confirm arrives
	duringFetch = func() {
		_, ferr := h.st.Put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "claude", Writer: "operator"})
		if ferr != nil {
			putErr = ferr
		}
	}
	mu.Unlock()
	h.round()
	if putErr != nil {
		t.Fatal(putErr)
	}
	t.Run("UC-055/main step 5 a round that read the older local value does not mark the key in sync", func(t *testing.T) {
		if p, oos, _ := h.link.Links().Counts(); p != 0 || oos != 1 {
			t.Fatalf("pending %d out_of_sync %d, want 0 1: the stack runs claude, Flagsmith shows openai", p, oos)
		}
	})
	close(release)
	h.waitFor("the openai push ended", func() bool { return len(h.ended(flagsmith.SpanPush)) == 1 })
	t.Run("UC-055/main step 5 the confirm of the older push keeps the key out of sync", func(t *testing.T) {
		if p, oos, _ := h.link.Links().Counts(); p != 0 || oos != 1 {
			t.Fatalf("pending %d out_of_sync %d, want 0 1", p, oos)
		}
		if n := h.requests(http.MethodPatch, backendPath); n != 1 {
			t.Fatalf("%d PATCHes, want 1: the round must not queue the openai it read before the change", n)
		}
		if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "claude" {
			t.Fatalf("decision.backend = %+v, want the operator's claude", v)
		}
	})
}
