package decide

import (
	"strings"
	"testing"
)

// UC: UC-040 BR-045
// magnitude_ends_uc040_test.go: every range line of the ffa.magnitude question
// names what its lowest and highest value do, so the model can tell which end
// helps (#2696; the per-player sensitivity floor of 0.2 makes a wrong guess costly).

func TestMagnitudeEnds_EveryRangeLineNamesItsEnds(t *testing.T) {
	st := State{Players: []PlayerState{{Serial: "A", Alive: true}}}
	text := FFAQuestions(st, MagnitudeInterventions())[2].Text
	for _, iv := range MagnitudeInterventions() {
		t.Run("UC-040/BR-045 "+iv, func(t *testing.T) {
			e, ok := MagnitudeEndEffects(iv)
			if !ok || e.Low == "" || e.High == "" {
				t.Fatalf("%s has no end effects", iv)
			}
			b, _ := MagnitudeBounds(iv)
			want := iv + ": lowest " + fmtNum(b.Min) + " (" + e.Low + "), highest " + fmtNum(b.Max) + " (" + e.High + "), in " + b.Unit
			if !strings.Contains(text, want) {
				t.Fatalf("no line %q in\n%s", want, text)
			}
			if e.HelpKnown {
				help, hurt := e.High, e.Low
				if !e.HelpIsHigh {
					help, hurt = hurt, help
				}
				if strings.Contains(help, "easier to eliminate") || strings.Contains(hurt, "harder to eliminate") {
					t.Fatalf("%s: helping end %q, other end %q contradict HelpIsHigh=%v", iv, help, hurt, e.HelpIsHigh)
				}
			}
		})
	}
	t.Run("UC-040/BR-045 the per-player levers: sensitivity helps low, handicap helps high", func(t *testing.T) {
		if e, _ := MagnitudeEndEffects("adjust_player_sensitivity"); e.HelpIsHigh || e.Low != "target harder to eliminate" {
			t.Fatalf("adjust_player_sensitivity %+v", e)
		}
		if e, _ := MagnitudeEndEffects("set_player_handicap"); !e.HelpIsHigh || e.High != "target harder to eliminate" {
			t.Fatalf("set_player_handicap %+v", e)
		}
	})
}
