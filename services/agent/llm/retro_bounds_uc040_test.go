package llm

import (
	"strings"
	"testing"

	"github.com/joustmania/agent/decide"
	"github.com/joustmania/agent/gamecontext"
)

// UC: UC-040 BR-045
// retro_bounds_uc040_test.go: the post-game retro's intervention catalog shows the
// game's own magnitude ranges (decision.magnitude_bounds, #2696), with the same
// range-end effects as the decide question, and the clamp note names the 0.2 floor.

func TestRetro_CatalogUsesTheGamesBounds(t *testing.T) {
	snap := retroSnapshot()
	snap.InterventionsAllowed = []string{"adjust_player_sensitivity", "grant_shield"}
	widened := decide.ParseMagnitudeOverride(map[string]any{
		"adjust_player_sensitivity": map[string]any{"min": 0.2, "max": 1.5},
	})
	build := func(b *decide.MagnitudeTable) string {
		return BuildRetro(RetroInput{Snapshot: snap, Context: gamecontext.GameContext{SessionID: "s"}, Now: fixedNow, Bounds: b}).System
	}
	t.Run("UC-040/BR-045 a widened flag range shows up in the retro catalog", func(t *testing.T) {
		sys := build(widened)
		if !strings.Contains(sys, "Value 0.2 (target harder to eliminate) to 1.5 (target easier to eliminate) factor") {
			t.Fatalf("catalog lacks the widened range:\n%s", sys)
		}
		if !strings.Contains(sys, "Value 3 (shortest protection) to 10 (longest protection) seconds") {
			t.Fatal("an intervention the flag does not name must keep its default range")
		}
	})
	t.Run("UC-040/BR-045 without the game's table the catalog shows the code defaults", func(t *testing.T) {
		if !strings.Contains(build(nil), "Value 0.7 (target harder to eliminate) to 1.5") {
			t.Fatal("nil bounds must render the code default range")
		}
	})
	t.Run("UC-040/BR-045 the range ends say what the decide question says", func(t *testing.T) {
		sys := build(nil)
		for _, iv := range snap.InterventionsAllowed {
			e, _ := decide.MagnitudeEndEffects(iv)
			if !strings.Contains(sys, "("+e.Low+")") || !strings.Contains(sys, "("+e.High+")") {
				t.Errorf("%s: catalog lacks its end effects %q / %q", iv, e.Low, e.High)
			}
		}
	})
	t.Run("UC-040/BR-045 the clamp note names the per-player floor 0.2", func(t *testing.T) {
		if !strings.Contains(decide.FactorClampNote, "never below 0.2") || !strings.Contains(build(nil), decide.FactorClampNote) {
			t.Fatalf("clamp note %q", decide.FactorClampNote)
		}
	})
}
