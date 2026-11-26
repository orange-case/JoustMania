package flagsmith_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joustmania/flag-service/api"
	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/state"
)

// Only changes MADE while the link was not connected are discarded (#2621,
// review of #2627, maintainer rulings A and B). The real out side runs here.

// sentValue reports whether any Admin API write carried value in its body.
func sentValue(h *harness, value string) bool {
	for _, m := range []string{http.MethodPatch, http.MethodPost, http.MethodPut} {
		for _, r := range h.fake.RequestsTo(m, "") {
			if bytes.Contains(r.Body, []byte(value)) {
				return true
			}
		}
	}
	return false
}

// UC: UC-054 A2 BR-007
// UC: UC-055 A3
// A write made while online whose push fails (a 502 takes the link down) is
// not discarded at the reconnect: it is sent, and the stack keeps it.
func TestOnlineWriteInFlightSurvivesBlip(t *testing.T) {
	m := newFSModel()
	m.values["decision.min_interval_ms"] = 700
	path := futureAPI + strconv.Itoa(inoutIDs["decision.min_interval_ms"]) + "/"
	h := inoutHarness(t, m, map[string]http.HandlerFunc{"PATCH " + path: func(w http.ResponseWriter, _ *http.Request) {
		m.set("decision.min_interval_ms", 900) // Flagsmith holds the accepted value
		flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
	}}, nil)
	h.online()
	h.fake.Fail(http.StatusBadGateway) // the PATCH of the online write gets a 502
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.min_interval_ms", Value: json.RawMessage("900")})
	h.waitFor("link offline (push failed)", func() bool { return h.link.Mode() == flagsmith.ModeOffline })
	h.fake.Fail(0)
	h.waitFor("link online again", func() bool { return h.link.Mode() == flagsmith.ModeOnline })
	h.waitFor("the online write sent", func() bool { p, _, _ := h.link.Links().Counts(); return sentValue(h, "900") && p == 0 })
	h.roundAndSettle()
	if v, _ := h.local("agent", "decision.min_interval_ms", flagsmith.ScopeStack); string(v.Value) != "900" {
		t.Fatalf("UC-054/A2 decision.min_interval_ms = %s after the blip, want the online write 900 kept and sent", v.Value)
	}
}

// UC: UC-054 A3 A2 BR-007
// Writes made before the first successful connect (an offline boot) are
// discarded at the first connect like at a reconnect: Flagsmith's value is
// applied, with none the baseline; nothing of them is sent.
func TestOfflineBootThenConnectDiscards(t *testing.T) {
	graceKillSwitch(t)
	h := newHarness(t, harnessOpts{realOut: true, featureIDs: inoutIDs,
		before: func(h *harness) {
			h.setDoc([]map[string]any{fsState("decision.min_interval_ms", true, 700)})
			h.fake.Fail(http.StatusBadGateway)
		},
	})
	h.waitFor("offline", func() bool { return h.link.Mode() == flagsmith.ModeOffline })
	h.put(state.WriteRequest{Set: "agent", Flag: "experiment_completed_grace_seconds", Value: json.RawMessage("3")})
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.min_interval_ms", Value: json.RawMessage("900")})
	h.fake.Fail(0)
	h.waitFor("online", func() bool { return h.link.Mode() == flagsmith.ModeOnline })
	h.round()
	h.round()
	if v, _ := h.local("agent", "experiment_completed_grace_seconds", flagsmith.ScopeStack); v.Present {
		t.Fatalf("UC-054/A3 experiment_completed_grace_seconds = %+v after the first connect, want the baseline (Flagsmith holds none)", v)
	}
	if v, _ := h.local("agent", "decision.min_interval_ms", flagsmith.ScopeStack); string(v.Value) != "700" {
		t.Fatalf("UC-054/A3 decision.min_interval_ms = %s after the first connect, want Flagsmith's 700", v.Value)
	}
	if sentValue(h, `"900"`) || sentValue(h, `"value":3`) || sentValue(h, `"3"`) {
		t.Fatal("UC-054/A3 a change made before the first connect was sent to Flagsmith")
	}
}

// deferWhileArmed answers every pulled write of one flag with 503
// no_sync_stream (UC-054 A5) while armed.
type deferWhileArmed struct {
	*api.API
	flag  string
	armed atomic.Bool
}

func (w *deferWhileArmed) FlagsmithPut(ctx context.Context, req state.WriteRequest) (state.WriteResult, *fserr.Error) {
	if req.Flag == w.flag && w.armed.Load() {
		return state.WriteResult{}, &fserr.Error{Status: http.StatusServiceUnavailable, Reason: fserr.NoSyncStream}
	}
	return w.API.FlagsmithPut(ctx, req)
}

// UC: UC-054 A2 A5
// A key whose Flagsmith apply is deferred across the drop (skipped by the
// reconnect round) still discards the offline change: it is never sent, and
// Flagsmith's value lands once the apply goes through.
func TestDeferredKeyOfflineChangeNotSent(t *testing.T) {
	w := &deferWhileArmed{flag: "decision.min_interval_ms"}
	h := newHarness(t, harnessOpts{realOut: true, featureIDs: inoutIDs,
		writer: func(a *api.API, _ *state.State) flagsmith.LocalWriter { w.API = a; return w },
		before: func(h *harness) { h.setDoc([]map[string]any{fsState("decision.min_interval_ms", true, 700)}) },
	})
	h.online()
	w.armed.Store(true)
	h.setDoc([]map[string]any{fsState("decision.min_interval_ms", true, 750)})
	for range 5 {
		h.round()
	}
	h.fake.Fail(http.StatusBadGateway)
	h.round()
	h.waitFor("link offline", func() bool { return h.link.Mode() == flagsmith.ModeOffline })
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.min_interval_ms", Value: json.RawMessage("900")})
	h.fake.Fail(0)
	h.waitFor("online", func() bool { return h.link.Mode() == flagsmith.ModeOnline })
	h.round()
	w.armed.Store(false)
	h.round()
	h.round()
	if sentValue(h, "900") {
		t.Fatal("UC-054/A2 the offline change 900 was sent to Flagsmith after the reconnect")
	}
	if v, _ := h.local("agent", "decision.min_interval_ms", flagsmith.ScopeStack); string(v.Value) != "750" {
		t.Fatalf("UC-054/A2 decision.min_interval_ms = %s, want Flagsmith's 750 once the apply goes through", v.Value)
	}
}

// UC: UC-055 A4
// A stack value Flagsmith refuses is reverted although the document did not
// change: the refusal forces a full pull, so a conditional pull answered 304
// never leaves the refused value in force (#2621, CI run 43865190304).
func TestRefusedStackValueRevertedWithoutDocumentChange(t *testing.T) {
	updated := time.Date(2026, 11, 15, 11, 59, 0, 0, time.UTC)
	backendPath := futureAPI + strconv.Itoa(inoutIDs["decision.backend"]) + "/"
	h := newHarness(t, harnessOpts{realOut: true, featureIDs: inoutIDs,
		before: func(h *harness) {
			h.fake.Handle("PATCH "+backendPath, answer(http.StatusConflict, `{"detail":"change requests enabled"}`))
			// Last-Modified as the link sends it: an unchanged document answers 304.
			h.fake.SetDocument(document(updated, []map[string]any{fsState("decision.backend", true, "claude")}), updated.Format(http.TimeFormat))
		},
	})
	h.online()
	h.round()
	h.round() // settled: the next pulls are conditional
	if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "claude" {
		t.Fatalf("setup: decision.backend = %+v, want claude", v)
	}
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev"})
	h.waitFor("the push refused", func() bool {
		_, side := h.link.Links().Refusal(backendKey)
		return side == flagsmith.SideRemote
	})
	h.round()
	h.round()
	if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "claude" {
		t.Fatalf("UC-055/A4 decision.backend = %+v with an unchanged document, want the refused jev reverted to claude", v)
	}
}
