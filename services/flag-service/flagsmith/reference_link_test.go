package flagsmith_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/joustmania/flag-service/flagsmith"
)

// Reference values end to end (FR-227, architecture note §5.2, #2623): the
// real start sequence, in side and out side. The reference writes are never
// read back as a change, a conflict or an out-of-sync key, by a pull of the
// document from before them or after them.
//
// UC: UC-054 A3 BR-002
func TestReferenceValuesThroughTheLink(t *testing.T) {
	var backend, tick int
	staleDoc := []map[string]any{
		fsState("decision.backend", false, "jev"),
		fsState("experiment_tick_seconds", false, "fast"),
	}
	h := newHarness(t, harnessOpts{
		realStart: true,
		doc:       document(time.Date(2026, 11, 15, 11, 59, 0, 0, time.UTC), staleDoc),
		before: func(hh *harness) {
			hh.fake.ServeFlagAPI()
			backend = hh.fake.AddFeature("decision.backend")
			tick = hh.fake.AddFeature("experiment_tick_seconds")
			// Every other registry row is a feature already and shows in no
			// document: not written (only features the start created are).
			for _, p := range hh.st.Registry().Policies() {
				if p.Key != "decision.backend" && p.Key != "experiment_tick_seconds" {
					hh.fake.AddFeature(p.Key)
				}
			}
		},
	})
	h.online()

	stored := func(id int) (string, bool) {
		d, ok := h.fake.Options.Default(id)
		if !ok {
			return "", true
		}
		var v struct {
			Value string `json:"value"`
		}
		_ = json.Unmarshal(d.Value, &v)
		return v.Value, d.Enabled
	}

	t.Run("UC-054/A3 FR-227 a feature without a stack value shows its baseline default with the toggle off", func(t *testing.T) {
		for id, want := range map[int]string{backend: "rules", tick: "default"} {
			if v, enabled := stored(id); enabled || v != want {
				t.Fatalf("feature %d in Flagsmith = %q enabled=%v, want %q with enabled=false", id, v, enabled, want)
			}
		}
		for _, flag := range []string{"decision.backend", "experiment_tick_seconds"} {
			if v, _ := h.local("agent", flag, flagsmith.ScopeStack); v.Present {
				t.Fatalf("%s local %+v, want no overlay (a reference is never applied)", flag, v)
			}
		}
	})
	t.Run("UC-054/A3 FR-227 later pulls, stale or fresh, apply nothing and report no conflict", func(t *testing.T) {
		h.setDoc(staleDoc) // a copy from before the reference writes
		h.round()
		h.setDoc([]map[string]any{
			fsState("decision.backend", false, "rules"),
			fsState("experiment_tick_seconds", false, "default"),
		})
		h.round()
		for _, flag := range []string{"decision.backend", "experiment_tick_seconds"} {
			if n := len(h.writeSpans(flag)); n != 0 {
				t.Fatalf("%d writes of %s, want none from a reference value", n, flag)
			}
		}
		if n := h.counter(flagsmith.MetricConflicts, "resolution", flagsmith.ResolutionFlagsmith); n != 0 {
			t.Fatalf("%d conflicts, want 0", n)
		}
		if st := h.link.Status(); st.Pending != 0 || st.OutOfSync != 0 || st.Refused != 0 {
			t.Fatalf("status %+v, want nothing pending, out of sync or refused", st)
		}
		if n := h.counter(flagsmith.MetricPushes, "op", flagsmith.OpReference); n != 2 {
			t.Fatalf("%d reference pushes counted, want 2 (one per feature written)", n)
		}
	})
}
