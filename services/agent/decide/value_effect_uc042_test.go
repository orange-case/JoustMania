package decide

import "testing"

// UC: UC-042 BR-010
// Every lever with a numeric value has a ValueEffect text, and no other lever has
// one, so the retro catalog says the direction of every value it shows.
func TestCatalogValueEffect(t *testing.T) {
	t.Run("UC-042/BR-010 value effect exactly for the bounded levers", func(t *testing.T) {
		for _, iv := range Catalog {
			_, bounded := MagnitudeBounds(iv.Name)
			if bounded != (iv.ValueEffect != "") {
				t.Errorf("%s: bounded=%v but ValueEffect=%q", iv.Name, bounded, iv.ValueEffect)
			}
		}
		for _, name := range MagnitudeInterventions() {
			if _, ok := ByName(name); !ok {
				t.Errorf("bounded lever %s is not in the catalog", name)
			}
		}
	})
}
