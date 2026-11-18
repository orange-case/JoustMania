package jev

import (
	"strings"
	"testing"

	"github.com/joustmania/agent/decide"
)

// UC: UC-040 BR-045
// magnitude_bounds_uc040_test.go: jev's magnitude ladders, levels and the mapping
// of its answer use the request's ranges (#2696, decide-contract.md §2.1): the
// "most protected" / "shortest shield" ends are the per-game ends.

func widenedTable() *decide.MagnitudeTable {
	return decide.ParseMagnitudeOverride(map[string]any{
		"adjust_player_sensitivity": map[string]any{"min": 0.2, "max": 1.8},
		"grant_shield":              map[string]any{"min": 2, "max": 8},
	})
}

func TestMagnitudeBounds_Jev(t *testing.T) {
	mt := widenedTable()
	t.Run("UC-040/BR-045 the first-stage ladder ends at the per-game range", func(t *testing.T) {
		req := testRequest()
		req.Bounds = mt
		var mag decide.Question
		for _, q := range req.Questions {
			if q.ID == decide.QuestionFFAMagnitude {
				mag = q
			}
		}
		_, ladder := scoreLadder(mag, req, "")
		if !strings.Contains(ladder[0], "grant_shield 2 seconds (shortest shield)") ||
			!strings.Contains(ladder[len(ladder)-1], "grant_shield 8 seconds (longest shield)") {
			t.Fatalf("ladder ends %q / %q", ladder[0], ladder[len(ladder)-1])
		}
	})
	t.Run("UC-040/BR-045 the chosen lever's levels span the per-game range", func(t *testing.T) {
		levels := magnitudeLevels(mt, "grant_shield")
		if levels[0] != 2 || levels[len(levels)-1] != 8 {
			t.Fatalf("levels %v", levels)
		}
	})
	t.Run("UC-040/BR-045 a two-way lever's help side ends at the widened 0.2", func(t *testing.T) {
		help := strengthLevels(mt, "adjust_player_sensitivity", directionHelp)
		if help[len(help)-1] != 0.2 {
			t.Fatalf("help levels %v, want the strongest at 0.2", help)
		}
	})
	t.Run("UC-040/BR-045 the answer maps back onto the per-game range", func(t *testing.T) {
		req := detailsRequest(testRequest(), "grant_shield").ask
		req.Bounds = mt
		var mag decide.Question
		for _, q := range req.Questions {
			if q.ID == decide.QuestionFFAMagnitude {
				mag = q
			}
		}
		n := len(magnitudeLevels(mt, "grant_shield"))
		s, err := scoreToRange(float64(n-1), nil, mag, "grant_shield", mt)
		if err != nil {
			t.Fatal(err)
		}
		if v, _ := mt.Resolve("grant_shield", s); v != 8 {
			t.Fatalf("top level resolves to %v, want 8", v)
		}
	})
	t.Run("UC-040/BR-045 the details request keeps the request's table", func(t *testing.T) {
		req := testRequest()
		req.Bounds = mt
		if detailsRequest(req, "grant_shield").ask.Bounds != mt {
			t.Fatal("the details request lost the bounds")
		}
	})
}
