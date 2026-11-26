package flagsmith_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/joustmania/flag-service/api"
	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/state"
)

// A pull round's write returns before the round records its outcome: these
// journeys make an operator write in between (UC-055).

// afterPut runs a hook once, right after the pulled write of flag returns.
type afterPut struct {
	*api.API
	mu   sync.Mutex
	flag string
	hook func()
}

func (a *afterPut) arm(flag string, hook func()) {
	a.mu.Lock()
	a.flag, a.hook = flag, hook
	a.mu.Unlock()
}

func (a *afterPut) FlagsmithPut(ctx context.Context, req state.WriteRequest) (state.WriteResult, *fserr.Error) {
	res, ferr := a.API.FlagsmithPut(ctx, req)
	a.mu.Lock()
	hook := a.hook
	if req.Flag != a.flag || ferr != nil {
		hook = nil
	}
	if hook != nil {
		a.hook = nil
	}
	a.mu.Unlock()
	if hook != nil {
		hook()
	}
	return res, ferr
}

// afterPutHarness is inoutHarness with the pulled writes wrapped by w.
func afterPutHarness(t *testing.T, m *fsModel, w *afterPut, handlers map[string]http.HandlerFunc, before func(h *harness)) *harness {
	t.Helper()
	return newHarness(t, harnessOpts{
		realOut: true, featureIDs: inoutIDs,
		writer: func(a *api.API, _ *state.State) flagsmith.LocalWriter {
			w.API = a
			return w
		},
		before: func(h *harness) {
			m.h = h
			for pattern, hf := range handlers {
				h.fake.Handle(pattern, hf)
			}
			if before != nil {
				before(h)
			}
			m.publish()
		},
	})
}

// UC: UC-055 main step 5
// A round applies Flagsmith's 800; the operator saves 900 before the round
// records the write. 900 is pending, so the key is not in sync.
func TestAppliedWriteDoesNotMarkInSyncOverLaterChange(t *testing.T) {
	m := newFSModel()
	m.values["decision.min_interval_ms"] = 700
	path := futureAPI + strconv.Itoa(inoutIDs["decision.min_interval_ms"]) + "/"
	entered, release := make(chan struct{}, 1), make(chan struct{})
	w := &afterPut{}
	h := afterPutHarness(t, m, w, map[string]http.HandlerFunc{"PATCH " + path: blockedPatch(entered, release)}, func(h *harness) {
		h.put(state.WriteRequest{Set: "agent", Flag: "decision.min_interval_ms", Value: json.RawMessage("700")})
	})
	releaseAll(t, release)
	h.online()
	var putErr error
	w.arm("decision.min_interval_ms", h.putDuring(state.WriteRequest{Set: "agent", Flag: "decision.min_interval_ms", Value: json.RawMessage("900")}, &putErr))
	m.set("decision.min_interval_ms", 800)
	h.round()
	if putErr != nil {
		t.Fatal(putErr)
	}
	t.Run("UC-055/main step 5 the applied write does not mark the key in sync while 900 is pending", func(t *testing.T) {
		if p, oos, _ := h.link.Links().Counts(); p != 1 || oos != 1 {
			t.Fatalf("pending %d out_of_sync %d, want 1 1", p, oos)
		}
	})
}

// UC: UC-055 A4
// Flagsmith refused jev; the round reverts the stack to claude, and the
// operator re-saves jev before the round records the revert. The re-save is
// held as refused, not sent again.
func TestRevertHoldsRefusedValueResavedDuringWrite(t *testing.T) {
	m := newFSModel()
	m.values["decision.backend"] = "claude"
	backendPath := futureAPI + strconv.Itoa(inoutIDs["decision.backend"]) + "/"
	w := &afterPut{}
	h := afterPutHarness(t, m, w, map[string]http.HandlerFunc{
		"PATCH " + backendPath: answer(http.StatusConflict, `{"detail":"change requests enabled"}`),
	}, nil)
	h.online()
	var putErr error
	w.arm("decision.backend", h.putDuring(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev"}, &putErr))
	pulls := h.pulls()
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev"})
	h.waitFor("the revert round", func() bool { return h.pulls() > pulls })
	if putErr != nil {
		t.Fatal(putErr)
	}
	h.roundAndSettle()
	t.Run("UC-055/A4 step 2 a re-save during the revert is not sent", func(t *testing.T) {
		if n := h.requests(http.MethodPatch, backendPath); n != 1 {
			t.Fatalf("%d PATCHes of decision.backend, want 1 (jev refused once)", n)
		}
		if _, _, r := h.link.Links().Counts(); r != 1 {
			t.Fatalf("refused %d, want 1: the refusal of jev stays reported", r)
		}
	})
}

// UC: UC-055 A4
// A round applies Flagsmith's rules; before it records the write, the
// operator saves jev and Flagsmith refuses it. The round must not end that
// refusal, so pull rounds do not send jev again.
func TestAppliedWriteKeepsLaterPushRefusal(t *testing.T) {
	m := newFSModel()
	m.values["decision.backend"] = "claude"
	backendPath := futureAPI + strconv.Itoa(inoutIDs["decision.backend"]) + "/"
	w := &afterPut{}
	h := afterPutHarness(t, m, w, map[string]http.HandlerFunc{
		"PATCH " + backendPath: answer(http.StatusConflict, `{"detail":"change requests enabled"}`),
	}, nil)
	h.online()
	var putErr error
	resave := h.putDuring(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev"}, &putErr)
	w.arm("decision.backend", func() {
		resave()
		h.waitFor("the refusal of jev", func() bool {
			v, ok := h.link.Links().PushRefused(backendKey)
			return ok && v.Variant == "jev"
		})
	})
	m.set("decision.backend", "rules")
	h.round()
	if putErr != nil {
		t.Fatal(putErr)
	}
	for range 2 {
		h.roundAndSettle()
	}
	t.Run("UC-055/A4 step 2 the applied write keeps the later refusal and jev is not sent again", func(t *testing.T) {
		if n := h.requests(http.MethodPatch, backendPath); n != 1 {
			t.Fatalf("%d PATCHes of decision.backend, want 1 (jev refused once)", n)
		}
		if _, _, r := h.link.Links().Counts(); r != 1 {
			t.Fatalf("refused %d, want 1", r)
		}
	})
}
