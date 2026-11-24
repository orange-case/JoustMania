package api

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
)

func (e *env) layer(flag, layer string) map[string]any {
	e.t.Helper()
	status, body := e.do("GET", flag, nil)
	if status != http.StatusOK {
		e.t.Fatalf("GET %s %d %v", flag, status, body)
	}
	v, _ := body[layer].(map[string]any)
	return v
}

func (e *env) stackWriter(flag string) any {
	e.t.Helper()
	if v := e.layer(flag, "stack_overlay"); v != nil {
		return v["writer"]
	}
	return nil
}

// UC: UC-053 A14
// A row with a writer list (agent/goal.profile: flagsmith, menu) refuses an
// HTTP write whose label is not in it with 403 writer_not_allowed, a dry run
// and an empty label included; a request labelled flagsmith stays 400 first.
// Nothing changes.
func TestWriterListRefusedOverHTTP(t *testing.T) {
	e := newEnv(t, true)
	before := e.st.Version()
	rejected := e.writes("rejected", fserr.WriterNotAllowed)
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
		{"DELETE without a writer", "DELETE", "/agent/goal.profile", nil},
		{"DELETE persisted", "DELETE", "/agent/goal.profile?persist=true", nil},
		{"DELETE with if_writer=menu but writer agent", "DELETE", "/agent/goal.profile?writer=agent&if_writer=menu", nil},
	}
	for i, c := range cases {
		t.Run("UC-053/A14 "+c.name+" is 403 writer_not_allowed", func(t *testing.T) {
			status, body := e.do(c.method, c.path, c.body)
			if status != http.StatusForbidden || body["error"] != fserr.WriterNotAllowed {
				t.Fatalf("%d %v, want 403 writer_not_allowed", status, body)
			}
			if d, _ := body["detail"].(string); !strings.Contains(d, "flagsmith, menu") {
				t.Fatalf("detail %q does not name the list", d)
			}
			if got := e.writes("rejected", fserr.WriterNotAllowed); got != rejected+int64(i+1) {
				t.Fatalf("rejected writer_not_allowed %d, want %d", got, rejected+int64(i+1))
			}
		})
	}
	t.Run("UC-053/A14 a request labelled flagsmith is 400 malformed first", func(t *testing.T) {
		for _, c := range []struct {
			method, path string
			body         any
		}{
			{"PUT", "/agent/goal.profile", map[string]any{"variant": "quick_game", "writer": "flagsmith"}},
			{"DELETE", "/agent/goal.profile?writer=flagsmith", nil},
		} {
			status, body := e.do(c.method, c.path, c.body)
			if status != http.StatusBadRequest || body["error"] != fserr.Malformed {
				t.Fatalf("%s %s: %d %v, want 400 malformed", c.method, c.path, status, body)
			}
		}
	})
	t.Run("UC-053/A14 an over-long writer label is 400 malformed before the writer list", func(t *testing.T) {
		long := strings.Repeat("w", state.MaxIDLength+1)
		for _, c := range []struct {
			method, path string
			body         any
		}{
			{"PUT", "/agent/goal.profile", map[string]any{"variant": "quick_game", "writer": long}},
			{"POST", "/agent/goal.profile", map[string]any{"scope": map[string]any{"game_id": "g"}, "payload": map[string]any{}, "writer": long}},
			{"DELETE", "/agent/goal.profile?writer=" + long, nil},
		} {
			status, body := e.do(c.method, c.path, c.body)
			if status != http.StatusBadRequest || body["error"] != fserr.Malformed {
				t.Fatalf("%s: %d %v, want 400 malformed", c.method, status, body)
			}
		}
	})
	t.Run("UC-053/A14 nothing changed", func(t *testing.T) {
		if e.st.Version() != before {
			t.Fatalf("version moved %d -> %d: a refused write changed state", before, e.st.Version())
		}
	})
	t.Run("UC-053/A14 a row without a list stays writable by any label", func(t *testing.T) {
		if status, body := e.do("PUT", "/agent/objectives", map[string]any{"variant": "chaos_focused", "writer": "stage"}); status != http.StatusOK {
			t.Fatalf("PUT objectives %d %v, want 200", status, body)
		}
	})
}

// UC: UC-016 BR-012
// The menu is in goal.profile's writer list: its PUT, dry run and DELETE are
// accepted within the row's bounds, and the GET policy serves the list.
func TestWriterListMenuWrites(t *testing.T) {
	e := newEnv(t, true)
	t.Run("UC-016/BR-012 the GET policy serves writers", func(t *testing.T) {
		_, body := e.do("GET", "/agent/goal.profile", nil)
		pol, _ := body["policy"].(map[string]any)
		if got, _ := pol["writers"].([]any); !reflect.DeepEqual(got, []any{"flagsmith", "menu"}) {
			t.Fatalf("policy writers %v, want [flagsmith menu]", pol["writers"])
		}
		_, body = e.do("GET", "/agent/objectives", nil)
		if pol, _ := body["policy"].(map[string]any); pol == nil || pol["writers"] != nil {
			t.Fatalf("objectives policy %v: want no writers", pol)
		}
	})
	t.Run("UC-016/BR-012 a dry run by menu is 200 and writes nothing", func(t *testing.T) {
		if status, body := e.do("PUT", "/agent/goal.profile?dry_run=true", map[string]any{"variant": "quick_game", "writer": "menu"}); status != http.StatusOK {
			t.Fatalf("%d %v, want 200", status, body)
		}
		if w := e.stackWriter("/agent/goal.profile"); w != nil {
			t.Fatalf("a dry run left an overlay of %v", w)
		}
	})
	t.Run("UC-016/BR-012 a PUT by menu is applied", func(t *testing.T) {
		if status, body := e.do("PUT", "/agent/goal.profile", map[string]any{"variant": "quick_game", "writer": "menu"}); status != http.StatusOK {
			t.Fatalf("%d %v, want 200", status, body)
		}
		if w := e.stackWriter("/agent/goal.profile"); w != "menu" {
			t.Fatalf("stack overlay writer %v, want menu", w)
		}
	})
	t.Run("UC-016/BR-012 the bounds still hold for menu", func(t *testing.T) {
		status, body := e.do("PUT", "/agent/goal.profile", map[string]any{"value": map[string]any{"terms": []any{}}, "writer": "menu"})
		if status != http.StatusUnprocessableEntity || body["error"] != fserr.OutOfBounds {
			t.Fatalf("%d %v, want 422 out_of_bounds", status, body)
		}
	})
	t.Run("UC-016/BR-012 a DELETE by menu clears it", func(t *testing.T) {
		if status, body := e.do("DELETE", "/agent/goal.profile?writer=menu", nil); status != http.StatusNoContent {
			t.Fatalf("%d %v, want 204", status, body)
		}
		if w := e.stackWriter("/agent/goal.profile"); w != nil {
			t.Fatalf("stack overlay of %v after the clear", w)
		}
	})
}

// UC: UC-053 A14
// A row whose list is exactly [flagsmith] answers 403 flagsmith_only to every
// HTTP label, menu included, as the Flagsmith-only marker of #2705 did.
func TestFlagsmithOnlyListRefusedOverHTTP(t *testing.T) {
	e := newEnv(t, true)
	e.reg.Get("agent", "goal.profile").Writers = []string{registry.FlagsmithWriter}
	rejected := e.writes("rejected", fserr.FlagsmithOnly)
	for i, c := range []struct {
		method, path string
		body         any
	}{
		{"PUT", "/agent/goal.profile", map[string]any{"variant": "quick_game", "writer": "menu"}},
		{"PUT", "/agent/goal.profile?dry_run=true", map[string]any{"variant": "quick_game", "writer": "agent"}},
		{"DELETE", "/agent/goal.profile?writer=menu&if_writer=menu", nil},
	} {
		status, body := e.do(c.method, c.path, c.body)
		if status != http.StatusForbidden || body["error"] != fserr.FlagsmithOnly {
			t.Fatalf("UC-053/A14 %s %s: %d %v, want 403 flagsmith_only", c.method, c.path, status, body)
		}
		if got := e.writes("rejected", fserr.FlagsmithOnly); got != rejected+int64(i+1) {
			t.Fatalf("UC-053/A14 rejected flagsmith_only %d, want %d", got, rejected+int64(i+1))
		}
	}
	if _, ferr := e.api.FlagsmithPut(context.Background(), state.WriteRequest{Set: "agent", Flag: "goal.profile", Variant: "quick_game"}); ferr != nil {
		t.Fatalf("UC-054/BR-011 the in-process write: %v", ferr)
	}
	for _, w := range []string{"flagsmith", "menu"} {
		e.do("DELETE", "/agent?writer="+w, nil)
	}
	if w := e.stackWriter("/agent/goal.profile"); w != registry.FlagsmithWriter {
		t.Fatalf("UC-053/A14 stack overlay writer %v after the writer resets, want flagsmith", w)
	}
}

// UC: UC-053 A14
// A writer reset clears a writer-list row only for a label in its list, and
// never for flagsmith, even when Flagsmith wrote the value.
func TestWriterListWriterReset(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, true)
	reset := func(w string) {
		t.Helper()
		if status, body := e.do("DELETE", "/agent?writer="+w, nil); status != http.StatusNoContent {
			t.Fatalf("DELETE /agent?writer=%s: %d %v", w, status, body)
		}
	}
	t.Run("UC-053/A14 a menu value survives ?writer=flagsmith and ?writer=stage, and goes with ?writer=menu", func(t *testing.T) {
		if status, body := e.do("PUT", "/agent/goal.profile", map[string]any{"variant": "quick_game", "writer": "menu"}); status != http.StatusOK {
			t.Fatalf("%d %v", status, body)
		}
		reset("flagsmith")
		reset("stage")
		if w := e.stackWriter("/agent/goal.profile"); w != "menu" {
			t.Fatalf("writer %v after the resets of other labels, want menu", w)
		}
		reset("menu")
		if w := e.stackWriter("/agent/goal.profile"); w != nil {
			t.Fatalf("writer %v after ?writer=menu, want none", w)
		}
	})
	t.Run("UC-053/A14 a Flagsmith value survives ?writer=flagsmith", func(t *testing.T) {
		if _, ferr := e.api.FlagsmithPut(ctx, state.WriteRequest{Set: "agent", Flag: "goal.profile", Variant: "lively_finale"}); ferr != nil {
			t.Fatal(ferr)
		}
		reset("flagsmith")
		reset("menu")
		if w := e.stackWriter("/agent/goal.profile"); w != registry.FlagsmithWriter {
			t.Fatalf("writer %v, want flagsmith", w)
		}
	})
	t.Run("UC-053/A14 ?writer=flagsmith still clears a row without a list", func(t *testing.T) {
		if _, ferr := e.api.FlagsmithPut(ctx, state.WriteRequest{Set: "agent", Flag: "objectives", Variant: "chaos_focused"}); ferr != nil {
			t.Fatal(ferr)
		}
		reset("flagsmith")
		if w := e.stackWriter("/agent/objectives"); w != nil {
			t.Fatalf("objectives writer %v after ?writer=flagsmith, want none", w)
		}
	})
}

// UC: UC-016 BR-013
// D14: ?if_writer= is a compare-and-delete on the in-memory stack overlay too,
// so the menu's reset (DELETE ?writer=menu&if_writer=menu) clears only its
// own overlay.
func TestIfWriterStackOverlay(t *testing.T) {
	ctx := context.Background()
	const goal = "/agent/goal.profile?writer=menu&if_writer=menu"
	t.Run("UC-016/BR-013 the menu's own overlay is cleared", func(t *testing.T) {
		e := newEnv(t, true)
		e.do("PUT", "/agent/goal.profile", map[string]any{"variant": "quick_game", "writer": "menu"})
		if status, body := e.do("DELETE", goal, nil); status != http.StatusNoContent {
			t.Fatalf("%d %v, want 204", status, body)
		}
		if w := e.stackWriter("/agent/goal.profile"); w != nil {
			t.Fatalf("writer %v, want none", w)
		}
	})
	t.Run("UC-016/BR-013 Flagsmith's overlay is 412 writer_mismatch and stays", func(t *testing.T) {
		e := newEnv(t, true)
		if _, ferr := e.api.FlagsmithPut(ctx, state.WriteRequest{Set: "agent", Flag: "goal.profile", Variant: "lively_finale"}); ferr != nil {
			t.Fatal(ferr)
		}
		v := e.st.Version()
		if status, body := e.do("DELETE", goal, nil); status != http.StatusPreconditionFailed || body["error"] != fserr.WriterMismatch {
			t.Fatalf("%d %v, want 412 writer_mismatch", status, body)
		}
		if w := e.stackWriter("/agent/goal.profile"); w != registry.FlagsmithWriter || e.st.Version() != v {
			t.Fatalf("writer %v, version %d -> %d: want the Flagsmith overlay unchanged", w, v, e.st.Version())
		}
	})
	t.Run("UC-016/BR-013 a stage script's overlay of a row without a list stays", func(t *testing.T) {
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
		if e.layer(backend, "admin_value") == nil {
			t.Fatal("the persisted admin value was cleared by an in-memory if_writer clear")
		}
	})
}

// UC: UC-054 BR-011
// The Flagsmith link's in-process path is a writer of goal.profile: its write
// and clear are accepted within the row's bounds.
func TestWriterListWrittenInProcess(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, true)
	stackVariant := func() string {
		t.Helper()
		v, _ := e.layer("/agent/goal.profile", "stack_overlay")["variant"].(string)
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
