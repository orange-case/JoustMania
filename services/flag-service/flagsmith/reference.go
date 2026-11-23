package flagsmith

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Reference values (FR-227, architecture note §5.2, F13, #2623): every synced
// feature without a stack overlay shows the registry baseline default under
// enabled=false, so Flagsmith shows the whole configuration of the case. The
// start sequence writes it where Flagsmith shows another value. The pull
// reads enabled=false as no value whatever the value is (decodeState), so a
// reference write is never a change, a conflict or a refusal, and the link's
// state (remote, pending, in_sync, refused) is not touched.
//
// The reference is always the baseline default, also for a persisted admin
// setting (maintainer decision 2026-11-23): that is a stack overlay, which the
// start reconcile sends with enabled=true (§5.1), and toggle off means "the
// baseline applies".
//
// One bulk read (the environment document) decides which features may need
// a reference; right before each write the feature's environment state is
// read again from the Admin API (§5.2, decision 2026-11-23), so a toggle a
// person or an API key turned on meanwhile (a kill switch included) is never
// turned off: the window is the one request between a re-read and its
// write. The write itself is never retried as is: after a 429, a transient
// failure or an options relist the feature is read again first.

// referenceJob is one usable synced feature of the start.
type referenceJob struct {
	k         Key
	featureID int
	// created: the start created the feature, so a document that does not
	// show it yet still means "disabled, no value".
	created bool
}

// writeReferences runs the reference step (§5.2): one paced document read,
// then, per feature whose toggle is off and whose value is not the reference
// value, a paced re-read of its environment state and a paced PATCH. It runs
// inside Start, before the start reconcile and before the sender sends
// anything, so no push of a stack value races it.
//
// Failures (§5.2, decision 2026-11-23): a transient read failure (no answer,
// timeout, 5xx, a 429 after the pacer's retries) is returned, so the start
// fails and is retried with backoff (every step is idempotent); a 4xx on the
// document read is logged once and the process writes no reference; the
// first refusal that holds for the whole environment stops the step with one
// log line; any other refusal is logged for its feature and the step goes on.
func (s *startup) writeReferences(ctx context.Context, jobs []referenceJob) error {
	if len(jobs) == 0 {
		return nil
	}
	var doc *EnvironmentDocument
	_, _, err := s.pace.do(ctx, true, nil, func() (int, error) {
		d, _, _, err := s.l.client.EnvironmentDocument(ctx, "")
		doc = d
		return httpStatus(err), err
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if transientRead(err) {
			return err
		}
		s.l.Logger().Warn("flagsmith reference not written", "detail", "the document could not be read; this process writes no reference value", "reason", Reason(err))
		return nil
	}
	shown := map[string]FeatureState{}
	for _, fs := range doc.FeatureStates {
		if isSegment(fs.FeatureSegment) {
			continue
		}
		shown[fs.Feature.Name] = fs
	}
	for _, j := range jobs {
		fv, ok := s.referenceOf(j.k)
		if !ok {
			continue
		}
		fs, inDoc := shown[j.k.Flag]
		if !needsReference(fs, inDoc, j.created, fv) {
			continue
		}
		stop, err := s.writeReference(ctx, j, fv)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
	return nil
}

// transientRead: a read failure the start retries (no answer, a timeout, a
// 5xx, a 429 left after the pacer's retries).
func transientRead(err error) bool {
	return isTransient(err) || httpStatus(err) == http.StatusTooManyRequests
}

// referenceOf is the reference value of k (§2.1: the baseline default
// encoded as a cleared overlay sends it); ok false when the flag has no
// baseline.
func (s *startup) referenceOf(k Key) (FSValue, bool) {
	if s.l.BaselineFlag(k.Set, k.Flag) == nil {
		return FSValue{}, false
	}
	fv, err := s.l.encodeFor(k, Value{})
	return fv, err == nil
}

// needsReference: the feature's environment value is not the reference
// value under an off toggle (§5.2 table). An enabled feature is never
// written; one absent from the document only when the start created it.
func needsReference(fs FeatureState, inDoc, created bool, ref FSValue) bool {
	if !inDoc {
		return created
	}
	if fs.Enabled {
		return false
	}
	for _, mv := range fs.MultivariateValues {
		if mv.PercentageAllocation != 0 {
			return true
		}
	}
	return fsText(fs.FeatureStateValue) != fsText(ref.Value)
}

// fsText is a Flagsmith value as text: a JSON string's content, else the
// literal (a number's digits, true / false); "" for none. So the integer
// reference 10 equals a document "10" (§2.1, #2682).
func fsText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if raw[0] == '"' && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// listedState is one feature state of the v1 listing
// GET environments/{key}/featurestates/?feature=<id>.
type listedState struct {
	ID             int             `json:"id"`
	Feature        json.RawMessage `json:"feature"`
	Enabled        bool            `json:"enabled"`
	Identity       json.RawMessage `json:"identity"`
	FeatureSegment json.RawMessage `json:"feature_segment"`
	Value          json.RawMessage `json:"feature_state_value"`
	LiveFrom       string          `json:"live_from"`
	Weights        []struct {
		PercentageAllocation float64 `json:"percentage_allocation"`
	} `json:"multivariate_feature_state_values"`
}

// featureID is the item's feature id: the listing names it by id; an
// object with an id is read too. 0 when it names none.
func (it listedState) featureID() int {
	var id int
	if json.Unmarshal(it.Feature, &id) == nil {
		return id
	}
	var obj struct {
		ID int `json:"id"`
	}
	if json.Unmarshal(it.Feature, &obj) == nil {
		return obj.ID
	}
	return 0
}

// ofFeature keeps the items of the feature featureID, so a listing that
// ignored the ?feature= filter never decides on another feature's state.
func ofFeature(items []listedState, featureID int) []listedState {
	var kept []listedState
	for _, it := range items {
		if it.featureID() == featureID {
			kept = append(kept, it)
		}
	}
	return kept
}

// environmentDefault picks the feature's environment default from a
// listing: identity and feature_segment null. Flagsmith answers one per
// feature (get_environment_flags_queryset keeps the latest live version per
// feature, segment and identity, under v2 versioning too); when several come
// back the newest live one wins (live_from not after now, then the latest
// live_from, then the highest id), else the newest. ok false: none listed.
func environmentDefault(items []listedState, now time.Time) (FeatureState, bool) {
	best, bestLive, found := listedState{}, false, false
	var bestAt time.Time
	for _, it := range items {
		if isSegment(it.Identity) || isSegment(it.FeatureSegment) {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, it.LiveFrom)
		live := !at.After(now)
		better := !found || (live && !bestLive) ||
			(live == bestLive && (at.After(bestAt) || (at.Equal(bestAt) && it.ID > best.ID)))
		if better {
			best, bestLive, bestAt, found = it, live, at, true
		}
	}
	if !found {
		return FeatureState{}, false
	}
	fs := FeatureState{Enabled: best.Enabled, FeatureStateValue: best.Value}
	for _, w := range best.Weights {
		fs.MultivariateValues = append(fs.MultivariateValues, MVStateValue{PercentageAllocation: w.PercentageAllocation})
	}
	return fs, true
}

// ReasonStateNotListed is the reason of a re-read that listed no environment
// default for the feature (an unknown answer, or a filter Flagsmith ignored). Flagsmith keeps
// one for every feature of the environment, so the state is unknown and the
// feature is not written (§5.2).
const ReasonStateNotListed = "state_not_listed"

// readState reads the feature's environment state again right before its
// write (paced like every Admin call of the start). A listing without the
// feature's environment default is a failed re-read (ReasonStateNotListed),
// not a transient one.
func (s *startup) readState(ctx context.Context, featureID int) (FeatureState, error) {
	var raw json.RawMessage
	if _, _, err := s.pace.call(ctx, http.MethodGet, s.l.envPath("featurestates/")+"?feature="+strconv.Itoa(featureID), nil, &raw, true, nil); err != nil {
		return FeatureState{}, err
	}
	fs, ok := environmentDefault(ofFeature(listedItems[listedState](raw), featureID), s.l.Now())
	if !ok {
		return FeatureState{}, ReasonError{Reason: ReasonStateNotListed}
	}
	return fs, nil
}

// writeReference re-reads one feature and, when its toggle is still off and
// it still does not show the reference value, sends the reference: enabled
// false, the reference value, 0 % on every option (the body of a cleared
// overlay, §4.1 step 3). The write is sent once, with no retry of its own:
// a 429 waits its Retry-After (at most maxRateLimitRetries times), a 5xx or
// a request without an answer waits transientBackoff (transientAttempts
// writes in all), and a refusal of the option weights lists the options
// once (as the out side does, §2.3); after each of these the feature is read
// again before the next write, so a toggle turned on meanwhile (a kill
// switch included) is never turned off: the window is the one request
// between a re-read and its write. All writes of the feature are one push
// span. stop is true when a refusal holds for the whole environment; err
// ends the start.
func (s *startup) writeReference(ctx context.Context, j referenceJob, fv FSValue) (stop bool, err error) {
	var (
		sctx                   = ctx
		span                   trace.Span
		werr                   error
		rateRetries, transient int
		relisted               bool
	)
	end := func() {
		if span == nil {
			return
		}
		switch {
		case werr == nil:
			s.endPush(sctx, span, OpReference, ResultOK, "")
		case httpStatus(werr) >= 400 && httpStatus(werr) < 500:
			s.endPush(sctx, span, OpReference, ResultRejected, Reason(werr))
		default:
			s.endPush(sctx, span, OpReference, ResultError, Reason(werr))
		}
	}
	for {
		cur, rerr := s.readState(ctx, j.featureID)
		if rerr != nil {
			end()
			return s.referenceFailed(ctx, j, rerr, false)
		}
		if !needsReference(cur, true, false, fv) {
			if !cur.Enabled {
				// Off and at the reference: Flagsmith applied the failed
				// write (a timed-out PATCH), so the push ends ok. Turned on
				// meanwhile: the result of the last write sent stays.
				werr = nil
			}
			end()
			return false, nil // turned on meanwhile, or the reference already
		}
		if span == nil {
			sctx, span = s.pushSpan(ctx, j.k, OpReference)
		}
		werr = s.patchReference(sctx, j, fv)
		if werr == nil {
			end()
			if old := fsText(cur.FeatureStateValue); isKillKey(j.k) && old != "" && old != fsText(fv.Value) {
				// A kill switch value staged under the off toggle is not in
				// force; the start resets it (decision 2026-11-23, stage sheet).
				s.l.Logger().Info("flagsmith reference overwrote a kill switch value", "flag_set", j.k.Set, "flag", j.k.Flag, "old_value", old)
			}
			return false, nil
		}
		if ctx.Err() != nil {
			end()
			return false, ctx.Err()
		}
		var wait time.Duration
		var he *HTTPError
		switch {
		case errors.As(werr, &he) && he.Status == http.StatusTooManyRequests && rateRetries < maxRateLimitRetries:
			rateRetries++
			wait = he.RetryAfter
			if wait <= 0 {
				wait = time.Second
			}
		case variantsRefused(werr) && !relisted:
			// The feature's options changed since the feature list (a person
			// added or removed one): list them once, read again, send again (#2709).
			relisted = true
			if lerr := s.relistOptions(sctx, j.k.Flag, j.featureID); lerr != nil {
				end()
				return s.referenceFailed(ctx, j, werr, true)
			}
			continue
		case isTransient(werr) && transient+1 < transientAttempts:
			transient++
			wait = transientBackoff
		default:
			end()
			return s.referenceFailed(ctx, j, werr, true)
		}
		if serr := s.l.Sleep(ctx, wait); serr != nil {
			end()
			return false, serr
		}
	}
}

// patchReference sends the reference write once (adminPacer.once): its
// retries are writeReference's, each after a re-read.
func (s *startup) patchReference(ctx context.Context, j referenceJob, fv FSValue) error {
	return s.pace.once(ctx, http.MethodPatch, s.l.futurePath(j.featureID), futureBodyOf(fv, false, s.l.options(j.k.Flag)), nil)
}

// relistOptions lists the feature's options again with the out side's
// listOptions (the out side does not run during Start), paced by the start.
func (s *startup) relistOptions(ctx context.Context, flag string, featureID int) error {
	if err := s.pace.wait(ctx); err != nil {
		return err
	}
	return (&out{l: s.l}).listOptions(ctx, flag, featureID)
}

// referenceFailed handles a failed re-read (write false) or write: a cancel
// and a transient read failure end the start; the first refusal that holds
// for the whole environment stops the step with one line (INFO when the host
// has no __future__ API, else WARN); any other is logged for its feature.
func (s *startup) referenceFailed(ctx context.Context, j referenceJob, err error, write bool) (stop bool, _ error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if !write && transientRead(err) {
		return false, err
	}
	if noFutureAPI(err) && write {
		s.l.Logger().Info("flagsmith: no __future__ API, reference values are not written")
		return true, nil
	}
	if environmentRefusal(err) {
		s.l.Logger().Warn("flagsmith reference values not written", "detail", "Flagsmith refused the environment; no further reference is written at this start", "flag_set", j.k.Set, "flag", j.k.Flag, "reason", Reason(err))
		return true, nil
	}
	s.l.Logger().Warn("flagsmith reference not written", "flag_set", j.k.Set, "flag", j.k.Flag, "reason", Reason(err))
	return false, nil
}

// noFutureAPI: a 404 that is not Flagsmith's JSON answer (push.go).
func noFutureAPI(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound && !he.JSONDetail
}

// environmentRefusal: a refusal that holds for every feature of the
// environment (bad or missing permissions, change requests, no route).
func environmentRefusal(err error) bool {
	var he *HTTPError
	if !errors.As(err, &he) {
		return false
	}
	switch he.Status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict:
		return true
	case http.StatusNotFound:
		return !he.JSONDetail
	}
	return Reason(err) == ReasonChangeRequestRequired || strings.Contains(strings.ToLower(string(he.Body)), "change_request")
}
