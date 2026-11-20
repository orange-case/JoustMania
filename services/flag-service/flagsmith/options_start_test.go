package flagsmith

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
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
	})
	t.Run("UC-055/BR-005 a created feature gets the declared variants at 0 % in every environment", func(t *testing.T) {
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
		if got, want := opts.Values(objectives), []string{"endurance_focused", "balanced_focused", "accelerate_focused", "chaos_focused", "mixed"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("objectives options %v, want the row's bounds.variants %v", got, want)
		}
		if got, want := opts.Values(brief), []string{"off", "unkillable", "protect_weakest"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("decision.operator_brief options %v, want its declared variants %v", got, want)
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
		if len(got) != 4 || got[0].ID != rules.ID || got[3].Value != "jev" || !got[3].Text {
			t.Fatalf("known options %+v", got)
		}
	})
	t.Run("UC-055/BR-005 a second start with options in step sends no option request", func(t *testing.T) {
		posts, deletes := optionRequests(e.admin.f, http.MethodPost), optionRequests(e.admin.f, http.MethodDelete)
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		<-e.s.optionsDone()
		if p, d := optionRequests(e.admin.f, http.MethodPost), optionRequests(e.admin.f, http.MethodDelete); p != posts || d != deletes {
			t.Fatalf("second start: %d creates and %d deletes, want none", p-posts, d-deletes)
		}
	})
}
