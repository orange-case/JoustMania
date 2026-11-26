package experiment

import (
	"strings"
	"testing"
)

// UC: UC-042 A3
func TestRetroNotes_ReachTheProposerPrompt(t *testing.T) {
	t.Run("UC-042/A3 the four most recent suggestions are kept, oldest first", func(t *testing.T) {
		var r RetroNotes
		r.Record(RetroSuggestion{})
		for _, reason := range []string{"a", "b", "c", "d", "e"} {
			r.Record(RetroSuggestion{InterventionType: "adjust_global_difficulty", Emphasis: "weight_up", Reason: reason})
		}
		got := r.Recent()
		if len(got) != MaxRetroNotes || got[0].Reason != "b" || got[3].Reason != "e" {
			t.Fatalf("Recent = %+v, want b..e", got)
		}
	})
	t.Run("UC-042/A3 the prompt lists them", func(t *testing.T) {
		usr := buildProposalPrompt(Narrative{RetroSuggestions: []RetroSuggestion{
			{InterventionType: "adjust_global_difficulty", Emphasis: "weight_up", Reason: "the room faded"},
		}}, nil).User
		want := "RECENT RETRO SUGGESTIONS (oldest first):\n  - adjust_global_difficulty (weight_up): the room faded\n"
		if !strings.Contains(usr, want) {
			t.Fatalf("prompt user part = %q, want it to contain %q", usr, want)
		}
	})
	t.Run("UC-042/A3 no suggestions, no section", func(t *testing.T) {
		if usr := buildProposalPrompt(Narrative{}, nil).User; strings.Contains(usr, "RETRO") {
			t.Fatalf("prompt user part = %q, want no retro section", usr)
		}
	})
}
