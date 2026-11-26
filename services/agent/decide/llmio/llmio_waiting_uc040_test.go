package llmio

import (
	"strings"
	"testing"
)

// UC: UC-040 A49 BR-038
func TestSystemPrompt_DefinesWaiting(t *testing.T) {
	t.Run("UC-040/A49 the prompt defines the waiting status and the session count", func(t *testing.T) {
		for _, sys := range []string{systemPrompt, systemPromptGoal} {
			if !strings.Contains(sys, `the status "waiting" is a Fight Club or Tournament player between its fights: still in the game`) {
				t.Error("system prompt does not define the waiting status")
			}
			if !strings.Contains(sys, `"waiting=<n>" counts these players`) {
				t.Error("system prompt does not explain the session line's waiting=<n>")
			}
		}
	})
}
