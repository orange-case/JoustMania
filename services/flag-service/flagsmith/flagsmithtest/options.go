package flagsmithtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
)

// Multivariate options and the __future__ flag API with variant weights
// (#2709), as the Flagsmith API source has them (api/features/multivariate/,
// api/features/future/): options are project-level, per feature; creating one
// makes the feature MULTIVARIATE, deleting the last makes it STANDARD; the
// flag API's environment_default.variants must name every option of the
// feature exactly once, with weights between 0 and 100 summing to at most 100.

// MVOption is one multivariate option.
type MVOption struct {
	ID                          int     `json:"id"`
	Feature                     int     `json:"feature"`
	Type                        string  `json:"type"`
	StringValue                 *string `json:"string_value"`
	IntegerValue                *int    `json:"integer_value"`
	BooleanValue                *bool   `json:"boolean_value"`
	DefaultPercentageAllocation float64 `json:"default_percentage_allocation"`
}

// Variant is one weight of the __future__ flag API.
type Variant struct {
	ID     int     `json:"id"`
	Weight float64 `json:"weight"`
}

// EnvironmentDefault is what the __future__ flag API last stored for a feature.
type EnvironmentDefault struct {
	Enabled  bool            `json:"enabled"`
	Value    json.RawMessage `json:"value"`
	Variants []Variant       `json:"variants"`
}

// MVOptions holds the fake's multivariate options by feature id.
type MVOptions struct {
	mu        sync.Mutex
	next      int
	byFeature map[int][]MVOption
	defaults  map[int]EnvironmentDefault
	// Exists reports whether a feature id is a feature of the project (nil:
	// every id is). The fake sets it to its own feature list.
	Exists func(id int) bool
}

// NewMVOptions returns an empty store.
func NewMVOptions() *MVOptions {
	return &MVOptions{next: 5000, byFeature: map[int][]MVOption{}, defaults: map[int]EnvironmentDefault{}}
}

func (o *MVOptions) exists(id int) bool { return o.Exists == nil || o.Exists(id) }

// Add stores a text option on a feature (a person's, or one a test seeds) and
// returns it.
func (o *MVOptions) Add(feature int, value string) MVOption {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.addLocked(MVOption{Feature: feature, Type: "unicode", StringValue: &value})
}

func (o *MVOptions) addLocked(opt MVOption) MVOption {
	o.next++
	opt.ID = o.next
	o.byFeature[opt.Feature] = append(o.byFeature[opt.Feature], opt)
	return opt
}

// Of returns a feature's options in creation order.
func (o *MVOptions) Of(feature int) []MVOption {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]MVOption(nil), o.byFeature[feature]...)
}

// Values returns the text values of a feature's options in creation order.
func (o *MVOptions) Values(feature int) []string {
	var out []string
	for _, opt := range o.Of(feature) {
		if opt.StringValue != nil {
			out = append(out, *opt.StringValue)
		}
	}
	return out
}

// Default returns what the flag API last stored for a feature.
func (o *MVOptions) Default(feature int) (EnvironmentDefault, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	d, ok := o.defaults[feature]
	return d, ok
}

// FeatureType is MULTIVARIATE while a feature has an option, else STANDARD.
func (o *MVOptions) FeatureType(feature int) string {
	if len(o.Of(feature)) > 0 {
		return "MULTIVARIATE"
	}
	return "STANDARD"
}

// AddFeature stores a feature of the project (as a person or an earlier start
// created it) and returns its id.
func (f *Fake) AddFeature(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.features = append(f.features, map[string]any{"id": f.nextID, "name": name, "is_archived": false})
	return f.nextID
}

// registerOptionRoutes serves the mv-options routes of a feature.
func (f *Fake) registerOptionRoutes() {
	d := f.defaults
	base := APIPath + "projects/{pid}/features/{fid}/mv-options/"
	d.HandleFunc(methodGet+base+"{$}", f.listOptions)
	d.HandleFunc(methodPost+base+"{$}", f.createOption)
	d.HandleFunc(methodDelete+base+"{id}/{$}", f.deleteOption)
}

func featureOf(w http.ResponseWriter, r *http.Request, o *MVOptions) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("fid"))
	if err != nil || !o.exists(id) {
		WriteJSON(w, http.StatusNotFound, map[string]any{"detail": "No Feature matches the given query."})
		return 0, false
	}
	return id, true
}

func (f *Fake) listOptions(w http.ResponseWriter, r *http.Request) {
	id, ok := featureOf(w, r, f.Options)
	if !ok {
		return
	}
	opts := f.Options.Of(id)
	if opts == nil {
		opts = []MVOption{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"count": len(opts), "next": nil, "previous": nil, "results": opts})
}

func (f *Fake) createOption(w http.ResponseWriter, r *http.Request) {
	id, ok := featureOf(w, r, f.Options)
	if !ok {
		return
	}
	var body MVOption
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Feature != id {
		WriteJSON(w, http.StatusBadRequest, map[string]any{"feature": []string{"This field is required."}})
		return
	}
	o := f.Options
	o.mu.Lock()
	defer o.mu.Unlock()
	total := body.DefaultPercentageAllocation
	for _, sib := range o.byFeature[id] {
		total += sib.DefaultPercentageAllocation
	}
	if total > 100 {
		WriteJSON(w, http.StatusBadRequest, map[string]any{"default_percentage_allocation": "Invalid percentage allocation"})
		return
	}
	WriteJSON(w, http.StatusCreated, o.addLocked(body))
}

func (f *Fake) deleteOption(w http.ResponseWriter, r *http.Request) {
	id, ok := featureOf(w, r, f.Options)
	if !ok {
		return
	}
	optID, _ := strconv.Atoi(r.PathValue("id"))
	o := f.Options
	o.mu.Lock()
	defer o.mu.Unlock()
	opts := o.byFeature[id]
	for i, opt := range opts {
		if opt.ID == optID {
			o.byFeature[id] = append(opts[:i:i], opts[i+1:]...)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	WriteJSON(w, http.StatusNotFound, map[string]any{"detail": "Not found."})
}

// ServeFlagAPI serves PATCH /api/__future__/environments/{env}/features/{id}/
// with the variant checks of the Flagsmith source (check_variants). Off by
// default: tests of the v1 fallback rely on the route answering 404.
func (f *Fake) ServeFlagAPI() {
	f.defaults.HandleFunc("PATCH /api/__future__/environments/{env}/features/{fid}/{$}", f.patchFlag)
}

func (f *Fake) patchFlag(w http.ResponseWriter, r *http.Request) {
	id, ok := featureOf(w, r, f.Options)
	if !ok {
		return
	}
	var body struct {
		EnvironmentDefault *struct {
			Enabled  bool            `json:"enabled"`
			Value    json.RawMessage `json:"value"`
			Variants *[]Variant      `json:"variants"`
		} `json:"environment_default"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]any{"detail": "invalid body"})
		return
	}
	ed := body.EnvironmentDefault
	if ed == nil {
		WriteJSON(w, http.StatusOK, map[string]any{})
		return
	}
	var variants []Variant
	if ed.Variants != nil {
		if msg := f.checkVariants(id, *ed.Variants); msg != "" {
			WriteJSON(w, http.StatusBadRequest, map[string]any{"environment_default": map[string]any{"variants": []string{msg}}})
			return
		}
		variants = *ed.Variants
	}
	o := f.Options
	o.mu.Lock()
	prev := o.defaults[id]
	if ed.Variants == nil {
		variants = prev.Variants // a PATCH leaves what it does not name
	}
	o.defaults[id] = EnvironmentDefault{Enabled: ed.Enabled, Value: ed.Value, Variants: variants}
	o.mu.Unlock()
	WriteJSON(w, http.StatusOK, map[string]any{})
}

// checkVariants is check_variants of api/features/future/services.py.
func (f *Fake) checkVariants(feature int, variants []Variant) string {
	opts := f.Options.Of(feature)
	if len(opts) == 0 {
		return "Feature is not multivariate."
	}
	known := map[int]bool{}
	for _, o := range opts {
		known[o.ID] = true
	}
	seen := map[int]bool{}
	var unknown []int
	total := 0.0
	for _, v := range variants {
		if seen[v.ID] {
			return fmt.Sprintf("Duplicate variant: %d.", v.ID)
		}
		seen[v.ID] = true
		if !known[v.ID] {
			unknown = append(unknown, v.ID)
		}
		if v.Weight < 0 || v.Weight > 100 {
			return "Ensure this value is less than or equal to 100."
		}
		total += v.Weight
	}
	if len(unknown) > 0 {
		sort.Ints(unknown)
		return fmt.Sprintf("Variant not found: %d.", unknown[0])
	}
	if len(seen) != len(known) {
		return "Must include all feature's variants."
	}
	if total > 100 {
		return fmt.Sprintf("Total weight must not exceed 100, got %g.", total)
	}
	return ""
}
