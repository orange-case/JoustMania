package llmio

import (
	"strings"
	"testing"

	"github.com/joustmania/agent/decide"
)

// UC: UC-040 A50 BR-039
func TestSystemPrompt_ReadingsStale(t *testing.T) {
	t.Run("UC-040/A50 the prompt defines readings_stale and names every fresh-readings lever", func(t *testing.T) {
		for _, sys := range []string{systemPrompt, systemPromptGoal} {
			i := strings.Index(sys, `"readings_stale" marks an alive player whose movement readings are not live (no new value in the last 3 seconds)`)
			if i < 0 {
				t.Fatal("system prompt lacks the readings_stale sentence")
			}
			sentence := sys[i:]
			sentence = sentence[:strings.Index(sentence, " are refused.")]
			for _, iv := range decide.FreshReadingsLevers() {
				if !strings.Contains(sentence, iv) {
					t.Errorf("readings_stale sentence does not name %s: %q", iv, sentence)
				}
			}
		}
	})
}
