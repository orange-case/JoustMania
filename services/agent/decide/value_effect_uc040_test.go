package decide

import (
	"strings"
	"testing"
)

// UC: UC-040 BR-045
// value_effect_uc040_test.go: which end of a lever helps is said in ONE place,
// MagnitudeEndEffects (#2696): the catalog's ValueEffect says what the value does
// and carries no direction of its own, so the decide question and the retro
// catalog cannot disagree.

func TestValueEffect_HasNoDirectionOfItsOwn(t *testing.T) {
	for _, iv := range Catalog {
		if iv.ValueEffect == "" {
			continue
		}
		t.Run("UC-040/BR-045 "+iv.Name, func(t *testing.T) {
			for _, w := range []string{"help", "hinder", "easier", "harder"} {
				if strings.Contains(strings.ToLower(iv.ValueEffect), w) {
					t.Fatalf("ValueEffect %q says %q: the direction belongs to MagnitudeEndEffects", iv.ValueEffect, w)
				}
			}
			if _, ok := MagnitudeEndEffects(iv.Name); !ok {
				t.Fatalf("%s has a ValueEffect but no end effects", iv.Name)
			}
		})
	}
}
