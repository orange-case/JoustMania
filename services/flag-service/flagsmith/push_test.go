package flagsmith

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
)

type stackCase struct {
	name    string
	flag    string
	variant string
	raw     string
	clear   bool
	want    string
}

// UC: UC-055 BR-002
// A stack value is the feature's environment value: switched on with the
// value in its Flagsmith type, switched off with the baseline default when the
// overlay is cleared.
func TestStackValuePatchBody(t *testing.T) {
	cases := []stackCase{
		{name: "declared variant", flag: "decision.backend", variant: "jev", raw: `"jev"`, want: `{"environment_default":{"enabled":true,"value":{"type":"string","value":"jev"}}}`},
		{name: "three-state enabled watch", flag: "enabled", variant: "watch", raw: `"watch"`, want: `{"environment_default":{"enabled":true,"value":{"type":"string","value":"watch"}}}`},
		{name: "three-state enabled act", flag: "enabled", variant: "act", raw: `"act"`, want: `{"environment_default":{"enabled":true,"value":{"type":"string","value":"act"}}}`},
		{name: "integer range", flag: "decision.min_interval_ms", raw: `2500`, want: `{"environment_default":{"enabled":true,"value":{"type":"integer","value":"2500"}}}`},
		{name: "number without bounds is decimal text", flag: "decision.claude.act_confidence_threshold", raw: `0.8`, want: `{"environment_default":{"enabled":true,"value":{"type":"string","value":"0.8"}}}`},
		{name: "object is compact JSON text", flag: "llm.eligible_game_kinds", raw: `{"b": 1, "a": [1, 2]}`, want: `{"environment_default":{"enabled":true,"value":{"type":"string","value":"{\"b\":1,\"a\":[1,2]}"}}}`},
		{name: "cleared is off with the baseline default", flag: "decision.backend", clear: true, want: `{"environment_default":{"enabled":false,"value":{"type":"string","value":"rules"}}}`},
	}
	for _, c := range cases {
		t.Run("UC-055/BR-002 "+c.name, func(t *testing.T) {
			e := newOutEnv(t)
			if c.clear {
				e.link.StateChanged(e.change(state.ChangeCleared, "agent", c.flag, registry.ScopeStack, state.Scope{}, "", ""))
			} else {
				e.link.StateChanged(e.stack(c.flag, c.variant, c.raw))
			}
			e.drain()
			got := e.bodies(http.MethodPatch, e.futurePath(c.flag))
			if len(got) != 1 || got[0] != c.want {
				t.Fatalf("UC-055/BR-002 request bodies %v, want [%s]", got, c.want)
			}
		})
	}
}

type overrideCase struct {
	name       string
	set        string
	flag       string
	scopeName  string
	scope      state.Scope
	variant    string
	raw        string
	identifier string
	value      string
	// game is registered first (a player value needs its live game).
	game *state.Game
}

// UC: UC-055 BR-002
// Every scope but the stack is an override on its identity, with the value in
// its Flagsmith type; an ended value deletes the override.
func TestIdentityOverrideBody(t *testing.T) {
	cases := []overrideCase{
		{name: "game float", set: "interventions", flag: "music_tempo_override", scopeName: registry.ScopeGame, scope: state.Scope{GameID: "g1"}, raw: `1.25`, identifier: "game:g1", value: `"1.25"`},
		{name: "game integer", set: "interventions", flag: "global_sensitivity_override", scopeName: registry.ScopeGame, scope: state.Scope{GameID: "g1"}, raw: `3`, identifier: "game:g1", value: `3`},
		{name: "real game player (its controller, #2624)", set: "interventions", flag: "player_sensitivity_factor", scopeName: registry.ScopeGameSerial, scope: state.Scope{GameID: "g1", Serial: "MOCK0001"}, raw: `1.5`, identifier: "controller:MOCK0001", value: `"1.5"`, game: gameOf("g1", "real", "MOCK0001")},
		{name: "shadow game player", set: "interventions", flag: "player_sensitivity_factor", scopeName: registry.ScopeGameSerial, scope: state.Scope{GameID: "g1", Serial: "MOCK0001"}, raw: `1.5`, identifier: "game:g1:MOCK0001", value: `"1.5"`, game: gameOf("g1", "shadow", "MOCK0001")},
		{name: "experiment arm", set: "agent", flag: "decision.prompt_profile", scopeName: registry.ScopeExperiment, scope: state.Scope{ExperimentID: "e1", Arm: "armA"}, variant: "neutral", raw: `"neutral"`, identifier: "experiment:e1:armA", value: `"neutral"`},
		{name: "real", set: "game", flag: "windows", scopeName: registry.ScopeReal, scope: state.Scope{GameKind: "real"}, raw: `{"min": 1}`, identifier: "real", value: `"{\"min\":1}"`},
	}
	for _, c := range cases {
		t.Run("UC-055/BR-002 "+c.name, func(t *testing.T) { checkOverrideCase(t, c) })
	}
}

// checkOverrideCase pushes c's value, checks the POST on the identity's
// override list (#2682), ends the value and checks the DELETE of the override
// itself.
func checkOverrideCase(t *testing.T, c overrideCase) {
	e := newOutEnv(t)
	if c.game != nil {
		e.link.StateChanged(e.registered(c.game, nil))
	}
	e.link.StateChanged(e.change(state.ChangeValue, c.set, c.flag, c.scopeName, c.scope, c.variant, c.raw))
	e.drain()
	path := apiEnv + "edge-identities/" + uuidOf(c.identifier) + "/edge-featurestates/"
	want := fmt.Sprintf(`{"feature":%d,"enabled":true,"feature_state_value":%s}`, e.fid(c.flag), c.value)
	if got := e.overrideWrites(http.MethodPost); len(got) != 1 || got[0].Path != path || string(got[0].Body) != want {
		t.Fatalf("UC-055/BR-002 override POSTs %v, want one to %s with %s", got, path, want)
	}
	if v, ok := e.overrideOn(c.identifier, c.flag); !ok || v != c.value {
		t.Fatalf("UC-055/BR-002 override on %s = %s (%v), want %s", c.identifier, v, ok, c.value)
	}
	if n := e.count(http.MethodPatch, "/api/__future__/"); n != 0 {
		t.Fatalf("UC-055/BR-002 %d stack PATCH requests for a %s value, want 0", n, c.name)
	}
	// The value ends: the override itself is deleted, by its featurestate_uuid.
	e.link.StateChanged(e.change(state.ChangeCleared, c.set, c.flag, c.scopeName, c.scope, "", ""))
	e.drain()
	del := e.overrideWrites(http.MethodDelete)
	if len(del) != 1 || !strings.HasPrefix(del[0].Path, path+"fs-") || len(del[0].Body) != 0 {
		t.Fatalf("UC-055/BR-002 override DELETEs %v, want one below %s", del, path)
	}
	if _, ok := e.overrideOn(c.identifier, c.flag); ok {
		t.Fatalf("UC-055/BR-002 the override on %s is still there", c.identifier)
	}
	if n := e.oldRouteRequests(); n != 0 {
		t.Fatalf("UC-055/BR-002 %d requests to the upsert route the SaaS does not have", n)
	}
}

// UC: UC-055 A4
// A Flagsmith without the __future__ API answers 404 with something other
// than its JSON error: the stack value goes through the v1 feature state, and
// every later key goes there directly.
func TestFutureFallbackLatch(t *testing.T) {
	t.Run("UC-055/A4 a plain 404 latches the v1 feature state", func(t *testing.T) {
		e := newOutEnv(t)
		for _, flag := range []string{"decision.backend", "decision.min_interval_ms"} {
			e.setOn(http.MethodPatch, e.futurePath(flag), func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
		}
		e.link.StateChanged(e.stack("decision.backend", "jev", `"jev"`))
		e.link.StateChanged(e.stack("decision.min_interval_ms", "", `2500`))
		e.drain()
		if n := e.count(http.MethodPatch, "/api/__future__/"); n != 1 {
			t.Fatalf("UC-055/A4 %d __future__ requests, want 1 (the second key goes straight to the fallback)", n)
		}
		if n := e.count(http.MethodGet, apiEnv+"featurestates/"); n != 2 {
			t.Fatalf("UC-055/A4 %d feature state lookups, want 2", n)
		}
		backend := e.bodies(http.MethodPut, apiEnv+fmt.Sprintf("featurestates/%d0/", e.fid("decision.backend")))
		if len(backend) != 1 || backend[0] != `{"enabled":true,"feature_state_value":"jev"}` {
			t.Fatalf("UC-055/A4 fallback body %v", backend)
		}
		interval := e.bodies(http.MethodPut, apiEnv+fmt.Sprintf("featurestates/%d0/", e.fid("decision.min_interval_ms")))
		if len(interval) != 1 || interval[0] != `{"enabled":true,"feature_state_value":2500}` {
			t.Fatalf("UC-055/A4 fallback body %v", interval)
		}
		if pending, _, refused := e.link.Links().Counts(); pending != 0 || refused != 0 {
			t.Fatalf("UC-055/A4 pending %d refused %d, want 0", pending, refused)
		}
	})
	t.Run("UC-055/A4 Flagsmith's own 404 is a refusal, not a fallback", func(t *testing.T) {
		e := newOutEnv(t)
		e.setOn(http.MethodPatch, e.futurePath("decision.backend"), func(w http.ResponseWriter, _ *http.Request) {
			flagsmithtest.WriteJSON(w, http.StatusNotFound, json.RawMessage(`{"detail":"Not found."}`))
		})
		e.link.StateChanged(e.stack("decision.backend", "jev", `"jev"`))
		e.drain()
		if n := e.count(http.MethodGet, apiEnv+"featurestates/"); n != 0 {
			t.Fatalf("UC-055/A4 %d feature state lookups, want 0", n)
		}
		k := Key{Set: "agent", Flag: "decision.backend", Scope: ScopeStack}
		if reason, side := e.link.Links().Refusal(k); reason != ReasonNotFound || side != SideRemote {
			t.Fatalf("UC-055/A4 refusal %q %q, want not_found on the remote side", reason, side)
		}
		if e.out.fallback.Load() {
			t.Fatal("UC-055/A4 the fallback latched on a JSON 404")
		}
	})
}

// UC: UC-055 A3 A4
// How an Admin API answer is told to the link: a refusal marks one key, only
// a missing answer or a 5xx takes the link offline.
func TestClassify(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		result     string
		reason     string
		linkDown   bool
		retryAfter time.Duration
	}{
		{name: "ok", result: ResultOK},
		{name: "change requests (409)", err: &HTTPError{Status: 409}, result: ResultRejected, reason: ReasonChangeRequestRequired},
		{name: "change requests (400 text)", err: &HTTPError{Status: 400, Body: []byte("a change request is required")}, result: ResultRejected, reason: ReasonChangeRequestRequired},
		{name: "unauthorized", err: &HTTPError{Status: 401}, result: ResultRejected, reason: ReasonUnauthorized},
		{name: "forbidden", err: &HTTPError{Status: 403}, result: ResultRejected, reason: ReasonUnauthorized},
		{name: "not found", err: &HTTPError{Status: 404}, result: ResultRejected, reason: ReasonNotFound},
		{name: "bad request", err: &HTTPError{Status: 400}, result: ResultRejected, reason: "http_400"},
		{name: "rate limited", err: &HTTPError{Status: 429, RetryAfter: 7 * time.Second}, result: ResultRejected, reason: ReasonRateLimited, retryAfter: 7 * time.Second},
		{name: "rate limited without a wait", err: &HTTPError{Status: 429}, result: ResultRejected, reason: ReasonRateLimited, retryAfter: time.Second},
		{name: "rate limited for an hour", err: &HTTPError{Status: 429, RetryAfter: time.Hour}, result: ResultRejected, reason: ReasonRateLimited, retryAfter: time.Minute},
		{name: "server error", err: &HTTPError{Status: 503}, result: ResultError, reason: "http_5xx", linkDown: true},
		{name: "timeout", err: &TransportError{Op: "PATCH", Err: context.DeadlineExceeded}, result: ResultError, reason: ReasonTimeout, linkDown: true},
	}
	for _, c := range cases {
		t.Run("UC-055/A4 "+c.name, func(t *testing.T) {
			result, reason, down := classify(c.err)
			if result != c.result || reason != c.reason || down != c.linkDown {
				t.Fatalf("classify = %q %q %v, want %q %q %v", result, reason, down, c.result, c.reason, c.linkDown)
			}
			if c.retryAfter > 0 {
				if got := retryWait(c.err); got != c.retryAfter {
					t.Fatalf("retryWait = %v, want %v", got, c.retryAfter)
				}
			}
		})
	}
}

// UC: UC-055 A1 A2
// The identity queue keeps the latest wish per identifier, and a listing that
// was never sent is dropped when the identity is not wanted any more.
func TestIdentityQueueCoalesce(t *testing.T) {
	t.Run("UC-055/A2 listed then removed before it was sent is dropped", func(t *testing.T) {
		q := newIdentityQueue()
		q.Want("game:g1", true, nil)
		q.Want("game:g1", false, nil)
		if op, ok := q.Peek(); ok {
			t.Fatalf("UC-055/A2 left %+v, want nothing to do", op)
		}
	})
	t.Run("UC-055/A2 a created identity is removed", func(t *testing.T) {
		q := newIdentityQueue()
		q.Want("game:g1", true, nil)
		op, _ := q.Peek()
		q.Created(op, "u1")
		q.Want("game:g1", false, nil)
		got, ok := q.Peek()
		if !ok || got.Present || got.Identifier != "game:g1" {
			t.Fatalf("UC-055/A2 next %+v %v, want the removal of game:g1", got, ok)
		}
	})
	t.Run("UC-055/A1 a finished op does not touch a newer wish", func(t *testing.T) {
		q := newIdentityQueue()
		q.Want("game:g1", true, nil)
		op, _ := q.Peek()
		q.Want("game:g1", false, nil)
		q.Want("game:g1", true, nil)
		q.Created(op, "stale")
		if again, ok := q.Peek(); !ok || again.Created {
			t.Fatalf("UC-055/A1 next %+v %v, want a listing that is not created yet", again, ok)
		}
	})
}
