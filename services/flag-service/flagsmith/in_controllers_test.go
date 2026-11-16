package flagsmith_test

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/state"
)

// Pull side of the controller identities (#2624, architecture note §2.2).

func registerReal(t *testing.T, h *harness, id string, serials ...string) {
	t.Helper()
	if _, ferr := h.st.RegisterGame(state.Game{ID: id, CoordinatorInstance: "c1", Kind: "real", Serials: serials}); ferr != nil {
		t.Fatal(ferr)
	}
}

// UC: UC-054 BR-003 BR-010
// A person's override on controller:<serial> is the player value of the
// controller's live game; a game:<id>:<serial> identity of a real game is not
// read (one source per player value), and an override on a controller
// without a live game, or not yet settled for its game, is ignored.
func TestControllerOverrideAppliesToItsLiveGame(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.online()
	registerReal(t, h, "g1", "MOCK0001")
	h.setDoc(nil,
		override{"controller:MOCK0001", []map[string]any{fsState("player_sensitivity_factor", true, "1.5")}},
		override{"game:g1:MOCK0001", []map[string]any{fsState("player_sensitivity_factor", true, "2")}},
		override{"controller:MOCK0002", []map[string]any{fsState("player_sensitivity_factor", true, "1.75")}},
	)
	h.round()
	t.Run("UC-054/BR-003 not read before the controller is settled for its game", func(t *testing.T) {
		if v, _ := h.local("interventions", "player_sensitivity_factor", "game:g1:MOCK0001"); v.Present {
			t.Fatalf("UC-054/BR-003 applied %+v before the controller was settled", v)
		}
	})
	flagsmith.SettleControllers(h.link)
	h.round()
	t.Run("UC-054/BR-003 the controller's override is its live game's player value", func(t *testing.T) {
		if v, _ := h.local("interventions", "player_sensitivity_factor", "game:g1:MOCK0001"); string(v.Value) != "1.5" {
			t.Fatalf("UC-054/BR-003 game:g1:MOCK0001 = %+v, want the controller's 1.5 (never game:g1:MOCK0001's 2)", v)
		}
	})
	t.Run("UC-054/BR-003 a controller without a live game is ignored", func(t *testing.T) {
		changes, _ := h.st.Changes()
		for _, c := range changes {
			if c.Scope.Serial == "MOCK0002" {
				t.Fatalf("UC-054/BR-003 a value for MOCK0002 was written: %+v", c)
			}
		}
		if st := h.link.Status(); st.Refused != 0 {
			t.Fatalf("UC-054/BR-003 status %+v: an ignored controller override was refused", st)
		}
	})
}

// UC: UC-054 BR-003 BR-010
// A document that lags behind a controller's settle still shows the former
// game's override: it is not handed to the new owner. The guard ends when the
// document shows another value, which applies.
func TestControllerRemovedOverrideNotAppliedFromLaggingDocument(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.online()
	registerReal(t, h, "g2", "MOCK0001")
	flagsmith.SettleControllers(h.link)
	flagsmith.NoteControllerRemoved(h.link, "MOCK0001", "player_sensitivity_factor", 1, `"1.5"`)
	h.setDoc(nil, override{"controller:MOCK0001", []map[string]any{fsState("player_sensitivity_factor", true, "1.5")}})
	h.round()
	if v, _ := h.local("interventions", "player_sensitivity_factor", "game:g2:MOCK0001"); v.Present {
		t.Fatalf("UC-054/BR-003 the former game's removed override was applied to the new owner: %+v", v)
	}
	h.setDoc(nil, override{"controller:MOCK0001", []map[string]any{fsState("player_sensitivity_factor", true, "1.75")}})
	h.round()
	if v, _ := h.local("interventions", "player_sensitivity_factor", "game:g2:MOCK0001"); string(v.Value) != "1.75" {
		t.Fatalf("UC-054/BR-003 a new value on the controller = %+v, want 1.75", v)
	}
}

// UC: UC-054 BR-010
// The controller moves to a later game while the document request is in
// flight: the round read its owner before the fetch, so it must not apply
// the document's controller value to the former owner's key. The round skips
// the controller; the next round applies the value to the new owner.
func TestControllerMovedDuringFetchSkipped(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.online()
	registerReal(t, h, "g1", "MOCK0001")
	flagsmith.SettleControllers(h.link)
	doc := mustJSON(document(h.clock.Now().Add(-time.Minute), nil,
		override{"controller:MOCK0001", []map[string]any{fsState("player_sensitivity_factor", true, "1.75")}}))
	var moved atomic.Bool
	h.fake.Handle("GET "+flagsmithtest.APIPath+"environment-document/", func(w http.ResponseWriter, _ *http.Request) {
		if !moved.Swap(true) {
			registerReal(t, h, "g2", "MOCK0001")
			flagsmith.SettleControllers(h.link)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	})
	h.round()
	if v, _ := h.local("interventions", "player_sensitivity_factor", "game:g1:MOCK0001"); v.Present {
		t.Fatalf("UC-054/BR-010 the new owner's value was applied to the former owner: %+v", v)
	}
	if st := h.link.Status(); st.Refused != 0 {
		t.Fatalf("UC-054/BR-010 status %+v: the former owner's key was refused", st)
	}
	h.round()
	if v, _ := h.local("interventions", "player_sensitivity_factor", "game:g2:MOCK0001"); string(v.Value) != "1.75" {
		t.Fatalf("UC-054/BR-010 next round: game:g2:MOCK0001 = %+v, want 1.75", v)
	}
}
