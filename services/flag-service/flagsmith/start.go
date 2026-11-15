package flagsmith

import (
	"context"
	"sync"
)

// The start sequence (UC-054 A3, services.md §7.3 Start, architecture note
// §5.0 / §5.1; #2591). Link.Run calls Start until it succeeds once per
// process; every step is idempotent, so a re-run after a failure redoes only
// what is missing:
//
//  1. ping: one full environment-document pull (kept for the restart rule; no
//     NotePull, the first Reconcile pulls in full again). Unreachable: the
//     error goes back and no Admin call is made;
//  2. resolve ids and check the environment (project_mismatch,
//     identity_overrides_disabled);
//  3. create the missing features and tags (FR-214);
//  4. classify the document's values by author (the restart rule, FR-215);
//  5. check that the project has edge identities (edge_required);
//  6. the final step: seed the API-key values, record the RestartReport, and
//     schedule the one-shot stale game identity cleanup (StaleIdentityGrace
//     after this success; start itself never deletes an identity).

func newStarter(l *Link) starter { return &startup{l: l, pace: newAdminPacer(l)} }

// startup is the start sequence of one link.
type startup struct {
	l    *Link
	pace *adminPacer

	mu      sync.Mutex
	cleanup chan struct{} // closed when the stale identity cleanup ended
}

// Start runs the start sequence; see the file comment.
func (s *startup) Start(ctx context.Context) error {
	doc, _, _, err := s.l.client.EnvironmentDocument(ctx, "")
	if err != nil {
		return err
	}
	if err := s.resolveIDs(ctx); err != nil {
		return err
	}
	if err := s.ensureFeatures(ctx); err != nil {
		return err
	}
	plan, err := s.restartRule(ctx, doc)
	if err != nil {
		return err
	}
	if err := s.checkEdgeIdentities(ctx); err != nil {
		return err
	}
	s.apply(plan)
	s.scheduleCleanup(ctx)
	return nil
}

// scheduleCleanup starts the stale identity cleanup in the background: after
// StaleIdentityGrace, so the coordinator has re-registered its live games
// (two reconcile periods); liveness is checked again before every delete.
func (s *startup) scheduleCleanup(ctx context.Context) {
	done := make(chan struct{})
	s.mu.Lock()
	s.cleanup = done
	s.mu.Unlock()
	go func() {
		defer close(done)
		if s.l.Sleep(ctx, StaleIdentityGrace) != nil {
			return
		}
		s.cleanupStaleIdentities(ctx)
	}()
}

// cleanupDone is closed when the scheduled cleanup ended (nil before Start
// succeeded).
func (s *startup) cleanupDone() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanup
}
