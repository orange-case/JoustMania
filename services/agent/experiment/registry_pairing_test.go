package experiment

import (
	"testing"

	"github.com/joustmania/agent/patterngame"
)

// UC: UC-046 BR-010
//
// TestPlanPairLocked pins the arm/pair arithmetic nextPair delegates to
// planPairLocked (#2635): the CRN pair (#1003) is reserved as min(remaining arms in
// the pair, free effective budget, equal-share room), a pattern experiment spawns
// one arm at a time and stops after its last pinned archetype (UC-046 A6), and an
// experiment that is not pinned yet never spawns. planPairLocked is side-effect
// free: it moves no cursor and reserves no slot.
func TestPlanPairLocked(t *testing.T) {
	arms := []string{"experimental", "control"}
	twoArchetypes := testPin(t, "arch_a", "arch_b")

	cases := []struct {
		name      string
		patterns  bool             // registry has a PatternPinner
		pin       *patterngame.Pin // the experiment's pin
		armCursor int              // arms dispatched so far
		games     map[string]string
		free      int
		share     int
		wantOK    bool
		wantN     int
		wantPair  int
		wantArch  bool // a pattern arm (PatternSpawn set)
	}{
		{name: "UC-046/BR-010 a: free 1 share 2 fresh pair reserves one arm",
			free: 1, share: 2, wantOK: true, wantN: 1, wantPair: 0},
		{name: "UC-046/BR-010 b: the sibling arm keeps the pair index",
			armCursor: 1, games: map[string]string{"game_a": "experimental"},
			free: 1, share: 2, wantOK: true, wantN: 1, wantPair: 0},
		{name: "UC-046/BR-010 c: share room bounds a large free budget",
			free: 4, share: 1, wantOK: true, wantN: 1, wantPair: 0},
		{name: "UC-046/BR-010 fresh pair with room reserves both arms",
			armCursor: 2, free: 4, share: 4, wantOK: true, wantN: 2, wantPair: 1},
		{name: "UC-046/A6 d: pinned pattern past its last archetype is skipped",
			patterns: true, pin: &twoArchetypes, armCursor: 4,
			free: 4, share: 4, wantOK: false},
		{name: "UC-046/A6 pinned pattern spawns one arm with its archetype",
			patterns: true, pin: &twoArchetypes, armCursor: 2,
			free: 4, share: 4, wantOK: true, wantN: 1, wantPair: 1, wantArch: true},
		{name: "UC-046/A1 e: patterns set but not pinned is skipped",
			patterns: true, free: 4, share: 4, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			games := tc.games
			if games == nil {
				games = map[string]string{}
			}
			e := &entry{id: "exp", status: StatusRunning, arms: arms, games: games,
				gamePairs: map[string]int{}, armCursor: tc.armCursor, pin: tc.pin}
			r := &Registry{entries: map[string]*entry{"exp": e}}
			if tc.patterns {
				r.patterns = &queuePinner{pins: []patterngame.Pin{twoArchetypes}}
			}
			spawnsBefore := r.spawnsInFlight
			gamesBefore := len(e.games)

			n, pairIndex, pattern, ok := r.planPairLocked("exp", e, tc.free, tc.share)

			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok {
				if n != tc.wantN || pairIndex != tc.wantPair {
					t.Errorf("n, pairIndex = %d, %d, want %d, %d", n, pairIndex, tc.wantN, tc.wantPair)
				}
				if (pattern != nil) != tc.wantArch {
					t.Errorf("pattern = %v, want set = %v", pattern, tc.wantArch)
				}
				if pattern != nil && (pattern.PairIndex != pairIndex || pattern.Archetype.Name != tc.pin.Archetypes[pairIndex].Name) {
					t.Errorf("pattern = %+v, want archetype %q at pair %d", pattern, tc.pin.Archetypes[pairIndex].Name, pairIndex)
				}
			}
			if e.armCursor != tc.armCursor || r.spawnsInFlight != spawnsBefore || len(e.games) != gamesBefore {
				t.Errorf("planPairLocked had side effects: armCursor %d->%d, spawnsInFlight %d->%d, games %d->%d",
					tc.armCursor, e.armCursor, spawnsBefore, r.spawnsInFlight, gamesBefore, len(e.games))
			}
		})
	}
}
