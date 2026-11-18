package main

import (
	"context"

	"github.com/joustmania/agent/decide"
)

// retroMagnitudeBounds is the game's magnitude table for the post-game retro
// (#2696, UC-040 BR-045): the table its latest decide request used (BoundsLog),
// else decision.magnitude_bounds read in the game's context (a game that never
// decided), so the retro catalog shows the ranges the game had.
func (c *components) retroMagnitudeBounds(gameID string) *decide.MagnitudeTable {
	if t, ok := c.boundsLog.Last(gameID); ok {
		return t
	}
	return c.agentFlags.MagnitudeBounds(context.Background(), gameID)
}
