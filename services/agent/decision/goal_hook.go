package decision

import (
	"context"

	"github.com/joustmania/agent/decide"
	"github.com/joustmania/agent/flags"
	"github.com/joustmania/agent/gamecontext"
	"github.com/joustmania/agent/goal"
)

// goal_hook.go connects the decision loop to the goal profiles (#2479, FR-201,
// decide-contract.md §14.4). Report-only: the goal lines change the words a
// claude or jev request carries, never the questions, the permission chain,
// the allow-list, the budgets or the writer.

// sampleGoal lets the shared book take this game's due goal sample.
func (l *Loop) sampleGoal(c gamecontext.GameContext) {
	if l.predictions != nil {
		l.predictions.SampleGoal(c)
	}
}

// goalLines are the request's goal lines, nil unless the backend is an LLM
// backend (jev builds its goal state and gate criterion from them), a goal
// source is wired and the game's profile has terms. The now line is the game's running
// measurements (the same values the game-end attainment scores); without a book
// it is one sample of c.
func (l *Loop) goalLines(ctx context.Context, fired flags.Snapshot, backend string, c gamecontext.GameContext) *decide.GoalLines {
	if l.goalSource == nil || !llmBackend(backend) {
		return nil
	}
	profile := l.goalSource(ctx, c)
	if profile.Empty() {
		return nil
	}
	var acc goal.Accumulator
	if l.predictions != nil {
		acc = l.predictions.GoalAccumulator(c.SessionID)
	} else {
		acc.Sample(c, l.nowOrDefault()())
	}
	measure := func(char, phase string) (float64, bool) { return acc.Measure(c, char, phase) }
	return &decide.GoalLines{
		Profile: profile.Name,
		Source:  goal.SourceOf(profile),
		Goal:    profile.GoalLine(),
		Now:     goal.NowLine(profile, measure),
		OffBand: goal.OffBand(profile, measure),
	}
}
