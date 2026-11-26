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
//  5. write the reference values (FR-227, §5.2, #2623): one more full
//     document pull decides; every usable feature whose toggle is off and
//     whose value is not its baseline default is read again from the Admin
//     API right before it gets it (enabled=false), so a toggle turned on
//     meanwhile is never turned off. A transient read failure fails the
//     start. It runs before the start reconcile, so no push of a stack value
//     races it;
//  6. the final step: remember the document's identity uuids, start the
//     variant option sync in the background (§2.3, #2709: the link does not
//     wait for it) and schedule the stale game identity cleanup (start itself never deletes an identity):
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
	// stopOn: the created agent/enabled was switched on by its reference write.
	stopOn bool

	mu      sync.Mutex
	cleanup chan struct{} // closed when the stale identity cleanup ended
	options chan struct{} // closed when the option sync ended (#2709)

	// optionJobs are the features whose options the start keeps in step
	// once it succeeded (§2.3); only the Start goroutine touches them.
	optionJobs []optionJob
	// referenceJobs are the features of the reference step (§5.2, #2623);
	// only the Start goroutine touches them.
	referenceJobs []referenceJob
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
	if err := s.writeReferences(ctx, s.referenceJobs); err != nil {
		return err
	}
	// A cancel during the last Admin call may still have answered: the final
	// step runs only for a start that ended (O7).
	if err := ctx.Err(); err != nil {
		return err
	}
	s.rememberIdentities(doc)
	s.scheduleOptions(ctx)
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
