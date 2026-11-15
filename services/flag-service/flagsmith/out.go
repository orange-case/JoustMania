package flagsmith

import (
	"context"

	"github.com/joustmania/flag-service/state"
)

// Stub out side (#2588): records nothing and sends nothing. #2589 replaces
// this file wholesale (UC-055: stack values, identities, game end cleanup,
// the outbox with merge and rate limit).

func newOut(_ *Link) outSide { return stubOut{} }

type stubOut struct{}

func (stubOut) Changed(state.Change) {}

func (stubOut) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
