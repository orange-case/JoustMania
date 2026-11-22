package flagsmith

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/registry"
)

// optionRequests counts the start's mv-options requests of method.
func optionRequests(f *flagsmithtest.Fake, method string) int {
	n := 0
	for _, r := range f.RequestsTo(method, flagsmithtest.APIPath+"projects/") {
		if strings.Contains(r.Path, "/mv-options/") {
			n++
		}
	}
	return n
}

// UC: UC-055 BR-005
func TestStartSyncsVariantOptions(t *testing.T) {
	e := newStartEnv(t)
	opts := e.admin.f.Options
	e.admin.features = []fsFeature{{ID: 1, Name: "decision.backend"}, {ID: 2, Name: "eliminate_player"}, {ID: 3, Name: "not_a_registry_flag"}}
	rules := opts.Add(1, "rules")
	opts.Add(1, "gone")  // a variant the baseline dropped
	opts.Add(1, "rules") // a duplicate
	opts.Add(2, "x")     // a person's option on an event list
	opts.Add(3, "keep")  // not a registry flag: never touched
	if err := e.s.Start(e.ctx); err != nil {
		t.Fatal(err)
	}
	<-e.s.optionsDone()

	t.Run("UC-055/BR-005 an existing feature's options are made equal to the row's variants, in order", func(t *testing.T) {
		if got, want := opts.Values(1), []string{"rules", "claude", "openai", "jev"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("decision.backend options %v, want %v", got, want)
		}
		if opts.Of(1)[0].ID != rules.ID {
			t.Fatal("the existing rules option was replaced, want it kept")
		}
		if got, want := opts.Keys(1), []string{"rules", "claude", "openai", "jev"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("UC-055/BR-005 decision.backend titles %v, want the variant names %v", got, want)
		}
	})
	t.Run("UC-055/BR-005 a created feature gets the declared variants at 0 % in every environment, titled by name with the variant's value", func(t *testing.T) {
		var objectives, brief int
		e.admin.mu.Lock()
		for _, f := range e.admin.features {
			switch f.Name {
			case "objectives":
				objectives = f.ID
			case "decision.operator_brief":
				brief = f.ID
			}
		}
		e.admin.mu.Unlock()
		if got, want := opts.Keys(objectives), []string{"endurance_focused", "balanced_focused", "accelerate_focused", "chaos_focused", "mixed"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("objectives option titles %v, want the row's bounds.variants %v", got, want)
		}
		if got := opts.Values(objectives)[3]; got != `{"endurance":0.1,"balanced":0.1,"accelerate":0.1,"chaos":0.7}` {
			t.Fatalf("UC-055/BR-005 objectives chaos_focused value %s, want its JSON weights", got)
		}
		if got, want := opts.Keys(brief), []string{"off", "unkillable", "protect_weakest"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("decision.operator_brief option titles %v, want its declared variants %v", got, want)
		}
		b := e.l.BaselineFlag("agent", "decision.operator_brief")
		var unkillable string
		if err := json.Unmarshal(b.Variants["unkillable"], &unkillable); err != nil || unkillable == "" {
			t.Fatalf("baseline unkillable %s", b.Variants["unkillable"])
		}
		if got := opts.Values(brief); got[0] != "" || got[1] != unkillable {
			t.Fatalf("UC-055/BR-005 decision.operator_brief values %q, want the brief texts", got)
		}
		for _, o := range opts.Of(objectives) {
			if o.DefaultPercentageAllocation != 0 {
				t.Fatalf("option %s created at %v %%, want 0", *o.StringValue, o.DefaultPercentageAllocation)
			}
		}
	})
	t.Run("UC-055/BR-005 an event list has no options; a feature that is no registry flag is never touched", func(t *testing.T) {
		if got := opts.Values(2); len(got) != 0 {
			t.Fatalf("eliminate_player options %v, want none", got)
		}
		if got := opts.Values(3); !reflect.DeepEqual(got, []string{"keep"}) {
			t.Fatalf("not_a_registry_flag options %v, want untouched", got)
		}
	})
	t.Run("UC-055/BR-005 the link knows the options for its weight writes", func(t *testing.T) {
		got := e.l.options("decision.backend")
		if len(got) != 4 || got[0].ID != rules.ID || got[3].Value != "jev" || !got[3].Text || got[3].Variant != "jev" || got[0].Variant != "rules" {
			t.Fatalf("known options %+v", got)
		}
	})
	t.Run("UC-055/BR-005 a second start with options in step sends no option request", func(t *testing.T) {
		posts, deletes, puts := optionRequests(e.admin.f, http.MethodPost), optionRequests(e.admin.f, http.MethodDelete), optionRequests(e.admin.f, http.MethodPut)
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		<-e.s.optionsDone()
		if p, d, u := optionRequests(e.admin.f, http.MethodPost), optionRequests(e.admin.f, http.MethodDelete), optionRequests(e.admin.f, http.MethodPut); p != posts || d != deletes || u != puts {
			t.Fatalf("second start: %d creates, %d deletes and %d updates, want none", p-posts, d-deletes, u-puts)
		}
	})
}

// UC: UC-055 BR-005
func TestStartUpdatesOptionsInPlace(t *testing.T) {
	e := newStartEnv(t)
	opts := e.admin.f.Options
	e.admin.features = []fsFeature{{ID: 1, Name: "objectives"}, {ID: 2, Name: "decision.backend"}}
	// The options #2711 created: value = the variant name, no title.
	legacy := map[string]int{}
	for _, n := range []string{"endurance_focused", "balanced_focused", "accelerate_focused", "chaos_focused", "mixed"} {
		legacy[n] = opts.Add(1, n).ID
	}
	// decision.backend: a titled option whose value is out of date, a
	// keyless duplicate of the same variant.
	stale := opts.AddKeyed(2, "jev", "old jev value")
	dup := opts.Add(2, "jev")
	if err := e.s.Start(e.ctx); err != nil {
		t.Fatal(err)
	}
	<-e.s.optionsDone()

	t.Run("UC-055/BR-005 #2713 the #2711 options are rewritten in place: same ids, titled, the variant's value", func(t *testing.T) {
		got := opts.Of(1)
		if len(got) != 5 {
			t.Fatalf("objectives has %d options, want 5", len(got))
		}
		for _, o := range got {
			if o.Key == nil || legacy[*o.Key] != o.ID {
				t.Fatalf("option %d key %v: want the #2711 option of its variant kept, ids %v", o.ID, o.Key, legacy)
			}
		}
		if v := *got[3].StringValue; v != `{"endurance":0.1,"balanced":0.1,"accelerate":0.1,"chaos":0.7}` {
			t.Fatalf("chaos_focused value %s", v)
		}
		if n := optionRequests(e.admin.f, http.MethodPut); n < 5 {
			t.Fatalf("%d updates, want at least the 5 objectives options", n)
		}
	})
	t.Run("UC-055/BR-005 #2713 a changed variant value updates the titled option, a keyless duplicate is removed", func(t *testing.T) {
		var jev []flagsmithtest.MVOption
		for _, o := range opts.Of(2) {
			if o.Key != nil && *o.Key == "jev" {
				jev = append(jev, o)
			}
			if o.ID == dup.ID {
				t.Fatal("the keyless duplicate of jev was kept")
			}
		}
		if len(jev) != 1 || jev[0].ID != stale.ID || *jev[0].StringValue != "jev" {
			t.Fatalf("jev options %+v, want option %d updated to the value jev", jev, stale.ID)
		}
	})
	t.Run("UC-055/BR-005 #2713 the link knows each option's variant for its weight writes", func(t *testing.T) {
		for _, o := range e.l.options("objectives") {
			if o.Variant == "" || o.Variant != o.Key || legacy[o.Variant] != o.ID {
				t.Fatalf("known option %+v", o)
			}
		}
	})
}

// UC: UC-055 BR-005
func TestOptionText(t *testing.T) {
	e := newStartEnv(t)
	t.Run("UC-055/BR-005 #2713 object, string, number and boolean variants as option text", func(t *testing.T) {
		for _, c := range []struct{ set, key, variant, want string }{
			{"agent", "objectives", "mixed", `{"endurance":0.25,"balanced":0.25,"accelerate":0.25,"chaos":0.25}`},
			{"agent", "decision.backend", "jev", "jev"},
			{"agent", "decision.operator_brief", "off", ""},
		} {
			p, b := e.l.Policy(c.set, c.key), e.l.BaselineFlag(c.set, c.key)
			if got := OptionText(p, b, c.variant); got != c.want {
				t.Fatalf("%s/%s: %q, want %q", c.key, c.variant, got, c.want)
			}
		}
	})
	t.Run("UC-055/BR-005 #2713 a value longer than Flagsmith's 20000 characters is shown as the name", func(t *testing.T) {
		long, _ := json.Marshal(strings.Repeat("é", optionValueLimit+1))
		fits, _ := json.Marshal(strings.Repeat("é", optionValueLimit))
		b := &baseline.Flag{Variants: map[string]json.RawMessage{"long": long, "fits": fits}, DefaultVariant: "fits"}
		p := &registry.Policy{Set: "agent", Key: "x"}
		p.Bounds.FreeString = true
		if got := OptionText(p, b, "long"); got != "long" {
			t.Fatalf("a too long value: %d characters, want the name", len(got))
		}
		if got := OptionText(p, b, "fits"); got != strings.Repeat("é", optionValueLimit) {
			t.Fatal("a value of exactly the limit was not kept")
		}
	})
}
