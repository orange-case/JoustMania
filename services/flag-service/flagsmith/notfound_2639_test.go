package flagsmith

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
)

// The 404 of an override push to an identity this process just listed or
// settled (#2639): Flagsmith's edge storage may not show it yet, so the push
// is retried a bounded number of times; any other 404 is refused at once, and
// every refusal is named in GET /state and logged once.

// captureLog makes the link log into a buffer (syncBuffer: start_test.go).
func (e *outEnv) captureLog() *syncBuffer {
	buf := &syncBuffer{}
	e.link.deps.Logger = slog.New(slog.NewTextHandler(buf, nil))
	return buf
}

// overrideNotFound answers the first n override PUTs 404 (Flagsmith's JSON
// "Not found."), the rest 200, and counts them.
func (e *outEnv) overrideNotFound(n int32) *atomic.Int32 {
	var calls atomic.Int32
	e.setOn(http.MethodPut, apiEnv+"edge-identities-featurestates", func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= n {
			flagsmithtest.WriteJSON(w, http.StatusNotFound, json.RawMessage(`{"detail":"Not found."}`))
			return
		}
		flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
	})
	return &calls
}

func (e *outEnv) gameValue(gameID, raw string) state.Change {
	return e.change(state.ChangeValue, "interventions", "music_tempo_override", registry.ScopeGame, state.Scope{GameID: gameID}, "", raw)
}

func sleptAll(slept []time.Duration, want ...time.Duration) bool {
	i := 0
	for _, d := range slept {
		if i < len(want) && d == want[i] {
			i++
		}
	}
	return i == len(want)
}

// UC: UC-055 A4
// A 404 right after the link listed the game identity (or settled the
// controller) is retried with a backoff and the value lands: nothing is
// refused (#2639).
func TestNotFoundAfterListingRetried(t *testing.T) {
	t.Run("UC-055/A4 game identity listed moments before", func(t *testing.T) {
		e := newOutEnv(t)
		calls := e.overrideNotFound(2)
		e.link.StateChanged(e.registered(gameOf("g1", "shadow"), nil))
		e.link.StateChanged(e.gameValue("g1", "1.25"))
		e.drain()
		if n := calls.Load(); n != 3 {
			t.Fatalf("UC-055/A4 %d override PUTs, want 3 (two 404s retried, then accepted)", n)
		}
		k := Key{Set: "interventions", Flag: "music_tempo_override", Scope: "game:g1"}
		if _, ok := e.link.Links().Remote(k); !ok {
			t.Fatal("UC-055/A4 the value was not confirmed after the retries")
		}
		if _, outOfSync, refused := e.link.Links().Counts(); outOfSync != 0 || refused != 0 {
			t.Fatalf("UC-055/A4 out_of_sync %d refused %d, want 0, 0", outOfSync, refused)
		}
		if !sleptAll(e.clk.Slept(), 500*time.Millisecond, time.Second) {
			t.Fatalf("UC-055/A4 slept %v, want a 0.5s then a 1s backoff", e.clk.Slept())
		}
	})
	t.Run("UC-055/A4 controller identity settled moments before", func(t *testing.T) {
		e := newOutEnv(t)
		newIDModel(e)
		calls := e.overrideNotFound(1)
		e.link.StateChanged(e.registered(realGame("g1", e.clk.Now(), "A"), nil))
		e.link.StateChanged(e.player("g1", "A", "1.5"))
		e.drain()
		if n := calls.Load(); n != 2 {
			t.Fatalf("UC-055/A4 %d override PUTs, want 2 (one 404 retried)", n)
		}
		k := Key{Set: "interventions", Flag: "player_sensitivity_factor", Scope: "game:g1:A"}
		if r, _ := e.link.Links().Refusal(k); r != "" {
			t.Fatalf("UC-055/A4 refusal %q, want none", r)
		}
		if _, ok := e.link.Links().Remote(k); !ok {
			t.Fatal("UC-055/A4 the player value was not confirmed after the retry")
		}
	})
}

// UC: UC-055 A4
// A 404 that persists is retried at most three times (0.5, 1, 2 s), then the
// value is refused as before: never a loop.
func TestNotFoundPersistsRefusedAfterRetries(t *testing.T) {
	e := newOutEnv(t)
	calls := e.overrideNotFound(1 << 20)
	e.link.StateChanged(e.registered(gameOf("g1", "shadow"), nil))
	e.link.StateChanged(e.gameValue("g1", "1.25"))
	e.drain()
	if n := calls.Load(); n != 4 {
		t.Fatalf("UC-055/A4 %d override PUTs, want 4 (the push and three retries)", n)
	}
	if !sleptAll(e.clk.Slept(), 500*time.Millisecond, time.Second, 2*time.Second) {
		t.Fatalf("UC-055/A4 slept %v, want backoffs 0.5s, 1s, 2s", e.clk.Slept())
	}
	k := Key{Set: "interventions", Flag: "music_tempo_override", Scope: "game:g1"}
	if r, side := e.link.Links().Refusal(k); r != ReasonNotFound || side != SideRemote {
		t.Fatalf("UC-055/A4 refusal %q %q, want not_found on the remote side", r, side)
	}
	if n := e.pushes(ReasonNotFound); n != 4 {
		t.Fatalf("UC-055/A4 pushes_total{reason=not_found} = %d, want 4 (every answer counted)", n)
	}
	// Refused for good: a wake sends nothing more.
	r := e.start()
	e.out.poke()
	time.Sleep(50 * time.Millisecond)
	_ = r.stop()
	if n := calls.Load(); n != 4 {
		t.Fatalf("UC-055/A4 %d override PUTs after a wake, want 4", n)
	}
}

// UC: UC-055 A4
// A 404 on an identity this process did not list, or listed longer ago than
// the edge needs, is refused at once, as before #2639.
func TestNotFoundWithoutRecentListingRefused(t *testing.T) {
	t.Run("UC-055/A4 an identity the link never lists (real)", func(t *testing.T) {
		e := newOutEnv(t)
		calls := e.overrideNotFound(1 << 20)
		k := Key{Set: "agent", Flag: "decision.backend", Scope: "real"}
		e.link.Links().Offer(k, Value{Present: true, Variant: "jev", Value: json.RawMessage(`"jev"`)}, e.clk.Now(), 1)
		e.drain()
		if n := calls.Load(); n != 1 {
			t.Fatalf("UC-055/A4 %d override PUTs, want 1 (no retry)", n)
		}
		if r, _ := e.link.Links().Refusal(k); r != ReasonNotFound {
			t.Fatalf("UC-055/A4 refusal %q, want not_found", r)
		}
	})
	t.Run("UC-055/A4 a game identity listed more than 30 s ago", func(t *testing.T) {
		e := newOutEnv(t)
		e.link.StateChanged(e.registered(gameOf("g1", "shadow"), nil))
		e.drain()
		_ = e.clk.Sleep(t.Context(), notFoundFreshFor)
		calls := e.overrideNotFound(1 << 20)
		e.link.StateChanged(e.gameValue("g1", "1.25"))
		e.drain()
		if n := calls.Load(); n != 1 {
			t.Fatalf("UC-055/A4 %d override PUTs, want 1 (no retry)", n)
		}
	})
}

// UC: UC-055 A4
// GET /state names every refused or out-of-sync key (flag set, flag, scope,
// reason, side, Flagsmith's HTTP status, since) and never a value, and each
// refusal logs one WARN with the same names (#2639).
func TestRefusedKeysNamedInStateAndLog(t *testing.T) {
	e := newOutEnv(t)
	logs := e.captureLog()
	e.overrideNotFound(1 << 20)
	e.link.StateChanged(e.registered(gameOf("g1", "shadow"), nil))
	e.link.StateChanged(e.gameValue("g1", "1.25"))
	// A key refused without a request: its feature id is unknown.
	delete(e.link.IDs().FeatureID, "decision.backend")
	e.link.StateChanged(e.stack("decision.backend", "jev", `"jev"`))
	e.drain()

	e.link.mu.Lock()
	e.link.mode = ModeOnline
	e.link.mu.Unlock()
	st := e.link.Status()
	if st.Refused != 2 || len(st.Keys) != 2 || st.KeysOmitted != 0 {
		t.Fatalf("UC-055/A4 refused %d keys %+v omitted %d, want 2 keys", st.Refused, st.Keys, st.KeysOmitted)
	}
	byScope := map[string]KeyState{}
	for _, ks := range st.Keys {
		byScope[ks.Scope] = ks
	}
	g := byScope["game:g1"]
	if g.FlagSet != "interventions" || g.Flag != "music_tempo_override" || g.Reason != ReasonNotFound || g.Side != SideRemote || g.HTTPStatus != http.StatusNotFound || g.InSync || g.Since.IsZero() {
		t.Fatalf("UC-055/A4 game key %+v, want interventions/music_tempo_override not_found remote 404 out of sync with a time", g)
	}
	s := byScope[ScopeStack]
	if s.Flag != "decision.backend" || s.Reason != ReasonNotFound || s.HTTPStatus != 0 {
		t.Fatalf("UC-055/A4 stack key %+v, want decision.backend not_found without an HTTP status", s)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "1.25") || strings.Contains(string(raw), "jev") {
		t.Fatalf("UC-055/A4 GET /state carries a value: %s", raw)
	}

	var warns []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "flagsmith push refused") {
			warns = append(warns, line)
		}
	}
	if len(warns) != 2 {
		t.Fatalf("UC-055/A4 %d refusal WARNs, want 2 (one per refusal):\n%s", len(warns), logs.String())
	}
	for _, want := range []string{"level=WARN", "flag_set=interventions", "flag=music_tempo_override", "scope=game:g1", "reason=not_found", "http_status=404"} {
		if !strings.Contains(warns[0], want) {
			t.Fatalf("UC-055/A4 WARN %q lacks %q", warns[0], want)
		}
	}
	for _, w := range warns {
		if strings.Contains(w, "1.25") || strings.Contains(w, "jev") {
			t.Fatalf("UC-055/A4 WARN carries a value: %q", w)
		}
	}
}

// UC: UC-055 A4
// The keys list of GET /state is bounded: refused keys first, the rest counted
// in keys_omitted.
func TestStateKeysBounded(t *testing.T) {
	s := NewLinkState(nil)
	for i := range MaxStateKeys + 5 {
		k := Key{Set: "s", Flag: "f", Scope: "game:g" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)}
		s.Offer(k, Value{Present: true, Value: json.RawMessage(`1`)}, time.Now(), uint64(i+1))
	}
	refused := Key{Set: "s", Flag: "z", Scope: ScopeStack}
	s.Offer(refused, Value{Present: true, Value: json.RawMessage(`1`)}, time.Now(), 999)
	s.RefuseValueStatus(refused, Value{Present: true, Value: json.RawMessage(`1`)}, SideRemote, ReasonChangeRequestRequired, http.StatusConflict)
	list, total := s.Unsynced(MaxStateKeys)
	if total != MaxStateKeys+6 || len(list) != MaxStateKeys {
		t.Fatalf("UC-055/A4 %d keys of %d, want %d of %d", len(list), total, MaxStateKeys, MaxStateKeys+6)
	}
	if list[0].Flag != "z" || list[0].HTTPStatus != http.StatusConflict {
		t.Fatalf("UC-055/A4 first key %+v, want the refused one", list[0])
	}
}
