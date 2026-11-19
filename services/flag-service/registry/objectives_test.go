package registry

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"testing"

	"github.com/joustmania/flag-service/baseline"
)

// agent/objectives (the goal, flags.md §13.2) is runtime-writable by the operator: a
// stack-wide overlay, not persisted, and only one of its declared variants. Every
// variant of both baselines is allowed, and a free weighting is refused, so a write
// can pick a reviewed goal but never invent one.
//
// UC: UC-053 FR-133
func TestObjectivesWritableDeclaredVariantsOnly(t *testing.T) {
	for _, dir := range []string{"", "ci"} {
		t.Run("UC-053/FR-133 services/flagd/"+dir+" objectives takes its declared variants only", func(t *testing.T) {
			b, err := baseline.Load(filepath.Join(root(t), "..", "flagd", dir), baseline.DefaultFiles)
			if err != nil {
				t.Fatal(err)
			}
			reg, err := LoadFile(filepath.Join(root(t), "registry.yaml"), b)
			if err != nil {
				t.Fatal(err)
			}
			flag := b.Flag("agent", "objectives")
			p := reg.Get("agent", "objectives")
			if flag == nil || p == nil {
				t.Fatalf("objectives: flag %v, registry row %v; want both", flag != nil, p != nil)
			}
			if p.Kind != KindOverlay || !p.AllowsScope(ScopeStack) || len(p.Scopes) != 1 || p.Persist {
				t.Fatalf("objectives policy kind=%s scope=%v persist=%v, want overlay, [stack] only, not persisted", p.Kind, p.Scopes, p.Persist)
			}
			declared := flag.VariantNames()
			allowed := append([]string(nil), p.Bounds.Variants...)
			sort.Strings(declared)
			sort.Strings(allowed)
			if len(declared) != len(allowed) {
				t.Fatalf("registry variants %v, flag variants %v: every variant must be listed", allowed, declared)
			}
			for i := range declared {
				if declared[i] != allowed[i] {
					t.Fatalf("registry variants %v, flag variants %v: every variant must be listed", allowed, declared)
				}
				if _, ferr := CheckValue(p, flag, declared[i], nil); ferr != nil {
					t.Errorf("variant %s refused: %v", declared[i], ferr)
				}
			}
			free := json.RawMessage(`{"endurance":0.25,"balanced":0.25,"accelerate":0.25,"chaos":0.25,"x":1}`)
			if _, ferr := CheckValue(p, flag, "", free); ferr == nil {
				t.Fatalf("free weighting %s accepted, want refused", free)
			}
			if _, ferr := CheckValue(p, flag, "", json.RawMessage(`{"endurance":1,"balanced":0,"accelerate":0,"chaos":0}`)); ferr == nil {
				t.Fatal("an undeclared weighting accepted, want refused (declared variants only)")
			}
		})
	}
}
