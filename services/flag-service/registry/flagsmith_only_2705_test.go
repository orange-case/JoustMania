package registry

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/joustmania/flag-service/baseline"
)

// The Flagsmith-only set (#2705, FR-226, flags.md §13 "Flagsmith only") is a Go
// set, not a registry.yaml attribute: released images parse the checkout's
// registry.yaml strictly (#2403). flags.md §13 names the same flags; each one is
// a stack overlay row without persist, outside interventions.
//
// UC: UC-053 A14
func TestFlagsmithOnlySetMatchesFlagsMD(t *testing.T) {
	t.Run("UC-053/A14 flags.md §13 lists exactly the Go Flagsmith-only set", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join(root(t), "..", "..", "docs", "specs", "flags.md"))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ReplaceAll(string(raw), "\r\n", "\n")
		const marker = "**Flagsmith only (#2705"
		start := strings.Index(text, marker)
		if start < 0 {
			t.Fatalf("flags.md has no %q paragraph", marker)
		}
		para := text[start:]
		if end := strings.Index(para, "\n\n"); end > 0 {
			para = para[:end]
		}
		// The paragraph names its flags after "single source of truth".
		const lead = "single source of truth"
		i := strings.Index(para, lead)
		if i < 0 {
			t.Fatalf("the Flagsmith-only paragraph has no %q sentence", lead)
		}
		tail := para[i:]
		if end := strings.Index(tail, ". "); end > 0 {
			tail = tail[:end]
		}
		var got []string
		for _, m := range backticked.FindAllStringSubmatch(tail, -1) {
			if strings.Contains(m[1], "/") && !strings.ContainsAny(m[1], " ?{") {
				got = append(got, m[1])
			}
		}
		sort.Strings(got)
		if want := FlagsmithOnlyFlags(); len(want) == 0 || !reflect.DeepEqual(got, want) {
			t.Fatalf("flags.md Flagsmith-only paragraph names %v, the Go set is %v", got, want)
		}
	})
	for _, dir := range []string{"", "ci"} {
		t.Run("UC-053/A14 every Flagsmith-only flag is a stack overlay row without persist against flagd/"+dir, func(t *testing.T) {
			reg := loadReal(t, dir)
			for _, name := range FlagsmithOnlyFlags() {
				set, key, _ := strings.Cut(name, "/")
				p := reg.Get(set, key)
				if p == nil {
					t.Errorf("%s: Flagsmith-only flag with no registry row", name)
					continue
				}
				if !p.FlagsmithOnly() {
					t.Errorf("%s: FlagsmithOnly is false", name)
				}
				if p.Set == "interventions" || p.Kind != KindOverlay || !reflect.DeepEqual(p.Scopes, []string{ScopeStack}) || p.Persist {
					t.Errorf("%s: %+v, want an overlay of scope [stack] without persist outside interventions", name, p)
				}
			}
			for _, p := range reg.Policies() {
				if p.FlagsmithOnly() && !strings.Contains(strings.Join(FlagsmithOnlyFlags(), ","), id(p.Set, p.Key)) {
					t.Errorf("%s/%s: FlagsmithOnly but not in the set", p.Set, p.Key)
				}
			}
		})
	}
	t.Run("UC-053/A14 an unmarked row is not Flagsmith only", func(t *testing.T) {
		reg := loadReal(t, "ci")
		for _, k := range []string{"objectives", "enabled", "decision.backend"} {
			if reg.Get("agent", k).FlagsmithOnly() {
				t.Errorf("agent/%s is Flagsmith only", k)
			}
		}
		var nilPolicy *Policy
		if nilPolicy.FlagsmithOnly() {
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
	for _, name := range []string{"interventions/volume_override", "agent/interventions_allowed", "game/sensitivity"} {
		flagsmithOnly[name] = true
	}
	t.Cleanup(func() {
		for _, name := range []string{"interventions/volume_override", "agent/interventions_allowed", "game/sensitivity"} {
			delete(flagsmithOnly, name)
		}
	})
	bad := map[string]string{
		"interventions row": `flags: [{set: interventions, key: volume_override, kind: state, scope: [game], bounds: {range: [0.3, 1.0]}}]`,
		"persist row":       `flags: [{set: agent, key: interventions_allowed, kind: overlay, scope: [stack], persist: true}]`,
		"wider scope":       `flags: [{set: game, key: sensitivity, kind: overlay, scope: [stack, experiment+arm], description: "x"}]`,
	}
	for name, doc := range bad {
		t.Run("UC-053/A14 "+name+" is refused", func(t *testing.T) {
			if _, err := Parse([]byte(doc), b); err == nil || !strings.Contains(err.Error(), "Flagsmith only") {
				t.Errorf("%s: %v, want a Flagsmith-only refusal", name, err)
			}
		})
	}
	t.Run("UC-053/A14 a stack overlay without persist is accepted", func(t *testing.T) {
		if _, err := Parse([]byte(`flags: [{set: agent, key: goal.profile, kind: overlay, scope: [stack]}]`), b); err != nil {
			t.Fatal(err)
		}
	})
}
