package flagsmith

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/state"
)

// Reference values (FR-227, architecture note §5.2, #2623).

// referencePatches returns the start's __future__ PATCHes by feature id.
func referencePatches(f *flagsmithtest.Fake) map[int]int {
	out := map[int]int{}
	for _, r := range f.RequestsTo(http.MethodPatch, "/api/__future__/") {
		parts := strings.Split(strings.Trim(r.Path, "/"), "/")
		id, _ := strconv.Atoi(parts[len(parts)-1])
		out[id]++
	}
	return out
}

// storedDefault is the value text and enabled the fake's flag API stored.
func storedDefault(t *testing.T, f *flagsmithtest.Fake, id int) (string, bool, []flagsmithtest.Variant) {
	t.Helper()
	d, ok := f.Options.Default(id)
	if !ok {
		t.Fatalf("feature %d: no environment default written", id)
	}
	var v struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(d.Value, &v); err != nil {
		t.Fatal(err)
	}
	return v.Value, d.Enabled, d.Variants
}

func weighted(st map[string]any, optionID int, value string, pct float64) map[string]any {
	st["multivariate_feature_state_values"] = []map[string]any{{
		"multivariate_feature_option": map[string]any{"id": optionID, "value": value},
		"percentage_allocation":       pct,
	}}
	return st
}

// UC: UC-054 A3 BR-002
func TestStartWritesReferenceValues(t *testing.T) {
	e := newStartEnv(t)
	f := e.admin.f
	f.ServeFlagAPI()
	e.admin.features = []fsFeature{
		{ID: 1, Name: "decision.backend"},
		{ID: 2, Name: "experiment_tick_seconds"},
		{ID: 3, Name: "decision.capture_content"},
		{ID: 4, Name: "not_a_registry_flag"},
		{ID: 5, Name: "experiment_dynamic_enabled"},
		{ID: 6, Name: "llm.max_requests_per_minute"},
	}
	on := f.Options.Add(5, "on")
	f.Options.Add(5, "off")
	rpm := e.l.BaselineFlag("agent", "llm.max_requests_per_minute").DefaultVariant
	f.SetDocument(testDocument([]map[string]any{
		fstate("decision.backend", false, nil),                                         // a blank feature
		fstate("experiment_tick_seconds", false, "fast"),                               // an old default, or a person's staged value
		fstate("decision.capture_content", true, "on"),                                 // a stack value Flagsmith holds
		fstate("not_a_registry_flag", false, nil),                                      // not synced
		weighted(fstate("experiment_dynamic_enabled", false, "off"), on.ID, "on", 100), // a pick staged under the off toggle
		fstate("llm.max_requests_per_minute", false, rpm),                              // the reference already
	}, nil), "")
	if err := e.s.Start(e.ctx); err != nil {
		t.Fatal(err)
	}
	patches := referencePatches(f)

	t.Run("UC-054/A3 FR-227 a feature that is off gets the baseline default under enabled=false", func(t *testing.T) {
		for id, want := range map[int]string{1: "rules", 2: "default", 5: "off"} {
			got, enabled, _ := storedDefault(t, f, id)
			if enabled || got != want {
				t.Fatalf("feature %d: enabled=%v value %q, want enabled=false %q (the baseline default)", id, enabled, got, want)
			}
		}
	})
	t.Run("UC-054/A3 FR-227 a pick staged under the off toggle goes back to 0 %", func(t *testing.T) {
		_, _, variants := storedDefault(t, f, 5)
		if len(variants) != 2 {
			t.Fatalf("experiment_dynamic_enabled weights %+v, want both options named", variants)
		}
		for _, v := range variants {
			if v.Weight != 0 {
				t.Fatalf("experiment_dynamic_enabled weights %+v, want 0 %% on every option", variants)
			}
		}
	})
	t.Run("UC-054/A3 FR-227 a feature that is on, one that shows the reference already and one that is no registry flag are not written", func(t *testing.T) {
		for _, id := range []int{3, 4, 6} {
			if patches[id] != 0 {
				t.Fatalf("feature %d written %d times, want never", id, patches[id])
			}
		}
	})
	t.Run("UC-054/A3 FR-227 a feature the start created is written though the document does not show it yet", func(t *testing.T) {
		e.admin.mu.Lock()
		var created []fsFeature
		created = append(created, e.admin.features[6:]...)
		e.admin.mu.Unlock()
		if len(created) == 0 {
			t.Fatal("no feature created")
		}
		for _, c := range created {
			p := e.reg.Get(e.st.Baseline().SetOf(c.Name), c.Name)
			got, enabled, _ := storedDefault(t, f, c.ID)
			if want := e.l.BaselineFlag(p.Set, p.Key).DefaultVariant; enabled || got != want {
				t.Fatalf("%s: enabled=%v %q, want enabled=false %q", c.Name, enabled, got, want)
			}
		}
	})
	t.Run("UC-054/A3 FR-227 the writes are push spans with op reference and leave the link state alone", func(t *testing.T) {
		st := e.l.Status()
		if st.Pending != 0 || st.OutOfSync != 0 || st.Refused != 0 {
			t.Fatalf("status %+v after the reference step, want nothing pending, out of sync or refused", st)
		}
		if _, ok := e.l.links.Remote(Key{Set: "agent", Flag: "decision.backend", Scope: ScopeStack}); ok {
			t.Fatal("the reference write seeded a remote value, want none (the start reconcile reads the document)")
		}
	})
	t.Run("UC-054/A3 FR-227 a second start with the references in place writes nothing", func(t *testing.T) {
		var states []map[string]any
		e.admin.mu.Lock()
		features := append([]fsFeature(nil), e.admin.features...)
		e.admin.mu.Unlock()
		for _, ft := range features {
			d, ok := f.Options.Default(ft.ID)
			if !ok {
				continue
			}
			var v struct {
				Value string `json:"value"`
			}
			_ = json.Unmarshal(d.Value, &v)
			states = append(states, fstate(ft.Name, d.Enabled, v.Value))
		}
		states = append(states, fstate("decision.capture_content", true, "on"))
		f.SetDocument(testDocument(states, nil), "")
		before := len(f.RequestsTo(http.MethodPatch, "/api/__future__/"))
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		if n := len(f.RequestsTo(http.MethodPatch, "/api/__future__/")) - before; n != 0 {
			t.Fatalf("second start wrote %d reference values, want 0 (idempotent)", n)
		}
	})
	t.Run("UC-054/A3 FR-227 a baseline changed by a new release is written at the next start", func(t *testing.T) {
		f.SetDocument(testDocument([]map[string]any{fstate("decision.backend", false, "claude")}, nil), "")
		f.Options.SetDefault(1, flagDefault(false, "string", "claude")) // the old release's reference
		before := referencePatches(f)[1]
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		if got, enabled, _ := storedDefault(t, f, 1); referencePatches(f)[1] != before+1 || enabled || got != "rules" {
			t.Fatalf("decision.backend %q enabled=%v after %d writes, want the new reference rules written once", got, enabled, referencePatches(f)[1]-before)
		}
	})
}

// The decision of #2623: a persisted admin setting's reference is the
// baseline default, not the persisted value (its value is the stack overlay,
// which the start reconcile sends with the toggle on).
//
// UC: UC-054 A3 BR-006
func TestReferenceOfAdminSettingIsTheBaseline(t *testing.T) {
	t.Run("UC-054/A3 BR-006 FR-227 a persisted admin setting's reference is the flag-file default", func(t *testing.T) {
		e := newStartEnv(t)
		f := e.admin.f
		f.ServeFlagAPI()
		sub, _ := e.st.Subscribe("") // an open sync stream, so the write is accepted
		defer sub.Close()
		if _, ferr := e.st.Put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "claude", Writer: "operator", Persist: true}); ferr != nil {
			t.Fatal(ferr)
		}
		e.admin.features = []fsFeature{{ID: 1, Name: "decision.backend"}}
		f.SetDocument(testDocument([]map[string]any{fstate("decision.backend", false, nil)}, nil), "")
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		if got, enabled, _ := storedDefault(t, f, 1); enabled || got != "rules" {
			t.Fatalf("decision.backend reference %q enabled=%v, want the baseline default rules with enabled=false (not the persisted claude)", got, enabled)
		}
	})
}

// UC: UC-054 A3
func TestReferenceStepFailures(t *testing.T) {
	t.Run("UC-054/A3 FR-227 a Flagsmith without the __future__ API gets no reference value and the start goes on", func(t *testing.T) {
		e := newStartEnv(t) // the fake's flag API answers a plain 404
		e.admin.features = []fsFeature{{ID: 1, Name: "decision.backend"}, {ID: 2, Name: "experiment_tick_seconds"}}
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		if n := len(e.admin.f.RequestsTo(http.MethodPatch, "/api/__future__/")); n != 1 {
			t.Fatalf("%d reference writes tried, want 1 (the first plain 404 ends the step)", n)
		}
		if !strings.Contains(e.logs.String(), "reference values are not written") {
			t.Fatalf("log %q, want the INFO that reference values are not written", e.logs.String())
		}
	})
	t.Run("UC-054/A3 FR-227 a refused write is logged and the other features are written", func(t *testing.T) {
		e := newStartEnv(t)
		f := e.admin.f
		f.ServeFlagAPI()
		f.Handle("PATCH /api/__future__/environments/{env}/features/1/{$}", func(w http.ResponseWriter, _ *http.Request) {
			flagsmithtest.WriteJSON(w, http.StatusBadRequest, map[string]any{"environment_default": map[string]any{"value": []string{"invalid"}}})
		})
		e.admin.features = []fsFeature{{ID: 1, Name: "decision.backend"}, {ID: 2, Name: "experiment_tick_seconds"}}
		f.SetDocument(testDocument([]map[string]any{fstate("decision.backend", false, nil), fstate("experiment_tick_seconds", false, nil)}, nil), "")
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatalf("start %v, want it to go on past a refused reference", err)
		}
		if got, enabled, _ := storedDefault(t, f, 2); enabled || got != "default" {
			t.Fatalf("experiment_tick_seconds %q enabled=%v, want the reference written after the refusal", got, enabled)
		}
		if !strings.Contains(e.logs.String(), "flagsmith reference not written") || !strings.Contains(e.logs.String(), "decision.backend") {
			t.Fatalf("log %q, want a WARN naming decision.backend", e.logs.String())
		}
		if st := e.l.Status(); st.Refused != 0 || st.OutOfSync != 0 {
			t.Fatalf("status %+v, want no key refused or out of sync by a reference refusal", st)
		}
	})
	t.Run("UC-054/A3 FR-227 a cancel during the step ends the start", func(t *testing.T) {
		e := newStartEnv(t)
		f := e.admin.f
		ctx, cancel := context.WithCancel(e.ctx)
		defer cancel()
		f.Handle("PATCH /api/__future__/environments/{env}/features/{fid}/{$}", func(w http.ResponseWriter, _ *http.Request) {
			cancel()
			flagsmithtest.WriteJSON(w, http.StatusOK, map[string]any{})
		})
		e.admin.features = []fsFeature{{ID: 1, Name: "decision.backend"}}
		if err := e.s.Start(ctx); err == nil {
			t.Fatal("start succeeded after a cancel during the reference step")
		}
	})
}

// UC: UC-054 A3
func TestNeedsReference(t *testing.T) {
	ref := unicode("rules")
	intRef := FSValue{Type: TypeInt, Value: json.RawMessage("10")}
	cases := []struct {
		name    string
		fs      FeatureState
		inDoc   bool
		created bool
		ref     FSValue
		want    bool
	}{
		{"enabled feature never", FeatureState{Enabled: true, FeatureStateValue: json.RawMessage(`"jev"`)}, true, false, ref, false},
		{"reference shown", FeatureState{FeatureStateValue: json.RawMessage(`"rules"`)}, true, false, ref, false},
		{"integer shown as a string is equal (#2682)", FeatureState{FeatureStateValue: json.RawMessage(`"10"`)}, true, false, intRef, false},
		{"integer shown as a number is equal", FeatureState{FeatureStateValue: json.RawMessage(`10`)}, true, false, intRef, false},
		{"no value", FeatureState{FeatureStateValue: json.RawMessage(`null`)}, true, false, ref, true},
		{"other value", FeatureState{FeatureStateValue: json.RawMessage(`"claude"`)}, true, false, ref, true},
		{"weight above 0", FeatureState{FeatureStateValue: json.RawMessage(`"rules"`), MultivariateValues: []MVStateValue{{PercentageAllocation: 30}}}, true, false, ref, true},
		{"absent, created now", FeatureState{}, false, true, ref, true},
		{"absent, not created", FeatureState{}, false, false, ref, false},
	}
	for _, c := range cases {
		t.Run("UC-054/A3 FR-227 "+c.name, func(t *testing.T) {
			if got := needsReference(c.fs, c.inDoc, c.created, c.ref); got != c.want {
				t.Fatalf("needsReference = %v, want %v", got, c.want)
			}
		})
	}
}
