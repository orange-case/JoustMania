package flagsmithtest

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// tagPageSize is Flagsmith's page size for the tag list.
const tagPageSize = 10

// WriteJSON writes v as a JSON response with status; shared by the tests so
// follow-ups declare no writeJSON of their own.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// registerAdminDefaults serves working defaults for the Admin endpoints the
// link's start-up uses; Handle overrides any of them.
func (f *Fake) registerAdminDefaults() {
	d := f.defaults
	d.HandleFunc("GET "+APIPath+"environments/{env}/", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]any{
			"id": 1, "name": "test", "api_key": r.PathValue("env"), "project": f.ProjectID,
			"use_v2_feature_versioning":            false,
			"use_identity_overrides_in_local_eval": true,
		})
	})
	d.HandleFunc("GET "+APIPath+"projects/{pid}/features/", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		results := make([]map[string]any, 0, len(f.features))
		for _, feat := range f.features {
			c := map[string]any{}
			for k, v := range feat {
				c[k] = v
			}
			results = append(results, c)
		}
		f.mu.Unlock()
		for _, c := range results {
			id, _ := c["id"].(int)
			opts := f.Options.Of(id)
			if opts == nil {
				opts = []MVOption{}
			}
			c["multivariate_options"], c["type"] = opts, f.Options.FeatureType(id)
		}
		WriteJSON(w, http.StatusOK, map[string]any{"count": len(results), "next": nil, "previous": nil, "results": results})
	})
	d.HandleFunc("POST "+APIPath+"projects/{pid}/features/", func(w http.ResponseWriter, r *http.Request) {
		f.create(w, r, &f.features)
	})
	d.HandleFunc("GET "+APIPath+"projects/{pid}/tags/", f.listTags)
	d.HandleFunc("POST "+APIPath+"projects/{pid}/tags/", func(w http.ResponseWriter, r *http.Request) {
		f.create(w, r, &f.tags)
	})
	d.HandleFunc("GET "+APIPath+"environments/{env}/edge-identities/", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]any{"results": []any{}, "last_evaluated_key": nil})
	})
	d.HandleFunc(methodDelete+APIPath+"environments/{env}/edge-identities/{uuid}/", func(w http.ResponseWriter, r *http.Request) {
		f.Overrides.Forget(r.PathValue("uuid"))
		w.WriteHeader(http.StatusNoContent)
	})
	f.registerOverrideRoutes()
	f.registerOptionRoutes()
}

// hasFeature reports whether id is a feature the fake created.
func (f *Fake) hasFeature(id int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, feat := range f.features {
		if feat["id"] == id {
			return true
		}
	}
	return false
}

// Method prefixes of ServeMux patterns.
const (
	methodGet    = "GET "
	methodPost   = "POST "
	methodPut    = "PUT "
	methodDelete = "DELETE "
)

// registerOverrideRoutes serves the SaaS's identity override routes from
// f.Overrides and answers the upsert by identifier the SaaS does not route
// with Django's HTML 404 (#2682).
func (f *Fake) registerOverrideRoutes() {
	d := f.defaults
	overrides := func(w http.ResponseWriter, r *http.Request) {
		uuid, rest, _ := OverrideRoute(r.URL.Path)
		if !f.Overrides.Serve(w, r, uuid, rest) {
			DjangoNotFound(w, r)
		}
	}
	list := APIPath + "environments/{env}/edge-identities/{uuid}/edge-featurestates/"
	for _, m := range []string{methodGet, methodPost} {
		d.HandleFunc(m+list+"{$}", overrides)
	}
	for _, m := range []string{methodGet, methodPut, methodDelete} {
		d.HandleFunc(m+list+"{fs}/{$}", overrides)
	}
	upsert := APIPath + "environments/{env}/edge-identities-featurestates"
	for _, m := range []string{methodGet, methodPost, methodPut, "PATCH ", methodDelete, "OPTIONS "} {
		d.HandleFunc(m+upsert, DjangoNotFound)
		d.HandleFunc(m+upsert+"/{$}", DjangoNotFound)
	}
}

// create stores the request body (a JSON object) with a new id and answers it
// with 201.
func (f *Fake) create(w http.ResponseWriter, r *http.Request, into *[]map[string]any) {
	var obj map[string]any
	if err := json.NewDecoder(r.Body).Decode(&obj); err != nil || obj == nil {
		WriteJSON(w, http.StatusBadRequest, map[string]any{"detail": "invalid body"})
		return
	}
	f.mu.Lock()
	f.nextID++
	obj["id"] = f.nextID
	*into = append(*into, obj)
	f.mu.Unlock()
	WriteJSON(w, http.StatusCreated, obj)
}

func (f *Fake) listTags(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	f.mu.Lock()
	all := append([]map[string]any{}, f.tags...)
	f.mu.Unlock()
	lo, hi := (page-1)*tagPageSize, page*tagPageSize
	if lo > len(all) {
		lo = len(all)
	}
	if hi > len(all) {
		hi = len(all)
	}
	var next any
	if hi < len(all) {
		next = f.Server.URL + r.URL.Path + "?page=" + strconv.Itoa(page+1)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"count": len(all), "next": next, "previous": nil, "results": all[lo:hi]})
}
