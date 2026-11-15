package flagsmith

import "context"

// Stub in side (#2588): no reconcile, no poll. #2590 replaces this file
// wholesale (UC-054: pull, 3-way merge, kill-switch order, events, feature
// health).

func newIn(_ *Link) inSide { return stubIn{} }

type stubIn struct{}

func (stubIn) Reconcile(context.Context, string) error { return nil }

func (stubIn) Poll(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
