package flagsmith

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/joustmania/flag-service/baseline"
)

// Retry-After bounds of a 429 (UC-055 A4): the wait Flagsmith asks for, 1 s
// when it names none, never more than a minute.
const (
	defaultRetryWait = time.Second
	maxRetryWait     = 60 * time.Second
)

// A 404 on an override push to an identity this process listed or settled
// less than notFoundFreshFor ago is retried, at most notFoundRetries times per
// pending value, after notFoundBackoff, then twice and four times that
// (UC-055 A4, #2639): Flagsmith's edge storage may not show an identity it
// just created yet. The waiting value goes to the back of the queue, so it
// never holds the other keys back. Any other 404, and the one after the last
// retry, refuses the value as before.
const (
	notFoundFreshFor = 30 * time.Second
	notFoundRetries  = 3
	notFoundBackoff  = 500 * time.Millisecond
)

// pace waits until the next request may go out: at most PushRate requests per
// second, on the link's injected clock and sleep (UC-055 BR-001). Only the
// Run goroutine calls it.
func (o *out) pace(ctx context.Context) error {
	rate := o.l.Config().PushRate
	if rate <= 0 {
		rate = DefaultPushRate
	}
	if now := o.l.Now(); now.Before(o.nextAt) {
		if err := o.l.Sleep(ctx, o.nextAt.Sub(now)); err != nil {
			return err
		}
	}
	o.nextAt = o.l.Now().Add(time.Second / time.Duration(rate))
	return nil
}

// outcome is the result of one Admin API round trip.
type outcome struct {
	// ok: Flagsmith accepted it (or a delete found nothing left to delete).
	ok bool
	// reason is the refusal reason when the call failed and the link stays up.
	reason string
	// wait > 0: a 429, try again after wait.
	wait time.Duration
	// err ends the Run: the link is down, or ctx ended.
	err    error
	status int
}

// roundTrip runs fn inside a flagservice.flagsmith.push span, counts the call
// and classifies its answer.
func (o *out) roundTrip(ctx context.Context, k Key, op, writer string, merged int, link trace.SpanContext, fn func(context.Context) (int, error)) outcome {
	sctx, span := StartPush(ctx, o.l.Tracer(), k, op, writer, merged, link)
	status, err := fn(sctx)
	if status > 0 {
		span.SetAttributes(attribute.Int(AttrHTTPStatus, status))
	}
	if err != nil && ctx.Err() != nil {
		EndSpan(span, ResultError, ReasonStopped)
		return outcome{err: ctx.Err(), status: status}
	}
	result, reason, down := classify(err)
	o.l.Metrics().RecordPush(ctx, op, result, reason)
	EndSpan(span, result, reason)
	oc := outcome{status: status, reason: reason}
	switch {
	case err == nil:
		oc.ok = true
	case reason == ReasonRateLimited:
		oc.wait = retryWait(err)
	case down:
		oc.err = err
	}
	return oc
}

// classify maps the answer of one Admin API call to the push result and
// reason of telemetry.md §10.4. linkDown: the whole link goes offline (no
// answer, TLS, a 5xx); every other refusal marks one key refused and keeps the
// link up, a 401 or 403 included (UC-055 A4: the document pull may still work).
func classify(err error) (result, reason string, linkDown bool) {
	if err == nil {
		return ResultOK, "", false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		switch {
		case he.Status >= http.StatusInternalServerError:
			return ResultError, "http_5xx", true
		case he.Status == http.StatusConflict:
			return ResultRejected, ReasonChangeRequestRequired, false
		}
		return ResultRejected, Reason(err), false
	}
	return ResultError, Reason(err), IsLinkDown(err)
}

// retryWait is the wait of a 429.
func retryWait(err error) time.Duration {
	var he *HTTPError
	d := defaultRetryWait
	if errors.As(err, &he) && he.RetryAfter > 0 {
		d = he.RetryAfter
	}
	if d > maxRetryWait {
		d = maxRetryWait
	}
	return d
}

// valueOp is the push op of a key's value.
func valueOp(k Key, v Value) string {
	switch {
	case IdentityOf(k) == "":
		return OpValue
	case v.Present:
		return OpOverride
	}
	return OpDeleteOverride
}

// envPath is a path below the stack's environment.
func (l *Link) envPath(elem string) string {
	return "environments/" + url.PathEscape(l.Config().EnvironmentKey) + "/" + elem
}

// featureID returns the Flagsmith id of k's feature (resolved by the start
// sequence).
func (l *Link) featureID(k Key) (int, bool) {
	ids := l.IDs()
	ids.Lock()
	defer ids.Unlock()
	id, ok := ids.FeatureID[k.Flag]
	return id, ok
}

// encodeFor encodes v for k's flag (architecture note §2.1). A value that is
// not present encodes the baseline default.
func (l *Link) encodeFor(k Key, v Value) (FSValue, error) {
	b := l.BaselineFlag(k.Set, k.Flag)
	if b == nil {
		if !v.Present {
			return unicode(""), nil
		}
		b = &baseline.Flag{}
	}
	return Encode(l.Policy(k.Set, k.Flag), b, v)
}

// Request bodies. The field order is the order on the wire.
type futureValue struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type futureDefault struct {
	Enabled bool        `json:"enabled"`
	Value   futureValue `json:"value"`
	// Variants are the option weights of a feature with options (#2709):
	// Flagsmith wants every option of the feature, exactly once.
	Variants []futureVariant `json:"variants,omitempty"`
}

type futureVariant struct {
	ID     int     `json:"id"`
	Weight float64 `json:"weight"`
}

type futureBody struct {
	EnvironmentDefault futureDefault `json:"environment_default"`
}

type stateBody struct {
	Enabled bool            `json:"enabled"`
	Value   json.RawMessage `json:"feature_state_value"`
}

// futureBodyOf is the body of the stack value PATCH: the experimental flag API
// takes the type name and the value as text (architecture note §4.1).
func futureBodyOf(fv FSValue, enabled bool, opts []Option) futureBody {
	typ, text := "string", string(fv.Value)
	switch fv.Type {
	case TypeInt:
		typ = "integer"
	case TypeBool:
		typ = "boolean"
	default:
		var s string
		if json.Unmarshal(fv.Value, &s) == nil {
			text = s
		}
	}
	return futureBody{EnvironmentDefault: futureDefault{Enabled: enabled, Value: futureValue{Type: typ, Value: text}, Variants: weightsFor(opts, fv, enabled)}}
}

// futurePath is the stack value endpoint, from the host root.
func (l *Link) futurePath(featureID int) string {
	return "/api/__future__/environments/" + url.PathEscape(l.Config().EnvironmentKey) + "/features/" + strconv.Itoa(featureID) + "/"
}

// send sends one key's value: a stack value (id "") to the feature's
// environment value, any other scope to the override on identity id (a
// value that is not present deletes the override; overrides.go, #2682).
func (o *out) send(ctx context.Context, k Key, id string, v Value, fv FSValue, featureID int) (int, error) {
	l := o.l
	cl := l.Client()
	if id != "" {
		return o.sendOverride(ctx, id, v, fv, featureID)
	}
	if !o.fallback.Load() {
		status, err := cl.Admin(ctx, http.MethodPatch, l.futurePath(featureID), futureBodyOf(fv, v.Present, l.options(k.Flag)), nil)
		if variantsRefused(err) {
			// The feature's options changed since the start (a person added
			// or removed one): list them once and send again (#2709, §2.3).
			if lerr := o.listOptions(ctx, k, featureID); lerr != nil {
				return status, err
			}
			if perr := o.pace(ctx); perr != nil {
				return 0, perr
			}
			return cl.Admin(ctx, http.MethodPatch, l.futurePath(featureID), futureBodyOf(fv, v.Present, l.options(k.Flag)), nil)
		}
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusNotFound || he.JSONDetail {
			return status, err
		}
		// A 404 that is not Flagsmith's JSON answer: the host has no
		// __future__ API. Use the v1 feature state for the rest of the process.
		if !o.fallback.Swap(true) {
			l.Logger().Info("flagsmith: no __future__ API, writing stack values through the v1 feature states")
		}
		if err := o.pace(ctx); err != nil {
			return 0, err
		}
	}
	return o.sendFallback(ctx, v, fv, featureID)
}

// sendFallback sets the stack value through the v1 feature state of the
// environment (a Flagsmith without the __future__ API).
func (o *out) sendFallback(ctx context.Context, v Value, fv FSValue, featureID int) (int, error) {
	l := o.l
	cl := l.Client()
	var found json.RawMessage
	status, err := cl.Admin(ctx, http.MethodGet, l.envPath("featurestates/")+"?feature="+strconv.Itoa(featureID), nil, &found)
	if err != nil {
		return status, err
	}
	id, ok := firstID(found)
	if !ok {
		return status, &HTTPError{Status: http.StatusNotFound, JSONDetail: true}
	}
	if err := o.pace(ctx); err != nil {
		return 0, err
	}
	return cl.Admin(ctx, http.MethodPut, l.envPath("featurestates/"+strconv.Itoa(id)+"/"), stateBody{Enabled: v.Present, Value: fv.Value}, nil)
}

// firstID returns the id of the first feature state of a list answer, paged
// ({"results": [...]}) or plain ([...]).
func firstID(raw json.RawMessage) (int, bool) {
	type item struct {
		ID int `json:"id"`
	}
	items := listedItems[item](raw)
	if len(items) == 0 {
		return 0, false
	}
	return items[0].ID, true
}

// listedItems are the items of a list answer, paged ({"results": [...]}) or
// plain ([...]); none when it is neither.
func listedItems[T any](raw json.RawMessage) []T {
	var paged struct {
		Results []T `json:"results"`
	}
	if json.Unmarshal(raw, &paged) == nil && len(paged.Results) > 0 {
		return paged.Results
	}
	var plain []T
	if json.Unmarshal(raw, &plain) == nil {
		return plain
	}
	return nil
}

// ignoreNotFound turns the 404 of a DELETE into success: what it should
// remove is gone already.
func ignoreNotFound(err error) error {
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// pushNext sends the oldest pending value, if any. It reports whether it did
// something (a request, a refusal or a wait) and returns an error only when
// the Run must end: the link is down (the value stays pending, because Next
// peeks) or ctx ended.
func (o *out) pushNext(ctx context.Context) (bool, error) {
	l := o.l
	k, v, merged, version, ok := l.Links().Next()
	if !ok {
		return false, nil
	}
	identifier, decision := l.ctrl.identityFor(k)
	switch decision {
	case sendDrop:
		// A player value of a game that ended or does not own the
		// controller (any more): never sent to a controller another game
		// owns (#2624).
		l.Links().ForgetIdentity(k.Scope)
		return true, nil
	case sendWait:
		// The controller's settle is due (it became due after this step
		// looked for controller work): the next step settles it, then
		// sends this value.
		return true, nil
	}
	featureID, haveFeature := l.featureID(k)
	if !haveFeature {
		o.refuseUnsent(ctx, k, v, merged, version, identifier, ReasonNotFound)
		return true, nil
	}
	fv, encErr := l.encodeFor(k, v)
	if encErr != nil {
		o.refuseUnsent(ctx, k, v, merged, version, identifier, ReasonBadValue)
		return true, nil
	}
	if err := o.pace(ctx); err != nil {
		return false, err
	}
	var link trace.SpanContext
	if sc, found := l.WriteSpanContext(version); found {
		link = sc
	}
	oc := o.roundTrip(ctx, k, valueOp(k, v), v.Writer, merged, link, func(c context.Context) (int, error) {
		return o.send(c, k, identifier, v, fv, featureID)
	})
	if oc.status == http.StatusNotFound && !oc.ok && oc.err == nil && identifier != "" && o.listedRecently(identifier) {
		if try, ok := l.Links().RetryNotFound(k, notFoundRetries); ok {
			// Per key: the value waits at the back of the queue, the other
			// keys go on; a timer wakes the sender when it is due.
			wait := notFoundBackoff << (try - 1)
			l.Links().Retry(k, l.Now().Add(wait))
			go func() {
				if l.Sleep(ctx, wait) == nil {
					o.poke()
				}
			}()
			return true, nil
		}
	}
	switch {
	case oc.err != nil:
		if ctx.Err() == nil {
			// The link goes down on this key. Keep it pending but move it to
			// the back, so a key that always fails does not stay first after
			// every reconnect and starve the others (the link backs off, not the key).
			l.Links().Retry(k, l.Now())
		}
		return false, oc.err
	case oc.wait > 0:
		return true, l.Sleep(ctx, oc.wait)
	case oc.ok:
		// ConfirmPushed ends the refusal itself, except while the local
		// value is the refused one again (no ClearRefusal here, UC-055 A4).
		l.Links().ConfirmPushed(k, v)
	default:
		// Refuse the value that was pushed: a newer one merged meanwhile
		// stays pending. The refused value is remembered, so no pull round
		// re-sends it before a different local value (UC-055 A4).
		l.Links().RefuseValueStatus(k, v, SideRemote, oc.reason, oc.status)
		o.warnRefused(k, valueOp(k, v), identifier, oc.reason, oc.status)
		if k.Scope == ScopeStack {
			// The in side reverts it to Flagsmith's value at once (UC-055 A4,
			// #2621), with a full pull: the document did not change.
			l.fullPull.Store(true)
			l.Wake()
		}
	}
	return true, nil
}

// refuseUnsent refuses v of k without a request (its feature id is unknown, or
// the value cannot be encoded) and records the push like a refused call: one
// pushes_total point and one push span.
func (o *out) refuseUnsent(ctx context.Context, k Key, v Value, merged int, version uint64, identifier, reason string) {
	l := o.l
	var link trace.SpanContext
	if sc, found := l.WriteSpanContext(version); found {
		link = sc
	}
	op := valueOp(k, v)
	_, span := StartPush(ctx, l.Tracer(), k, op, v.Writer, merged, link)
	l.Metrics().RecordPush(ctx, op, ResultRejected, reason)
	EndSpan(span, ResultRejected, reason)
	l.Links().RefuseValue(k, v, SideRemote, reason)
	o.warnRefused(k, op, identifier, reason, 0)
	if k.Scope == ScopeStack {
		l.fullPull.Store(true)
		l.Wake()
	}
}

// warnRefused logs one WARN per refused push (#2639): flag set, flag, scope,
// the identity it was sent to, op, reason and Flagsmith's HTTP status (0: no
// request was sent). Never the value.
func (o *out) warnRefused(k Key, op, identifier, reason string, status int) {
	o.l.Logger().Warn("flagsmith push refused", "flag_set", k.Set, "flag", k.Flag, "scope", k.Scope,
		"identity", identifier, "op", op, "reason", reason, "http_status", status)
}

// markListed records that this process just listed or settled identifier
// (the identity exists in Flagsmith now). Only the Run goroutine calls it.
func (o *out) markListed(identifier string) {
	now := o.l.Now()
	if len(o.listed) >= 64 {
		for id, at := range o.listed {
			if now.Sub(at) >= notFoundFreshFor {
				delete(o.listed, id)
			}
		}
	}
	o.listed[identifier] = now
}

// listedRecently reports whether this process listed or settled identifier
// less than notFoundFreshFor ago.
func (o *out) listedRecently(identifier string) bool {
	at, ok := o.listed[identifier]
	return ok && o.l.Now().Sub(at) < notFoundFreshFor
}
