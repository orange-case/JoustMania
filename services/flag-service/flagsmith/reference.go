package flagsmith

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// Reference values (FR-227, architecture note §5.2, F13, #2623): every synced
// feature without a stack overlay shows the registry baseline default under
// enabled=false, so Flagsmith shows the whole configuration of the case. The
// start sequence writes it where the document shows another value. The pull
// reads enabled=false as no value whatever the value is (decodeState), so a
// reference write is never a change, a conflict or a refusal, and the link's
// state (remote, pending, in_sync, refused) is not touched.
//
// The reference is always the baseline default, also for a persisted admin
// setting: that is a stack overlay, which the start reconcile sends with
// enabled=true (§5.1), and toggle off means "the baseline applies".

// referenceJob is one usable synced feature of the start.
type referenceJob struct {
	k         Key
	featureID int
	// created: the start created the feature, so a document that does not
	// show it yet still means "disabled, no value".
	created bool
}

// writeReferences runs the reference step (§5.2): one full document pull,
// then a paced PATCH per feature whose toggle is off and whose value is not
// the reference value. It runs inside Start, before the start reconcile and
// before the sender sends anything, so no push of a stack value races it.
// Only a cancel is returned; every other failure is logged and the start
// goes on (the next start tries again).
func (s *startup) writeReferences(ctx context.Context, jobs []referenceJob) error {
	if len(jobs) == 0 {
		return nil
	}
	doc, _, _, err := s.l.client.EnvironmentDocument(ctx, "")
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.l.Logger().Warn("flagsmith reference not written", "detail", "the document could not be read", "reason", Reason(err))
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

// writeReference sends one reference value: enabled false, the reference
// value, 0 % on every option the feature list named (the body of a cleared
// overlay, §4.1 step 3). stop is true when the host has no __future__ API
// (no reference value is written on such a host).
func (s *startup) writeReference(ctx context.Context, j referenceJob, fv FSValue) (stop bool, err error) {
	sctx, span := s.pushSpan(ctx, j.k, OpReference)
	body := futureBodyOf(fv, false, s.l.options(j.k.Flag))
	_, _, err = s.pace.call(sctx, http.MethodPatch, s.l.futurePath(j.featureID), body, nil, true, nil)
	switch {
	case err == nil:
		s.endPush(sctx, span, OpReference, ResultOK, "")
		return false, nil
	case ctx.Err() != nil:
		s.endPush(sctx, span, OpReference, ResultError, Reason(err))
		return false, ctx.Err()
	case httpStatus(err) >= 400 && httpStatus(err) < 500:
		s.endPush(sctx, span, OpReference, ResultRejected, Reason(err))
	default:
		s.endPush(sctx, span, OpReference, ResultError, Reason(err))
	}
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusNotFound && !he.JSONDetail {
		s.l.Logger().Info("flagsmith: no __future__ API, reference values are not written")
		return true, nil
	}
	s.l.Logger().Warn("flagsmith reference not written", "flag_set", j.k.Set, "flag", j.k.Flag, "reason", Reason(err))
	return false, nil
}
