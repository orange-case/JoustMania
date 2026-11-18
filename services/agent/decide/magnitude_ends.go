package decide

// magnitude_ends.go: what each end of a numeric lever's range does (#2696, UC-040
// BR-045). The ffa.magnitude text names it next to each range end, so the model can
// tell which end helps the target: with the per-player sensitivity floor at 0.2 a
// wrong guess eliminates the player it meant to save. Effects, never purposes, so
// the same words serve the v1 and the neutral prompt profile. jev's ladder ends
// (backends/jev/criteria.go magnitudeEnds) say the same in its own words; a test
// keeps the directions in sync.
//
// The parallel retro catalog (#2697) adds a ValueEffect text per lever with the
// same directions; the two should become one source.

// MagnitudeEnds are the effects of a lever's lowest and highest value.
type MagnitudeEnds struct {
	Low, High string
	// HelpIsHigh says which end protects the player(s) the lever acts on; HelpKnown
	// is false for a lever that does not move the death threshold (volume).
	HelpIsHigh, HelpKnown bool
}

var magnitudeEnds = map[string]MagnitudeEnds{
	"adjust_music_tempo":        {Low: "slower music, everyone easier to eliminate", High: "faster music, everyone harder to eliminate", HelpIsHigh: true, HelpKnown: true},
	"adjust_volume":             {Low: "quieter", High: "louder"},
	"adjust_global_sensitivity": {Low: "everyone easier to eliminate", High: "everyone harder to eliminate", HelpIsHigh: true, HelpKnown: true},
	"adjust_player_sensitivity": {Low: "target harder to eliminate", High: "target easier to eliminate", HelpIsHigh: false, HelpKnown: true},
	"adjust_global_difficulty":  {Low: "everyone harder to eliminate", High: "everyone easier to eliminate", HelpIsHigh: false, HelpKnown: true},
	"set_player_handicap":       {Low: "target easier to eliminate", High: "target harder to eliminate", HelpIsHigh: true, HelpKnown: true},
	"grant_shield":              {Low: "shortest protection", High: "longest protection", HelpIsHigh: true, HelpKnown: true},
	"toughness_boost":           {Low: "shortest boost", High: "longest boost", HelpIsHigh: true, HelpKnown: true},
}

// MagnitudeEndEffects returns what intervention's lowest and highest value do, and
// false for an intervention without numeric bounds.
func MagnitudeEndEffects(intervention string) (MagnitudeEnds, bool) {
	e, ok := magnitudeEnds[intervention]
	return e, ok
}
