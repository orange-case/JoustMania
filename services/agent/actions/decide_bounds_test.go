package actions

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/joustmania/agent/decide"
	"github.com/joustmania/agent/decision"
	"github.com/joustmania/agent/flagservice/fake"
)

// boundsSource ties each entry of the decide FFA bounds table (#1225) to the
// writer default and the interventions.json flag it was derived from, so the
// table cannot drift from the code it documents.
//
// The table checked here is the CODE DEFAULT (decide.MagnitudeBounds): the
// interventions.json variants are the admin presets it mirrors. The live range of
// a game comes from the operator flag decision.magnitude_bounds (agent set, #2696,
// UC-040 BR-045), whose baseline default equals this table and which can move a
// range only inside decide.HardMagnitudeLimits (checked against the flag service's
// registry below).
var boundsSource = map[string]struct {
	flag    string
	neutral string
	def     float64
	// sentinel marks a neutral variant that means "no override" (the game's own
	// schedule/default) rather than a writable no-op value.
	sentinel bool
}{
	decision.InterventionAdjustMusicTempo:        {flagMusicTempoOverride, neutralNone, defaultMusicTempo, true},
	decision.InterventionAdjustVolume:            {flagVolumeOverride, neutralNone, defaultVolume, true},
	decision.InterventionAdjustGlobalSensitivity: {flagGlobalSensitivityOverride, neutralNone, defaultGlobalSensitivity, true},
	decision.InterventionAdjustPlayerSensitivity: {flagPlayerSensitivityFactor, neutralDefault, defaultPlayerSensitivity, false},
	decision.InterventionAdjustGlobalDifficulty:  {flagGlobalDifficultyFactor, neutralDefault, defaultGlobalDifficulty, false},
	decision.InterventionSetPlayerHandicap:       {flagPlayerHandicapFactor, neutralDefault, defaultPlayerHandicap, false},
	decision.InterventionGrantShield:             {flagShieldSeconds, neutralNone, defaultShieldSeconds, false},
	decision.InterventionToughnessBoost:          {flagToughnessBoostSeconds, neutralNone, toughnessBoostDefaultSeconds(defaultToughnessBoost), false},
}

// TestDecideBounds_IntegerFlags: int-valued flags round in ResolveMagnitude.
func TestDecideBounds_IntegerFlags(t *testing.T) {
	b, _ := decide.MagnitudeBounds(decision.InterventionAdjustGlobalSensitivity)
	if !b.Integer {
		t.Fatal("adjust_global_sensitivity is an int flag (value_kind=\"int\" in game_coordinator/interventions.py)")
	}
	for _, iv := range decide.MagnitudeInterventions() {
		if b, _ := decide.MagnitudeBounds(iv); b.Integer && iv != decision.InterventionAdjustGlobalSensitivity {
			t.Errorf("%s unexpectedly marked Integer", iv)
		}
	}
}

func toughnessBoostDefaultSeconds(directive string) float64 {
	v, _ := strconv.ParseFloat(strings.SplitN(directive, ":", 2)[0], 64)
	return v
}

// UC: UC-040 BR-045
// TestDecideBounds_HardLimitsMatchTheRegistry: the agent's absolute magnitude
// limits equal the flag service's registry range of the flag each intervention
// writes (#2696), so an operator range the agent accepts is one the flag service
// writes.
func TestDecideBounds_HardLimitsMatchTheRegistry(t *testing.T) {
	policies, err := fake.RegistryPolicies(filepath.Join("..", "..", "flag-service", "registry.yaml"), "interventions")
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	for _, iv := range decide.MagnitudeInterventions() {
		t.Run("UC-040/BR-045 "+iv, func(t *testing.T) {
			l, ok := decide.HardMagnitudeLimits(iv)
			if !ok {
				t.Fatalf("%s has no hard limit", iv)
			}
			p, ok := policies[boundsSource[iv].flag]
			if !ok {
				t.Fatalf("registry has no row for %s", boundsSource[iv].flag)
			}
			if p.Range == nil {
				// toughness_boost_seconds is a pattern ("float 0.1 30", optional boost).
				if iv != decision.InterventionToughnessBoost || l.Min != 0.1 || l.Max != 30 {
					t.Fatalf("%s: hard %+v, registry has no range", iv, l)
				}
				return
			}
			if l.Min != p.Range[0] || l.Max != p.Range[1] || l.Integer != p.Integer {
				t.Fatalf("%s: hard %+v, registry %v integer %v", iv, l, p.Range, p.Integer)
			}
		})
	}
}

func TestDecideBounds_MatchWriterDefaultsAndFlagVariants(t *testing.T) {
	if got, want := len(decide.MagnitudeInterventions()), len(boundsSource); got != want {
		t.Fatalf("decide bounds table has %d entries, source map %d: keep them in sync", got, want)
	}

	// The real flag file (not the testdata copy, which can lag behind it).
	raw, err := os.ReadFile(filepath.Join("..", "..", "flagd", "interventions.json"))
	if err != nil {
		t.Skipf("services/flagd/interventions.json not available: %v", err)
	}
	var doc struct {
		Flags map[string]struct {
			Variants map[string]any `json:"variants"`
		} `json:"flags"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse interventions.json: %v", err)
	}

	for _, iv := range decide.MagnitudeInterventions() {
		src, ok := boundsSource[iv]
		if !ok {
			t.Errorf("%s: bounds entry without a documented source", iv)
			continue
		}
		b, _ := decide.MagnitudeBounds(iv)
		if b.Default != src.def {
			t.Errorf("%s: bounds default %v, writer default %v", iv, b.Default, src.def)
		}
		flag, ok := doc.Flags[src.flag]
		if !ok {
			t.Errorf("%s: flag %q missing from interventions.json", iv, src.flag)
			continue
		}
		lo, hi := math.Inf(1), math.Inf(-1)
		for name, v := range flag.Variants {
			if name == src.neutral {
				continue
			}
			n, ok := variantNumber(v)
			if !ok {
				t.Fatalf("%s: variant %s=%v is not numeric", src.flag, name, v)
			}
			lo, hi = math.Min(lo, n), math.Max(hi, n)
		}
		if b.Min != lo || b.Max != hi {
			t.Errorf("%s: bounds [%v,%v], %s non-neutral variants span [%v,%v]", iv, b.Min, b.Max, src.flag, lo, hi)
		}
		if nv, ok := flag.Variants[src.neutral]; ok {
			n, _ := variantNumber(nv)
			if src.sentinel && b.HasNeutral {
				t.Errorf("%s: HasNeutral set but %s's neutral variant %v is a no-override sentinel", iv, src.flag, nv)
			}
			if !src.sentinel && (!b.HasNeutral || b.Neutral != n) {
				t.Errorf("%s: neutral = %v (has=%v), %s neutral variant = %v", iv, b.Neutral, b.HasNeutral, src.flag, n)
			}
		}
		if b.Default < b.Min || b.Default > b.Max {
			t.Errorf("%s: default %v outside [%v,%v]", iv, b.Default, b.Min, b.Max)
		}
	}
}

// variantNumber reads a numeric variant, or the seconds of a "<seconds>:<boost>"
// toughness_boost directive.
func variantNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		n, err := strconv.ParseFloat(strings.SplitN(x, ":", 2)[0], 64)
		return n, err == nil
	}
	return 0, false
}
