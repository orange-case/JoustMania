package jev

import (
	"strings"
	"testing"

	"github.com/joustmania/agent/decide"
)

// UC: UC-040 BR-045
// magnitude_ends_uc040_test.go: jev's ladder-end labels and two-way directions
// agree with the decide question's end effects (#2696): the end jev's own label
// calls protective is the end decide says helps. The protective end is read from
// the label text in criteria.go magnitudeEnds, so a swapped label fails here.

// protectiveWords mark a jev end label that protects the player(s) the lever acts on.
var protectiveWords = []string{"most protected", "help", "hardest to eliminate", "easiest game", "most movement tolerated", "longest"}

func protective(label string) bool {
	for _, w := range protectiveWords {
		if strings.Contains(label, w) {
			return true
		}
	}
	return false
}

func TestMagnitudeEnds_JevAgreesWithDecide(t *testing.T) {
	for _, iv := range decide.MagnitudeInterventions() {
		t.Run("UC-040/BR-045 "+iv, func(t *testing.T) {
			e, _ := decide.MagnitudeEndEffects(iv)
			ends, ok := magnitudeEnds[iv]
			if !ok {
				t.Fatalf("jev has no ladder ends for %s", iv)
			}
			if !e.HelpKnown {
				return
			}
			low, high := protective(ends[0]), protective(ends[1])
			if low == high {
				t.Fatalf("%s: jev labels %q / %q do not name exactly one protective end", iv, ends[0], ends[1])
			}
			if high != e.HelpIsHigh {
				t.Fatalf("%s: jev label helps at high=%v (%q / %q), decide at high=%v", iv, high, ends[0], ends[1], e.HelpIsHigh)
			}
			if tw, ok := twoWay[iv]; ok && tw.helpIsHigh != e.HelpIsHigh {
				t.Fatalf("%s: jev two-way helpIsHigh=%v, decide %v", iv, tw.helpIsHigh, e.HelpIsHigh)
			}
		})
	}
}
