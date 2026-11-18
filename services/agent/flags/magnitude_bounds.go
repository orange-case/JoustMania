package flags

import (
	"context"

	"github.com/joustmania/agent/decide"
)

// keyDecisionMagnitudeBounds is decision.magnitude_bounds (agent set, flags.md §3,
// #2696, UC-040 BR-045). Human-only (C-034): the agent reads it and never writes it.
const keyDecisionMagnitudeBounds = "decision.magnitude_bounds"

// MagnitudeBounds reads decision.magnitude_bounds for one game from the agent set,
// in the game's context (GameContext), and parses it into the request's magnitude
// table (decide.ParseMagnitudeOverride, decide-contract.md §2.1): entries outside
// the hard limits or malformed fall back to the code default per intervention. A
// nil wrapper, a nil client or any evaluation error gives the code default table
// (debug log only: it is read per decide request). A WARN
// agent.magnitude_bounds_invalid is logged when the set of invalid entries changes.
func (f *Flags) MagnitudeBounds(ctx context.Context, gameID string) *decide.MagnitudeTable {
	if f == nil || f.client == nil {
		return decide.ParseMagnitudeOverride(nil)
	}
	v, err := f.client.ObjectValue(ctx, keyDecisionMagnitudeBounds, nil, f.GameContext(gameID))
	if err != nil {
		f.log.Debug("flags decision.magnitude_bounds unreadable, code default ranges", "game_id", gameID, "error", err)
		return decide.ParseMagnitudeOverride(nil)
	}
	t := decide.ParseMagnitudeOverride(v)
	invalid := t.InvalidString()
	f.magnitudeMu.Lock()
	changed := invalid != f.magnitudeInvalid
	f.magnitudeInvalid = invalid
	f.magnitudeMu.Unlock()
	if changed && invalid != "" {
		f.log.Warn("agent.magnitude_bounds_invalid: entries fall back to the code default ranges",
			"game_id", gameID, "invalid", invalid)
	}
	return t
}
