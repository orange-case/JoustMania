package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
)

// UC: UC-053 A14
// A Flagsmith-only flag (#2705, FR-226, flags.md §13: agent/goal.profile) is
// refused over HTTP with 403 flagsmith_only whatever writer label the caller
// sends, a dry run included; a request labelled flagsmith stays 400 first. A
// writer reset skips it. Nothing changes.
func TestFlagsmithOnlyRefusedOverHTTP(t *testing.T) {
	e := newEnv(t, true)
	before := e.st.Version()
	rejected := e.writes("rejected", fserr.FlagsmithOnly)
	cases := []struct {
		name, method, path string
		body               any
	}{
		{"PUT by the agent", "PUT", "/agent/goal.profile", map[string]any{"variant": "lively_finale", "writer": "agent"}},
		{"PUT by an operator script", "PUT", "/agent/goal.profile", map[string]any{"variant": "quick_game", "writer": "stage"}},
		{"PUT without a writer", "PUT", "/agent/goal.profile", map[string]any{"variant": "quick_game"}},
		{"PUT of a value", "PUT", "/agent/goal.profile", map[string]any{"value": map[string]any{"from_objectives": true}, "writer": "agent"}},
		{"PUT as a dry run", "PUT", "/agent/goal.profile?dry_run=true", map[string]any{"variant": "quick_game", "writer": "agent"}},
		{"POST", "POST", "/agent/goal.profile", map[string]any{"scope": map[string]any{"game_id": "g"}, "payload": map[string]any{}, "writer": "agent"}},
		{"DELETE", "DELETE", "/agent/goal.profile?writer=agent", nil},
		{"DELETE persisted", "DELETE", "/agent/goal.profile?persist=true", nil},
	}
	for i, c := range cases {
		t.Run("UC-053/A14 "+c.name+" is 403 flagsmith_only", func(t *testing.T) {
			status, body := e.do(c.method, c.path, c.body)
			if status != http.StatusForbidden || body["error"] != fserr.FlagsmithOnly {
				t.Fatalf("%d %v, want 403 flagsmith_only", status, body)
			}
			if got := e.writes("rejected", fserr.FlagsmithOnly); got != rejected+int64(i+1) {
				t.Fatalf("rejected flagsmith_only %d, want %d", got, rejected+int64(i+1))
			}
		})
	}
	t.Run("UC-053/A14 a request labelled flagsmith is 400 malformed first", func(t *testing.T) {
		status, body := e.do("PUT", "/agent/goal.profile", map[string]any{"variant": "quick_game", "writer": "flagsmith"})
		if status != http.StatusBadRequest || body["error"] != fserr.Malformed {
			t.Fatalf("%d %v, want 400 malformed", status, body)
		}
	})
	t.Run("UC-053/A14 nothing changed", func(t *testing.T) {
		if e.st.Version() != before {
			t.Fatalf("version moved %d -> %d: a refused write changed state", before, e.st.Version())
		}
	})
	t.Run("UC-053/A14 an unmarked agent row stays writable over HTTP", func(t *testing.T) {
		if status, body := e.do("PUT", "/agent/objectives", map[string]any{"variant": "chaos_focused", "writer": "stage"}); status != http.StatusOK {
			t.Fatalf("PUT objectives %d %v, want 200", status, body)
		}
	})
}

// UC: UC-054 BR-011
// The Flagsmith link's in-process path is the one writer of a Flagsmith-only
// flag: its write and clear are accepted within the row's bounds, and a writer
// reset over HTTP (even DELETE /agent?writer=flagsmith) leaves the value alone.
func TestFlagsmithOnlyWrittenInProcess(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, true)
	stackVariant := func() string {
		t.Helper()
		status, body := e.do("GET", "/agent/goal.profile", nil)
		if status != http.StatusOK {
			t.Fatalf("GET goal.profile %d %v", status, body)
		}
		ov, _ := body["stack_overlay"].(map[string]any)
		v, _ := ov["variant"].(string)
		return v
	}
	t.Run("UC-054/BR-011 the pulled write is applied with writer flagsmith", func(t *testing.T) {
		res, ferr := e.api.FlagsmithPut(ctx, state.WriteRequest{Set: "agent", Flag: "goal.profile", Variant: "lively_finale"})
		if ferr != nil || res.Variant != "lively_finale" {
			t.Fatalf("res %+v err %v", res, ferr)
		}
		if w := e.writeSpan("goal.profile")["flagservice.writer"].AsString(); w != flagsmith.WriterLabel {
			t.Fatalf("span writer %q, want flagsmith", w)
		}
		if got := stackVariant(); got != "lively_finale" {
			t.Fatalf("stack overlay %q, want lively_finale", got)
		}
	})
	t.Run("UC-054/BR-011 the row's bounds still hold for Flagsmith", func(t *testing.T) {
		_, ferr := e.api.FlagsmithPut(ctx, state.WriteRequest{Set: "agent", Flag: "goal.profile", Value: []byte(`{"terms":[{"characteristic":"duration","min":60,"max":null}]}`)})
		if ferr == nil || ferr.Reason != fserr.OutOfBounds {
			t.Fatalf("a free terms object: %v, want 422 out_of_bounds", ferr)
		}
	})
	t.Run("UC-054/BR-011 a writer reset over HTTP leaves the Flagsmith value", func(t *testing.T) {
		for _, w := range []string{"flagsmith", "stage"} {
			if status, _ := e.do("DELETE", "/agent?writer="+w, nil); status != http.StatusNoContent {
				t.Fatalf("DELETE /agent?writer=%s: %d, want 204", w, status)
			}
		}
		if got := stackVariant(); got != "lively_finale" {
			t.Fatalf("stack overlay %q after the writer reset, want lively_finale", got)
		}
	})
	t.Run("UC-054/BR-011 the pulled clear is applied", func(t *testing.T) {
		if _, ferr := e.api.FlagsmithClear(ctx, state.ClearRequest{Set: "agent", Flag: "goal.profile"}); ferr != nil {
			t.Fatal(ferr)
		}
		if got := stackVariant(); got != "" {
			t.Fatalf("stack overlay %q after the clear, want none", got)
		}
	})
	t.Run("UC-054/BR-011 the reserved label is the link's", func(t *testing.T) {
		if flagsmith.WriterLabel != registry.FlagsmithWriter {
			t.Fatalf("flagsmith.WriterLabel %q, registry.FlagsmithWriter %q", flagsmith.WriterLabel, registry.FlagsmithWriter)
		}
	})
}
