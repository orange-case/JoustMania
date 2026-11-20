package flagsmith

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/joustmania/flag-service/registry"
)

// Feature creation (FR-214, UC-054 A3 step 3, BR-002; architecture note §5.0).

// Refusal reasons of feature creation (services.md §7.3 Start).
const (
	ReasonFeatureNameCase     = "feature_name_case"
	ReasonFeatureArchived     = "feature_archived"
	ReasonFeatureCreateFailed = "feature_create_failed"
)

// Admin call limits of the start sequence.
const (
	// maxRateLimitRetries bounds the 429 retries of one call (Retry-After).
	maxRateLimitRetries = 3
	// transientAttempts is how often a create is tried on a
	// 5xx or a transport error before it is given up (O6).
	transientAttempts = 3
	// transientBackoff is the wait between two such attempts.
	transientBackoff = time.Second
	// maxListPages bounds every paged listing (features, tags, identities).
	maxListPages = 100
	// featurePageSize is the page size of the feature list.
	featurePageSize = 999
)

// adminPacer paces the start sequence's Admin API calls at FLAGSMITH_PUSH_RATE
// per second and retries a 429 after its Retry-After.
type adminPacer struct {
	l        *Link
	interval time.Duration

	mu   sync.Mutex
	next time.Time
}

func newAdminPacer(l *Link) *adminPacer {
	rate := l.cfg.PushRate
	if rate <= 0 {
		rate = DefaultPushRate
	}
	return &adminPacer{l: l, interval: time.Second / time.Duration(rate)}
}

// wait blocks until the next call slot.
func (p *adminPacer) wait(ctx context.Context) error {
	p.mu.Lock()
	now := p.l.Now()
	if p.next.Before(now) {
		p.next = now
	}
	d := p.next.Sub(now)
	p.next = p.next.Add(p.interval)
	p.mu.Unlock()
	if d > 0 {
		return p.l.Sleep(ctx, d)
	}
	return ctx.Err()
}

// call makes one paced Admin request. A 429 waits its Retry-After (1 s when
// absent) and is retried up to maxRateLimitRetries times. With transient, a
// 5xx or a transport error is tried transientAttempts times in all. ready,
// when not nil, is asked right before each request: false skips it (skipped
// true, no request).
func (p *adminPacer) call(ctx context.Context, method, path string, body, out any, transient bool, ready func() bool) (status int, skipped bool, err error) {
	rateRetries, attempts := 0, 0
	for {
		if err := p.wait(ctx); err != nil {
			return 0, false, err
		}
		if ready != nil && !ready() {
			return 0, true, nil
		}
		status, err = p.l.client.Admin(ctx, method, path, body, out)
		if err == nil || ctx.Err() != nil {
			return status, false, err
		}
		var he *HTTPError
		if errors.As(err, &he) && he.Status == http.StatusTooManyRequests && rateRetries < maxRateLimitRetries {
			rateRetries++
			d := he.RetryAfter
			if d <= 0 {
				d = time.Second
			}
			if serr := p.l.Sleep(ctx, d); serr != nil {
				return status, false, serr
			}
			continue
		}
		attempts++
		if transient && isTransient(err) && attempts < transientAttempts {
			if serr := p.l.Sleep(ctx, transientBackoff); serr != nil {
				return status, false, serr
			}
			continue
		}
		return status, false, err
	}
}

// isTransient: a 5xx, a timeout or a request without an answer.
func isTransient(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status >= 500
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var te *TransportError
	return errors.As(err, &te)
}

// httpStatus is err's HTTP status, 0 when it has none.
func httpStatus(err error) int {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

// listPaged GETs path page by page (DRF page-number pagination; a plain JSON
// array is one page) and hands every page's results to each.
func (s *startup) listPaged(ctx context.Context, path string, each func(json.RawMessage) error) error {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for page := 1; page <= maxListPages; page++ {
		var raw json.RawMessage
		if _, _, err := s.pace.call(ctx, http.MethodGet, path+sep+"page="+strconv.Itoa(page), nil, &raw, false, nil); err != nil {
			return err
		}
		raw = bytes.TrimSpace(raw)
		if len(raw) > 0 && raw[0] == '[' {
			return each(raw)
		}
		var p struct {
			Next    *string         `json:"next"`
			Results json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return ReasonError{Reason: "malformed_response"}
		}
		if len(p.Results) > 0 {
			if err := each(p.Results); err != nil {
				return err
			}
		}
		if p.Next == nil || *p.Next == "" {
			return nil
		}
	}
	return nil
}

// environmentInfo is the part of GET environments/{key}/ the start reads.
type environmentInfo struct {
	ID                              int    `json:"id"`
	Name                            string `json:"name"`
	Project                         int    `json:"project"`
	UseIdentityOverridesInLocalEval bool   `json:"use_identity_overrides_in_local_eval"`
}

func (s *startup) environmentPath() string {
	return "environments/" + url.PathEscape(s.l.cfg.EnvironmentKey) + "/"
}

// resolveIDs reads the environment and checks it (architecture note §5.0):
// its project must be FLAGSMITH_PROJECT_ID (project_mismatch) and identity
// overrides must be in the document (identity_overrides_disabled).
func (s *startup) resolveIDs(ctx context.Context) error {
	var env environmentInfo
	if _, _, err := s.pace.call(ctx, http.MethodGet, s.environmentPath(), nil, &env, false, nil); err != nil {
		return err
	}
	if env.Project != s.l.cfg.ProjectID {
		return ReasonError{Reason: ReasonProjectMismatch}
	}
	if !env.UseIdentityOverridesInLocalEval {
		return ReasonError{Reason: ReasonIdentityOverridesDisabled}
	}
	ids := s.l.ids
	ids.Lock()
	ids.ProjectID, ids.EnvironmentID, ids.EnvironmentName = s.l.cfg.ProjectID, env.ID, env.Name
	ids.Unlock()
	return nil
}

// fsFeature is a feature of the project's feature list.
type fsFeature struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	IsArchived bool   `json:"is_archived"`
	// MultivariateOptions are the feature's options (#2709): the list
	// names them, so the start reads no option of its own.
	MultivariateOptions []fsOption `json:"multivariate_options,omitempty"`
}

type fsTag struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
}

func (s *startup) projectPath(rest string) string {
	return "projects/" + strconv.Itoa(s.l.cfg.ProjectID) + "/" + rest
}

// listFeatures returns the project's features by lower-case name.
func (s *startup) listFeatures(ctx context.Context) (map[string]fsFeature, error) {
	out := map[string]fsFeature{}
	err := s.listPaged(ctx, s.projectPath("features/?page_size="+strconv.Itoa(featurePageSize)), func(raw json.RawMessage) error {
		var fs []fsFeature
		if err := json.Unmarshal(raw, &fs); err != nil {
			return ReasonError{Reason: "malformed_response"}
		}
		for _, f := range fs {
			out[strings.ToLower(f.Name)] = f
		}
		return nil
	})
	return out, err
}

// listTags returns the project's tags by label (labels are not unique in
// Flagsmith: the first wins).
func (s *startup) listTags(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	err := s.listPaged(ctx, s.projectPath("tags/"), func(raw json.RawMessage) error {
		var ts []fsTag
		if err := json.Unmarshal(raw, &ts); err != nil {
			return ReasonError{Reason: "malformed_response"}
		}
		for _, t := range ts {
			if _, ok := out[t.Label]; !ok {
				out[t.Label] = t.ID
			}
		}
		return nil
	})
	return out, err
}

// tagColor is the colour of the flag-set tags the link creates.
const tagColor = "#3d4db6"

// ensureTag returns the id of the tag named after set, creating it when the
// project has none. A tag that cannot be created is logged; the feature is
// then created untagged (0).
func (s *startup) ensureTag(ctx context.Context, set string, tags map[string]int) (int, error) {
	if id, ok := tags[set]; ok {
		return id, nil
	}
	k := Key{Set: set, Scope: ScopeStack}
	sctx, span := s.pushSpan(ctx, k, OpTag)
	var t fsTag
	_, _, err := s.pace.call(sctx, http.MethodPost, s.projectPath("tags/"), map[string]any{"label": set, "color": tagColor}, &t, true, nil)
	if err != nil {
		s.endPush(sctx, span, OpTag, ResultError, Reason(err))
		if ctx.Err() != nil {
			return 0, err
		}
		s.l.Logger().Warn("flagsmith tag not created; its features are created untagged", "flag_set", set, "reason", Reason(err))
		tags[set] = 0
		return 0, nil
	}
	s.endPush(sctx, span, OpTag, ResultOK, "")
	tags[set] = t.ID
	s.l.ids.Lock()
	s.l.ids.TagID[set] = t.ID
	s.l.ids.Unlock()
	return t.ID, nil
}

// ensureFeatures makes every registry flag a feature (BR-002): an existing
// feature (name equal) keeps its id and is never re-tagged or re-described; a
// missing one is created, tagged with its flag set, without initial_value
// (O11). A name equal only ignoring case, an archived feature or a create
// that keeps failing refuses that key and the start goes on; only an error
// of the feature list, or a cancel, is returned.
func (s *startup) ensureFeatures(ctx context.Context) error {
	if s.l.deps.Registry == nil {
		return nil
	}
	policies := append([]*registry.Policy(nil), s.l.deps.Registry.Policies()...)
	sort.Slice(policies, func(i, j int) bool {
		if policies[i].Set != policies[j].Set {
			return policies[i].Set < policies[j].Set
		}
		return policies[i].Key < policies[j].Key
	})
	lower := map[string]int{}
	for _, p := range policies {
		lower[strings.ToLower(p.Key)]++
	}
	existing, err := s.listFeatures(ctx)
	if err != nil {
		return err
	}
	var tags map[string]int
	relisted := false
	s.optionJobs = nil
	for _, p := range policies {
		k := Key{Set: p.Set, Flag: p.Key, Scope: ScopeStack}
		if lower[strings.ToLower(p.Key)] > 1 {
			s.l.Logger().Error("flagsmith: two registry keys differ only in case; neither is a feature", "flag_set", p.Set, "flag", p.Key)
			continue
		}
		if f, ok := existing[strings.ToLower(p.Key)]; ok {
			if s.useExisting(k, f) {
				s.queueOptions(k, p, f.ID, f.MultivariateOptions)
			}
			continue
		}
		if tags == nil {
			if tags, err = s.listTags(ctx); err != nil {
				return err
			}
		}
		tagID, err := s.ensureTag(ctx, p.Set, tags)
		if err != nil {
			return err
		}
		f, err := s.createFeature(ctx, k, p, tagID)
		switch {
		case err == nil:
			s.setFeatureID(k, f.ID)
			s.queueOptions(k, p, f.ID, f.MultivariateOptions)
		case ctx.Err() != nil:
			return err
		case httpStatus(err) == http.StatusBadRequest && alreadyExists(err) && !relisted:
			// Created meanwhile (a parallel start, a person): list once more.
			relisted = true
			if existing, err = s.listFeatures(ctx); err != nil {
				return err
			}
			if f, ok := existing[strings.ToLower(p.Key)]; ok {
				if s.useExisting(k, f) {
					s.queueOptions(k, p, f.ID, f.MultivariateOptions)
				}
				continue
			}
			s.refuse(k, "feature_create_400")
		case isTransient(err) || httpStatus(err) == 0:
			s.refuse(k, ReasonFeatureCreateFailed)
		default:
			s.refuse(k, fmt.Sprintf("feature_create_%d", httpStatus(err)))
		}
	}
	return nil
}

func alreadyExists(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && strings.Contains(strings.ToLower(string(he.Body)), "already exists")
}

// useExisting takes an existing feature's id, unless its name differs from
// the key in case (Flagsmith would refuse a second one) or it is archived:
// both refuse the key (O4, O10); the link never renames or unarchives. It
// reports whether the feature is used.
func (s *startup) useExisting(k Key, f fsFeature) bool {
	switch {
	case f.Name != k.Flag:
		s.refuse(k, ReasonFeatureNameCase)
	case f.IsArchived:
		s.refuse(k, ReasonFeatureArchived)
	default:
		s.setFeatureID(k, f.ID)
		return true
	}
	return false
}

// setFeatureID records k's feature id and clears a refusal an earlier,
// failed start run left on k (feature_create_*, feature_name_case,
// feature_archived): the feature exists and is usable now (O7, every step
// idempotent).
func (s *startup) setFeatureID(k Key, id int) {
	s.l.ids.Lock()
	s.l.ids.FeatureID[k.Flag] = id
	s.l.ids.Unlock()
	if r, side := s.l.links.Refusal(k); side == SideRemote && isStartRefusal(r) {
		s.l.links.ClearRefusal(k)
	}
}

// isStartRefusal: a refusal reason only the start sequence sets.
func isStartRefusal(reason string) bool {
	return strings.HasPrefix(reason, "feature_create_") || reason == ReasonFeatureNameCase || reason == ReasonFeatureArchived
}

// refuse marks k refused on the remote side with a warning.
func (s *startup) refuse(k Key, reason string) {
	s.l.links.SetInSync(k, false)
	s.l.links.Refuse(k, SideRemote, reason)
	s.l.Logger().Warn("flagsmith feature refused", "flag_set", k.Set, "flag", k.Flag, "reason", reason)
}

// createFeature POSTs one feature: STANDARD, disabled, the registry
// description, the flag-set tag; never initial_value (O11: it would change
// what every other environment of the project serves).
func (s *startup) createFeature(ctx context.Context, k Key, p *registry.Policy, tagID int) (fsFeature, error) {
	body := map[string]any{
		"name":            p.Key,
		"description":     p.Description,
		"type":            "STANDARD",
		"default_enabled": false,
	}
	if tagID != 0 {
		body["tags"] = []int{tagID}
	}
	sctx, span := s.pushSpan(ctx, k, OpFeature)
	var f fsFeature
	_, _, err := s.pace.call(sctx, http.MethodPost, s.projectPath("features/"), body, &f, true, nil)
	switch {
	case err == nil:
		s.endPush(sctx, span, OpFeature, ResultOK, "")
	case httpStatus(err) >= 400 && httpStatus(err) < 500:
		s.endPush(sctx, span, OpFeature, ResultRejected, Reason(err))
	default:
		s.endPush(sctx, span, OpFeature, ResultError, Reason(err))
	}
	return f, err
}

// pushSpan starts a flagservice.flagsmith.push span of the start sequence.
func (s *startup) pushSpan(ctx context.Context, k Key, op string) (context.Context, trace.Span) {
	return StartPush(ctx, s.l.Tracer(), k, op, WriterLabel, 1, trace.SpanContext{})
}

// endPush ends a push span and counts the call.
func (s *startup) endPush(ctx context.Context, span trace.Span, op, result, reason string) {
	EndSpan(span, result, reason)
	s.l.Metrics().RecordPush(ctx, op, result, reason)
}
