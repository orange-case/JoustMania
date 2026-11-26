package state

import (
	"testing"
)

// realRegistry mirrors registry.yaml: a hold sub-parameter and sensitivity in
// scope real, shield.activation without it.
const realRegistry = `
flags:
  - {set: game, key: shield.activation, kind: overlay, scope: [stack, experiment+arm], bounds: {variants: [instant, trigger, hold]}, description: "test row"}
  - {set: game, key: shield.energy_budget_seconds, kind: overlay, scope: [stack, experiment+arm, real], bounds: {range: [3, 30]}, description: "test row"}
  - {set: game, key: sensitivity, kind: overlay, scope: [stack, experiment+arm, real], description: "test row"}
`

var realScope = Scope{GameKind: "real"}

func budgetReal(value string) WriteRequest {
	return WriteRequest{Set: "game", Flag: "shield.energy_budget_seconds", Value: raw(value), Scope: realScope, Writer: "promotion"}
}

func (f *fixture) registerKind(id, kind string) {
	f.t.Helper()
	if _, err := f.state.RegisterGame(Game{ID: id, CoordinatorInstance: "c1", Kind: kind}); err != nil {
		f.t.Fatalf("register %s: %v", id, err)
	}
}

// The game latches every real-scoped key at its start (UC-048 BR-011), so a real
// write and its revert land at once, live real game or not.
//
// UC: UC-043 A17
func TestRealWriteDuringLiveRealGame(t *testing.T) {
	t.Run("UC-043/A17 a real write of a hold key lands while a real game is registered", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{registryYAML: realRegistry})
		f.registerKind("g-real", "real")
		f.put(budgetReal("20"))
		if _, val := f.resolve("shield.energy_budget_seconds", map[string]any{"game_kind": "real"}); val != float64(20) {
			t.Fatalf("real default = %v, want 20", val)
		}
	})

	t.Run("UC-043/A17 the revert of a hold key's real overlay lands while a real game is registered", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{registryYAML: realRegistry})
		f.put(budgetReal("20"))
		f.registerKind("g-real", "real")
		res, err := f.state.Clear(ClearRequest{Set: "game", Flag: "shield.energy_budget_seconds", Scope: realScope, Writer: "promotion", IfWriter: "promotion"})
		if err != nil || res.Cleared != 1 {
			t.Fatalf("clear: %+v %v, want 1 cleared", res, err)
		}
		if v, _ := f.resolve("shield.energy_budget_seconds", map[string]any{"game_kind": "real"}); v != "default" {
			t.Fatalf("real default variant %q after the revert, want default", v)
		}
	})
}
