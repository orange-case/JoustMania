package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/state"
)

func (e *env) stackWriter(flag string) any {
	e.t.Helper()
	status, body := e.do("GET", flag, nil)
	if status != http.StatusOK {
		e.t.Fatalf("GET %s %d %v", flag, status, body)
	}
	if v, _ := body["stack_overlay"].(map[string]any); v != nil {
		return v["writer"]
	}
	return nil
}

// UC: UC-016 BR-013
// ?if_writer= is a compare-and-delete on the in-memory stack overlay too, so
// the menu's reset (DELETE ?writer=menu&if_writer=menu) clears only its own
// overlay; another writer's is 412 writer_mismatch and stays.
func TestIfWriterStackOverlay(t *testing.T) {
	const objectives = "/agent/objectives"
	const reset = objectives + "?writer=menu&if_writer=menu"
	t.Run("UC-016/BR-013 the menu's own overlay is cleared", func(t *testing.T) {
		e := newEnv(t, true)
		if status, body := e.do("PUT", objectives, map[string]any{"variant": "chaos_focused", "writer": "menu"}); status != http.StatusOK {
			t.Fatalf("PUT %d %v", status, body)
		}
		if status, body := e.do("DELETE", reset, nil); status != http.StatusNoContent {
			t.Fatalf("%d %v, want 204", status, body)
		}
		if w := e.stackWriter(objectives); w != nil {
			t.Fatalf("writer %v, want none", w)
		}
	})
	t.Run("UC-016/BR-013 Flagsmith's overlay is 412 writer_mismatch and stays", func(t *testing.T) {
		e := newEnv(t, true)
		if _, ferr := e.api.FlagsmithPut(context.Background(), state.WriteRequest{Set: "agent", Flag: "objectives", Variant: "mixed"}); ferr != nil {
			t.Fatal(ferr)
		}
		v := e.st.Version()
		if status, body := e.do("DELETE", reset, nil); status != http.StatusPreconditionFailed || body["error"] != fserr.WriterMismatch {
			t.Fatalf("%d %v, want 412 writer_mismatch", status, body)
		}
		if w := e.stackWriter(objectives); w != flagsmith.WriterLabel || e.st.Version() != v {
			t.Fatalf("writer %v, version %d -> %d: want the Flagsmith overlay unchanged", w, v, e.st.Version())
		}
	})
	t.Run("UC-016/BR-013 a stage script's overlay stays", func(t *testing.T) {
		e := newEnv(t, true)
		const rate = "/agent/policy.max_interventions_per_minute"
		e.do("PUT", rate, map[string]any{"variant": "aggressive", "writer": "stage"})
		if status, body := e.do("DELETE", rate+"?writer=menu&if_writer=menu", nil); status != http.StatusPreconditionFailed || body["error"] != fserr.WriterMismatch {
			t.Fatalf("%d %v, want 412 writer_mismatch", status, body)
		}
		if w := e.stackWriter(rate); w != "stage" {
			t.Fatalf("writer %v, want stage", w)
		}
	})
	t.Run("UC-016/BR-013 no stack overlay is a 204 that leaves the persisted admin value", func(t *testing.T) {
		e := newEnv(t, true)
		const backend = "/agent/decision.backend"
		if status, body := e.do("PUT", backend, map[string]any{"variant": "claude", "writer": "menu", "persist": true}); status != http.StatusOK {
			t.Fatalf("%d %v", status, body)
		}
		if status, body := e.do("DELETE", backend+"?writer=menu&if_writer=menu", nil); status != http.StatusNoContent {
			t.Fatalf("%d %v, want 204", status, body)
		}
		_, body := e.do("GET", backend, nil)
		if body["admin_value"] == nil {
			t.Fatal("the persisted admin value was cleared by an in-memory if_writer clear")
		}
	})
	t.Run("UC-053/A14 goal.profile stays 403 flagsmith_only for the menu", func(t *testing.T) {
		e := newEnv(t, true)
		for _, c := range []struct {
			method, path string
			body         any
		}{
			{"PUT", "/agent/goal.profile", map[string]any{"variant": "quick_game", "writer": "menu"}},
			{"DELETE", "/agent/goal.profile?writer=menu&if_writer=menu", nil},
		} {
			if status, body := e.do(c.method, c.path, c.body); status != http.StatusForbidden || body["error"] != fserr.FlagsmithOnly {
				t.Fatalf("%s: %d %v, want 403 flagsmith_only", c.method, status, body)
			}
		}
	})
}
