package flagsmith

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A restart (UC-054 A3, FR-215, architecture note §5.1; #2621): Flagsmith
// wins. The start sequence does not classify the start document by author and
// seeds nothing: the link has no last state seen from Flagsmith, so the start
// reconcile applies every Flagsmith value that differs from the local value
// (the baseline, or the persisted value of an admin setting), whoever wrote
// it, a kill switch included (merge.go, "R absent"). An admin setting
// (persist: yes) pulled from Flagsmith is persisted like the menu admin's
// value. Two things are not values and keep their own rule:
//   - game identities (game:<id>, game:<id>:<serial>): applied only when the
//     game registers; identities of games that never register are removed
//     after StaleIdentityGrace (below);
//   - event lists: one-shot payloads the link pushed before the restart, not
//     brought back (in.go, D3).

// StaleIdentityGrace is how long after a successful start the stale game
// identities are removed: two coordinator reconcile periods (15 s) and a grace
// (O1, services.md §7.3 Start).
const StaleIdentityGrace = 45 * time.Second

// rememberIdentities keeps the identity uuids of the start document's game
// identities, so a later write or delete needs no lookup.
func (s *startup) rememberIdentities(doc *EnvironmentDocument) {
	if doc == nil {
		return
	}
	ids := s.l.ids
	ids.Lock()
	defer ids.Unlock()
	for _, io := range doc.IdentityOverrides {
		if _, _, ok := ScopeOfIdentity(io.Identifier); !ok || io.IdentityUUID == "" {
			continue
		}
		ids.IdentityUUID[io.Identifier] = io.IdentityUUID
	}
}

// isGameIdentity: a game or player scope.
func isGameIdentity(scope string) bool { return strings.HasPrefix(scope, "game:") }

// checkEdgeIdentities fails the start when the project has no edge
// identities (O8): Flagsmith answers 400 DynamoNotEnabled.
func (s *startup) checkEdgeIdentities(ctx context.Context) error {
	_, _, err := s.pace.call(ctx, http.MethodGet, s.environmentPath()+"edge-identities/?q=game:&page_size=1", nil, nil, false, nil)
	if httpStatus(err) == http.StatusBadRequest {
		return ReasonError{Reason: ReasonEdgeRequired}
	}
	return err
}

// edgeIdentity is one row of the edge identity list.
type edgeIdentity struct {
	IdentityUUID string `json:"identity_uuid"`
	Identifier   string `json:"identifier"`
}

// gameIdentityLive reports whether a registered game matches identifier as a
// whole: game:<id> or game:<id>:<serial> of one of its serials (O1, O9: never
// parsed on ':'). Without a State nothing is stale.
func (s *startup) gameIdentityLive(identifier string) bool {
	st := s.l.State()
	if st == nil {
		return true
	}
	for _, g := range st.Games() {
		if identifier == "game:"+g.ID {
			return true
		}
		for _, serial := range g.Serials {
			if identifier == "game:"+g.ID+":"+serial {
				return true
			}
		}
	}
	return false
}

// cleanupStaleIdentities deletes the edge identities of games that are not
// registered (UC-054 A3 step 4): one listing of game:* identities, liveness
// checked again right before each delete. It reports whether the pass was
// complete: false after a listing error or a transient (5xx, timeout,
// transport) delete error, so the caller retries (game ends while running are
// the out side's, #2589).
func (s *startup) cleanupStaleIdentities(ctx context.Context) bool {
	complete := true
	lek := ""
	for page := 0; page < maxListPages; page++ {
		path := s.environmentPath() + "edge-identities/?q=game:&page_size=100"
		if lek != "" {
			path += "&last_evaluated_key=" + url.QueryEscape(lek)
		}
		var resp struct {
			Results          []edgeIdentity `json:"results"`
			LastEvaluatedKey *string        `json:"last_evaluated_key"`
		}
		if _, _, err := s.pace.call(ctx, http.MethodGet, path, nil, &resp, true, nil); err != nil {
			if ctx.Err() == nil {
				s.l.Logger().Warn("flagsmith stale identities not listed", "reason", Reason(err))
			}
			return false
		}
		for _, id := range resp.Results {
			if !isGameIdentity(id.Identifier) {
				continue
			}
			if s.gameIdentityLive(id.Identifier) {
				s.rememberIdentity(id)
				continue
			}
			switch s.deleteIdentity(ctx, id) {
			case deleteCancelled:
				return false
			case deleteRetry:
				complete = false
			}
		}
		if resp.LastEvaluatedKey == nil || *resp.LastEvaluatedKey == "" {
			return complete
		}
		lek = *resp.LastEvaluatedKey
	}
	return complete
}

func (s *startup) rememberIdentity(id edgeIdentity) {
	s.l.ids.Lock()
	s.l.ids.IdentityUUID[id.Identifier] = id.IdentityUUID
	s.l.ids.Unlock()
}

// deleteOutcome is what one stale identity delete did.
type deleteOutcome int

const (
	deleteDone      deleteOutcome = iota // deleted, already gone, kept (live) or refused for good
	deleteRetry                          // a transient error: the next pass tries again
	deleteCancelled                      // ctx ended: the cleanup stops
)

// deleteIdentity deletes one stale identity (404: already gone), unless its
// game registered meanwhile.
func (s *startup) deleteIdentity(ctx context.Context, id edgeIdentity) deleteOutcome {
	k := Key{Scope: id.Identifier}
	sctx, span := s.pushSpan(ctx, k, OpDeleteIdentity)
	path := s.environmentPath() + "edge-identities/" + url.PathEscape(id.IdentityUUID) + "/"
	stale := func() bool { return !s.gameIdentityLive(id.Identifier) }
	_, skipped, err := s.pace.call(sctx, http.MethodDelete, path, nil, nil, true, stale)
	switch {
	case skipped:
		// The game registered between the listing and the delete: no call.
		EndSpan(span, ResultOK, "game_live")
		s.rememberIdentity(id)
		return deleteDone
	case err == nil || httpStatus(err) == http.StatusNotFound:
		s.endPush(sctx, span, OpDeleteIdentity, ResultOK, "")
	case ctx.Err() != nil:
		s.endPush(sctx, span, OpDeleteIdentity, ResultError, Reason(err))
		return deleteCancelled
	default:
		s.endPush(sctx, span, OpDeleteIdentity, ResultError, Reason(err))
		s.l.Logger().Warn("flagsmith stale identity not deleted", "reason", Reason(err))
		if isTransient(err) {
			return deleteRetry
		}
		return deleteDone
	}
	s.l.links.ForgetIdentity(id.Identifier)
	s.l.ids.Lock()
	delete(s.l.ids.IdentityUUID, id.Identifier)
	s.l.ids.Unlock()
	return deleteDone
}
