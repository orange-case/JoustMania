package flagsmith

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Identity overrides go through the per-identity routes of the Admin API
// (architecture note §4.1 step 4, #2682): the SaaS routes no upsert by
// identifier.
//
//	GET, POST          …/edge-identities/{identity_uuid}/edge-featurestates/
//	GET, PUT, DELETE   …/edge-identities/{identity_uuid}/edge-featurestates/{featurestate_uuid}/
//
// A write needs the identity's uuid and the featurestate_uuid of the
// feature's override on it. Both are cached (the uuid in IDs.IdentityUUID,
// the featurestate uuids here), so a value whose override is known costs one
// request; every other request is paced like any other.

// maxOverrideIdentities bounds the featurestate uuid cache.
const maxOverrideIdentities = 1024

// identityOverrides is what the out side knows of one identity's overrides.
type identityOverrides struct {
	// complete: every override of the identity is in byFeature (a fresh
	// identity, or a full listing), so a feature that is not in it has none.
	complete  bool
	byFeature map[int]string // feature id -> featurestate_uuid
}

// overrideOf returns the featurestate uuid of featureID's override on the
// identity ("" = none) and whether it is known.
func (o *out) overrideOf(uuid string, featureID int) (string, bool) {
	io := o.overrides[uuid]
	if io == nil {
		return "", false
	}
	fs, ok := io.byFeature[featureID]
	if ok || io.complete {
		return fs, true
	}
	return "", false
}

func (o *out) overridesFor(uuid string) *identityOverrides {
	io := o.overrides[uuid]
	if io == nil {
		if len(o.overrides) >= maxOverrideIdentities {
			o.overrides = map[string]*identityOverrides{}
		}
		io = &identityOverrides{byFeature: map[int]string{}}
		o.overrides[uuid] = io
	}
	return io
}

// setOverride records featureID's override on the identity (fs "": none).
func (o *out) setOverride(uuid string, featureID int, fs string) {
	io := o.overridesFor(uuid)
	if fs == "" && io.complete {
		delete(io.byFeature, featureID)
		return
	}
	if fs == "" {
		// Known to have none, on an identity not fully known.
		io.byFeature[featureID] = ""
		return
	}
	io.byFeature[featureID] = fs
}

// freshOverrides records an identity this process just created: it has no
// override.
func (o *out) freshOverrides(uuid string) {
	o.overrides[uuid] = nil
	o.overridesFor(uuid).complete = true
}

// listedOverrides records a full listing of the identity's overrides.
func (o *out) listedOverrides(uuid string, states []FeatureState) {
	o.overrides[uuid] = nil
	io := o.overridesFor(uuid)
	io.complete = true
	for _, fs := range states {
		if fs.FeatureStateUUID == "" {
			// Cannot be addressed: know nothing of this identity.
			delete(o.overrides, uuid)
			return
		}
		io.byFeature[fs.Feature.ID] = fs.FeatureStateUUID
	}
}

// forgetOverrides forgets what is known of the identity's overrides.
func (o *out) forgetOverrides(uuid string) { delete(o.overrides, uuid) }

// identityGoneError is a 404 on an identity's override list or create: the
// identity is not (or not yet) in Flagsmith.
type identityGoneError struct{ err error }

func (e identityGoneError) Error() string { return e.err.Error() }
func (e identityGoneError) Unwrap() error { return e.err }

func isNotFound(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

// requests runs the Admin calls of one push: the first goes out at once (the
// caller paced it), every further one waits for the push pacing (UC-055
// BR-001).
type requests struct {
	o *out
	n int
}

func (r *requests) admin(ctx context.Context, method, path string, body, out any) (int, error) {
	if r.n > 0 {
		if err := r.o.pace(ctx); err != nil {
			return 0, err
		}
	}
	r.n++
	return r.o.l.Client().Admin(ctx, method, path, body, out)
}

// overrideBody is the body of an override POST or PUT; the field order is the
// order on the wire.
type overrideBody struct {
	Feature int             `json:"feature"`
	Enabled bool            `json:"enabled"`
	Value   json.RawMessage `json:"feature_state_value"`
}

// sendOverride writes v of featureID as the override on identity identifier,
// or removes the override when v is not present. The identity's uuid is the
// known one, else the exact search finds it, else (a write) it is created.
// A 404 on the known identity (a person deleted it) forgets the uuid and
// starts over once, unless this process listed or settled the identity
// moments before: then the 404 is returned as it is and the caller retries
// (#2639).
func (o *out) sendOverride(ctx context.Context, identifier string, v Value, fv FSValue, featureID int) (int, error) {
	l := o.l
	r := &requests{o: o}
	cached := l.identityUUID(identifier)
	uuid := cached
	for {
		if uuid == "" {
			found, status, err := o.findIdentity(ctx, r, identifier, v.Present)
			if err != nil || found == "" {
				// Not found, nothing to delete; or a refusal.
				return status, err
			}
			uuid = found
		}
		status, err := o.writeOverride(ctx, r, uuid, v, fv, featureID)
		var gone identityGoneError
		if !errors.As(err, &gone) {
			return status, err
		}
		recent := o.listedRecently(identifier)
		if !recent {
			l.forgetIdentityUUID(identifier)
			o.forgetOverrides(uuid)
		}
		switch {
		case !v.Present:
			// No identity, no override: nothing to delete.
			return status, nil
		case recent || uuid != cached:
			return status, err
		}
		uuid, cached = "", ""
	}
}

// findIdentity finds identifier's uuid with the exact search and, when create
// is set and the search finds none, creates the identity. "" with a nil error:
// the identity does not exist (and create is not set).
func (o *out) findIdentity(ctx context.Context, r *requests, identifier string, create bool) (string, int, error) {
	l := o.l
	var found struct {
		Results []edgeIdentity `json:"results"`
	}
	q := url.QueryEscape(`"` + identifier + `"`)
	status, err := r.admin(ctx, http.MethodGet, l.envPath("edge-identities/")+"?q="+q, nil, &found)
	if err != nil {
		return "", status, err
	}
	for _, it := range found.Results {
		if it.Identifier == identifier && it.IdentityUUID != "" {
			l.setIdentityUUID(identifier, it.IdentityUUID)
			return it.IdentityUUID, status, nil
		}
	}
	if !create {
		return "", status, nil
	}
	var created struct {
		IdentityUUID string `json:"identity_uuid"`
	}
	status, err = r.admin(ctx, http.MethodPost, l.envPath("edge-identities/"), map[string]string{"identifier": identifier}, &created)
	var he *HTTPError
	switch {
	case errors.As(err, &he) && he.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(string(he.Body)), "already exist"):
		// Flagsmith has it, but the search did not show it (yet).
		return "", status, &HTTPError{Status: http.StatusNotFound, JSONDetail: true}
	case err != nil:
		return "", status, err
	case created.IdentityUUID == "":
		return "", status, &HTTPError{Status: http.StatusNotFound, JSONDetail: true}
	}
	l.setIdentityUUID(identifier, created.IdentityUUID)
	o.freshOverrides(created.IdentityUUID)
	return created.IdentityUUID, status, nil
}

// listOverrides lists the identity's overrides into the cache. A 404 is an
// identityGoneError.
func (o *out) listOverrides(ctx context.Context, r *requests, uuid string) (int, error) {
	var raw json.RawMessage
	status, err := r.admin(ctx, http.MethodGet, o.l.envPath("edge-identities/"+url.PathEscape(uuid)+"/edge-featurestates/"), nil, &raw)
	if err != nil {
		if isNotFound(err) {
			return status, identityGoneError{err}
		}
		return status, err
	}
	o.listedOverrides(uuid, overrideList(raw))
	return status, nil
}

// writeOverride writes or removes featureID's override on the identity uuid:
// POST when it has none, else PUT (or DELETE) the override itself. When
// Flagsmith's override is not the one the cache knows (a POST answered
// "already exists", or the override answered 404: a person changed it), the
// overrides are listed again and the write is made once more; the same
// override answering 404 again is returned as it is.
func (o *out) writeOverride(ctx context.Context, r *requests, uuid string, v Value, fv FSValue, featureID int) (int, error) {
	base := o.l.envPath("edge-identities/" + url.PathEscape(uuid) + "/edge-featurestates/")
	status := 0
	retried, listed := false, false
	for {
		fs, known := o.overrideOf(uuid, featureID)
		// A removal never trusts a cached "none": a person may have set the
		// override since (a write finds out by its POST answering "already
		// exists").
		if !known || (!v.Present && fs == "" && !listed) {
			listed = true
			var err error
			if status, err = o.listOverrides(ctx, r, uuid); err != nil {
				return status, err
			}
			fs, _ = o.overrideOf(uuid, featureID)
		}
		var err error
		post := v.Present && fs == ""
		switch {
		case !v.Present && fs == "":
			// No override: nothing to delete.
			return status, nil
		case !v.Present:
			status, err = r.admin(ctx, http.MethodDelete, base+url.PathEscape(fs)+"/", nil, nil)
			if err == nil {
				o.setOverride(uuid, featureID, "")
			}
		case post:
			var created struct {
				FeatureStateUUID string `json:"featurestate_uuid"`
			}
			status, err = r.admin(ctx, http.MethodPost, base, overrideBody{Feature: featureID, Enabled: true, Value: fv.Value}, &created)
			switch {
			case err == nil && created.FeatureStateUUID != "":
				o.setOverride(uuid, featureID, created.FeatureStateUUID)
			case err == nil:
				o.forgetOverrides(uuid)
			}
		default:
			status, err = r.admin(ctx, http.MethodPut, base+url.PathEscape(fs)+"/", overrideBody{Feature: featureID, Enabled: true, Value: fv.Value}, nil)
		}
		if err == nil {
			return status, nil
		}
		var he *HTTPError
		isHTTP := errors.As(err, &he)
		switch {
		case post && isNotFound(err):
			return status, identityGoneError{err}
		case retried || !isHTTP:
			return status, err
		case post && he.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(string(he.Body)), "already exist"):
		case !post && he.Status == http.StatusNotFound:
		default:
			return status, err
		}
		// Flagsmith's override is not the one this process knows: list again.
		retried, listed = true, true
		o.forgetOverrides(uuid)
		if lstatus, lerr := o.listOverrides(ctx, r, uuid); lerr != nil {
			return lstatus, lerr
		}
		if now, _ := o.overrideOf(uuid, featureID); now == fs && fs != "" {
			// The same override answers 404: not a stale cache.
			return status, err
		}
	}
}

// overridePath is the path of one override (the controller removal).
func (l *Link) overridePath(uuid, featurestateUUID string) string {
	return l.envPath("edge-identities/" + url.PathEscape(uuid) + "/edge-featurestates/" + url.PathEscape(featurestateUUID) + "/")
}
