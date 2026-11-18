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

var sensG1 = flagsmith.Key{Set: "interventions", Flag: "global_sensitivity_override", Scope: "game:g1"}

// storePatch is a stack PATCH handler that stores the value in the model as
// the document shows it: as text (the worst case; the SaaS's typing of an
// integer environment value could not be observed).
func storePatch(m *fsModel, name string) (http.HandlerFunc, func() int) {
	calls := 0
	return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				EnvironmentDefault struct {
					Value struct {
						Value string `json:"value"`
					} `json:"value"`
				} `json:"environment_default"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			calls++
			m.set(name, body.EnvironmentDefault.Value.Value)
			flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
		}, func() int {
			return calls
		}
}

// UC: UC-054 A4 BR-004
// UC: UC-055 main step 3
// The link's own integer pushes (a stack kill-switch value and a game
// override) read back as strings: the rounds apply nothing, refuse nothing
// and send nothing back.
func TestOwnIntegerPushReadBackAsString(t *testing.T) {
	m := newFSModel()
	maxPath := futureAPI + strconv.Itoa(inoutIDs["experiment_max_games"]) + "/"
	patch, patches := storePatch(m, "experiment_max_games")
	h := inoutHarness(t, m, map[string]http.HandlerFunc{"PATCH " + maxPath: patch}, nil)
	h.online()
	if _, ferr := h.st.RegisterGame(state.Game{ID: "g1", CoordinatorInstance: "c1", Kind: "shadow"}); ferr != nil {
		t.Fatal(ferr)
	}
	h.put(state.WriteRequest{Set: "agent", Flag: "experiment_max_games", Value: json.RawMessage("7")})
	h.put(state.WriteRequest{Set: "interventions", Flag: "global_sensitivity_override", Scope: state.Scope{GameID: "g1"}, Value: json.RawMessage("3")})
	h.waitFor("both values confirmed", func() bool {
		_, okStack := h.link.Links().Remote(maxGamesKey)
		_, okGame := h.link.Links().Remote(sensG1)
		return okStack && okGame
	})
	// Flagsmith shows the override as the edge document does: "3".
	m.setOverride("game:g1", "global_sensitivity_override", "3")
	for range 3 {
		h.roundAndSettle()
	}
	t.Run("UC-054/A4 the string \"3\" and \"7\" are the pushed 3 and 7: nothing refused, nothing out of sync", func(t *testing.T) {
		if p, oos, r := h.link.Links().Counts(); p != 0 || oos != 0 || r != 0 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 0 0", p, oos, r)
		}
		for _, k := range []flagsmith.Key{maxGamesKey, sensG1} {
			if r, _ := h.link.Links().Refusal(k); r != "" {
				t.Fatalf("%v refused %q, want none", k, r)
			}
		}
	})
	t.Run("UC-054/BR-004 nothing is applied and nothing is sent back", func(t *testing.T) {
		if v, _ := h.local("agent", "experiment_max_games", flagsmith.ScopeStack); string(v.Value) != "7" {
			t.Fatalf("experiment_max_games = %s, want the local 7", v.Value)
		}
		if v, _ := h.local("interventions", "global_sensitivity_override", "game:g1"); string(v.Value) != "3" {
			t.Fatalf("global_sensitivity_override on game:g1 = %s, want the local 3", v.Value)
		}
		if n := patches(); n != 1 {
			t.Fatalf("%d stack PATCHes, want 1 (no echo)", n)
		}
		if n := h.requests(http.MethodPost, g1Overrides) + h.requests(http.MethodPut, g1Overrides); n != 1 {
			t.Fatalf("%d override writes, want 1 (no echo)", n)
		}
	})
}

// UC: UC-054 main step 5 A4
// A person's "7" on an integer flag (a kill switch, agent/experiment_max_games)
// applies as the number 7, at the start reconcile and in a round.
func TestPersonIntegerTextApplies(t *testing.T) {
	m := newFSModel()
	m.values["experiment_max_games"] = "5" // in Flagsmith before the start
	h := inoutHarness(t, m, nil, nil)
	h.online()
	t.Run("UC-054/A4 the start reconcile applies \"5\" as 5", func(t *testing.T) {
		if v, _ := h.local("agent", "experiment_max_games", flagsmith.ScopeStack); string(v.Value) != "5" {
			t.Fatalf("experiment_max_games = %+v, want 5", v)
		}
	})
	m.set("experiment_max_games", "7")
	h.roundAndSettle()
	t.Run("UC-054/main step 5 a round applies \"7\" as 7", func(t *testing.T) {
		if v, _ := h.local("agent", "experiment_max_games", flagsmith.ScopeStack); string(v.Value) != "7" {
			t.Fatalf("experiment_max_games = %+v, want 7", v)
		}
		if p, oos, r := h.link.Links().Counts(); p != 0 || oos != 0 || r != 0 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 0 0", p, oos, r)
		}
	})
	m.set("experiment_max_games", "7.5")
	h.roundAndSettle()
	t.Run("UC-054/A4 \"7.5\" on an integer flag is flagsmith_type_mismatch", func(t *testing.T) {
		if r, _ := h.link.Links().Refusal(maxGamesKey); r != "flagsmith_type_mismatch" {
			t.Fatalf("refusal %q, want flagsmith_type_mismatch", r)
		}
		if v, _ := h.local("agent", "experiment_max_games", flagsmith.ScopeStack); string(v.Value) != "7" {
			t.Fatalf("experiment_max_games = %+v, want 7 kept", v)
		}
	})
}
