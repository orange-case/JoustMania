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
}

type futureBody struct {
	EnvironmentDefault futureDefault `json:"environment_default"`
}

type overrideBody struct {
	Identifier string          `json:"identifier"`
	Feature    int             `json:"feature"`
	Enabled    bool            `json:"enabled"`
	Value      json.RawMessage `json:"feature_state_value"`
}

type overrideKey struct {
	Identifier string `json:"identifier"`
	Feature    int    `json:"feature"`
}

type stateBody struct {
	Enabled bool            `json:"enabled"`
	Value   json.RawMessage `json:"feature_state_value"`
}

// futureBodyOf is the body of the stack value PATCH: the experimental flag API
// takes the type name and the value as text (architecture note §4.1).
func futureBodyOf(fv FSValue, enabled bool) futureBody {
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
	return futureBody{EnvironmentDefault: futureDefault{Enabled: enabled, Value: futureValue{Type: typ, Value: text}}}
}

// futurePath is the stack value endpoint, from the host root.
func (l *Link) futurePath(featureID int) string {
	return "/api/__future__/environments/" + url.PathEscape(l.Config().EnvironmentKey) + "/features/" + strconv.Itoa(featureID) + "/"
}

// send sends one key's value: a stack value to the feature's environment
// value, any other scope to the override on its identity (a value that is
// not present deletes the override).
func (o *out) send(ctx context.Context, k Key, v Value, fv FSValue, featureID int) (int, error) {
	l := o.l
	cl := l.Client()
	if id := IdentityOf(k); id != "" {
		path := l.envPath("edge-identities-featurestates")
		if !v.Present {
			status, err := cl.Admin(ctx, http.MethodDelete, path, overrideKey{Identifier: id, Feature: featureID}, nil)
			return status, ignoreNotFound(err)
		}
		return cl.Admin(ctx, http.MethodPut, path, overrideBody{Identifier: id, Feature: featureID, Enabled: true, Value: fv.Value}, nil)
	}
	if !o.fallback.Load() {
		status, err := cl.Admin(ctx, http.MethodPatch, l.futurePath(featureID), futureBodyOf(fv, v.Present), nil)
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
	var paged struct {
		Results []item `json:"results"`
	}
	if json.Unmarshal(raw, &paged) == nil && len(paged.Results) > 0 {
		return paged.Results[0].ID, true
	}
	var plain []item
	if json.Unmarshal(raw, &plain) == nil && len(plain) > 0 {
		return plain[0].ID, true
	}
	return 0, false
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
	featureID, haveFeature := l.featureID(k)
	if !haveFeature {
		o.refuseUnsent(ctx, k, v, merged, version, ReasonNotFound)
		return true, nil
	}
	fv, encErr := l.encodeFor(k, v)
	if encErr != nil {
		o.refuseUnsent(ctx, k, v, merged, version, ReasonBadValue)
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
		return o.send(c, k, v, fv, featureID)
	})
	switch {
	case oc.err != nil:
		if ctx.Err() == nil {
			// The link goes down on this key. Keep it pending but move it to
			// the back, so a key that always fails does not stay first after
			// every reconnect and starve the others.
			l.Links().Retry(k, l.Now().Add(defaultRetryWait))
		}
		return false, oc.err
	case oc.wait > 0:
		return true, l.Sleep(ctx, oc.wait)
	case oc.ok:
		l.Links().ConfirmPushed(k, v)
		l.Links().ClearRefusal(k)
	default:
		// Refuse the value that was pushed: a newer one merged meanwhile
		// stays pending.
		l.Links().RefuseValue(k, v, SideRemote, oc.reason)
	}
	return true, nil
}

// refuseUnsent refuses v of k without a request (its feature id is unknown, or
// the value cannot be encoded) and records the push like a refused call: one
// pushes_total point and one push span.
func (o *out) refuseUnsent(ctx context.Context, k Key, v Value, merged int, version uint64, reason string) {
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
}
