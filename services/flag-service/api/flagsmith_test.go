package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/state"
)

// UC: UC-053 BR-002
// The writer label "flagsmith" is reserved for the Flagsmith link
// (services.md §7.2/§7.3, the one exception to D11): an HTTP write that labels
// itself with it is 400 malformed, counted, and changes nothing; naming it as
// a selector stays allowed.
func TestReservedWriterLabel(t *testing.T) {
	e := newEnv(t, true)
	e.do("PUT", "/games/game_a", map[string]any{"coordinator_instance": "c1", "game_kind": "shadow", "serials": []string{"MOCK0001"}})
	before := e.st.Version()
	rejected := e.writes("rejected", "malformed")
	cases := []struct {
		name, method, path string
		body               any
	}{
		{"PUT", "PUT", "/agent/decision.backend", map[string]any{"variant": "jev", "writer": "flagsmith"}},
		{"PUT unknown flag answers 400, not 404", "PUT", "/agent/nope", map[string]any{"variant": "jev", "writer": "flagsmith"}},
		{"POST", "POST", "/interventions/audio_cue", map[string]any{"scope": map[string]any{"game_id": "game_a"}, "payload": map[string]any{"sound": "agent_cue"}, "writer": "flagsmith"}},
		{"DELETE flag", "DELETE", "/agent/decision.backend?writer=flagsmith", nil},
	}
	for i, c := range cases {
		status, body := e.do(c.method, c.path, c.body)
		if status != http.StatusBadRequest || body["error"] != fserr.Malformed {
			t.Fatalf("UC-053/BR-002 %s: %d %v, want 400 malformed", c.name, status, body)
		}
		if got := e.writes("rejected", "malformed"); got != rejected+int64(i+1) {
			t.Fatalf("UC-053/BR-002 %s: rejected malformed %d, want %d", c.name, got, rejected+int64(i+1))
		}
	}
	if e.st.Version() != before {
		t.Fatalf("UC-053/BR-002 version moved %d -> %d: a refused write changed state", before, e.st.Version())
	}
	if status, _ := e.do("DELETE", "/agent?writer=flagsmith", nil); status != http.StatusNoContent {
		t.Fatalf("UC-053/BR-002 DELETE /{set}?writer=flagsmith: %d, want 204 (a selector)", status)
	}
	if status, _ := e.do("DELETE", "/game/sensitivity?game_kind=real&if_writer=flagsmith", nil); status != http.StatusNoContent {
		t.Fatalf("UC-053/BR-002 if_writer=flagsmith: %d, want 204 (a selector)", status)
	}
}

// UC: UC-054 BR-003
// The in-process entry: same validation, span and metrics as HTTP, writer
// forced to flagsmith; experiment-arm, real and fraction scopes are refused
// first with 422 flagsmith_scope_read_only.
func TestFlagsmithInProcessWrites(t *testing.T) {
	ctx := context.Background()
	t.Run("UC-054/BR-003 a stack write is labelled flagsmith on its span and counted", func(t *testing.T) {
		e := newEnv(t, true)
		ok := e.writes("ok", "ok")
		res, ferr := e.api.FlagsmithPut(ctx, state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev", Writer: "someone"})
		if ferr != nil || res.Variant != "jev" {
			t.Fatalf("res %+v err %v", res, ferr)
		}
		if w := e.writeSpan("decision.backend")["flagservice.writer"].AsString(); w != flagsmith.WriterLabel {
			t.Fatalf("span writer %q, want flagsmith", w)
		}
		if e.writes("ok", "ok") != ok+1 {
			t.Fatal("the in-process write was not counted")
		}
	})
	t.Run("UC-054/BR-003 experiment, real and fraction scopes are read-only, before any other check", func(t *testing.T) {
		e := newEnv(t, false) // no stream: a 503 would come first if the scope check were not first
		before := e.st.Version()
		pct := 10
		for _, req := range []state.WriteRequest{
			{Set: "game", Flag: "sensitivity", Variant: "slow", Scope: state.Scope{ExperimentID: "e1", Arm: "a"}},
			{Set: "game", Flag: "sensitivity", Variant: "slow", Scope: state.Scope{GameKind: "real"}},
			{Set: "controller", Flag: "bluetooth_backend", Variant: "rust", Scope: state.Scope{FractionPercent: &pct}},
		} {
			_, ferr := e.api.FlagsmithPut(ctx, req)
			if ferr == nil || ferr.Status != 422 || ferr.Reason != fserr.FlagsmithScopeReadOnly {
				t.Fatalf("%s %+v: %v, want 422 flagsmith_scope_read_only", req.Flag, req.Scope, ferr)
			}
		}
		_, ferr := e.api.FlagsmithClear(ctx, state.ClearRequest{Set: "game", Flag: "sensitivity", Scope: state.Scope{GameKind: "real"}})
		if ferr == nil || ferr.Reason != fserr.FlagsmithScopeReadOnly {
			t.Fatalf("clear real: %v, want flagsmith_scope_read_only", ferr)
		}
		if e.st.Version() != before {
			t.Fatal("a read-only scope write changed state")
		}
	})
}

// UC: UC-054 A1
// GET /state carries the link's status; the old fields stay top level.
func TestStateHasFlagsmithBlock(t *testing.T) {
	e := newEnv(t, true)
	status, body := e.do("GET", "/state", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /state %d", status)
	}
	fs, ok := body["flagsmith"].(map[string]any)
	if !ok || fs["link"] != "off" || fs["pending"] != float64(0) || fs["out_of_sync"] != float64(0) || fs["refused"] != float64(0) {
		t.Fatalf("flagsmith %v, want link off with zero counts", body["flagsmith"])
	}
	if _, has := fs["restart_rule"]; has {
		t.Fatalf("flagsmith %v: restart_rule before Start", fs)
	}
	if body["epoch"] != "e1" || body["snapshot_version"] == nil {
		t.Fatalf("state %v, want epoch and snapshot_version at the top level", body)
	}
}
