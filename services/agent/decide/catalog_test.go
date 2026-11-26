package decide

import "testing"

// TestCatalogRowsWellFormed: every row has a unique non-noop name, a positive
// weight and an effect text; the derived sets agree with the rows.
func TestCatalogRowsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, iv := range Catalog {
		if iv.Name == "" || iv.Name == IntvNoop || seen[iv.Name] {
			t.Errorf("bad or duplicate catalog name %q", iv.Name)
		}
		seen[iv.Name] = true
		if iv.Weight <= 0 || iv.Effect == "" {
			t.Errorf("%s: weight %v, effect %q", iv.Name, iv.Weight, iv.Effect)
		}
		if got := PlayerTargeted(iv.Name); got != iv.PlayerTargeted {
			t.Errorf("PlayerTargeted(%s) = %v, row says %v", iv.Name, got, iv.PlayerTargeted)
		}
	}
	if PlayerTargeted(IntvNoop) {
		t.Error("noop must not be player-targeted")
	}
}
