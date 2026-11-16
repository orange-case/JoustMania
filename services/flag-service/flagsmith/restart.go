package flagsmith

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/joustmania/flag-service/registry"
)

// The restart rule (FR-215, UC-054 A3 step 4, BR-006; architecture note §5.1).
//
// At start remote is empty, so the link cannot tell its own pushes from a
// person's changes by comparison: it reads who made each value of the start
// document from the audit log. Start never clears anything directly: a value
// made with an API key is SEEDED as the last remote state
// (LinkState.SetRemote), so the reconcile sees remote == document and local
// != remote and pushes the local value (the baseline clear, or the restored
// admin value of a persist: yes row). A person's value is not seeded and the
// reconcile applies it (no remote: the document wins). Seeding happens only
// in the start's final step (O7).
//
// Never seeded at start:
//   - game identities (game:<id>, game:<id>:<serial>): no game is registered
//     yet after a restart; their attribution goes into the RestartReport and
//     the in side applies it when the game registers (O1, O14, #2590 R6);
//   - kill switches (flags.md §13.6): with no remote the reconcile's
//     kill-switch rule keeps the more restrictive value, even over the API-key
//     clear (safety over reset, BR-005).
//
// Event-flag overrides are reported as AuthorAPIKey without an audit lookup:
// an event in Flagsmith is a one-shot payload, never brought back by a
// restart (D3).

// StaleIdentityGrace is how long after a successful start the stale game
// identities are removed: two coordinator reconcile periods (15 s) and a grace
// (O1, services.md §7.3 Start).
const StaleIdentityGrace = 45 * time.Second

// ReasonRestartRuleDegraded is the warning of a degraded restart rule.
const ReasonRestartRuleDegraded = "restart_rule_degraded"

// restartKillSwitches are the kill switches of flags.md §13.6 (UC-054
// BR-005). The same table drives the in side's order (killswitch.go, #2590).
var restartKillSwitches = map[string]bool{
	"agent/enabled":                                 true,
	"agent/interventions_allowed":                   true,
	"agent/code_improvement.mode":                   true,
	"agent/code_improvement.target":                 true,
	"agent/code_improvement.min_interval_seconds":   true,
	"agent/llm.max_requests_per_minute":             true,
	"agent/decision.claude.max_requests_per_minute": true,
	"agent/decision.openai.max_requests_per_minute": true,
	"agent/experiment_proposer_enabled":             true,
	"agent/interventions_enabled":                   true,
	"agent/experiments_enabled":                     true,
	"agent/rollout_enabled":                         true,
	"rollout/remediation_allowed":                   true,
	"agent/experiment_max_games":                    true,
	"agent/experiment_dynamic_max_concurrent":       true,
}

// candidate is one value of the start document the rule classifies.
type candidate struct {
	key   Key
	audit authorKey
	value Value
	event bool
}

// restartPlan is what the rule decided; apply makes it so.
type restartPlan struct {
	rule        string
	attribution map[Key]Author
	seed        map[Key]Value
	uuids       map[string]string
}

// candidates lists the enabled environment values (segment overrides
// excluded) and the identity overrides of registry features in doc.
func (s *startup) candidates(doc *EnvironmentDocument) ([]candidate, map[string]string) {
	uuids := map[string]string{}
	if doc == nil || s.l.deps.Registry == nil {
		return nil, uuids
	}
	byName := map[string]*registry.Policy{}
	for _, p := range s.l.deps.Registry.Policies() {
		byName[p.Key] = p
	}
	var out []candidate
	for _, fs := range doc.FeatureStates {
		p := byName[fs.Feature.Name]
		if p == nil || !fs.Enabled || hasSegment(fs) {
			continue
		}
		out = append(out, candidate{
			key:   Key{Set: p.Set, Flag: p.Key, Scope: ScopeStack},
			audit: authorKey{feature: p.Key},
			value: s.docValue(p, fs),
		})
	}
	for _, io := range doc.IdentityOverrides {
		if _, _, ok := ScopeOfIdentity(io.Identifier); !ok && !isControllerIdentity(io.Identifier) {
			continue
		}
		if io.IdentityUUID != "" {
			uuids[io.Identifier] = io.IdentityUUID
		}
		for _, fs := range io.IdentityFeatures {
			p := byName[fs.Feature.Name]
			if p == nil {
				continue
			}
			out = append(out, candidate{
				key:   Key{Set: p.Set, Flag: p.Key, Scope: io.Identifier},
				audit: authorKey{feature: p.Key, identity: io.Identifier},
				value: s.docValue(p, fs),
				event: p.Kind == registry.KindEvent,
			})
		}
	}
	return out, uuids
}

func hasSegment(fs FeatureState) bool {
	raw := bytes.TrimSpace(fs.FeatureSegment)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

// docValue is the Value of a document feature state as the reconcile reads
// it: Present is enabled; a declared variant carries its name and value; a
// value Decode refuses is kept raw.
func (s *startup) docValue(p *registry.Policy, fs FeatureState) Value {
	b := s.l.BaselineFlag(p.Set, p.Key)
	variant, value, ferr := Decode(p, b, FSValueOf(fs.FeatureStateValue))
	if ferr != nil {
		return Value{Present: fs.Enabled, Value: compact(fs.FeatureStateValue)}
	}
	if variant != "" && b != nil {
		value = compact(b.Variants[variant])
	}
	return Value{Present: fs.Enabled, Variant: variant, Value: value}
}

// isGameIdentity: a game or player scope, a controller identity included
// (#2624: it carries player values).
func isGameIdentity(scope string) bool {
	return strings.HasPrefix(scope, "game:") || isControllerIdentity(scope)
}

// isControllerIdentity: a controller:<serial> identity.
func isControllerIdentity(identifier string) bool {
	_, ok := SerialOfController(identifier)
	return ok
}

// restartRule classifies every candidate of doc by the author of its latest
// audit row and decides what to seed. The rule is versioning when the
// environment uses feature versioning v2 (its "New version published" rows
// carry the author, O3), else audit_log, and degraded when the audit log did
// not answer (any status, 401 included: an audit error never takes the link
// down, architecture note §5.1) or a candidate has no row (then counted as a
// person's, never an API key). Only a cancel is returned.
func (s *startup) restartRule(ctx context.Context, doc *EnvironmentDocument) (restartPlan, error) {
	cands, uuids := s.candidates(doc)
	plan := restartPlan{attribution: map[Key]Author{}, seed: map[Key]Value{}, uuids: uuids}
	want := map[authorKey]bool{}
	for _, c := range cands {
		if !c.event {
			want[c.audit] = true
		}
	}
	found, ok, err := s.latestAuthors(ctx, want)
	if err != nil {
		return plan, err
	}
	degraded := !ok
	for _, c := range cands {
		a := AuthorAPIKey
		if !c.event {
			var has bool
			if a, has = found[c.audit]; !has {
				a, degraded = AuthorUnknown, true
			}
		}
		plan.attribution[c.key] = a
		if a == AuthorAPIKey && !isGameIdentity(c.key.Scope) && !restartKillSwitches[c.key.Set+"/"+c.key.Flag] {
			plan.seed[c.key] = c.value
		}
	}
	s.l.ids.Lock()
	v2 := s.l.ids.V2Versioning
	s.l.ids.Unlock()
	switch {
	case degraded:
		plan.rule = RestartDegraded
	case v2:
		plan.rule = RestartVersioning
	default:
		plan.rule = RestartAuditLog
	}
	return plan, nil
}

// apply is the start's final step (O7): seed, remember the identity uuids
// and record the report.
func (s *startup) apply(plan restartPlan) {
	for k, v := range plan.seed {
		s.l.links.SetRemote(k, v)
	}
	ids := s.l.ids
	ids.Lock()
	for identifier, u := range plan.uuids {
		ids.IdentityUUID[identifier] = u
	}
	ids.Unlock()
	s.l.SetRestartReport(plan.rule, plan.attribution)
	if plan.rule == RestartDegraded {
		s.l.Logger().Warn(ReasonRestartRuleDegraded,
			"detail", "the audit log does not say who made every Flagsmith value; values without an author are applied as a person's",
			"values", len(plan.attribution))
	}
}

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
//
// Controller identities (#2624) are never deleted: one listing of
// controller:* identities hands each controller without a live game to the
// out side, which releases it (removes its overrides and traits) on its
// sender goroutine, the owner checked again right before.
func (s *startup) cleanupStaleIdentities(ctx context.Context) bool {
	complete := s.cleanupGameIdentities(ctx)
	if ctx.Err() != nil {
		return false
	}
	return s.releaseStaleControllers(ctx) && complete
}

// releaseStaleControllers lists the controller identities and queues the
// release of each one without an owner. It reports whether the listing was
// complete.
func (s *startup) releaseStaleControllers(ctx context.Context) bool {
	lek := ""
	queued := false
	defer func() {
		if queued {
			s.l.wakeOut()
		}
	}()
	for page := 0; page < maxListPages; page++ {
		path := s.environmentPath() + "edge-identities/?q=" + url.QueryEscape(ControllerPrefix) + "&page_size=100"
		if lek != "" {
			path += "&last_evaluated_key=" + url.QueryEscape(lek)
		}
		var resp struct {
			Results          []edgeIdentity `json:"results"`
			LastEvaluatedKey *string        `json:"last_evaluated_key"`
		}
		if _, _, err := s.pace.call(ctx, http.MethodGet, path, nil, &resp, true, nil); err != nil {
			if ctx.Err() == nil {
				s.l.Logger().Warn("flagsmith controller identities not listed", "reason", Reason(err))
			}
			return false
		}
		for _, id := range resp.Results {
			serial, ok := SerialOfController(id.Identifier)
			if !ok {
				continue
			}
			s.rememberIdentity(id)
			if s.l.ctrl.markRelease(serial) {
				queued = true
			}
		}
		if resp.LastEvaluatedKey == nil || *resp.LastEvaluatedKey == "" {
			return true
		}
		lek = *resp.LastEvaluatedKey
	}
	return true
}

// cleanupGameIdentities is the game:* half of cleanupStaleIdentities.
func (s *startup) cleanupGameIdentities(ctx context.Context) bool {
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
			if !strings.HasPrefix(id.Identifier, "game:") {
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
