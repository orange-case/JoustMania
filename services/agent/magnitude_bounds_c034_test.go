package main

// UC: UC-040 BR-045
// magnitude_bounds_c034_test.go (#2696, C-034, FR-098): decision.magnitude_bounds
// limits the agent (the ranges its magnitudes come from), so the agent can never
// write it. Next to the general guards (agent_set_never_written_c034_test.go
// drives every writer and refuses any request to the agent set;
// agent_set_writers_guard_c034_test.go refuses any new write call site), this
// pins the flag itself: it is in the agent set only, its registry row allows the
// stack scope only (no experiment+arm, game or real scope an agent writer uses;
// the experiment catalog is the game set's experiment+arm rows, so a stack-only
// agent row cannot be in it), and no non-test agent source names it except its
// one reader.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joustmania/agent/flagservice/fake"
)

const magnitudeBoundsKey = "decision.magnitude_bounds"

func TestC034_MagnitudeBoundsAreNeverWrittenByTheAgent(t *testing.T) {
	t.Run("UC-040/BR-045 the flag is in the agent set only, stack scope only", func(t *testing.T) {
		for _, set := range []string{"agent", "interventions", "game", "rollout"} {
			policies, err := fake.RegistryPolicies(filepath.Join("..", "flag-service", "registry.yaml"), set)
			if err != nil {
				t.Fatal(err)
			}
			p, ok := policies[magnitudeBoundsKey]
			if set != "agent" {
				if ok {
					t.Errorf("C-034 VIOLATION: %s has a registry row in the %s set", magnitudeBoundsKey, set)
				}
				continue
			}
			if !ok {
				t.Fatalf("%s has no agent registry row", magnitudeBoundsKey)
			}
			if len(p.Scopes) != 1 || p.Scopes[0] != "stack" {
				t.Errorf("C-034 VIOLATION: %s scopes %v, want [stack]", magnitudeBoundsKey, p.Scopes)
			}
		}
		for _, file := range []string{"../flagd/game.json", "../flagd/interventions.json", "../flagd/ci/game.json", "../flagd/ci/interventions.json"} {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), magnitudeBoundsKey) {
				t.Errorf("C-034 VIOLATION: %s declares %s", file, magnitudeBoundsKey)
			}
		}
	})
	t.Run("UC-040/BR-045 only the reader names the key in the agent's sources", func(t *testing.T) {
		var named []string
		err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(raw), `"`+magnitudeBoundsKey+`"`) {
				named = append(named, filepath.ToSlash(path))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(named) != 1 || named[0] != "flags/magnitude_bounds.go" {
			t.Fatalf("C-034: the key literal is in %v, want only flags/magnitude_bounds.go (the read)", named)
		}
	})
}
