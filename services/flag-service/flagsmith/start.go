package flagsmith

import "context"

// Stub start sequence (#2588): only the ping, one environment-document fetch,
// so a bad key or an unreachable Flagsmith keeps the link offline. #2591
// replaces this file wholesale (keeping the ping first): resolve ids, create
// features and tags (FR-214), the restart rule (FR-215).

func newStarter(l *Link) starter { return pingStarter{l: l} }

type pingStarter struct{ l *Link }

// Start pings Flagsmith. It does not call NotePull: the first Reconcile pulls
// in full.
func (s pingStarter) Start(ctx context.Context) error {
	_, _, _, err := s.l.client.EnvironmentDocument(ctx, "")
	return err
}
