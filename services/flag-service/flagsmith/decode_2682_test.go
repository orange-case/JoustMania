package flagsmith_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/state"
)

// The edge document shows an integer identity override as a JSON string
// (a pushed 3 reads back "3", verified on the SaaS 2026-11-18, #2682): the
// pull decodes a document value by the registry flag's type, so the link's
// own push reads back unchanged and a person's "7" is the number 7.

const (
	g1Identity = "game:g1"
	sensFlag   = "global_sensitivity_override"
)

var sensG1 = flagsmith.Key{Set: "interventions", Flag: sensFlag, Scope: g1Identity}

// patchText is the value of a stack PATCH (the __future__ API sends text).
type patchText struct {
	Value string `json:"value"`
}

type patchDefault struct {
	Value patchText `json:"value"`
}

type patchBody struct {
	EnvironmentDefault patchDefault `json:"environment_default"`
}

// storePatch is a stack PATCH handler that stores the value in the model as
// the document shows it: as text (the worst case; the SaaS's typing of an
// integer environment value could not be observed).
func storePatch(m *fsModel, name string) (http.HandlerFunc, func() int) {
	calls := 0
	return func(w http.ResponseWriter, r *http.Request) {
			var body patchBody
			_ = json.NewDecoder(r.Body).Decode(&body)
			calls++
			m.set(name, body.EnvironmentDefault.Value.Value)
			flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
		}, func() int {
			return calls
		}
}

// wantInSync fails t unless nothing is pending, out of sync or refused, and
// none of keys holds a refusal.
func (h *harness) wantInSync(t *testing.T, keys ...flagsmith.Key) {
	t.Helper()
	if p, oos, r := h.link.Links().Counts(); p != 0 || oos != 0 || r != 0 {
		t.Fatalf("pending %d out_of_sync %d refused %d, want 0 0 0", p, oos, r)
	}
	for _, k := range keys {
		if r, _ := h.link.Links().Refusal(k); r != "" {
			t.Fatalf("%v refused %q, want none", k, r)
		}
	}
}

// wantLocal fails t unless the local value of set/flag/scope is want.
func (h *harness) wantLocal(t *testing.T, set, flag, scope, want string) {
	t.Helper()
	if v, _ := h.local(set, flag, scope); string(v.Value) != want {
		t.Fatalf("%s/%s on %s = %s, want %s", set, flag, scope, v.Value, want)
	}
}

// UC: UC-054 A4 BR-004
// UC: UC-055 main step 3
// The link's own integer pushes (a stack kill-switch value and a game
// override) read back as strings: the rounds apply nothing, refuse nothing
// and send nothing back.
func TestOwnIntegerPushReadBackAsString(t *testing.T) {
	graceKillSwitch(t)
	m := newFSModel()
	gracePath := futureAPI + strconv.Itoa(inoutIDs["experiment_completed_grace_seconds"]) + "/"
	patch, patches := storePatch(m, "experiment_completed_grace_seconds")
	h := inoutHarness(t, m, map[string]http.HandlerFunc{"PATCH " + gracePath: patch}, nil)
	h.online()
	if _, ferr := h.st.RegisterGame(state.Game{ID: "g1", CoordinatorInstance: "c1", Kind: "shadow"}); ferr != nil {
		t.Fatal(ferr)
	}
	h.put(state.WriteRequest{Set: "agent", Flag: "experiment_completed_grace_seconds", Value: json.RawMessage("7")})
	h.put(state.WriteRequest{Set: "interventions", Flag: sensFlag, Scope: state.Scope{GameID: "g1"}, Value: json.RawMessage("3")})
	h.waitFor("both values confirmed", func() bool {
		_, okStack := h.link.Links().Remote(graceKey)
		_, okGame := h.link.Links().Remote(sensG1)
		return okStack && okGame
	})
	// Flagsmith shows the override as the edge document does: "3".
	m.setOverride(g1Identity, sensFlag, "3")
	for range 3 {
		h.roundAndSettle()
	}
	t.Run("UC-054/A4 the string \"3\" and \"7\" are the pushed 3 and 7: nothing refused, nothing out of sync", func(t *testing.T) {
		h.wantInSync(t, graceKey, sensG1)
	})
	t.Run("UC-054/BR-004 nothing is applied and nothing is sent back", func(t *testing.T) {
		h.wantLocal(t, "agent", "experiment_completed_grace_seconds", flagsmith.ScopeStack, "7")
		h.wantLocal(t, "interventions", sensFlag, g1Identity, "3")
		if n := patches(); n != 1 {
			t.Fatalf("%d stack PATCHes, want 1 (no echo)", n)
		}
		if n := h.requests(http.MethodPost, g1Overrides) + h.requests(http.MethodPut, g1Overrides); n != 1 {
			t.Fatalf("%d override writes, want 1 (no echo)", n)
		}
	})
}

// UC: UC-054 main step 5 A4
// A person's "7" on an integer kill switch (the stand-in
// agent/experiment_completed_grace_seconds) applies as the number 7, at the
// start reconcile and in a round.
func TestPersonIntegerTextApplies(t *testing.T) {
	graceKillSwitch(t)
	m := newFSModel()
	m.values["experiment_completed_grace_seconds"] = "5" // in Flagsmith before the start
	h := inoutHarness(t, m, nil, nil)
	h.online()
	t.Run("UC-054/A4 the start reconcile applies \"5\" as 5", func(t *testing.T) {
		if v, _ := h.local("agent", "experiment_completed_grace_seconds", flagsmith.ScopeStack); string(v.Value) != "5" {
			t.Fatalf("experiment_completed_grace_seconds = %+v, want 5", v)
		}
	})
	m.set("experiment_completed_grace_seconds", "7")
	h.roundAndSettle()
	t.Run("UC-054/main step 5 a round applies \"7\" as 7", func(t *testing.T) {
		if v, _ := h.local("agent", "experiment_completed_grace_seconds", flagsmith.ScopeStack); string(v.Value) != "7" {
			t.Fatalf("experiment_completed_grace_seconds = %+v, want 7", v)
		}
		if p, oos, r := h.link.Links().Counts(); p != 0 || oos != 0 || r != 0 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 0 0", p, oos, r)
		}
	})
	m.set("experiment_completed_grace_seconds", "5.0")
	h.roundAndSettle()
	t.Run("UC-054/A4 a whole-number \"5.0\" on an integer flag applies as 5", func(t *testing.T) {
		if v, _ := h.local("agent", "experiment_completed_grace_seconds", flagsmith.ScopeStack); string(v.Value) != "5" {
			t.Fatalf("experiment_completed_grace_seconds = %s, want 5", v.Value)
		}
		if r, _ := h.link.Links().Refusal(graceKey); r != "" {
			t.Fatalf("refusal %q, want none", r)
		}
	})
	m.set("experiment_completed_grace_seconds", "7.5")
	h.roundAndSettle()
	t.Run("UC-054/A4 \"7.5\" on an integer flag is flagsmith_type_mismatch", func(t *testing.T) {
		if r, _ := h.link.Links().Refusal(graceKey); r != "flagsmith_type_mismatch" {
			t.Fatalf("refusal %q, want flagsmith_type_mismatch", r)
		}
		if v, _ := h.local("agent", "experiment_completed_grace_seconds", flagsmith.ScopeStack); string(v.Value) != "5" {
			t.Fatalf("experiment_completed_grace_seconds = %+v, want 5 kept", v)
		}
	})
}
