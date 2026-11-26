package registry

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/joustmania/flag-service/baseline"
)

// The Flagsmith-only rows (#2705, FR-226, flags.md §13 "Flagsmith only") carry
// flagsmith_only: true in registry.yaml; each one is a stack overlay row without
// persist, outside interventions.
//
// UC: UC-053 A14
func TestFlagsmithOnlyRows(t *testing.T) {
	for _, dir := range []string{"", "ci"} {
		t.Run("UC-053/A14 agent/goal.profile is the Flagsmith-only stack overlay row against flagd/"+dir, func(t *testing.T) {
			reg := loadReal(t, dir)
			var got []string
			for _, p := range reg.Policies() {
				if !p.IsFlagsmithOnly() {
					continue
				}
				got = append(got, id(p.Set, p.Key))
				if p.Set == "interventions" || p.Kind != KindOverlay || !reflect.DeepEqual(p.Scopes, []string{ScopeStack}) || p.Persist {
					t.Errorf("%s/%s: %+v, want an overlay of scope [stack] without persist outside interventions", p.Set, p.Key, p)
				}
			}
			if want := []string{"agent/goal.profile"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("Flagsmith-only rows %v, want %v", got, want)
			}
		})
	}
	t.Run("UC-053/A14 an unmarked row is not Flagsmith only", func(t *testing.T) {
		reg := loadReal(t, "ci")
		for _, k := range []string{"objectives", "enabled", "decision.backend"} {
			if reg.Get("agent", k).IsFlagsmithOnly() {
				t.Errorf("agent/%s is Flagsmith only", k)
			}
		}
		var nilPolicy *Policy
		if nilPolicy.IsFlagsmithOnly() {
			t.Error("a nil policy (read-only baseline) is Flagsmith only")
		}
	})
}

// A Flagsmith-only row that is not a stack overlay without persist is refused at
// start, like any other bad row.
//
// UC: UC-053 A14
func TestParseRejectsBadFlagsmithOnlyRows(t *testing.T) {
	b, err := baseline.Load(filepath.Join(root(t), "..", "flagd", "ci"), baseline.DefaultFiles)
	if err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"interventions row": `flags: [{set: interventions, key: volume_override, kind: state, scope: [game], flagsmith_only: true, bounds: {range: [0.3, 1.0]}}]`,
		"persist row":       `flags: [{set: agent, key: interventions_allowed, kind: overlay, scope: [stack], persist: true, flagsmith_only: true}]`,
		"wider scope":       `flags: [{set: game, key: sensitivity, kind: overlay, scope: [stack, experiment+arm], flagsmith_only: true, description: "x"}]`,
	}
	for name, doc := range bad {
		t.Run("UC-053/A14 "+name+" is refused", func(t *testing.T) {
			if _, err := Parse([]byte(doc), b); err == nil || !strings.Contains(err.Error(), "flagsmith_only") {
				t.Errorf("%s: %v, want a Flagsmith-only refusal", name, err)
			}
		})
	}
	t.Run("UC-053/A14 a stack overlay without persist is accepted", func(t *testing.T) {
		r, err := Parse([]byte(`flags: [{set: agent, key: goal.profile, kind: overlay, scope: [stack], flagsmith_only: true}]`), b)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Get("agent", "goal.profile").IsFlagsmithOnly() {
			t.Fatal("flagsmith_only lost")
		}
	})
}
