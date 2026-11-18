package llmio

import (
	"strings"
	"testing"

	"github.com/joustmania/agent/decide"
)

// UC: UC-040 BR-045
// magnitude_bounds_uc040_test.go: a widened decision.magnitude_bounds reaches the
// rendered magnitude question (#2696, decide-contract.md §2.1); the default table
// renders today's prompt.

func TestMagnitudeBounds_Rendered(t *testing.T) {
	base := testRequest()
	allowed := []string{"grant_shield", "adjust_music_tempo", "adjust_player_sensitivity"}
	t.Run("UC-040/BR-045 the widened range is in the user message", func(t *testing.T) {
		req := base
		req.Bounds = decide.ParseMagnitudeOverride(map[string]any{
			"adjust_player_sensitivity": map[string]any{"min": 0.2, "max": 1.8},
		})
		req.Questions = decide.FFAQuestionsWith(req.State, allowed, req.Bounds)
		user := Render(req).User
		if !strings.Contains(user, "adjust_player_sensitivity: lowest 0.2 (target harder to eliminate), highest 1.8 (target easier to eliminate)") {
			t.Fatalf("user message has no widened range:\n%s", user)
		}
	})
	t.Run("UC-040/BR-045 the default table renders the same prompt as no table", func(t *testing.T) {
		with := base
		with.Bounds = decide.ParseMagnitudeOverride(nil)
		with.Questions = decide.FFAQuestionsWith(with.State, []string{"grant_shield", "adjust_music_tempo"}, with.Bounds)
		a, b := Render(base), Render(with)
		if a.User != b.User || a.System != b.System {
			t.Fatal("the default table changed the prompt")
		}
	})
}
