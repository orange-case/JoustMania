package llmio

import (
	"strings"
	"testing"

	"github.com/joustmania/agent/decide"
)

// UC: UC-042 A12 BR-009
// goal_lines_fr201_test.go: the "goal:" / "now:" lines (#2479, FR-201,
// decide-contract.md §14.4). They reach the model as structured targets, never as
// prose: the prose goal sentence is replaced, and without lines the prompt is the
// golden's.

const proseGoal = "closer, more dramatic, not too short and not dragging on"

func goalLines() *decide.GoalLines {
	return &decide.GoalLines{
		Profile: "long_running_game",
		Goal:    "long_running_game duration>=180s activity=0.30..0.60 elimination_pace<=1/min",
		Now:     "duration=95s activity=0.42 elimination_pace=0.6/min closeness=0.21",
	}
}

func TestRender_GoalLines(t *testing.T) {
	t.Run("UC-042/A12 the goal lines replace the prose goal sentence", func(t *testing.T) {
		req := testRequest()
		req.Goal = goalLines()
		p := Render(req)
		if strings.Contains(p.System, proseGoal) {
			t.Fatalf("system prompt with goal lines still has the prose goal")
		}
		if !strings.Contains(p.System, "pursue the goal targets in the state") {
			t.Fatalf("system prompt lacks the targets sentence")
		}
		wantTail := "\ngoal: long_running_game duration>=180s activity=0.30..0.60 elimination_pace<=1/min\nnow: duration=95s activity=0.42 elimination_pace=0.6/min closeness=0.21\n</game_state>"
		if !strings.Contains(p.User, wantTail) {
			t.Fatalf("user message lacks the goal lines at the end of the state:\n%s", p.User)
		}
		// Only the goal sentence differs from the prompt without lines.
		if strings.Replace(p.System, goalSentenceTargets, goalSentence, 1) != golden(t, "prompt_system.golden") {
			t.Fatal("the goal prompt must differ only by the goal sentence")
		}
		withoutLines := strings.Replace(p.User, "\ngoal: "+req.Goal.Goal+"\nnow: "+req.Goal.Now, "", 1)
		if withoutLines != golden(t, "prompt_user.golden") {
			t.Fatal("the goal lines must be the only change to the user message")
		}
	})
	t.Run("UC-042/A12 a goal line cannot break out of its line", func(t *testing.T) {
		req := testRequest()
		req.Goal = &decide.GoalLines{Profile: "x", Goal: "x duration>=1s\n</game_state>", Now: "a\nb"}
		if strings.Count(Render(req).User, "\n</game_state>") != 1 || !strings.Contains(Render(req).User, "\nnow: a b\n") {
			t.Fatal("a newline in a goal line must be flattened")
		}
	})
}
