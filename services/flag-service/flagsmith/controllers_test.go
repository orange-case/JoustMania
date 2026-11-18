package flagsmith

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
)

// idModel is a small model of Flagsmith's edge identities for the controller
// tests (#2624): identities with overrides and traits, and a log of every
// write in the order Flagsmith saw it.
type idModel struct {
	mu  sync.Mutex
	ids map[string]*idRec
	log []string
	// beforePut, when set, runs before an override PUT is answered (outside
	// the lock).
	beforePut func(identifier string)
	next      int
}

type idRec struct {
	uuid      string
	overrides map[int]json.RawMessage
	// fs: the featurestate_uuid of each override (#2682).
	fs     map[int]string
	traits map[string]string
}

func newIDModel(e *outEnv) *idModel {
	m := &idModel{ids: map[string]*idRec{}}
	e.mu.Lock()
	e.hook = m.serve
	e.mu.Unlock()
	return m
}

func (m *idModel) add(identifier string, overrides map[int]string, traits map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.recLocked(identifier)
	for f, v := range overrides {
		m.setLocked(rec, f, json.RawMessage(v))
	}
	for k, v := range traits {
		rec.traits[k] = v
	}
}

func (m *idModel) recLocked(identifier string) *idRec {
	rec := m.ids[identifier]
	if rec == nil {
		rec = &idRec{uuid: uuidOf(identifier), overrides: map[int]json.RawMessage{}, fs: map[int]string{}, traits: map[string]string{}}
		m.ids[identifier] = rec
	}
	return rec
}

func (m *idModel) byUUIDLocked(uuid string) (string, *idRec) {
	for id, rec := range m.ids {
		if rec.uuid == uuid {
			return id, rec
		}
	}
	return "", nil
}

// get returns a copy of an identity (nil when absent).
func (m *idModel) get(identifier string) *idRec {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.ids[identifier]
	if rec == nil {
		return nil
	}
	cp := &idRec{uuid: rec.uuid, overrides: map[int]json.RawMessage{}, traits: map[string]string{}}
	for k, v := range rec.overrides {
		cp.overrides[k] = v
	}
	for k, v := range rec.traits {
		cp.traits[k] = v
	}
	return cp
}

func (m *idModel) events() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.log)
}

// index is the position of the first log line that starts with prefix (-1).
func (m *idModel) index(prefix string) int {
	for i, l := range m.events() {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

// serveOverride serves the SaaS's override routes of identity uuid (#2682):
// GET/POST the list, GET/PUT/DELETE one override by its featurestate_uuid; a
// POST for a feature the identity overrides already is 400. Writes log
// "put <identifier> <feature> <value>", removals "delete <identifier> <feature>".
func (m *idModel) serveOverride(w http.ResponseWriter, r *http.Request, uuid, rest string, body map[string]json.RawMessage, feature int) {
	if (r.Method == http.MethodPost || r.Method == http.MethodPut) && m.beforePut != nil {
		m.mu.Lock()
		identifier, _ := m.byUUIDLocked(uuid)
		m.mu.Unlock()
		m.beforePut(identifier)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	identifier, rec := m.byUUIDLocked(uuid)
	notFound := func() { flagsmithtest.WriteJSON(w, http.StatusNotFound, map[string]string{"detail": "Not found."}) }
	if rec == nil {
		notFound()
		return
	}
	answer := func(f int) map[string]any {
		return map[string]any{"feature": f, "enabled": true, "feature_state_value": rec.overrides[f], "featurestate_uuid": rec.fs[f], "identity_uuid": rec.uuid}
	}
	fs, item := strings.CutSuffix(strings.TrimPrefix(rest, "edge-featurestates/"), "/")
	if !item {
		switch r.Method {
		case http.MethodGet:
			out := []map[string]any{}
			for f := range rec.overrides {
				out = append(out, answer(f))
			}
			flagsmithtest.WriteJSON(w, http.StatusOK, out)
		case http.MethodPost:
			if _, has := rec.overrides[feature]; has {
				flagsmithtest.WriteJSON(w, http.StatusBadRequest, []string{"Feature state already exists."})
				return
			}
			m.setLocked(rec, feature, body["feature_state_value"])
			m.log = append(m.log, fmt.Sprintf("put %s %d %s", identifier, feature, body["feature_state_value"]))
			flagsmithtest.WriteJSON(w, http.StatusCreated, answer(feature))
		}
		return
	}
	f := 0
	for id, u := range rec.fs {
		if u == fs {
			f = id
		}
	}
	if f == 0 {
		notFound()
		return
	}
	switch r.Method {
	case http.MethodGet:
		flagsmithtest.WriteJSON(w, http.StatusOK, answer(f))
	case http.MethodPut:
		rec.overrides[f] = body["feature_state_value"]
		m.log = append(m.log, fmt.Sprintf("put %s %d %s", identifier, f, body["feature_state_value"]))
		flagsmithtest.WriteJSON(w, http.StatusOK, answer(f))
	case http.MethodDelete:
		m.log = append(m.log, fmt.Sprintf("delete %s %d", identifier, f))
		delete(rec.overrides, f)
		delete(rec.fs, f)
		w.WriteHeader(http.StatusNoContent)
	}
}

// setLocked sets an override with a new featurestate_uuid.
func (m *idModel) setLocked(rec *idRec, feature int, value json.RawMessage) {
	m.next++
	rec.overrides[feature] = value
	rec.fs[feature] = fmt.Sprintf("fs-%d", m.next)
}

func (m *idModel) serve(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	var body map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&body)
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(body[k], &s)
		return s
	}
	num := func(k string) int {
		var n int
		_ = json.Unmarshal(body[k], &n)
		return n
	}
	if strings.HasPrefix(p, apiEnv+"edge-identities-featurestates") {
		// The SaaS routes no upsert by identifier (#2682).
		flagsmithtest.DjangoNotFound(w, r)
		return true
	}
	if uuid, rest, ok := flagsmithtest.OverrideRoute(p); ok {
		m.serveOverride(w, r, uuid, rest, body, num("feature"))
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if p == apiEnv+"edge-identities/" {
		switch r.Method {
		case http.MethodGet:
			q := r.URL.Query().Get("q")
			results := []map[string]string{}
			for id, rec := range m.ids {
				if (strings.HasPrefix(q, `"`) && id == strings.Trim(q, `"`)) || (!strings.HasPrefix(q, `"`) && strings.HasPrefix(id, q)) {
					results = append(results, map[string]string{"identifier": id, "identity_uuid": rec.uuid})
				}
			}
			flagsmithtest.WriteJSON(w, http.StatusOK, map[string]any{"results": results, "last_evaluated_key": nil})
		case http.MethodPost:
			identifier := str("identifier")
			if _, ok := m.ids[identifier]; ok {
				flagsmithtest.WriteJSON(w, http.StatusBadRequest, json.RawMessage(`{"identifier":["Identity already exists."]}`))
				return true
			}
			rec := m.recLocked(identifier)
			m.log = append(m.log, "create "+identifier)
			flagsmithtest.WriteJSON(w, http.StatusCreated, map[string]string{"identifier": identifier, "identity_uuid": rec.uuid})
		}
		return true
	}
	rest, ok := strings.CutPrefix(p, apiEnv+"edge-identities/")
	if !ok {
		return false
	}
	uuid, sub, _ := strings.Cut(rest, "/")
	identifier, rec := m.byUUIDLocked(uuid)
	if rec == nil {
		http.NotFound(w, r)
		return true
	}
	switch {
	case sub == "" && r.Method == http.MethodDelete:
		delete(m.ids, identifier)
		m.log = append(m.log, "delete-identity "+identifier)
		w.WriteHeader(http.StatusNoContent)
	case sub == "list-traits/" && r.Method == http.MethodGet:
		var out []map[string]any
		for k, v := range rec.traits {
			out = append(out, map[string]any{"trait_key": k, "trait_value": v})
		}
		flagsmithtest.WriteJSON(w, http.StatusOK, out)
	case sub == "update-traits/" && r.Method == http.MethodPut:
		key := str("trait_key")
		if string(body["trait_value"]) == "null" {
			delete(rec.traits, key)
			m.log = append(m.log, fmt.Sprintf("trait %s %s=null", identifier, key))
		} else {
			rec.traits[key] = str("trait_value")
			m.log = append(m.log, fmt.Sprintf("trait %s %s=%s", identifier, key, str("trait_value")))
		}
		flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
	default:
		return false
	}
	return true
}

func realGame(id string, at time.Time, serials ...string) *state.Game {
	g := gameOf(id, "real", serials...)
	g.RegisteredAt = at
	return g
}

func (e *outEnv) player(gameID, serial, raw string) state.Change {
	return e.change(state.ChangeValue, "interventions", "player_sensitivity_factor", registry.ScopeGameSerial, state.Scope{GameID: gameID, Serial: serial}, "", raw)
}

// UC: UC-055 A1 A2 BR-002 BR-004
// A real game's player values are overrides on the controller's own identity
// controller:<serial> with the traits game_id, game_kind and
// coordinator_instance (#2624); no game:<id>:<serial> identity is made. At
// the game end the game identity is deleted, but each controller identity
// stays: its overrides are removed and its traits cleared.
func TestControllerIdentityForRealGamePlayers(t *testing.T) {
	e := newOutEnv(t)
	m := newIDModel(e)
	fid := e.fid("player_sensitivity_factor")
	g1 := realGame("g1", e.clk.Now(), "A", "B")
	e.link.StateChanged(e.registered(g1, nil))
	e.link.StateChanged(e.player("g1", "A", "1.5"))
	e.drain()
	t.Run("UC-055/A1 a real game's player value lives on controller:<serial>", func(t *testing.T) {
		for _, s := range []string{"A", "B"} {
			if m.get("game:g1:"+s) != nil {
				t.Fatalf("UC-055/A1 identity game:g1:%s made for a real game's player", s)
			}
			rec := m.get("controller:" + s)
			if rec == nil {
				t.Fatalf("UC-055/A1 no identity controller:%s", s)
			}
			want := map[string]string{"game_id": "g1", "game_kind": "real", "coordinator_instance": "c1"}
			if fmt.Sprint(rec.traits) != fmt.Sprint(want) {
				t.Fatalf("UC-055/A1 controller:%s traits %v, want %v", s, rec.traits, want)
			}
		}
		if v := string(m.get("controller:A").overrides[fid]); v != `"1.5"` {
			t.Fatalf("UC-055/BR-002 controller:A override %s, want \"1.5\"", v)
		}
		if tr, put := m.index("trait controller:A game_id=g1"), m.index("put controller:A"); tr < 0 || put < tr {
			t.Fatalf("UC-055/A1 the value went out before the controller was settled: %v", m.events())
		}
	})
	t.Run("UC-055/A2 the game end keeps the controller identity, without the game's overrides and traits", func(t *testing.T) {
		e.link.StateChanged(e.ended(g1))
		e.drain()
		if m.get("game:g1") != nil {
			t.Fatal("UC-055/A2 game:g1 not deleted")
		}
		rec := m.get("controller:A")
		if rec == nil {
			t.Fatal("UC-055/A2 controller:A deleted: the controller identity outlives games")
		}
		if len(rec.overrides) != 0 || len(rec.traits) != 0 {
			t.Fatalf("UC-055/A2 controller:A after the game end: overrides %v traits %v, want none", rec.overrides, rec.traits)
		}
		if strings.Contains(strings.Join(m.events(), "|"), "delete-identity controller:") {
			t.Fatalf("UC-055/A2 a controller identity was deleted: %v", m.events())
		}
	})
}

// UC: UC-055 A1 BR-003 BR-004
// A controller that moves to a later game while its former game is still
// registered belongs to the later game: the former game's overrides are
// removed before the new owner's value is sent, a value the former game
// writes afterwards never reaches the controller, and when the later game
// ends the controller goes back to the former one, cleared again.
func TestControllerMovesToLaterGame(t *testing.T) {
	e := newOutEnv(t)
	m := newIDModel(e)
	fid := e.fid("player_sensitivity_factor")
	g1 := realGame("g1", e.clk.Now(), "A")
	e.link.StateChanged(e.registered(g1, nil))
	e.link.StateChanged(e.player("g1", "A", "1.5"))
	e.drain()
	g2 := realGame("g2", e.clk.Now().Add(time.Second), "A")
	e.link.StateChanged(e.registered(g2, nil))
	e.link.StateChanged(e.player("g1", "A", "2"))
	e.link.StateChanged(e.player("g2", "A", "1.25"))
	e.drain()
	t.Run("UC-055/A1 the later game owns the controller", func(t *testing.T) {
		rec := m.get("controller:A")
		if v := string(rec.overrides[fid]); v != `"1.25"` {
			t.Fatalf("UC-055/A1 controller:A override %s, want g2's \"1.25\"", v)
		}
		if rec.traits["game_id"] != "g2" {
			t.Fatalf("UC-055/A1 controller:A game_id %q, want g2", rec.traits["game_id"])
		}
		del, put := m.index(fmt.Sprintf("delete controller:A %d", fid)), m.index(fmt.Sprintf(`put controller:A %d "1.25"`, fid))
		if del < 0 || put < del {
			t.Fatalf("UC-055/BR-003 g1's override not removed before g2's value: %v", m.events())
		}
		if strings.Contains(strings.Join(m.events(), "|"), `"2"`) {
			t.Fatalf("UC-055/BR-003 the former game's value reached the controller: %v", m.events())
		}
		for _, k := range e.link.Links().Keys() {
			if k.Scope == "game:g1:A" {
				t.Fatalf("UC-055/BR-003 key %v of the former owner still known", k)
			}
		}
	})
	t.Run("UC-055/A2 the later game ends: the controller goes back to the former game, cleared", func(t *testing.T) {
		e.link.StateChanged(e.ended(g2))
		e.drain()
		rec := m.get("controller:A")
		if len(rec.overrides) != 0 || rec.traits["game_id"] != "g1" {
			t.Fatalf("UC-055/A2 controller:A overrides %v game_id %q, want none and g1", rec.overrides, rec.traits["game_id"])
		}
	})
}

// UC: UC-055 A2
// A game that ends while one of its controller values is on the wire: the
// removal runs after the push on the one sender, so the override does not
// survive the game.
func TestControllerGameEndsWhilePushInFlight(t *testing.T) {
	e := newOutEnv(t)
	m := newIDModel(e)
	g1 := realGame("g1", e.clk.Now(), "A")
	e.link.StateChanged(e.registered(g1, nil))
	e.drain()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m.beforePut = func(string) {
		once.Do(func() {
			close(started)
			<-release
		})
	}
	e.link.StateChanged(e.player("g1", "A", "1.5"))
	r := e.start()
	<-started
	e.link.StateChanged(e.ended(g1))
	close(release)
	waitFor(t, "the out side to go idle", e.idle)
	if err := r.stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	if rec := m.get("controller:A"); rec == nil || len(rec.overrides) != 0 {
		t.Fatalf("UC-055/A2 controller:A after the game ended mid-push: %+v, want the identity without overrides", rec)
	}
	if put, del := m.index("put controller:A"), m.index("delete controller:A"); put < 0 || del < put {
		t.Fatalf("UC-055/A2 removal did not follow the push: %v", m.events())
	}
}

// UC: UC-055 A1
// The first settle of a controller in this process (a restart): when its
// game_id trait names the owner, the flag service restarted while the game
// was live and the overrides are kept; any other game_id (or none) means the
// overrides are a former game's and are removed.
func TestControllerFirstSettleChecksGameID(t *testing.T) {
	for _, c := range []struct {
		name, gameID string
		kept         bool
	}{
		{"UC-055/A1 game_id names the owner: kept", "g1", true},
		{"UC-055/A1 game_id names another game: removed", "g0", false},
		{"UC-055/A1 no game_id: removed", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newOutEnv(t)
			m := newIDModel(e)
			fid := e.fid("player_sensitivity_factor")
			traits := map[string]string{}
			if c.gameID != "" {
				traits["game_id"] = c.gameID
			}
			m.add("controller:A", map[int]string{fid: `"1.5"`}, traits)
			e.link.StateChanged(e.registered(realGame("g1", e.clk.Now(), "A"), nil))
			e.drain()
			_, has := m.get("controller:A").overrides[fid]
			if has != c.kept {
				t.Fatalf("UC-055/A1 override kept %v, want %v: %v", has, c.kept, m.events())
			}
			if got := m.get("controller:A").traits["game_id"]; got != "g1" {
				t.Fatalf("UC-055/A1 game_id %q after the settle, want g1", got)
			}
		})
	}
}

// UC: UC-055 BR-002
// A shadow game's players keep per-game identities and never touch the
// controller identity of a real game playing with the same serial.
func TestShadowPlayersNeverTouchControllers(t *testing.T) {
	e := newOutEnv(t)
	m := newIDModel(e)
	fid := e.fid("player_sensitivity_factor")
	e.link.StateChanged(e.registered(realGame("g1", e.clk.Now(), "A"), nil))
	e.link.StateChanged(e.player("g1", "A", "1.5"))
	e.drain()
	sh := gameOf("s1", "shadow", "A")
	e.link.StateChanged(e.registered(sh, nil))
	e.link.StateChanged(e.player("s1", "A", "2"))
	e.link.StateChanged(e.ended(sh))
	e.drain()
	if v := string(m.get("controller:A").overrides[fid]); v != `"1.5"` {
		t.Fatalf("UC-055/BR-002 controller:A override %s after a shadow game with the same serial, want \"1.5\": %v", v, m.events())
	}
	if m.get("controller:A").traits["game_id"] != "g1" {
		t.Fatalf("UC-055/BR-002 controller:A traits changed by a shadow game: %v", m.get("controller:A").traits)
	}
}

// UC: UC-054 A3
// The start cleanup releases only controllers without a live game; the
// controller of a live game is left to its settle.
func TestControllerMarkReleaseOnlyWithoutOwner(t *testing.T) {
	e := newOutEnv(t)
	m := newIDModel(e)
	fid := e.fid("player_sensitivity_factor")
	m.add("controller:A", map[int]string{fid: `"1.5"`}, map[string]string{"game_id": "g1"})
	m.add("controller:Z", map[int]string{fid: `"2"`}, map[string]string{"game_id": "gone"})
	e.link.StateChanged(e.registered(realGame("g1", e.clk.Now(), "A"), nil))
	if e.link.ctrl.markRelease("A") {
		t.Fatal("UC-054/A3 release queued for a controller of a live game")
	}
	if !e.link.ctrl.markRelease("Z") {
		t.Fatal("UC-054/A3 no release queued for a controller without a game")
	}
	e.drain()
	if rec := m.get("controller:Z"); rec == nil || len(rec.overrides) != 0 || len(rec.traits) != 0 {
		t.Fatalf("UC-054/A3 controller:Z after the release: %+v, want the identity, no overrides, no traits", rec)
	}
	if _, has := m.get("controller:A").overrides[fid]; !has {
		t.Fatalf("UC-054/A3 the live game's controller override was removed: %v", m.events())
	}
}

// UC: UC-055 BR-004
// A value of the owner that is queued while its controller's settle is still
// due stays pending: nothing reaches the controller before the former
// overrides are removed.
func TestControllerValueWaitsForSettle(t *testing.T) {
	e := newOutEnv(t)
	newIDModel(e)
	e.link.StateChanged(e.registered(realGame("g1", e.clk.Now(), "A"), nil))
	e.link.StateChanged(e.player("g1", "A", "1.5"))
	// The sender looks at the value first (the settle became due after it
	// looked for controller work).
	did, err := e.out.pushNext(context.Background())
	if err != nil || !did {
		t.Fatalf("pushNext: did %v err %v", did, err)
	}
	if n := len(e.overrideWrites(http.MethodPost)) + len(e.overrideWrites(http.MethodPut)); n != 0 {
		t.Fatalf("UC-055/BR-004 %d override requests before the controller was settled, want 0", n)
	}
	if pending, _, _ := e.link.Links().Counts(); pending != 1 {
		t.Fatalf("UC-055/BR-004 pending %d, want the value kept", pending)
	}
	e.drain()
	if n := len(e.overrideWrites(http.MethodPost)) + len(e.overrideWrites(http.MethodPut)); n != 1 {
		t.Fatalf("UC-055/BR-004 %d override requests after the settle, want 1", n)
	}
}

// UC: UC-055 A1
// Flagsmith says the controller identity exists but the search does not show
// it: the settle is given up for this game (the controller is not read back),
// never retried in a loop, and the value is refused (no uuid, no route).
func TestControllerDuplicateCreateRefusedNotRetried(t *testing.T) {
	e := newOutEnv(t)
	m := newIDModel(e)
	m.add("controller:A", nil, nil)
	e.setOn(http.MethodGet, apiEnv+"edge-identities/", func(w http.ResponseWriter, _ *http.Request) {
		flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{"results":[],"last_evaluated_key":null}`))
	})
	e.link.StateChanged(e.registered(realGame("g1", e.clk.Now(), "A"), nil))
	e.link.StateChanged(e.player("g1", "A", "1.5"))
	e.drain()
	creates := 0
	for _, b := range e.bodies(http.MethodPost, apiEnv+"edge-identities/") {
		if strings.Contains(b, "controller:A") {
			creates++
		}
	}
	// The settle creates once, the refused value push once (#2682): no loop.
	if creates != 2 {
		t.Fatalf("UC-055/A1 %d creates of controller:A, want 2 (the settle and the value, no retry loop)", creates)
	}
	// No override route works without the uuid (#2682): the value is
	// refused, not_found, and the link never loops on it.
	if m.index("put controller:A") >= 0 {
		t.Fatalf("UC-055/A1 a value reached the controller without its uuid: %v", m.events())
	}
	k := Key{Set: "interventions", Flag: "player_sensitivity_factor", Scope: "game:g1:A"}
	if r, _ := e.link.Links().Refusal(k); r != ReasonNotFound {
		t.Fatalf("UC-055/A1 refusal %q, want not_found", r)
	}
	if v, _ := e.link.ctrl.view(); v["A"].readable {
		t.Fatal("UC-055/A1 controller read back although its settle was refused")
	}
}

// UC: UC-054 A3
// The start cleanup's release of a controller that holds nothing writes
// nothing (two reads); one that still names a game has those traits cleared.
func TestControllerReleaseWritesOnlyWhatIsSet(t *testing.T) {
	e := newOutEnv(t)
	m := newIDModel(e)
	m.add("controller:Z", nil, nil)
	m.add("controller:Y", nil, map[string]string{"game_id": "gone"})
	e.link.ctrl.markRelease("Z")
	e.link.ctrl.markRelease("Y")
	e.drain()
	got := m.events()
	want := []string{"trait controller:Y game_id=null"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("UC-054/A3 writes %v, want %v", got, want)
	}
}

// UC: UC-054 A2
// UC: UC-055 A3
// A controller's value (a real game's player value) written while the link is
// not connected is player-scoped live game state: it is never held back as an
// offline stack change, and it is sent to controller:<serial> (#2621).
func TestControllerValueWrittenOfflineIsSent(t *testing.T) {
	e := newOutEnv(t)
	m := newIDModel(e)
	fid := e.fid("player_sensitivity_factor")
	e.link.connected.Store(false) // the change is made offline
	g1 := realGame("g1", e.clk.Now(), "A")
	e.link.StateChanged(e.registered(g1, nil))
	e.link.StateChanged(e.player("g1", "A", "1.5"))
	if e.link.Links().PendingOffline(Key{Set: "interventions", Flag: "player_sensitivity_factor", Scope: "game:g1:A"}) {
		t.Fatal("UC-054/A2 a controller value written offline is marked for discard")
	}
	e.link.connected.Store(true) // back online: the sender drains
	e.drain()
	if v := string(m.get("controller:A").overrides[fid]); v != `"1.5"` {
		t.Fatalf("UC-055/A3 controller:A override %s after the reconnect, want \"1.5\" sent", v)
	}
}
