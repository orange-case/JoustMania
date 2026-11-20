package flagsmith_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/state"
)

// Variant options (#2709, architecture note §2.3): the operator picks a
// declared variant as the feature's option at 100 %; a split is refused.

// backendOptionIDs are the option ids of decision.backend in these tests.
var backendOptionIDs = map[string]int{"rules": 11, "claude": 12, "openai": 13, "jev": 14}

// mvState is a feature state of decision.backend with option weights, as the
// edge document carries them (api/util/mappers/engine.py).
func mvState(enabled bool, value string, weights map[string]float64) map[string]any {
	s := fsState("decision.backend", enabled, value)
	s["feature"] = map[string]any{"id": 1, "name": "decision.backend", "type": "MULTIVARIATE"}
	mv := []map[string]any{}
	for _, name := range []string{"rules", "claude", "openai", "jev"} {
		mv = append(mv, map[string]any{
			"id":                          100 + backendOptionIDs[name],
			"multivariate_feature_option": map[string]any{"id": backendOptionIDs[name], "value": name, "key": nil},
			"percentage_allocation":       weights[name],
			"mv_fs_value_uuid":            fmt.Sprintf("00000000-0000-4000-8000-%012d", backendOptionIDs[name]),
		})
	}
	s["multivariate_feature_state_values"] = mv
	return s
}

// UC: UC-054 A6 BR-012
func TestPullAppliesPickedOption(t *testing.T) {
	t.Run("UC-054/A6 BR-012 the option at 100 % is the stack value, not the feature's value", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.online()
		h.setDoc([]map[string]any{mvState(true, "rules", map[string]float64{"jev": 100})})
		h.round()
		if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "jev" {
			t.Fatalf("UC-054/A6 local %+v, want the picked variant jev", v)
		}
	})
	t.Run("UC-054/A6 every option at 0 % keeps the typed value", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.online()
		h.setDoc([]map[string]any{mvState(true, "claude", nil)})
		h.round()
		if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "claude" {
			t.Fatalf("UC-054/A6 local %+v, want the feature's value claude", v)
		}
	})
	t.Run("UC-054/A6 BR-012 a percentage split is refused and marks the feature unhealthy; a pick of the same value then applies", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.online()
		h.setDoc([]map[string]any{mvState(true, "jev", map[string]float64{"jev": 50, "claude": 50})})
		h.round()
		if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Present {
			t.Fatalf("UC-054/A6 a split was applied: %+v", v)
		}
		h.waitFor("an unhealthy post", func() bool { return len(h.healthPosts()) == 1 })
		if p := h.healthPosts()[0]; p["feature"] != "decision.backend" || p["status"] != "unhealthy" ||
			!strings.Contains(string(mustJSON(p["reason"])), "flagsmith_percentage_split") ||
			!strings.Contains(string(mustJSON(p["reason"])), "percentage splits are not applied by JoustMania") {
			t.Fatalf("UC-054/A6 health post %v", p)
		}
		if st := h.link.Status(); st.Refused != 1 {
			t.Fatalf("UC-054/A6 refused %d, want 1", st.Refused)
		}
		// The operator settles on jev, the value the feature showed all along.
		h.setDoc([]map[string]any{mvState(true, "jev", map[string]float64{"jev": 100})})
		h.round()
		if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "jev" {
			t.Fatalf("UC-054/A6 local %+v after the pick, want jev", v)
		}
		h.waitFor("a healthy post", func() bool { return len(h.healthPosts()) == 2 })
	})
	t.Run("UC-054/A6 a single option below 100 % is a split too", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.online()
		h.setDoc([]map[string]any{mvState(true, "rules", map[string]float64{"jev": 30})})
		h.round()
		if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Present {
			t.Fatalf("UC-054/A6 a 30 %% option was applied: %+v", v)
		}
		h.waitFor("an unhealthy post", func() bool { return len(h.healthPosts()) == 1 })
	})
	t.Run("UC-054/A6 a switched-off feature is the baseline whatever its weights", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.online()
		h.setDoc([]map[string]any{mvState(false, "rules", map[string]float64{"jev": 100})})
		h.round()
		if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Present {
			t.Fatalf("UC-054/A6 a switched-off feature was applied: %+v", v)
		}
	})
}

// UC: UC-055 BR-005
func TestStackPushWeightsOptions(t *testing.T) {
	var fid int
	h := newHarness(t, harnessOpts{realOut: true, before: func(h *harness) {
		h.fake.ServeFlagAPI()
		fid = h.fake.AddFeature("decision.backend")
		var known []flagsmith.Option
		for _, name := range []string{"rules", "claude", "openai", "jev"} {
			o := h.fake.Options.Add(fid, name)
			if name != "jev" {
				known = append(known, flagsmith.Option{ID: o.ID, Value: name, Text: true})
			}
		}
		// The start saw three options; a person added jev since.
		ids := h.link.IDs()
		ids.Lock()
		ids.FeatureID["decision.backend"] = fid
		ids.Options["decision.backend"] = known
		ids.Unlock()
	}})
	h.online()
	patch := "/api/__future__/environments/envkey/features/"
	h.put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev"})
	h.waitFor("the pick in Flagsmith", func() bool {
		d, ok := h.fake.Options.Default(fid)
		return ok && d.Enabled
	})
	t.Run("UC-055/BR-005 the running variant is the option at 100 %, every other option 0 %", func(t *testing.T) {
		d, _ := h.fake.Options.Default(fid)
		if !strings.Contains(string(d.Value), `"jev"`) {
			t.Fatalf("UC-055/BR-005 value %s, want jev", d.Value)
		}
		weights := map[int]float64{}
		for _, v := range d.Variants {
			weights[v.ID] = v.Weight
		}
		for _, o := range h.fake.Options.Of(fid) {
			want := 0.0
			if *o.StringValue == "jev" {
				want = 100
			}
			if w, ok := weights[o.ID]; !ok || w != want {
				t.Fatalf("UC-055/BR-005 weights %v, want %s at %v", d.Variants, *o.StringValue, want)
			}
		}
	})
	t.Run("UC-055/BR-005 options changed since the start: listed once and sent again", func(t *testing.T) {
		if n := h.requests(http.MethodPatch, patch); n != 2 {
			t.Fatalf("UC-055/BR-005 %d PATCHes, want 2 (refused, then sent again)", n)
		}
		if n := h.requests(http.MethodGet, flagsmithtest.APIPath+"projects/"); n != 1 {
			t.Fatalf("UC-055/BR-005 %d option listings, want 1", n)
		}
		if st := h.link.Status(); st.Refused != 0 {
			t.Fatalf("UC-055/BR-005 refused %d, want 0", st.Refused)
		}
	})
	t.Run("UC-054/BR-004 the pushed pick reads back as no change", func(t *testing.T) {
		st := mvState(true, "jev", map[string]float64{"jev": 100})
		st["feature"] = map[string]any{"id": fid, "name": "decision.backend", "type": "MULTIVARIATE"}
		h.setDoc([]map[string]any{st})
		h.roundAndSettle()
		if n := h.requests(http.MethodPatch, patch); n != 2 {
			t.Fatalf("UC-054/BR-004 %d PATCHes after the echo, want 2", n)
		}
		if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "jev" || v.Writer != "operator" {
			t.Fatalf("UC-054/BR-004 local %+v, want the operator's jev (no write from Flagsmith)", v)
		}
	})
	t.Run("UC-055/BR-005 a cleared overlay switches the feature off with every option at 0 %", func(t *testing.T) {
		if _, ferr := h.st.Clear(state.ClearRequest{Set: "agent", Flag: "decision.backend", Writer: "operator"}); ferr != nil {
			t.Fatal(ferr)
		}
		h.waitFor("the clear in Flagsmith", func() bool {
			d, _ := h.fake.Options.Default(fid)
			return !d.Enabled
		})
		d, _ := h.fake.Options.Default(fid)
		if len(d.Variants) != 4 {
			t.Fatalf("UC-055/BR-005 variants %v, want all 4", d.Variants)
		}
		for _, v := range d.Variants {
			if v.Weight != 0 {
				t.Fatalf("UC-055/BR-005 weights %v after the clear, want all 0", d.Variants)
			}
		}
	})
}
