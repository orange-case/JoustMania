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

// A pull round reads the local state before its fetch. These journeys make an
// operator write while the document is fetched: the round must not act on the
// local value it read before (UC-055).

// fetchDoc serves the document from shown and runs during (once) while a
// round fetches it, after the round read the local state.
type fetchDoc struct {
	m      *fsModel
	mu     sync.Mutex
	shown  map[string]any
	during func()
}

func (f *fetchDoc) handler(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	var states []map[string]any
	for name, v := range f.shown {
		states = append(states, fsState(name, true, v))
	}
	run := f.during
	f.during = nil
	f.mu.Unlock()
	if run != nil {
		run()
	}
	flagsmithtest.WriteJSON(w, http.StatusOK, document(f.m.h.clock.Now().Add(-time.Minute), states))
}

// show sets what Flagsmith shows and the write to make during the next fetch.
func (f *fetchDoc) show(name string, v any, during func()) {
	f.mu.Lock()
	f.shown[name] = v
	f.during = during
	f.mu.Unlock()
}

// blockedPatch signals entered and waits for release on every PATCH, then
// answers the call's status (409 refuses, beyond the list 200).
func blockedPatch(entered chan<- struct{}, release <-chan struct{}, statuses ...int) http.HandlerFunc {
	var mu sync.Mutex
	calls := 0
	return func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		entered <- struct{}{}
		<-release
		if n <= len(statuses) && statuses[n-1] == http.StatusConflict {
			flagsmithtest.WriteJSON(w, http.StatusConflict, json.RawMessage(`{"detail":"change requests enabled"}`))
			return
		}
		flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
	}
}

// releaseAll closes release once, at the latest at cleanup: a test that fails
// with a PATCH still held must not keep the fake server from closing.
func releaseAll(t *testing.T, release chan struct{}) func() {
	once := sync.OnceFunc(func() { close(release) })
	t.Cleanup(once)
	return once
}

func (h *harness) putDuring(req state.WriteRequest, errp *error) func() {
	req.Writer = "operator"
	return func() {
		if _, ferr := h.st.Put(req); ferr != nil {
			*errp = ferr
		}
	}
}

const docPath = "GET " + flagsmithtest.APIPath + "environment-document/"

// UC: UC-055 main step 5
// The operator goes back to Flagsmith's claude while the round fetches a
// document that already shows the rules in flight: the round adopts the
// rules it read before, but the key stays out of sync and rules is not
// queued again.
func TestRoundDoesNotActOnLocalValueChangedDuringFetch(t *testing.T) {
	m := newFSModel()
	m.values["decision.backend"] = "claude"
	backendPath := futureAPI + strconv.Itoa(inoutIDs["decision.backend"]) + "/"
	entered, release := make(chan struct{}, 1), make(chan struct{})
	doc := &fetchDoc{m: m, shown: map[string]any{"decision.backend": "claude"}}
	h := inoutHarness(t, m, map[string]http.HandlerFunc{
		"PATCH " + backendPath: blockedPatch(entered, release),
		docPath:                doc.handler,
	}, nil)
	confirm := releaseAll(t, release)
	h.online()
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "rules"})
	<-entered // rules in flight
	var putErr error
	doc.show("decision.backend", "rules", h.putDuring(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "claude"}, &putErr))
	h.round()
	if putErr != nil {
		t.Fatal(putErr)
	}
	t.Run("UC-055/main step 5 a round that read the older local value does not mark the key in sync", func(t *testing.T) {
		if p, oos, _ := h.link.Links().Counts(); p != 0 || oos != 1 {
			t.Fatalf("pending %d out_of_sync %d, want 0 1: the stack runs claude, Flagsmith shows rules", p, oos)
		}
	})
	confirm()
	h.waitFor("the rules push ended", func() bool { return len(h.ended(flagsmith.SpanPush)) == 1 })
	t.Run("UC-055/main step 5 the confirm of the older push keeps the key out of sync", func(t *testing.T) {
		if p, oos, _ := h.link.Links().Counts(); p != 0 || oos != 1 {
			t.Fatalf("pending %d out_of_sync %d, want 0 1", p, oos)
		}
		if n := h.requests(http.MethodPatch, backendPath); n != 1 {
			t.Fatalf("%d PATCHes, want 1: the round must not queue the rules it read before the change", n)
		}
		if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "claude" {
			t.Fatalf("decision.backend = %+v, want the operator's claude", v)
		}
	})
}

// UC: UC-055 A4
// Flagsmith refused jev; rules is in flight when the operator re-saves jev
// during a round's fetch of a document that already shows rules. The round
// adopts the rules it read before, but the refusal of jev stays and jev is
// not sent again.
func TestRoundKeepsRefusalOfValueResavedDuringFetch(t *testing.T) {
	m := newFSModel()
	m.values["decision.backend"] = "claude"
	backendPath := futureAPI + strconv.Itoa(inoutIDs["decision.backend"]) + "/"
	entered, release := make(chan struct{}, 1), make(chan struct{})
	doc := &fetchDoc{m: m, shown: map[string]any{"decision.backend": "claude"}}
	h := inoutHarness(t, m, map[string]http.HandlerFunc{
		"PATCH " + backendPath: blockedPatch(entered, release, http.StatusConflict),
		docPath:                doc.handler,
	}, nil)
	releaseAll(t, release)
	h.online()
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev"})
	<-entered // jev in flight
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "rules"})
	pulls := h.pulls()
	release <- struct{}{} // jev refused, rules pending
	<-entered             // rules in flight
	// The refusal wakes a pull of its own; the journey's round comes after it.
	h.waitFor("the pull the refusal woke", func() bool { return h.pulls() > pulls })
	var putErr error
	doc.show("decision.backend", "rules", h.putDuring(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev"}, &putErr))
	h.round()
	if putErr != nil {
		t.Fatal(putErr)
	}
	t.Run("UC-055/A4 a round that read the older local value keeps the refusal of the re-saved jev", func(t *testing.T) {
		if p, oos, r := h.link.Links().Counts(); p != 0 || oos != 1 || r != 1 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 1 1", p, oos, r)
		}
	})
	release <- struct{}{} // rules confirmed
	h.waitFor("the rules push ended", func() bool { return len(h.ended(flagsmith.SpanPush)) == 2 })
	t.Run("UC-055/A4 the confirm of rules keeps the refusal and jev is not sent again", func(t *testing.T) {
		if _, _, r := h.link.Links().Counts(); r != 1 {
			t.Fatalf("refused %d, want 1", r)
		}
		if n := h.requests(http.MethodPatch, backendPath); n != 2 {
			t.Fatalf("%d PATCHes, want 2 (jev refused once, rules)", n)
		}
	})
}

// UC: UC-055 main step 5
// Local, Flagsmith and the last remote value agree when the round reads them;
// the operator changes the value during the fetch. The round finds nothing to
// do but must not mark the key in sync while the new value is pending.
func TestRoundDoesNotMarkInSyncAfterChangeDuringFetch(t *testing.T) {
	m := newFSModel()
	m.values["decision.min_interval_ms"] = 700
	path := futureAPI + strconv.Itoa(inoutIDs["decision.min_interval_ms"]) + "/"
	entered, release := make(chan struct{}, 1), make(chan struct{})
	doc := &fetchDoc{m: m, shown: map[string]any{"decision.min_interval_ms": 700}}
	h := inoutHarness(t, m, map[string]http.HandlerFunc{
		"PATCH " + path: blockedPatch(entered, release),
		docPath:         doc.handler,
	}, func(h *harness) {
		h.put(state.WriteRequest{Set: "agent", Flag: "decision.min_interval_ms", Value: json.RawMessage("700")})
	})
	releaseAll(t, release)
	h.online()
	h.roundAndSettle()
	var putErr error
	doc.show("decision.min_interval_ms", 700, h.putDuring(state.WriteRequest{Set: "agent", Flag: "decision.min_interval_ms", Value: json.RawMessage("900")}, &putErr))
	h.round()
	if putErr != nil {
		t.Fatal(putErr)
	}
	t.Run("UC-055/main step 5 a round that read the older local value leaves the new pending value out of sync", func(t *testing.T) {
		if p, oos, _ := h.link.Links().Counts(); p != 1 || oos != 1 {
			t.Fatalf("pending %d out_of_sync %d, want 1 1: 900 waits to be sent", p, oos)
		}
	})
}
