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

// Journeys with the in side (UC-054) and the out side (UC-055) together: a
// pull round must not send again a value Flagsmith refused (UC-055 A4, #2604),
// and a value a pull round decides to send goes out without another local
// write (#2608).

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

var backendKey = flagsmith.Key{Set: "agent", Flag: "decision.backend", Scope: flagsmith.ScopeStack}

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

// inoutHarness starts the link with the real out side. Flagsmith lists
// identities; handlers add the other answers. before runs before the link
// starts.
func inoutHarness(t *testing.T, m *fsModel, handlers map[string]http.HandlerFunc, before func(h *harness)) *harness {
	t.Helper()
	return newHarness(t, harnessOpts{realOut: true, featureIDs: inoutIDs, before: func(h *harness) {
		m.h = h
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

// roundAndSettle runs one pull round and waits until nothing is pending: the
// round wakes the sender for whatever it queued (#2608), so a value it queued
// has been answered by then and shows in the request counts.
func (h *harness) roundAndSettle() {
	h.t.Helper()
	h.round()
	h.waitFor("the sender idle", func() bool {
		p, _, _ := h.link.Links().Counts()
		return p == 0
	})
}

// waitWithin is waitFor with a shorter deadline.
func (h *harness) waitWithin(d time.Duration, what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("%s not within %v", what, d)
}

// pushedBack is the PATCH handler of a flag whose first push the test holds
// in flight (entered, release) while the operator goes back to the value
// Flagsmith had: Flagsmith stores first, then back for every later push.
func pushedBack(m *fsModel, name string, first, back any, entered chan<- struct{}, release <-chan struct{}) (http.HandlerFunc, func() int) {
	var mu sync.Mutex
	calls := 0
	handler := func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			entered <- struct{}{}
			<-release
			m.set(name, first)
		} else {
			m.set(name, back)
		}
		flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
	return handler, count
}

// sendWithin is how long a value a pull round queued may take to reach
// Flagsmith after the round: the push interval (200 ms at PushRate 5) plus
// slack for -race. Without the wake it waits for the next local write.
const sendWithin = time.Second

// UC: UC-054 A2
// R = D, L ≠ R after a push that went out while the operator went back to
// the old value: the next pull round finds the local value unsent (push) and
// it reaches Flagsmith without any other local write (#2608).
func TestPullRoundPushWakesSender(t *testing.T) {
	m := newFSModel()
	m.values["decision.min_interval_ms"] = 700
	entered, release := make(chan struct{}, 1), make(chan struct{})
	path := futureAPI + strconv.Itoa(inoutIDs["decision.min_interval_ms"]) + "/"
	handler, calls := pushedBack(m, "decision.min_interval_ms", 900, 700, entered, release)
	h := inoutHarness(t, m, map[string]http.HandlerFunc{"PATCH " + path: handler}, func(h *harness) {
		h.put(state.WriteRequest{Set: "agent", Flag: "decision.min_interval_ms", Value: json.RawMessage("700")})
	})
	h.online()
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.min_interval_ms", Value: json.RawMessage("900")})
	<-entered // 900 in flight
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.min_interval_ms", Value: json.RawMessage("700")})
	close(release) // Flagsmith stores 900; the stack runs 700, nothing pending
	h.waitFor("900 confirmed", func() bool {
		r, ok := h.link.Links().Remote(minIntervalKey)
		return ok && string(r.Value) == "900"
	})
	if p, _, _ := h.link.Links().Counts(); p != 0 || calls() != 1 {
		t.Fatalf("setup: pending %d, %d PATCHes, want 0 and 1", p, calls())
	}
	h.round()
	t.Run("UC-054/A2 step 3 the local value a pull round queues is sent without another local write", func(t *testing.T) {
		h.waitWithin(sendWithin, "the local 700 confirmed in Flagsmith", func() bool {
			r, ok := h.link.Links().Remote(minIntervalKey)
			return ok && string(r.Value) == "700"
		})
		if n := calls(); n != 2 {
			t.Fatalf("%d PATCHes, want 2", n)
		}
		if p, oos, r := h.link.Links().Counts(); p != 0 || oos != 0 || r != 0 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 0 0", p, oos, r)
		}
	})
}

// UC: UC-054 A2
// A kill-switch conflict (Flagsmith looser than the local value) takes the
// Flagsmith value like any flag since #2621: nothing is pushed back.
func TestKillSwitchConflictFlagsmithWins(t *testing.T) {
	m := newFSModel()
	m.values["experiment_max_games"] = 10
	entered, release := make(chan struct{}, 1), make(chan struct{})
	path := futureAPI + strconv.Itoa(inoutIDs["experiment_max_games"]) + "/"
	handler, calls := pushedBack(m, "experiment_max_games", 100, 10, entered, release)
	h := inoutHarness(t, m, map[string]http.HandlerFunc{"PATCH " + path: handler}, func(h *harness) {
		h.put(state.WriteRequest{Set: "agent", Flag: "experiment_max_games", Value: json.RawMessage("10")})
	})
	h.online()
	h.put(state.WriteRequest{Set: "agent", Flag: "experiment_max_games", Value: json.RawMessage("100")})
	<-entered // 100 in flight
	h.put(state.WriteRequest{Set: "agent", Flag: "experiment_max_games", Value: json.RawMessage("10")})
	close(release) // Flagsmith stores 100; the stack runs 10, nothing pending
	h.waitFor("100 confirmed", func() bool {
		r, ok := h.link.Links().Remote(maxGamesKey)
		return ok && string(r.Value) == "100"
	})
	// Someone loosens it further in Flagsmith: R=100, D=500, L=10.
	h.clock.Advance(2 * time.Second)
	m.set("experiment_max_games", 500)
	h.round()
	t.Run("UC-054/A2 step 1 (BR-005) Flagsmith wins the kill-switch conflict; the local value is not sent", func(t *testing.T) {
		h.roundAndSettle()
		if n := h.counter(flagsmith.MetricConflicts, "resolution", flagsmith.ResolutionFlagsmith); n != 1 {
			t.Fatalf("conflicts{resolution=flagsmith} = %d, want 1", n)
		}
		if v, _ := h.local("agent", "experiment_max_games", flagsmith.ScopeStack); string(v.Value) != "500" {
			t.Fatalf("experiment_max_games = %+v, want the Flagsmith 500", v)
		}
		if n := calls(); n != 1 {
			t.Fatalf("%d PATCHes, want 1 (only the 100; the local 10 is not pushed back)", n)
		}
		if p, oos, r := h.link.Links().Counts(); p != 0 || oos != 0 || r != 0 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 0 0", p, oos, r)
		}
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
	for range 3 {
		h.roundAndSettle()
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
		h.roundAndSettle()
		if n := h.requests(http.MethodPatch, backendPath); n != 1 {
			t.Fatalf("%d PATCHes after a re-save of jev, want 1", n)
		}
	})
	t.Run("UC-055/A4 step 2 a different local value is sent", func(t *testing.T) {
		h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "openai"})
		h.waitFor("the second push", func() bool { return h.requests(http.MethodPatch, backendPath) == 2 })
		h.roundAndSettle()
		if n := h.requests(http.MethodPatch, backendPath); n != 2 {
			t.Fatalf("%d PATCHes after a different value and a round, want 2", n)
		}
	})
}

// UC: UC-054 BR-005
// A refused push of a kill-switch value is not sent again; a new document
// value is a conflict Flagsmith wins (#2621), which ends the refusal.
func TestRefusedKillSwitchPushThenFlagsmithWins(t *testing.T) {
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
	for range 3 {
		h.roundAndSettle()
	}
	if n := h.requests(http.MethodPatch, maxPath); n != 1 {
		t.Fatalf("UC-055/A4 %d PATCHes of experiment_max_games after 3 rounds, want 1", n)
	}
	t.Run("UC-054/BR-005 a looser document value wins the conflict; the refused 10 is not sent again", func(t *testing.T) {
		m.set("experiment_max_games", 500)
		h.roundAndSettle()
		if v, _ := h.local("agent", "experiment_max_games", flagsmith.ScopeStack); string(v.Value) != "500" {
			t.Fatalf("experiment_max_games = %+v, want the Flagsmith 500 (Flagsmith wins)", v)
		}
		if n := h.counter(flagsmith.MetricConflicts, "resolution", flagsmith.ResolutionFlagsmith); n != 1 {
			t.Fatalf("conflicts{resolution=flagsmith} = %d, want 1", n)
		}
		if n := h.requests(http.MethodPatch, maxPath); n != 1 {
			t.Fatalf("%d PATCHes, want 1: the refused 10 is not sent again", n)
		}
		if p, oos, r := h.link.Links().Counts(); p != 0 || oos != 0 || r != 0 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 0 0", p, oos, r)
		}
	})
	t.Run("UC-054/main step 3 a later document value is applied", func(t *testing.T) {
		m.set("experiment_max_games", 5)
		h.roundAndSettle()
		if v, _ := h.local("agent", "experiment_max_games", flagsmith.ScopeStack); string(v.Value) != "5" {
			t.Fatalf("experiment_max_games = %+v, want 5 applied", v)
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
	for range 3 {
		h.roundAndSettle()
	}
	t.Run("UC-055/A4 a refused override is not sent again (no override in Flagsmith)", func(t *testing.T) {
		if n := h.requests(http.MethodPut, overridesPath); n != 1 {
			t.Fatalf("%d PUTs after 3 rounds, want 1", n)
		}
	})
	t.Run("UC-055/A4 a refused removal is not sent again", func(t *testing.T) {
		// Flagsmith gets the same override another way: both sides agree.
		m.setOverride("game:g1", "volume_override", "0.5")
		h.roundAndSettle()
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
		for range 3 {
			h.roundAndSettle()
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

// UC: UC-055 A4
// The operator goes back to a refused value while a newer value is in
// flight: the newer value's confirmation does not end the refusal, so the
// key stays out of sync and pull rounds do not send the refused value again.
func TestRefusedValueBackWhileNewerInFlight(t *testing.T) {
	m := newFSModel()
	m.values["decision.backend"] = "claude"
	backendPath := futureAPI + strconv.Itoa(inoutIDs["decision.backend"]) + "/"
	entered := make(chan int, 2)
	release := make(chan struct{}, 2)
	var calls int
	var mu sync.Mutex
	h := inoutHarness(t, m, map[string]http.HandlerFunc{
		"PATCH " + backendPath: func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n <= 2 { // the two pushes the test steps through
				entered <- n
				<-release
			}
			if n != 2 {
				flagsmithtest.WriteJSON(w, http.StatusConflict, json.RawMessage(`{"detail":"change requests enabled"}`))
				return
			}
			m.set("decision.backend", "openai") // Flagsmith holds the accepted value
			flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
		},
	}, nil)
	h.online()
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev"})
	<-entered // jev in flight
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "openai"})
	release <- struct{}{} // jev refused, openai still pending
	<-entered             // openai in flight
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev"})
	release <- struct{}{} // openai confirmed
	h.waitFor("openai confirmed", func() bool {
		r, ok := h.link.Links().Remote(backendKey)
		return ok && r.Variant == "openai"
	})
	t.Run("UC-055/A4 the confirmation of the newer value keeps the refused local value out of sync", func(t *testing.T) {
		if p, oos, r := h.link.Links().Counts(); p != 0 || oos != 1 || r != 1 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 1 1: the stack runs the refused jev", p, oos, r)
		}
	})
	t.Run("UC-055/A4 step 2 pull rounds do not send the refused value again", func(t *testing.T) {
		for range 2 {
			h.roundAndSettle()
		}
		if n := h.requests(http.MethodPatch, backendPath); n != 2 {
			t.Fatalf("%d PATCHes of decision.backend, want 2 (jev refused once, openai confirmed)", n)
		}
		if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "jev" {
			t.Fatalf("decision.backend = %+v, want the local jev kept", v)
		}
	})
}
