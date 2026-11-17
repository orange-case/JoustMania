package state

import (
	"strings"
	"testing"
)

// decisionMeta returns the flag's metadata keys under the reserved
// decisionId. prefix (flags.md §5, #2665), keyed by variant.
func decisionMeta(f *fixture, key string) map[string]any {
	f.t.Helper()
	meta, _ := f.snapshotFlag(key)["metadata"].(map[string]any)
	out := map[string]any{}
	for k, v := range meta {
		if variant, ok := strings.CutPrefix(k, "decisionId."); ok {
			out[variant] = v
		}
	}
	return out
}

// UC: UC-101 BR-003 A3
func TestDecisionID_ServedWithTheValue(t *testing.T) {
	t.Run("UC-101/BR-003 an event entry carries the decision_id of its write", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{k: 3})
		f.register("game_a", "c/1", "MOCK0001", "MOCK0002")
		for _, w := range []struct{ serial, id string }{{"MOCK0001", "int_1"}, {"MOCK0002", ""}} {
			if _, err := f.state.Append(AppendRequest{Set: "interventions", Flag: "eliminate_player", Scope: Scope{GameID: "game_a"}, Payload: map[string]any{"serial": w.serial}, Writer: "agent", DecisionID: w.id}); err != nil {
				t.Fatal(err)
			}
		}
		_, val := f.resolve("eliminate_player", map[string]any{"gameId": "game_a"})
		events := val.(map[string]any)["events"].([]any)
		first, second := events[0].(map[string]any), events[1].(map[string]any)
		if first["decision_id"] != "int_1" || first["seq"] != float64(1) || first["payload"].(map[string]any)["serial"] != "MOCK0001" {
			t.Fatalf("first event %v, want seq 1, payload and decision_id int_1", first)
		}
		// UC-101 A3: a write without an id renders no key at all.
		if _, ok := second["decision_id"]; ok || len(second) != 2 {
			t.Fatalf("second event %v, want only seq and payload", second)
		}
	})

	t.Run("UC-101/BR-003 a grant, a level and a state carry decisionId.<variant> of the resolved variant", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		f.register("game_a", "c/1", "MOCK0001", "MOCK0002")
		grant := f.put(WriteRequest{Set: "interventions", Flag: "shield_seconds", Value: raw("3"), Scope: Scope{GameID: "game_a", Serial: "MOCK0001"}, Writer: "agent", DecisionID: "int_g"})
		level := f.put(WriteRequest{Set: "interventions", Flag: "player_handicap_factor", Value: raw("1.5"), Scope: Scope{GameID: "game_a", Serial: "MOCK0002"}, Writer: "agent", DecisionID: "int_l"})
		state := f.put(WriteRequest{Set: "interventions", Flag: "pacing_profile", Variant: "calm", Scope: Scope{GameID: "game_a"}, Writer: "agent", DecisionID: "int_s"})
		for _, c := range []struct {
			flag, variant, id string
			ctx               map[string]any
		}{
			{"shield_seconds", grant.Variant, "int_g", map[string]any{"gameId": "game_a", "targetingKey": "MOCK0001"}},
			{"player_handicap_factor", level.Variant, "int_l", map[string]any{"gameId": "game_a", "targetingKey": "MOCK0002"}},
			{"pacing_profile", state.Variant, "int_s", map[string]any{"gameId": "game_a"}},
		} {
			resolved, _ := f.resolve(c.flag, c.ctx)
			if !strings.HasPrefix(resolved, "re1_") || resolved != c.variant {
				t.Fatalf("%s resolved %q, want the generated variant %q", c.flag, resolved, c.variant)
			}
			if got := decisionMeta(f, c.flag); len(got) != 1 || got[resolved] != c.id {
				t.Fatalf("%s decision metadata %v, want {%s: %s}", c.flag, got, resolved, c.id)
			}
		}
		meta := f.snapshotFlag("shield_seconds")["metadata"].(map[string]any)
		if meta["flagSetId"] != "interventions" || meta[MetaSnapshotEpoch] != "e1" || meta[MetaSnapshotVersion] == nil {
			t.Fatalf("metadata %v: the id must not displace flagSetId or the snapshot stamp", meta)
		}
	})

	t.Run("UC-101/A3 a value written without an id carries no decisionId key", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		f.register("game_a", "c/1", "MOCK0001")
		f.put(WriteRequest{Set: "interventions", Flag: "shield_seconds", Value: raw("3"), Scope: Scope{GameID: "game_a", Serial: "MOCK0001"}, Writer: "test"})
		if got := decisionMeta(f, "shield_seconds"); len(got) != 0 {
			t.Fatalf("decision metadata %v, want none", got)
		}
	})

	t.Run("UC-101/BR-003 the id is dropped with its value: overwrite, clear and game end", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		f.register("game_a", "c/1", "MOCK0001")
		f.put(WriteRequest{Set: "interventions", Flag: "shield_seconds", Value: raw("3"), Scope: Scope{GameID: "game_a", Serial: "MOCK0001"}, DecisionID: "int_1"})
		second := f.put(WriteRequest{Set: "interventions", Flag: "shield_seconds", Value: raw("4"), Scope: Scope{GameID: "game_a", Serial: "MOCK0001"}, DecisionID: "int_2"})
		if got := decisionMeta(f, "shield_seconds"); len(got) != 1 || got[second.Variant] != "int_2" {
			t.Fatalf("after an overwrite: %v, want only {%s: int_2}", got, second.Variant)
		}
		if _, err := f.state.Clear(ClearRequest{Set: "interventions", Flag: "shield_seconds", Scope: Scope{GameID: "game_a", Serial: "MOCK0001"}}); err != nil {
			t.Fatalf("clear: %v", err)
		}
		if got := decisionMeta(f, "shield_seconds"); len(got) != 0 {
			t.Fatalf("after a clear: %v, want none", got)
		}
		f.put(WriteRequest{Set: "interventions", Flag: "volume_override", Value: raw("0.5"), Scope: Scope{GameID: "game_a"}, DecisionID: "int_3"})
		f.put(WriteRequest{Set: "interventions", Flag: "shield_seconds", Value: raw("5"), Scope: Scope{GameID: "game_a", Serial: "MOCK0001"}, DecisionID: "int_4"})
		if len(decisionMeta(f, "volume_override")) != 1 || len(decisionMeta(f, "shield_seconds")) != 1 {
			t.Fatal("the ids were not served before the game end")
		}
		if _, found := f.state.EndGame("game_a", "test"); !found {
			t.Fatal("game_a not found")
		}
		for _, key := range []string{"volume_override", "shield_seconds"} {
			if got := decisionMeta(f, key); len(got) != 0 {
				t.Fatalf("%s after the game end: %v, want none", key, got)
			}
		}
	})

	t.Run("UC-101/BR-003 overlay values carry no id", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		f.put(WriteRequest{Set: "game", Flag: "sensitivity", Variant: "slow", Writer: "test", DecisionID: "int_stack"})
		f.put(WriteRequest{Set: "agent", Flag: "policy.max_shields_per_player_per_game", Value: raw("5"), Writer: "test", DecisionID: "int_overlay"})
		for _, key := range []string{"sensitivity", "policy.max_shields_per_player_per_game"} {
			if got := decisionMeta(f, key); len(got) != 0 {
				t.Fatalf("%s overlay decision metadata %v, want none", key, got)
			}
		}
	})
}
