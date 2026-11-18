package flagsmith

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
)

// Identity overrides go through the routes the Flagsmith SaaS has (#2682):
// POST on the identity's override list, PUT and DELETE on one override by
// its featurestate_uuid. The fakes answer the upsert by identifier the SaaS
// does not route with Django's HTML 404, as the SaaS does.

const (
	idGame1     = "game:g1"
	idCtrlA     = "controller:A"
	idReal      = "real"
	flagTempo   = "music_tempo_override"
	flagBackend = "decision.backend"
)

func (e *outEnv) gameTempo(gameID, raw string) state.Change {
	if raw == "" {
		return e.change(state.ChangeCleared, "interventions", flagTempo, registry.ScopeGame, state.Scope{GameID: gameID}, "", "")
	}
	return e.change(state.ChangeValue, "interventions", flagTempo, registry.ScopeGame, state.Scope{GameID: gameID}, "", raw)
}

func (e *outEnv) realBackend(variant string) state.Change {
	return e.change(state.ChangeValue, "agent", flagBackend, registry.ScopeReal, state.Scope{GameKind: "real"}, variant, `"`+variant+`"`)
}

// overrideListGets counts the override listings of identifier.
func (e *outEnv) overrideListGets(identifier string) int {
	return e.count(http.MethodGet, apiEnv+"edge-identities/"+uuidOf(identifier)+"/edge-featurestates/")
}

// overrideOf returns identifier's override of flag in the model ("" = none).
func (m *idModel) overrideOf(e *outEnv, identifier, flag string) string {
	rec := m.get(identifier)
	if rec == nil {
		return ""
	}
	return string(rec.overrides[e.fid(flag)])
}

// wantOverride fails t unless identifier holds want for flag and nothing is
// out of sync.
func (e *outEnv) wantOverride(t *testing.T, m *idModel, identifier, flag, want string) {
	t.Helper()
	if v := m.overrideOf(e, identifier, flag); v != want {
		t.Fatalf("%s override of %s = %s, want %s (log %v)", identifier, flag, v, want, m.events())
	}
	if _, outOfSync, refused := e.link.Links().Counts(); outOfSync != 0 || refused != 0 {
		t.Fatalf("out_of_sync %d refused %d, want 0, 0", outOfSync, refused)
	}
}

// searchMisses makes the exact identity search show nothing for its first n
// calls (Flagsmith's search lagging a create) and counts the calls.
func (e *outEnv) searchMisses(n int32) *atomic.Int32 {
	var calls atomic.Int32
	e.mu.Lock()
	defer e.mu.Unlock()
	prev := e.hook
	e.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && r.URL.Path == apiEnv+"edge-identities/" && calls.Add(1) <= n {
			flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{"results":[],"last_evaluated_key":null}`))
			return true
		}
		return prev != nil && prev(w, r)
	}
	return &calls
}

// UC: UC-055 main step 3 A2 BR-002
// A game value and a real game's player value land as overrides on game:<id>
// and controller:<serial>; an ended game value and the controller release at
// the game end remove them through the override itself.
func TestOverridesThroughIdentityRoutes(t *testing.T) {
	e := newOutEnv(t)
	m := newIDModel(e)
	g1 := realGame("g1", e.clk.Now(), "A")
	e.link.StateChanged(e.registered(g1, nil))
	e.link.StateChanged(e.player("g1", "A", "1.5"))
	e.link.StateChanged(e.gameTempo("g1", "1.25"))
	e.drain()
	t.Run("UC-055/BR-002 the game value and the player value are overrides in Flagsmith", func(t *testing.T) {
		e.wantOverride(t, m, idGame1, flagTempo, `"1.25"`)
		e.wantOverride(t, m, idCtrlA, "player_sensitivity_factor", `"1.5"`)
		if n := e.oldRouteRequests(); n != 0 {
			t.Fatalf("UC-055/BR-002 %d requests to the upsert route the SaaS does not have", n)
		}
		// Both identities were created by this process: known to hold
		// nothing, so their first value is a POST with no listing.
		if n := e.overrideListGets(idGame1) + e.overrideListGets(idCtrlA); n != 0 {
			t.Fatalf("UC-055/BR-002 %d override listings of identities this process created, want 0", n)
		}
	})
	t.Run("UC-055/BR-002 a new value of a known override is one PUT, no listing", func(t *testing.T) {
		before := len(e.fake.Requests())
		e.link.StateChanged(e.gameTempo("g1", "1.5"))
		e.drain()
		reqs := e.fake.Requests()[before:]
		if len(reqs) != 1 || reqs[0].Method != http.MethodPut || !strings.HasPrefix(reqs[0].Path, apiEnv+"edge-identities/"+uuidOf(idGame1)+"/edge-featurestates/fs-") {
			t.Fatalf("UC-055/BR-002 requests %v, want one PUT of the override", reqs)
		}
		e.wantOverride(t, m, idGame1, flagTempo, `"1.5"`)
	})
	t.Run("UC-055/A2 an ended game value removes the override", func(t *testing.T) {
		e.link.StateChanged(e.gameTempo("g1", ""))
		e.drain()
		e.wantOverride(t, m, idGame1, flagTempo, "")
		if m.index(fmt.Sprintf("delete %s %d", idGame1, e.fid(flagTempo))) < 0 {
			t.Fatalf("UC-055/A2 no override DELETE: %v", m.events())
		}
	})
	t.Run("UC-055/A2 the game end releases the controller through its override", func(t *testing.T) {
		e.link.StateChanged(e.ended(g1))
		e.drain()
		if rec := m.get(idCtrlA); rec == nil || len(rec.overrides) != 0 {
			t.Fatalf("UC-055/A2 controller:A after the game end: %+v, want the identity without overrides", rec)
		}
		if m.index(fmt.Sprintf("delete %s %d", idCtrlA, e.fid("player_sensitivity_factor"))) < 0 {
			t.Fatalf("UC-055/A2 no override DELETE on controller:A: %v", m.events())
		}
		if n := e.oldRouteRequests(); n != 0 {
			t.Fatalf("UC-055/A2 %d requests to the upsert route the SaaS does not have", n)
		}
	})
}

// UC: UC-055 BR-004 BR-002
// A settle that removes a former game's override knows the controller holds
// none of that feature now: the owner's value is one POST, no PUT of the
// removed override, no second listing.
func TestSettleRemovalKeepsTheCache(t *testing.T) {
	e := newOutEnv(t)
	m := newIDModel(e)
	fid := e.fid("player_sensitivity_factor")
	m.add(idCtrlA, map[int]string{fid: `"0.5"`}, map[string]string{"game_id": "old"})
	e.link.setIdentityUUID(idCtrlA, uuidOf(idCtrlA))
	e.link.StateChanged(e.registered(realGame("g1", e.clk.Now(), "A"), nil))
	e.link.StateChanged(e.player("g1", "A", "1.5"))
	e.drain()
	e.wantOverride(t, m, idCtrlA, "player_sensitivity_factor", `"1.5"`)
	if puts, posts := len(e.overrideWrites(http.MethodPut)), len(e.overrideWrites(http.MethodPost)); puts != 0 || posts != 1 {
		t.Fatalf("UC-055/BR-004 %d PUTs %d POSTs after the settle, want 0 and 1", puts, posts)
	}
	if n := e.overrideListGets(idCtrlA); n != 1 {
		t.Fatalf("UC-055/BR-004 %d listings of controller:A, want 1 (the settle's)", n)
	}
}

// UC: UC-055 A4
// An override a person changed in Flagsmith is found again: a removed
// override (PUT 404) is listed and created again, one a person created (POST
// 400 "already exists") is listed and replaced, and a deleted identity (404 on
// its list) is found or created again; the value lands each time.
func TestOverrideRecoversFromPersonChanges(t *testing.T) {
	newEnv := func(t *testing.T) (*outEnv, *idModel, int) {
		e := newOutEnv(t)
		m := newIDModel(e)
		e.link.StateChanged(e.registered(gameOf("g1", "shadow"), nil))
		e.link.StateChanged(e.gameTempo("g1", "1.25"))
		e.drain()
		_ = e.clk.Sleep(t.Context(), notFoundFreshFor) // not listed moments before
		return e, m, e.fid(flagTempo)
	}
	t.Run("UC-055/A4 the override was removed by a person", func(t *testing.T) {
		e, m, fid := newEnv(t)
		m.mu.Lock()
		delete(m.ids[idGame1].overrides, fid)
		delete(m.ids[idGame1].fs, fid)
		m.mu.Unlock()
		e.link.StateChanged(e.gameTempo("g1", "1.5"))
		e.drain()
		e.wantOverride(t, m, idGame1, flagTempo, `"1.5"`)
	})
	t.Run("UC-055/A4 a person replaced the override", func(t *testing.T) {
		e, m, fid := newEnv(t)
		m.mu.Lock()
		m.setLocked(m.ids[idGame1], fid, []byte(`"2"`))
		m.mu.Unlock()
		e.link.StateChanged(e.gameTempo("g1", "1.5"))
		e.drain()
		e.wantOverride(t, m, idGame1, flagTempo, `"1.5"`)
	})
	t.Run("UC-055/A4 a person deleted the identity", func(t *testing.T) {
		e, m, _ := newEnv(t)
		m.mu.Lock()
		delete(m.ids, idGame1)
		m.mu.Unlock()
		e.link.StateChanged(e.gameTempo("g1", "1.5"))
		e.drain()
		e.wantOverride(t, m, idGame1, flagTempo, `"1.5"`)
		if n := strings.Count(strings.Join(m.events(), "|")+"|", "create "+idGame1+"|"); n != 2 {
			t.Fatalf("UC-055/A4 %d creates of game:g1, want 2 (listed, then created again): %v", n, m.events())
		}
	})
	t.Run("UC-055/A4 a POST for an override that exists already is listed and replaced", func(t *testing.T) {
		e := newOutEnv(t)
		m := newIDModel(e)
		// The identity exists with an override this process never saw.
		m.add(idReal, map[int]string{e.fid(flagBackend): `"rules"`}, nil)
		e.link.setIdentityUUID(idReal, uuidOf(idReal))
		e.out.freshOverrides(uuidOf(idReal)) // a stale "none"
		e.link.StateChanged(e.realBackend("jev"))
		e.drain()
		e.wantOverride(t, m, idReal, flagBackend, `"jev"`)
	})
}

// UC: UC-055 A4
// The same override answering 404 after a fresh listing still shows it is a
// refusal: one PUT, never a second one in the same push.
func TestSameOverrideNotFoundRefused(t *testing.T) {
	e := newOutEnv(t)
	m := newIDModel(e)
	e.link.StateChanged(e.registered(gameOf("g1", "shadow"), nil))
	e.link.StateChanged(e.gameTempo("g1", "1.25"))
	e.drain()
	_ = e.clk.Sleep(t.Context(), notFoundFreshFor)
	m.mu.Lock()
	fs := m.ids[idGame1].fs[e.fid(flagTempo)]
	m.mu.Unlock()
	path := apiEnv + "edge-identities/" + uuidOf(idGame1) + "/edge-featurestates/" + fs + "/"
	e.setOn(http.MethodPut, path, func(w http.ResponseWriter, _ *http.Request) {
		flagsmithtest.WriteJSON(w, http.StatusNotFound, json.RawMessage(`{"detail":"Not found."}`))
	})
	e.link.StateChanged(e.gameTempo("g1", "1.5"))
	e.drain()
	if n := e.count(http.MethodPut, path); n != 1 {
		t.Fatalf("UC-055/A4 %d PUTs of the override answering 404, want 1", n)
	}
	k := Key{Set: "interventions", Flag: flagTempo, Scope: idGame1}
	if r, _ := e.link.Links().Refusal(k); r != ReasonNotFound {
		t.Fatalf("UC-055/A4 refusal %q, want not_found", r)
	}
}

// UC: UC-055 A4
// Flagsmith's exact search may lag a create: a create answered "already
// exists" while the search shows nothing counts as a listing, so the value
// searches again with the #2639 backoff and lands; so does a 404 on the first
// POST right after the link created the identity (#2682).
func TestSearchLagAfterCreateRetried(t *testing.T) {
	t.Run("UC-055/A4 an identity the search does not show yet (real)", func(t *testing.T) {
		e := newOutEnv(t)
		m := newIDModel(e)
		m.add(idReal, nil, nil)
		e.searchMisses(2)
		e.link.StateChanged(e.realBackend("jev"))
		e.drain()
		e.wantOverride(t, m, idReal, flagBackend, `"jev"`)
		if !sleptAll(e.clk.Slept(), notFoundBackoff, 2*notFoundBackoff) {
			t.Fatalf("UC-055/A4 slept %v, want the 0.5 s and 1 s backoff", e.clk.Slept())
		}
	})
	t.Run("UC-055/A4 a controller the search does not show yet", func(t *testing.T) {
		e := newOutEnv(t)
		m := newIDModel(e)
		m.add(idCtrlA, nil, nil)
		// The settle's search and the value's first search miss.
		e.searchMisses(2)
		e.link.StateChanged(e.registered(realGame("g1", e.clk.Now(), "A"), nil))
		e.link.StateChanged(e.player("g1", "A", "1.5"))
		e.drain()
		e.wantOverride(t, m, idCtrlA, "player_sensitivity_factor", `"1.5"`)
	})
	t.Run("UC-055/A4 a 404 on the first POST after the link created the identity", func(t *testing.T) {
		e := newOutEnv(t)
		m := newIDModel(e)
		calls := e.overrideNotFound(1)
		e.link.StateChanged(e.realBackend("jev"))
		e.drain()
		e.wantOverride(t, m, idReal, flagBackend, `"jev"`)
		if n := calls.Load(); n != 2 {
			t.Fatalf("UC-055/A4 %d override POSTs, want 2 (the 404 retried)", n)
		}
	})
}
