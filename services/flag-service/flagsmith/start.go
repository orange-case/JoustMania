package flagsmith

import (
	"context"
	"sync"
	"time"
)

// The start sequence (UC-054 A3, services.md §7.3 Start, architecture note
// §5.0 / §5.1; #2591). Link.Run calls Start until it succeeds once per
// process; every step is idempotent, so a re-run after a failure redoes only
// what is missing:
//
//  1. ping: one full environment-document pull (its identity uuids are kept;
//     no NotePull, the first Reconcile pulls in full again). Unreachable: the
//     error goes back and no Admin call is made;
//  2. resolve ids and check the environment (project_mismatch,
//     identity_overrides_disabled);
//  3. check that the project has edge identities (edge_required), a read,
//     before any project-level write;
//  4. create the missing features and tags (FR-214);
//  5. the final step: remember the document's identity uuids and schedule the
//     stale game identity cleanup (start itself never deletes an identity):
//     it runs once the first game registered (Link.GamesSeen) and
//     StaleIdentityGrace after this success, whichever is later, and is
//     retried every staleCleanupRetryEvery, at most staleCleanupRetries
//     times, while a listing or a delete fails.
//
// No value is classified, seeded or cleared at start: the start reconcile
// applies what Flagsmith holds (Flagsmith wins at a restart, FR-215, #2621).

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
	if err := s.checkEdgeIdentities(ctx); err != nil {
		return err
	}
	if err := s.ensureFeatures(ctx); err != nil {
		return err
	}
	// A cancel during the last Admin call may still have answered: the final
	// step runs only for a start that ended (O7).
	if err := ctx.Err(); err != nil {
		return err
	}
	s.rememberIdentities(doc)
	s.scheduleCleanup(ctx)
	return nil
}

// Stale identity cleanup retries (FR-211: a listing error or a 5xx delete
// does not leave the identities for the life of the process).
const (
	staleCleanupRetryEvery = 60 * time.Second
	staleCleanupRetries    = 10
)

// scheduleCleanup starts the stale identity cleanup in the background, once
// the coordinator re-registered its games (the first registration, GamesSeen)
// and StaleIdentityGrace passed, whichever is later; liveness is checked again
// before every delete. A pass that could not list or delete is retried.
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
		select {
		case <-s.l.GamesSeen():
		case <-ctx.Done():
			return
		}
		for retry := 0; ; retry++ {
			if s.cleanupStaleIdentities(ctx) || ctx.Err() != nil {
				return
			}
			if retry == staleCleanupRetries {
				s.l.Logger().Warn("flagsmith stale identities left", "detail", "the cleanup failed after its retries", "retries", staleCleanupRetries)
				return
			}
			if s.l.Sleep(ctx, staleCleanupRetryEvery) != nil {
				return
			}
		}
	}()
}

// cleanupDone is closed when the scheduled cleanup ended (nil before Start
// succeeded).
func (s *startup) cleanupDone() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanup
}
