package flagsmithtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// The identity override routes of the Flagsmith SaaS (#2682, verified live on
// 2026-11-18; upstream api/environments/urls.py edge_identity_router and
// api/edge_api/identities EdgeIdentityFeatureStateViewSet):
//
//	GET, POST          environments/{env}/edge-identities/{uuid}/edge-featurestates/
//	GET, PUT, DELETE   environments/{env}/edge-identities/{uuid}/edge-featurestates/{featurestate_uuid}/
//
// A POST for a feature the identity overrides already is 400
// ["Feature state already exists."]; an unknown override is 404
// {"detail": "Not found."}. The SaaS has no
// environments/{env}/edge-identities-featurestates route: it answers Django's
// HTML 404 (DjangoNotFound).

// Override is one identity override the fake holds.
type Override struct {
	FeatureStateUUID string
	Feature          int
	Enabled          bool
	Value            json.RawMessage
}

// Overrides is a store of identity overrides by identity uuid, served like
// the SaaS serves them. Safe for concurrent use.
type Overrides struct {
	// Exists, when set, tells whether an identity uuid is in Flagsmith; the
	// override routes of any other uuid answer 404 (nil: every uuid exists).
	// Set it before the first request.
	Exists func(identityUUID string) bool

	mu   sync.Mutex
	next int
	by   map[string][]*Override
}

// NewOverrides returns an empty store.
func NewOverrides() *Overrides { return &Overrides{by: map[string][]*Override{}} }

// List returns the overrides of an identity.
func (s *Overrides) List(identityUUID string) []Override {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Override, 0, len(s.by[identityUUID]))
	for _, o := range s.by[identityUUID] {
		out = append(out, *o)
	}
	return out
}

// Get returns the override of feature on an identity.
func (s *Overrides) Get(identityUUID string, feature int) (Override, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.findLocked(identityUUID, feature); o != nil {
		return *o, true
	}
	return Override{}, false
}

// Set sets feature's override on an identity (a person in the dashboard);
// value nil removes it.
func (s *Overrides) Set(identityUUID string, feature int, value json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value == nil {
		s.removeLocked(identityUUID, feature)
		return
	}
	if o := s.findLocked(identityUUID, feature); o != nil {
		o.Value = value
		return
	}
	s.addLocked(identityUUID, feature, true, value)
}

// Forget removes every override of an identity (the identity was deleted).
func (s *Overrides) Forget(identityUUID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.by, identityUUID)
}

func (s *Overrides) findLocked(identityUUID string, feature int) *Override {
	for _, o := range s.by[identityUUID] {
		if o.Feature == feature {
			return o
		}
	}
	return nil
}

func (s *Overrides) removeLocked(identityUUID string, feature int) {
	list := s.by[identityUUID]
	for i, o := range list {
		if o.Feature == feature {
			s.by[identityUUID] = append(list[:i], list[i+1:]...)
			return
		}
	}
}

func (s *Overrides) addLocked(identityUUID string, feature int, enabled bool, value json.RawMessage) *Override {
	s.next++
	o := &Override{FeatureStateUUID: fmt.Sprintf("fs-%s-%d", identityUUID, s.next), Feature: feature, Enabled: enabled, Value: value}
	s.by[identityUUID] = append(s.by[identityUUID], o)
	return o
}

func (o *Override) answer(identityUUID string) map[string]any {
	return map[string]any{
		"feature": o.Feature, "enabled": o.Enabled, "feature_state_value": o.Value,
		"featurestate_uuid": o.FeatureStateUUID, "identity_uuid": identityUUID,
		"multivariate_feature_state_values": []any{},
	}
}

// Serve answers an override route of identity identityUUID; rest is the path
// after "edge-identities/{uuid}/". It reports false when rest is no override
// route (the caller answers it). An identity Exists rejects answers 404, as
// the SaaS does for an unknown or deleted identity.
func (s *Overrides) Serve(w http.ResponseWriter, r *http.Request, identityUUID, rest string) bool {
	sub, ok := strings.CutPrefix(rest, "edge-featurestates/")
	if !ok {
		return false
	}
	fs, item := strings.CutSuffix(sub, "/")
	if sub != "" && (!item || strings.Contains(fs, "/")) {
		return false
	}
	if s.Exists != nil && !s.Exists(identityUUID) {
		writeNotFound(w)
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sub == "" {
		s.serveListLocked(w, r, identityUUID)
	} else {
		s.serveItemLocked(w, r, identityUUID, fs)
	}
	return true
}

func writeNotFound(w http.ResponseWriter) {
	WriteJSON(w, http.StatusNotFound, map[string]string{"detail": "Not found."})
}

func writeNotAllowed(w http.ResponseWriter) {
	WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "Method not allowed."})
}

// serveListLocked answers GET and POST on the override list.
func (s *Overrides) serveListLocked(w http.ResponseWriter, r *http.Request, identityUUID string) {
	switch r.Method {
	case http.MethodGet:
		out := []map[string]any{}
		for _, o := range s.by[identityUUID] {
			out = append(out, o.answer(identityUUID))
		}
		WriteJSON(w, http.StatusOK, out)
	case http.MethodPost:
		b, ok := decodeOverride(w, r)
		if !ok {
			return
		}
		if s.findLocked(identityUUID, b.Feature) != nil {
			WriteJSON(w, http.StatusBadRequest, []string{"Feature state already exists."})
			return
		}
		WriteJSON(w, http.StatusCreated, s.addLocked(identityUUID, b.Feature, b.Enabled, b.Value).answer(identityUUID))
	default:
		writeNotAllowed(w)
	}
}

// serveItemLocked answers GET, PUT and DELETE on one override.
func (s *Overrides) serveItemLocked(w http.ResponseWriter, r *http.Request, identityUUID, fs string) {
	var found *Override
	for _, o := range s.by[identityUUID] {
		if o.FeatureStateUUID == fs {
			found = o
		}
	}
	if found == nil {
		writeNotFound(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		WriteJSON(w, http.StatusOK, found.answer(identityUUID))
	case http.MethodPut:
		if b, ok := decodeOverride(w, r); ok {
			found.Enabled, found.Value = b.Enabled, b.Value
			WriteJSON(w, http.StatusOK, found.answer(identityUUID))
		}
	case http.MethodDelete:
		s.removeLocked(identityUUID, found.Feature)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeNotAllowed(w)
	}
}

type overrideBody struct {
	Feature int             `json:"feature"`
	Enabled bool            `json:"enabled"`
	Value   json.RawMessage `json:"feature_state_value"`
}

func decodeOverride(w http.ResponseWriter, r *http.Request) (overrideBody, bool) {
	var b overrideBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Feature == 0 {
		WriteJSON(w, http.StatusBadRequest, map[string][]string{"feature": {"This field is required."}})
		return b, false
	}
	return b, true
}

// DjangoNotFound answers like the SaaS answers a path it does not route:
// Django's HTML 404, not Flagsmith's JSON {"detail": …}.
func DjangoNotFound(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("<!doctype html>\n<html lang=\"en\">\n<head>\n  <title>Not Found</title>\n</head>\n<body>\n  <h1>Not Found</h1><p>The requested resource was not found on this server.</p>\n</body>\n</html>\n"))
}

// OverrideRoute splits an Admin path into the identity uuid and the rest
// after "edge-identities/{uuid}/" (ok false: no such path).
func OverrideRoute(path string) (identityUUID, rest string, ok bool) {
	_, after, found := strings.Cut(path, "/edge-identities/")
	if !found {
		return "", "", false
	}
	identityUUID, rest, found = strings.Cut(after, "/")
	if !found || identityUUID == "" || !strings.HasPrefix(rest, "edge-featurestates/") {
		return "", "", false
	}
	return identityUUID, rest, true
}
