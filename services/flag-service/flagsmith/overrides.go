package flagsmith

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
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

// Path parts of the edge identity routes.
const (
	edgeIdentitiesPath = "edge-identities/"
	edgeOverridesPath  = "/edge-featurestates/"
)

// identityPath is an edge identity's path below the environment.
func (l *Link) identityPath(uuid string) string {
	return l.envPath(edgeIdentitiesPath + url.PathEscape(uuid) + "/")
}

// overridesPath is the override list of an edge identity.
func (l *Link) overridesPath(uuid string) string {
	return l.envPath(edgeIdentitiesPath + url.PathEscape(uuid) + edgeOverridesPath)
}

// overridePath is one override of an edge identity, by its featurestate_uuid.
func (l *Link) overridePath(uuid, featurestateUUID string) string {
	return l.overridesPath(uuid) + url.PathEscape(featurestateUUID) + "/"
}

func isNotFound(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

// notShownYet is the answer for an identity Flagsmith has but does not show
// (its create answered "already exists" while the exact search finds
// nothing): a 404, so the caller's #2639 retry searches again.
func notShownYet() (int, error) {
	return http.StatusNotFound, &HTTPError{Status: http.StatusNotFound, JSONDetail: true}
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

// searchIdentity finds identifier's uuid with the exact search (the quoted
// query); "" when Flagsmith shows none.
func (o *out) searchIdentity(ctx context.Context, admin func(context.Context, string, string, any, any) (int, error), identifier string) (string, int, error) {
	var found struct {
		Results []edgeIdentity `json:"results"`
	}
	q := url.QueryEscape(`"` + identifier + `"`)
	status, err := admin(ctx, http.MethodGet, o.l.envPath(edgeIdentitiesPath)+"?q="+q, nil, &found)
	if err != nil {
		return "", status, err
	}
	for _, it := range found.Results {
		if it.Identifier == identifier && it.IdentityUUID != "" {
			return it.IdentityUUID, status, nil
		}
	}
	return "", status, nil
}

// findIdentity finds identifier's uuid with the exact search and, when create
// is set and the search finds none, creates the identity. "" with a nil error:
// the identity does not exist (and create is not set). A create, or one
// answered "already exists" while the search does not show the identity yet,
// counts as a listing (markListed): a 404 right after is retried (#2639).
func (o *out) findIdentity(ctx context.Context, r *requests, identifier string, create bool) (string, int, error) {
	l := o.l
	uuid, status, err := o.searchIdentity(ctx, r.admin, identifier)
	switch {
	case err != nil:
		return "", status, err
	case uuid != "":
		l.setIdentityUUID(identifier, uuid)
		return uuid, status, nil
	case !create:
		return "", status, nil
	}
	var created struct {
		IdentityUUID string `json:"identity_uuid"`
	}
	status, err = r.admin(ctx, http.MethodPost, l.envPath(edgeIdentitiesPath), map[string]string{"identifier": identifier}, &created)
	switch {
	case alreadyExists(err), err == nil && created.IdentityUUID == "":
		// Flagsmith has it, but the search does not show it yet.
		o.markListed(identifier)
		status, err = notShownYet()
		return "", status, err
	case err != nil:
		return "", status, err
	}
	o.markListed(identifier)
	l.setIdentityUUID(identifier, created.IdentityUUID)
	o.freshOverrides(created.IdentityUUID)
	return created.IdentityUUID, status, nil
}

// listOverrides lists the identity's overrides into the cache. A 404 is an
// identityGoneError.
func (o *out) listOverrides(ctx context.Context, r *requests, uuid string) (int, error) {
	var raw json.RawMessage
	status, err := r.admin(ctx, http.MethodGet, o.l.overridesPath(uuid), nil, &raw)
	if err != nil {
		if isNotFound(err) {
			return status, identityGoneError{err}
		}
		return status, err
	}
	o.listedOverrides(uuid, overrideList(raw))
	return status, nil
}

// currentOverride is the featurestate uuid of featureID's override on the
// identity ("" = none), listing the overrides when it is not known. A removal
// never trusts a cached "none": a person may have set the override since (a
// write finds out by its POST answering "already exists").
func (o *out) currentOverride(ctx context.Context, r *requests, uuid string, featureID int, removal bool) (string, int, error) {
	if fs, known := o.overrideOf(uuid, featureID); known && (fs != "" || !removal) {
		return fs, 0, nil
	}
	status, err := o.listOverrides(ctx, r, uuid)
	if err != nil {
		return "", status, err
	}
	fs, _ := o.overrideOf(uuid, featureID)
	return fs, status, nil
}

// writeOverride writes or removes featureID's override on the identity uuid:
// POST when it has none, else PUT (or DELETE) the override itself. When
// Flagsmith's override is not the one the cache knows (a POST answered
// "already exists", or the override answered 404: a person changed it), the
// overrides are listed again and the write is made once more; the same
// override answering 404 again is returned as it is.
func (o *out) writeOverride(ctx context.Context, r *requests, uuid string, v Value, fv FSValue, featureID int) (int, error) {
	fs, status, err := o.currentOverride(ctx, r, uuid, featureID, !v.Present)
	if err != nil {
		return status, err
	}
	status, err = o.writeOnce(ctx, r, uuid, v, fv, featureID, fs, status)
	if !staleOverride(v.Present && fs == "", err) {
		return status, err
	}
	// Flagsmith's override is not the one this process knows: list again.
	o.forgetOverrides(uuid)
	if lstatus, lerr := o.listOverrides(ctx, r, uuid); lerr != nil {
		return lstatus, lerr
	}
	now, _ := o.overrideOf(uuid, featureID)
	if now == fs && fs != "" {
		// The same override answers 404: not a stale cache.
		return status, err
	}
	return o.writeOnce(ctx, r, uuid, v, fv, featureID, now, status)
}

// staleOverride: the answer says the cache does not match Flagsmith (a POST
// answered "already exists", or the override itself answered 404).
func staleOverride(post bool, err error) bool {
	if post {
		return alreadyExists(err)
	}
	var gone identityGoneError
	return isNotFound(err) && !errors.As(err, &gone)
}

// writeOnce sends one write for featureID's override fs ("" = none) and keeps
// the cache in step: DELETE (nothing to do without an override), POST (a 404:
// the identity is gone) or PUT. status is the answer of the request before,
// returned when nothing is sent.
func (o *out) writeOnce(ctx context.Context, r *requests, uuid string, v Value, fv FSValue, featureID int, fs string, status int) (int, error) {
	l := o.l
	body := overrideBody{Feature: featureID, Enabled: true, Value: fv.Value}
	switch {
	case !v.Present && fs == "":
		return status, nil
	case !v.Present:
		status, err := r.admin(ctx, http.MethodDelete, l.overridePath(uuid, fs), nil, nil)
		if err == nil {
			o.setOverride(uuid, featureID, "")
		}
		return status, err
	case fs == "":
		var created struct {
			FeatureStateUUID string `json:"featurestate_uuid"`
		}
		status, err := r.admin(ctx, http.MethodPost, l.overridesPath(uuid), body, &created)
		switch {
		case isNotFound(err):
			return status, identityGoneError{err}
		case err == nil && created.FeatureStateUUID != "":
			o.setOverride(uuid, featureID, created.FeatureStateUUID)
		case err == nil:
			o.forgetOverrides(uuid)
		}
		return status, err
	}
	return r.admin(ctx, http.MethodPut, l.overridePath(uuid, fs), body, nil)
}
