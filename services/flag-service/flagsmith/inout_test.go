package flagsmith_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/state"
)

// Journeys with the in side (UC-054) and the out side (UC-055) together: a
// pull round must not send again a value Flagsmith refused (UC-055 A4, #2604).

// Feature ids of the flags these journeys push.
var inoutIDs = map[string]int{
	"decision.backend":         101,
	"decision.min_interval_ms": 102,
	"experiment_max_games":     103,
	"volume_override":          104,
}

const (
	futureAPI     = "/api/__future__/environments/envkey/features/"
	overridesPath = flagsmithtest.APIPath + "environments/envkey/edge-identities-featurestates"
)

var (
	backendKey = flagsmith.Key{Set: "agent", Flag: "decision.backend", Scope: flagsmith.ScopeStack}
	nudgeKey   = minIntervalKey
)

// fsModel is what the fake Flagsmith holds: environment values by feature
// name and identity overrides; publish serves it as the document.
type fsModel struct {
	h      *harness
	mu     sync.Mutex
	values map[string]any
	ids    map[string]map[string]any
}

func newFSModel() *fsModel {
	return &fsModel{values: map[string]any{}, ids: map[string]map[string]any{}}
}

func (m *fsModel) set(name string, v any) {
	m.mu.Lock()
	m.values[name] = v
	m.mu.Unlock()
	m.publish()
}

func (m *fsModel) setOverride(identifier, name string, v any) {
	m.mu.Lock()
	if m.ids[identifier] == nil {
		m.ids[identifier] = map[string]any{}
	}
	m.ids[identifier][name] = v
	m.mu.Unlock()
	m.publish()
}

func (m *fsModel) publish() {
	m.mu.Lock()
	defer m.mu.Unlock()
	var states []map[string]any
	for name, v := range m.values {
		states = append(states, fsState(name, true, v))
	}
	var ovs []override
	for id, vs := range m.ids {
		o := override{identifier: id}
		for name, v := range vs {
			o.states = append(o.states, fsState(name, true, v))
		}
		ovs = append(ovs, o)
	}
	m.h.setDoc(states, ovs...)
}

// inoutHarness starts the link with the real out side. Flagsmith accepts the
// nudge flag (and updates the document like Flagsmith would); handlers add
// the other answers. before runs before the link starts.
func inoutHarness(t *testing.T, m *fsModel, handlers map[string]http.HandlerFunc, before func(h *harness)) *harness {
	t.Helper()
	return newHarness(t, harnessOpts{realOut: true, featureIDs: inoutIDs, before: func(h *harness) {
		m.h = h
		h.fake.Handle("PATCH "+futureAPI+strconv.Itoa(inoutIDs["decision.min_interval_ms"])+"/", func(w http.ResponseWriter, _ *http.Request) {
			flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
		})
		h.fake.Handle("POST "+flagsmithtest.APIPath+"environments/envkey/edge-identities/", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Identifier string `json:"identifier"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			flagsmithtest.WriteJSON(w, http.StatusCreated, map[string]any{"identifier": body.Identifier, "identity_uuid": "u-" + body.Identifier})
		})
		for pattern, hf := range handlers {
			h.fake.Handle(pattern, hf)
		}
		if before != nil {
			before(h)
		}
		m.publish()
	}})
}

// answer is a handler that always answers status with body.
func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		flagsmithtest.WriteJSON(w, status, json.RawMessage(body))
	}
}

func (h *harness) put(req state.WriteRequest) {
	h.t.Helper()
	req.Writer = "operator"
	if _, ferr := h.st.Put(req); ferr != nil {
		h.t.Fatal(ferr)
	}
}

func (h *harness) requests(method, prefix string) int {
	return len(h.fake.RequestsTo(method, prefix))
}

// roundAndNudge runs one pull round, then makes another local change that
// wakes the sender, as any local write in a live stack does, and waits until
// Flagsmith confirmed it. The sender sends in first-change order, so whatever
// the round queued has been sent by then.
func (h *harness) roundAndNudge(m *fsModel, i int) {
	h.t.Helper()
	h.round()
	n := 700 + 10*i
	m.set("decision.min_interval_ms", n)
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.min_interval_ms", Value: json.RawMessage(strconv.Itoa(n))})
	h.waitFor("the nudge confirmed", func() bool {
		r, ok := h.link.Links().Remote(nudgeKey)
		return ok && string(r.Value) == strconv.Itoa(n)
	})
}

// UC: UC-055 A4
// A push Flagsmith refused (409, change requests) is sent once: pull rounds
// that find the local value differing from Flagsmith do not send it again, a
// re-save of the same value does not either, a different local value does.
func TestRefusedPushNotResentByPolls(t *testing.T) {
	m := newFSModel()
	m.values["decision.backend"] = "claude"
	backendPath := futureAPI + strconv.Itoa(inoutIDs["decision.backend"]) + "/"
	h := inoutHarness(t, m, map[string]http.HandlerFunc{
		"PATCH " + backendPath: answer(http.StatusConflict, `{"detail":"change requests enabled"}`),
	}, nil)
	h.online()
	if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "claude" {
		t.Fatalf("setup: decision.backend = %+v, want claude applied", v)
	}
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev"})
	h.waitFor("the push refused", func() bool {
		r, side := h.link.Links().Refusal(backendKey)
		return r == flagsmith.ReasonChangeRequestRequired && side == flagsmith.SideRemote
	})
	for i := range 3 {
		h.roundAndNudge(m, i)
	}
	t.Run("UC-055/A4 step 2 pull rounds do not send the refused value again", func(t *testing.T) {
		if n := h.requests(http.MethodPatch, backendPath); n != 1 {
			t.Fatalf("%d PATCHes of decision.backend after 3 rounds, want 1", n)
		}
		if p, oos, r := h.link.Links().Counts(); p != 0 || oos != 1 || r != 1 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 1 1", p, oos, r)
		}
		if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "jev" {
			t.Fatalf("decision.backend = %+v, want the local jev in force (A4 step 1)", v)
		}
		if n := h.counter(flagsmith.MetricPushes, "reason", flagsmith.ReasonChangeRequestRequired); n != 1 {
			t.Fatalf("pushes_total{reason=change_request_required} = %d, want 1", n)
		}
		if posts := h.healthPosts(); len(posts) != 0 {
			t.Fatalf("health posts %v, want none (a push refusal is not a pulled-change refusal)", posts)
		}
	})
	t.Run("UC-055/A4 step 2 a re-save of the refused value does not retry", func(t *testing.T) {
		h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev"})
		h.roundAndNudge(m, 3)
		if n := h.requests(http.MethodPatch, backendPath); n != 1 {
			t.Fatalf("%d PATCHes after a re-save of jev, want 1", n)
		}
	})
	t.Run("UC-055/A4 step 2 a different local value is sent", func(t *testing.T) {
		h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "openai"})
		h.waitFor("the second push", func() bool { return h.requests(http.MethodPatch, backendPath) == 2 })
		h.roundAndNudge(m, 4)
		if n := h.requests(http.MethodPatch, backendPath); n != 2 {
			t.Fatalf("%d PATCHes after a different value and a round, want 2", n)
		}
	})
}

// UC: UC-054 BR-005
// A refused push of a tighter kill-switch value never loosens it and is not
// sent again: a looser document value is a conflict the local value wins
// (out of sync, nothing sent), a tighter one is applied.
func TestRefusedKillSwitchPushNeverLoosens(t *testing.T) {
	m := newFSModel()
	m.values["experiment_max_games"] = 100
	maxPath := futureAPI + strconv.Itoa(inoutIDs["experiment_max_games"]) + "/"
	h := inoutHarness(t, m, map[string]http.HandlerFunc{
		"PATCH " + maxPath: answer(http.StatusConflict, `{"detail":"change requests enabled"}`),
	}, func(h *harness) {
		h.put(state.WriteRequest{Set: "agent", Flag: "experiment_max_games", Value: json.RawMessage("100")})
	})
	h.online()
	if r, ok := h.link.Links().Remote(maxGamesKey); !ok || string(r.Value) != "100" {
		t.Fatalf("setup: remote %+v %v, want 100 adopted", r, ok)
	}
	h.put(state.WriteRequest{Set: "agent", Flag: "experiment_max_games", Value: json.RawMessage("10")})
	h.waitFor("the push refused", func() bool {
		_, side := h.link.Links().Refusal(maxGamesKey)
		return side == flagsmith.SideRemote
	})
	for i := range 3 {
		h.roundAndNudge(m, i)
	}
	if n := h.requests(http.MethodPatch, maxPath); n != 1 {
		t.Fatalf("UC-055/A4 %d PATCHes of experiment_max_games after 3 rounds, want 1", n)
	}
	t.Run("UC-054/BR-005 a looser document value keeps the refused local value, out of sync, unsent", func(t *testing.T) {
		m.set("experiment_max_games", 500)
		h.roundAndNudge(m, 3)
		if v, _ := h.local("agent", "experiment_max_games", flagsmith.ScopeStack); string(v.Value) != "10" {
			t.Fatalf("experiment_max_games = %+v, want 10 kept", v)
		}
		if n := h.counter(flagsmith.MetricConflicts, "resolution", flagsmith.ResolutionTightened); n != 1 {
			t.Fatalf("conflicts{resolution=tightened} = %d, want 1", n)
		}
		if n := h.requests(http.MethodPatch, maxPath); n != 1 {
			t.Fatalf("%d PATCHes, want 1: the refused 10 is not sent again", n)
		}
		if _, oos, _ := h.link.Links().Counts(); oos != 1 {
			t.Fatalf("out_of_sync %d, want 1 (the local 10 is not in Flagsmith)", oos)
		}
	})
	t.Run("UC-054/BR-005 a tighter document value is applied and ends the refusal", func(t *testing.T) {
		m.set("experiment_max_games", 5)
		h.roundAndNudge(m, 4)
		if v, _ := h.local("agent", "experiment_max_games", flagsmith.ScopeStack); string(v.Value) != "5" {
			t.Fatalf("experiment_max_games = %+v, want the tighter 5 applied", v)
		}
		if p, oos, r := h.link.Links().Counts(); p != 0 || oos != 0 || r != 0 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 0 0", p, oos, r)
		}
		if n := h.requests(http.MethodPatch, maxPath); n != 1 {
			t.Fatalf("%d PATCHes, want 1", n)
		}
	})
}

// UC: UC-055 A4
// A refused identity override (PUT) and a refused removal of one (DELETE)
// are sent once each, whatever the pull rounds find.
func TestRefusedOverrideAndDeleteNotResent(t *testing.T) {
	m := newFSModel()
	h := inoutHarness(t, m, map[string]http.HandlerFunc{
		"PUT " + overridesPath:    answer(http.StatusBadRequest, `{"detail":"invalid value"}`),
		"DELETE " + overridesPath: answer(http.StatusForbidden, `{"detail":"forbidden"}`),
	}, nil)
	h.online()
	if _, ferr := h.st.RegisterGame(state.Game{ID: "g1", CoordinatorInstance: "c1", Kind: "shadow", Serials: []string{"MOCK0001"}}); ferr != nil {
		t.Fatal(ferr)
	}
	g1 := state.Scope{GameID: "g1"}
	h.put(state.WriteRequest{Set: "interventions", Flag: "volume_override", Scope: g1, Value: json.RawMessage("0.5")})
	h.waitFor("the override refused", func() bool {
		_, side := h.link.Links().Refusal(volumeOverrideG1)
		return side == flagsmith.SideRemote
	})
	for i := range 3 {
		h.roundAndNudge(m, i)
	}
	t.Run("UC-055/A4 a refused override is not sent again (no override in Flagsmith)", func(t *testing.T) {
		if n := h.requests(http.MethodPut, overridesPath); n != 1 {
			t.Fatalf("%d PUTs after 3 rounds, want 1", n)
		}
	})
	t.Run("UC-055/A4 a refused removal is not sent again", func(t *testing.T) {
		// Flagsmith gets the same override another way: both sides agree.
		m.setOverride("game:g1", "volume_override", "0.5")
		h.roundAndNudge(m, 3)
		if r, ok := h.link.Links().Remote(volumeOverrideG1); !ok || !r.Present {
			t.Fatalf("setup: remote %+v %v, want the override adopted", r, ok)
		}
		if _, ferr := h.st.Clear(state.ClearRequest{Set: "interventions", Flag: "volume_override", Scope: g1, Writer: "operator"}); ferr != nil {
			t.Fatal(ferr)
		}
		h.waitFor("the removal refused", func() bool { return h.requests(http.MethodDelete, overridesPath) == 1 })
		h.waitFor("the removal refusal recorded", func() bool {
			r, _ := h.link.Links().Refusal(volumeOverrideG1)
			return r == flagsmith.ReasonUnauthorized
		})
		for i := range 3 {
			h.roundAndNudge(m, 4+i)
		}
		if n := h.requests(http.MethodDelete, overridesPath); n != 1 {
			t.Fatalf("%d DELETEs after 3 rounds, want 1", n)
		}
		if v, _ := h.local("interventions", "volume_override", "game:g1"); v.Present {
			t.Fatalf("volume_override on game:g1 = %+v, want the local removal in force", v)
		}
	})
}

// UC: UC-055 A4
// A 429 is not a refusal: the value stays pending, is sent again after the
// wait and nothing is remembered as refused.
func TestRateLimitedPushStillRetried(t *testing.T) {
	m := newFSModel()
	backendPath := futureAPI + strconv.Itoa(inoutIDs["decision.backend"]) + "/"
	var mu sync.Mutex
	calls := 0
	h := inoutHarness(t, m, map[string]http.HandlerFunc{
		"PATCH " + backendPath: func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			calls++
			first := calls == 1
			mu.Unlock()
			if first {
				w.Header().Set("Retry-After", "1")
				flagsmithtest.WriteJSON(w, http.StatusTooManyRequests, json.RawMessage(`{"detail":"throttled"}`))
				return
			}
			flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
		},
	}, nil)
	h.online()
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev"})
	h.waitFor("the push confirmed after the wait", func() bool {
		r, ok := h.link.Links().Remote(backendKey)
		return ok && r.Variant == "jev"
	})
	if n := h.requests(http.MethodPatch, backendPath); n != 2 {
		t.Fatalf("UC-055/A4 %d PATCHes, want 2 (429, then the retry)", n)
	}
	if r, _ := h.link.Links().Refusal(backendKey); r != "" {
		t.Fatalf("UC-055/A4 refusal %q after the retry, want none", r)
	}
	if p, _, r := h.link.Links().Counts(); p != 0 || r != 0 {
		t.Fatalf("UC-055/A4 pending %d refused %d, want 0 0", p, r)
	}
}
