package flagsmithtest

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// The v1 environment feature state listing, filtered by feature
// (GET environments/{env}/featurestates/?feature=<id>, EnvironmentFeatureStateViewSet
// of the Flagsmith API source): the environment default of one feature
// (identity and feature_segment null), not paged further. It shows what the
// __future__ flag API last stored for the feature (MVOptions.Default), so it
// agrees with the PATCHes; a feature it never stored shows Flagsmith's fresh
// state (disabled, no value). SetDefault seeds a state as a person's or an
// API key's change would (the reference step's re-read, #2623). The listing
// without ?feature= is not served.

// SetDefault stores a feature's environment default as if a person or an
// API key had set it (value in the flag API's {"type", "value"} form).
func (o *MVOptions) SetDefault(feature int, d EnvironmentDefault) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.defaults[feature] = d
}

func (f *Fake) registerFeatureStateRoutes() {
	f.defaults.HandleFunc(methodGet+APIPath+"environments/{env}/featurestates/{$}", f.listFeatureStates)
}

func (f *Fake) listFeatureStates(w http.ResponseWriter, r *http.Request) {
	results := []map[string]any{}
	if id, err := strconv.Atoi(r.URL.Query().Get("feature")); err == nil && f.Options.exists(id) {
		results = append(results, f.Options.featureState(id))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"count": len(results), "next": nil, "previous": nil, "results": results})
}

// featureState is a feature's environment default as the v1 serializer shows
// it: the value typed (an integer as a number), the weights by option id.
func (o *MVOptions) featureState(feature int) map[string]any {
	d, _ := o.Default(feature)
	var typed struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	var value any
	if json.Unmarshal(d.Value, &typed) == nil && d.Value != nil {
		value = typed.Value
		switch typed.Type {
		case "integer":
			if n, err := strconv.Atoi(typed.Value); err == nil {
				value = n
			}
		case "boolean":
			value = typed.Value == "true"
		}
	}
	weights := []map[string]any{}
	for _, v := range d.Variants {
		weights = append(weights, map[string]any{"multivariate_feature_option": v.ID, "percentage_allocation": v.Weight})
	}
	return map[string]any{
		"id": 100000 + feature, "feature": feature, "enabled": d.Enabled, "feature_state_value": value,
		"identity": nil, "feature_segment": nil, "live_from": "2026-01-01T00:00:00Z",
		"multivariate_feature_state_values": weights,
	}
}
