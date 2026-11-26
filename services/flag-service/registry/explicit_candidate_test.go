package registry

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joustmania/flag-service/baseline"
)

// Every shipped experiment_dynamic_candidates variant names experiment-arm game
// keys, and every `flag=variant` entry a declared variant of that key, so the
// dry-run's explicit candidate is never skipped.
//
// UC: UC-043 BR-012
func TestExplicitCandidateVariants(t *testing.T) {
	for _, dir := range []string{"", "ci"} {
		t.Run("UC-043/BR-012 services/flagd/"+dir+" candidates are declarable", func(t *testing.T) {
			b, err := baseline.Load(filepath.Join(root(t), "..", "flagd", dir), baseline.DefaultFiles)
			if err != nil {
				t.Fatal(err)
			}
			reg, err := LoadFile(filepath.Join(root(t), "registry.yaml"), b)
			if err != nil {
				t.Fatal(err)
			}
			flag := b.Flag("agent", "experiment_dynamic_candidates")
			if flag == nil {
				t.Fatal("experiment_dynamic_candidates missing")
			}
			for name, raw := range flag.Variants {
				var list string
				if err := json.Unmarshal(raw, &list); err != nil {
					t.Fatalf("variant %s is not a string: %v", name, err)
				}
				for _, entry := range strings.Split(list, ",") {
					key, variant, explicit := strings.Cut(strings.TrimSpace(entry), "=")
					if key == "" {
						continue
					}
					if p := reg.Get("game", key); p == nil || !p.AllowsScope(ScopeExperiment) {
						t.Fatalf("variant %s: game/%s has no registry row with %s scope", name, key, ScopeExperiment)
					}
					g := b.Flag("game", key)
					if g == nil {
						t.Fatalf("variant %s: game/%s is not in the baseline", name, key)
					}
					if _, ok := g.Variants[variant]; explicit && !ok {
						t.Fatalf("variant %s: game/%s has no variant %q", name, key, variant)
					}
				}
			}
		})
	}
}
