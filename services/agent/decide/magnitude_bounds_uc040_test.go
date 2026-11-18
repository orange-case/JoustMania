package decide

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// UC: UC-040 BR-045
// magnitude_bounds_uc040_test.go: the operator's decision.magnitude_bounds
// (#2696, FR-098, decide-contract.md §2.1) moves a magnitude range only inside the
// hard limits; a bad entry falls back to the code default for that intervention.

// shippedDefault reads the default variant of decision.magnitude_bounds from a
// shipped agent flag file, as the OpenFeature provider hands it over (a map).
func shippedDefault(t *testing.T, file string) any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "flagd", file))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Flags map[string]struct {
			Variants       map[string]any `json:"variants"`
			DefaultVariant string         `json:"defaultVariant"`
		} `json:"flags"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	f, ok := doc.Flags["decision.magnitude_bounds"]
	if !ok {
		t.Fatalf("%s has no decision.magnitude_bounds", file)
	}
	return f.Variants[f.DefaultVariant]
}

func TestMagnitudeBounds_DefaultIsTodaysTable(t *testing.T) {
	for _, file := range []string{"agent.json", "ci/agent.json"} {
		t.Run("UC-040/BR-045 the baseline default of "+file+" is the code default table", func(t *testing.T) {
			v := shippedDefault(t, file)
			obj, _ := v.(map[string]any)
			if len(obj) != len(MagnitudeInterventions()) {
				t.Fatalf("default names %d interventions, the code table %d", len(obj), len(MagnitudeInterventions()))
			}
			mt := ParseMagnitudeOverride(v)
			if mt.Source() != MagnitudeSourceDefault || len(mt.Invalid) != 0 {
				t.Fatalf("source %s invalid %v, want default and none", mt.Source(), mt.Invalid)
			}
			for _, iv := range MagnitudeInterventions() {
				got, _ := mt.Lookup(iv)
				want, _ := MagnitudeBounds(iv)
				if got != want {
					t.Errorf("%s: %+v, code default %+v", iv, got, want)
				}
			}
		})
	}
	t.Run("UC-040/BR-045 an unreadable flag (nil) and a nil table are the code defaults", func(t *testing.T) {
		if s := ParseMagnitudeOverride(nil).Source(); s != MagnitudeSourceDefault {
			t.Fatalf("nil value: source %s", s)
		}
		var nilTable *MagnitudeTable
		b, _ := nilTable.Lookup("grant_shield")
		if want, _ := MagnitudeBounds("grant_shield"); b != want {
			t.Fatalf("nil table %+v", b)
		}
	})
	t.Run("UC-040/BR-045 every code default range lies inside its hard limit", func(t *testing.T) {
		for _, iv := range MagnitudeInterventions() {
			b, _ := MagnitudeBounds(iv)
			l, ok := HardMagnitudeLimits(iv)
			if !ok || b.Min < l.Min || b.Max > l.Max {
				t.Errorf("%s: default [%v,%v], hard %+v", iv, b.Min, b.Max, l)
			}
		}
		if l, _ := HardMagnitudeLimits("adjust_player_sensitivity"); l.Min != 0.2 || l.Max != 2.0 {
			t.Errorf("adjust_player_sensitivity hard limit %+v, want 0.2 to 2.0 (#2696)", l)
		}
	})
}

func obj(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMagnitudeBounds_Override(t *testing.T) {
	t.Run("UC-040/BR-045 a widened range is applied, the rest keeps its default", func(t *testing.T) {
		mt := ParseMagnitudeOverride(obj(t, `{"adjust_player_sensitivity": {"min": 0.2, "max": 1.8}}`))
		b, _ := mt.Lookup("adjust_player_sensitivity")
		if b.Min != 0.2 || b.Max != 1.8 || b.Default != 1.5 || b.Neutral != 1.0 || !b.HasNeutral {
			t.Fatalf("got %+v", b)
		}
		if mt.Source() != MagnitudeSourceFlag || len(mt.Invalid) != 0 {
			t.Fatalf("source %s invalid %v", mt.Source(), mt.Invalid)
		}
		if g, _ := mt.Lookup("grant_shield"); g.Min != 3 || g.Max != 10 {
			t.Fatalf("grant_shield changed: %+v", g)
		}
		if v, _ := mt.Resolve("adjust_player_sensitivity", 0); v != 0.2 {
			t.Fatalf("score 0 resolves to %v, want 0.2", v)
		}
	})
	t.Run("UC-040/BR-045 a narrowed range clamps its default into it", func(t *testing.T) {
		mt := ParseMagnitudeOverride(obj(t, `{"grant_shield": {"min": 6, "max": 10}}`))
		if b, _ := mt.Lookup("grant_shield"); b.Default != 6 {
			t.Fatalf("default %v, want 6", b.Default)
		}
	})
	t.Run("UC-040/BR-045 a JSON text of the object is read too", func(t *testing.T) {
		mt := ParseMagnitudeOverride(`{"grant_shield": {"min": 4, "max": 8}}`)
		if b, _ := mt.Lookup("grant_shield"); b.Min != 4 || b.Max != 8 {
			t.Fatalf("got %+v", b)
		}
	})
}

func TestMagnitudeBounds_FallbackPerIntervention(t *testing.T) {
	for _, tc := range []struct {
		name, entry string
	}{
		{"below the hard limit", `{"min": 0.1, "max": 1.5}`},
		{"above the hard limit", `{"min": 0.7, "max": 2.5}`},
		{"min above max", `{"min": 1.4, "max": 0.8}`},
		{"min equal to max", `{"min": 1.0, "max": 1.0}`},
		{"leaves out the neutral 1.0", `{"min": 1.1, "max": 1.5}`},
		{"a string bound", `{"min": "0.5", "max": 1.5}`},
		{"a missing max", `{"min": 0.5}`},
		{"an extra field", `{"min": 0.5, "max": 1.5, "default": 1.0}`},
		{"not an object", `[0.5, 1.5]`},
	} {
		t.Run("UC-040/BR-045 "+tc.name+" falls back for that intervention only", func(t *testing.T) {
			mt := ParseMagnitudeOverride(obj(t, `{"adjust_player_sensitivity": `+tc.entry+`, "grant_shield": {"min": 4, "max": 8}}`))
			got, _ := mt.Lookup("adjust_player_sensitivity")
			want, _ := MagnitudeBounds("adjust_player_sensitivity")
			if got != want {
				t.Fatalf("adjust_player_sensitivity %+v, want the code default %+v", got, want)
			}
			if mt.InvalidString() != "adjust_player_sensitivity" {
				t.Fatalf("invalid %q", mt.InvalidString())
			}
			if g, _ := mt.Lookup("grant_shield"); g.Min != 4 || g.Max != 8 {
				t.Fatalf("the valid entry was dropped: %+v", g)
			}
		})
	}
	t.Run("UC-040/BR-045 a fraction on the integer lever falls back", func(t *testing.T) {
		mt := ParseMagnitudeOverride(obj(t, `{"adjust_global_sensitivity": {"min": 0.5, "max": 3}}`))
		if mt.InvalidString() != "adjust_global_sensitivity" {
			t.Fatalf("invalid %q", mt.InvalidString())
		}
	})
	t.Run("UC-040/BR-045 an unknown intervention is named and ignored", func(t *testing.T) {
		mt := ParseMagnitudeOverride(obj(t, `{"eliminate_player": {"min": 0, "max": 1}, "zz": {"min": 0, "max": 1}}`))
		if mt.InvalidString() != "eliminate_player,zz" || mt.Source() != MagnitudeSourceDefault {
			t.Fatalf("invalid %q source %s", mt.InvalidString(), mt.Source())
		}
	})
	t.Run("UC-040/BR-045 a value that is no object keeps every default", func(t *testing.T) {
		mt := ParseMagnitudeOverride("not json")
		if mt.InvalidString() != MagnitudeInvalidValue || mt.Source() != MagnitudeSourceDefault {
			t.Fatalf("invalid %q source %s", mt.InvalidString(), mt.Source())
		}
	})
}

func TestMagnitudeBounds_QuestionText(t *testing.T) {
	st := State{Players: []PlayerState{{Serial: "A", Alive: true}}}
	allowed := []string{"adjust_player_sensitivity"}
	t.Run("UC-040/BR-045 the magnitude question names the request's range", func(t *testing.T) {
		mt := ParseMagnitudeOverride(obj(t, `{"adjust_player_sensitivity": {"min": 0.2, "max": 1.8}}`))
		text := FFAQuestionsWith(st, allowed, mt)[2].Text
		if !strings.Contains(text, "adjust_player_sensitivity: lowest 0.2 (target harder to eliminate), highest 1.8 (target easier to eliminate), in factor") {
			t.Fatalf("text %q", text)
		}
	})
	t.Run("UC-040/BR-045 the default table renders today's questions byte for byte", func(t *testing.T) {
		a := FFAQuestions(st, allowed)
		b := FFAQuestionsWith(st, allowed, ParseMagnitudeOverride(shippedDefault(t, "agent.json")))
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if string(ja) != string(jb) {
			t.Fatalf("questions differ:\n%s\n%s", ja, jb)
		}
	})
}
