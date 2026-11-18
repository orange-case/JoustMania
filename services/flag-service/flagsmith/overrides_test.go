package flagsmith

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
)

// Identity overrides go through the routes the Flagsmith SaaS has (#2682):
// POST on the identity's override list, PUT and DELETE on one override by
// its featurestate_uuid. The fakes answer the upsert by identifier the SaaS
// does not route with Django's HTML 404, as the SaaS does.

func (e *outEnv) gameTempo(gameID, raw string) state.Change {
	if raw == "" {
		return e.change(state.ChangeCleared, "interventions", "music_tempo_override", registry.ScopeGame, state.Scope{GameID: gameID}, "", "")
	}
	return e.change(state.ChangeValue, "interventions", "music_tempo_override", registry.ScopeGame, state.Scope{GameID: gameID}, "", raw)
}

// UC: UC-055 main step 3 A2 BR-002
// A game value and a real game's player value land as overrides on game:<id>
// and controller:<serial>; an ended game value and the controller release at
// the game end remove them through the override itself.
func TestOverridesThroughIdentityRoutes(t *testing.T) {
	e := newOutEnv(t)
	m := newIDModel(e)
	tempo, factor := e.fid("music_tempo_override"), e.fid("player_sensitivity_factor")
	g1 := realGame("g1", e.clk.Now(), "A")
	e.link.StateChanged(e.registered(g1, nil))
	e.link.StateChanged(e.player("g1", "A", "1.5"))
	e.link.StateChanged(e.gameTempo("g1", "1.25"))
	e.drain()
	t.Run("UC-055/BR-002 the game value and the player value are overrides in Flagsmith", func(t *testing.T) {
		if v := string(m.get("game:g1").overrides[tempo]); v != `"1.25"` {
			t.Fatalf("UC-055/BR-002 game:g1 override %s, want \"1.25\" (log %v)", v, m.events())
		}
		if v := string(m.get("controller:A").overrides[factor]); v != `"1.5"` {
			t.Fatalf("UC-055/BR-002 controller:A override %s, want \"1.5\" (log %v)", v, m.events())
		}
		if n := e.oldRouteRequests(); n != 0 {
			t.Fatalf("UC-055/BR-002 %d requests to the upsert route the SaaS does not have", n)
		}
		if _, refused, _ := e.link.Links().Counts(); refused != 0 {
			t.Fatalf("UC-055/BR-002 %d keys out of sync, want 0", refused)
		}
	})
	t.Run("UC-055/BR-002 a new value of a known override is one PUT, no listing", func(t *testing.T) {
		before := len(e.fake.Requests())
		e.link.StateChanged(e.gameTempo("g1", "1.5"))
		e.drain()
		reqs := e.fake.Requests()[before:]
		if len(reqs) != 1 || reqs[0].Method != http.MethodPut || !strings.HasPrefix(reqs[0].Path, apiEnv+"edge-identities/"+uuidOf("game:g1")+"/edge-featurestates/fs-") {
			t.Fatalf("UC-055/BR-002 requests %v, want one PUT of the override", reqs)
		}
		if v := string(m.get("game:g1").overrides[tempo]); v != `"1.5"` {
			t.Fatalf("UC-055/BR-002 game:g1 override %s, want \"1.5\"", v)
		}
	})
	t.Run("UC-055/A2 an ended game value removes the override", func(t *testing.T) {
		e.link.StateChanged(e.gameTempo("g1", ""))
		e.drain()
		if _, has := m.get("game:g1").overrides[tempo]; has {
			t.Fatalf("UC-055/A2 game:g1 still overrides music_tempo_override: %v", m.events())
		}
		if m.index(fmt.Sprintf("delete game:g1 %d", tempo)) < 0 {
			t.Fatalf("UC-055/A2 no override DELETE: %v", m.events())
		}
	})
	t.Run("UC-055/A2 the game end releases the controller through its override", func(t *testing.T) {
		e.link.StateChanged(e.ended(g1))
		e.drain()
		if rec := m.get("controller:A"); rec == nil || len(rec.overrides) != 0 {
			t.Fatalf("UC-055/A2 controller:A after the game end: %+v, want the identity without overrides", rec)
		}
		if m.index(fmt.Sprintf("delete controller:A %d", factor)) < 0 {
			t.Fatalf("UC-055/A2 no override DELETE on controller:A: %v", m.events())
		}
		if n := e.oldRouteRequests(); n != 0 {
			t.Fatalf("UC-055/A2 %d requests to the upsert route the SaaS does not have", n)
		}
	})
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
		return e, m, e.fid("music_tempo_override")
	}
	landed := func(t *testing.T, e *outEnv, m *idModel, fid int, want string) {
		t.Helper()
		if v := string(m.get("game:g1").overrides[fid]); v != want {
			t.Fatalf("UC-055/A4 game:g1 override %s, want %s (log %v)", v, want, m.events())
		}
		if _, refused, _ := e.link.Links().Counts(); refused != 0 {
			t.Fatalf("UC-055/A4 %d keys out of sync, want 0", refused)
		}
	}
	t.Run("UC-055/A4 the override was removed by a person", func(t *testing.T) {
		e, m, fid := newEnv(t)
		m.mu.Lock()
		rec := m.ids["game:g1"]
		delete(rec.overrides, fid)
		delete(rec.fs, fid)
		m.mu.Unlock()
		e.link.StateChanged(e.gameTempo("g1", "1.5"))
		e.drain()
		landed(t, e, m, fid, `"1.5"`)
	})
	t.Run("UC-055/A4 a person replaced the override", func(t *testing.T) {
		e, m, fid := newEnv(t)
		m.mu.Lock()
		m.setLocked(m.ids["game:g1"], fid, []byte(`"2"`))
		m.mu.Unlock()
		e.link.StateChanged(e.gameTempo("g1", "1.5"))
		e.drain()
		landed(t, e, m, fid, `"1.5"`)
	})
	t.Run("UC-055/A4 a person deleted the identity", func(t *testing.T) {
		e, m, fid := newEnv(t)
		m.mu.Lock()
		delete(m.ids, "game:g1")
		m.mu.Unlock()
		e.link.StateChanged(e.gameTempo("g1", "1.5"))
		e.drain()
		landed(t, e, m, fid, `"1.5"`)
		creates := 0
		for _, l := range m.events() {
			if l == "create game:g1" {
				creates++
			}
		}
		if creates != 2 {
			t.Fatalf("UC-055/A4 %d creates of game:g1, want 2 (listed, then created again): %v", creates, m.events())
		}
	})
	t.Run("UC-055/A4 a POST for an override that exists already is listed and replaced", func(t *testing.T) {
		e := newOutEnv(t)
		m := newIDModel(e)
		// The identity exists with an override this process never saw.
		m.add("real", map[int]string{e.fid("decision.backend"): `"rules"`}, nil)
		e.link.setIdentityUUID("real", uuidOf("real"))
		e.out.freshOverrides(uuidOf("real")) // a stale "none"
		k := e.change(state.ChangeValue, "agent", "decision.backend", registry.ScopeReal, state.Scope{GameKind: "real"}, "jev", `"jev"`)
		e.link.StateChanged(k)
		e.drain()
		if v := string(m.get("real").overrides[e.fid("decision.backend")]); v != `"jev"` {
			t.Fatalf("UC-055/A4 real override %s, want \"jev\" (log %v)", v, m.events())
		}
	})
}
