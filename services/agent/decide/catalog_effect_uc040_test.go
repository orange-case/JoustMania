package decide

import (
	"strings"
	"testing"
)

// UC: UC-040 BR-036
// catalog_effect_uc040_test.go: every lever text of the catalog describes an
// effect, never a purpose or a verdict (decide-contract.md §3).

func TestCatalog_EffectDescriptions(t *testing.T) {
	banned := []string{"unfairly", "wasted", "matters most", "lopsided", "to build tension", "to keep a lopsided game close", "saves a player", "to balance"}
	for _, iv := range Catalog {
		t.Run("UC-040/BR-036 "+iv.Name+" describes an effect", func(t *testing.T) {
			if iv.Effect == "" {
				t.Fatal("no effect text")
			}
			lower := strings.ToLower(iv.Effect)
			for _, b := range banned {
				if strings.Contains(lower, b) {
					t.Errorf("effect %q contains %q", iv.Effect, b)
				}
			}
		})
	}
}
