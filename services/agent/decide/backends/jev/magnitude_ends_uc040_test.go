package jev

import (
	"testing"

	"github.com/joustmania/agent/decide"
)

// UC: UC-040 BR-045
// magnitude_ends_uc040_test.go: jev's ladder-end labels and two-way directions
// agree with the decide question's end effects (#2696): the end jev calls
// protective is the end decide says helps.

// jevHelpEnd is the end of each jev ladder label pair that protects the player(s):
// true = the high end, read from the labels in criteria.go magnitudeEnds.
var jevHelpEnd = map[string]bool{
	"adjust_music_tempo":        true,  // "fastest music, most movement tolerated"
	"adjust_global_sensitivity": true,  // "everyone hardest to eliminate"
	"adjust_player_sensitivity": false, // "player most protected" is the low end
	"adjust_global_difficulty":  false, // "easiest game" is the low end
	"set_player_handicap":       true,  // "biggest help for the player"
	"grant_shield":              true,  // "longest shield"
	"toughness_boost":           true,  // "longest toughness boost"
}

func TestMagnitudeEnds_JevAgreesWithDecide(t *testing.T) {
	for _, iv := range decide.MagnitudeInterventions() {
		t.Run("UC-040/BR-045 "+iv, func(t *testing.T) {
			e, _ := decide.MagnitudeEndEffects(iv)
			if _, ok := magnitudeEnds[iv]; !ok {
				t.Fatalf("jev has no ladder ends for %s", iv)
			}
			if !e.HelpKnown {
				return
			}
			if jevHelpEnd[iv] != e.HelpIsHigh {
				t.Fatalf("%s: jev helps at high=%v, decide at high=%v", iv, jevHelpEnd[iv], e.HelpIsHigh)
			}
			if tw, ok := twoWay[iv]; ok && tw.helpIsHigh != e.HelpIsHigh {
				t.Fatalf("%s: jev two-way helpIsHigh=%v, decide %v", iv, tw.helpIsHigh, e.HelpIsHigh)
			}
		})
	}
}
