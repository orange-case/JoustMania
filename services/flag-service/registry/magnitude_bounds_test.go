package registry

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/fserr"
)

func baselineFlag(t *testing.T, dir, set, key string) *baseline.Flag {
	t.Helper()
	b, err := baseline.Load(filepath.Join(root(t), "..", "flagd", dir), baseline.DefaultFiles)
	if err != nil {
		t.Fatal(err)
	}
	f := b.Flag(set, key)
	if f == nil {
		t.Fatalf("no %s/%s in services/flagd/%s", set, key, dir)
	}
	return f
}

// UC: UC-040 BR-045
// magnitude_bounds_test.go: the flag service validates agent/decision.magnitude_bounds
// against its range table (#2696, FR-098, flags.md §13.2), and the per-player
// sensitivity factor takes 0.2 to 2.0.

func magnitudeRow(t *testing.T) (*Registry, *Policy) {
	t.Helper()
	reg := loadReal(t, "ci")
	p := reg.Get("agent", "decision.magnitude_bounds")
	if p == nil || p.MagnitudeLimits == nil {
		t.Fatal("agent/decision.magnitude_bounds has no registry row or no range table")
	}
	return reg, p
}

func TestMagnitudeBounds_Check(t *testing.T) {
	for _, dir := range []string{"", "ci"} {
		reg := loadReal(t, dir)
		p := reg.Get("agent", "decision.magnitude_bounds")
		flag := baselineFlag(t, dir, "agent", "decision.magnitude_bounds")
		for _, tc := range []struct {
			name, value, wantErr string
		}{
			{"the default table selects the default variant", "", ""},
			{"a widened per-player sensitivity range down to 0.2", `{"adjust_player_sensitivity": {"min": 0.2, "max": 1.8}}`, ""},
			{"a narrowed shield range", `{"grant_shield": {"min": 4, "max": 8}}`, ""},
			{"below the hard limit", `{"adjust_player_sensitivity": {"min": 0.1, "max": 1.5}}`, fserr.OutOfBounds},
			{"above the hard limit", `{"grant_shield": {"min": 3, "max": 12}}`, fserr.OutOfBounds},
			{"min above max", `{"grant_shield": {"min": 8, "max": 4}}`, fserr.OutOfBounds},
			{"min equal to max", `{"grant_shield": {"min": 5, "max": 5}}`, fserr.OutOfBounds},
			{"an unknown intervention", `{"eliminate_player": {"min": 0, "max": 1}}`, fserr.OutOfBounds},
			{"an extra field", `{"grant_shield": {"min": 3, "max": 8, "default": 5}}`, fserr.OutOfBounds},
			{"a missing bound", `{"grant_shield": {"min": 3}}`, fserr.OutOfBounds},
			{"a string bound", `{"grant_shield": {"min": "3", "max": 8}}`, fserr.OutOfBounds},
			{"a fraction on the integer lever", `{"adjust_global_sensitivity": {"min": 0.5, "max": 3}}`, fserr.OutOfBounds},
			{"a two-way range without the neutral", `{"set_player_handicap": {"min": 1.1, "max": 1.75}}`, fserr.OutOfBounds},
			{"not an object", `"wide"`, fserr.OutOfBounds},
		} {
			t.Run("UC-040/BR-045 "+dir+" "+tc.name, func(t *testing.T) {
				value := json.RawMessage(tc.value)
				if tc.value == "" {
					value = flag.Variants[flag.DefaultVariant]
				}
				got, err := CheckValue(p, flag, "", value)
				if tc.wantErr == "" {
					if err != nil {
						t.Fatalf("refused: %v", err)
					}
					if tc.value == "" && got.Variant != flag.DefaultVariant {
						t.Fatalf("variant %q", got.Variant)
					}
					return
				}
				if err == nil || err.Reason != tc.wantErr {
					t.Fatalf("err %v, want %s", err, tc.wantErr)
				}
			})
		}
	}
}

func TestMagnitudeBounds_LimitsMatchTheInterventionsRows(t *testing.T) {
	reg, p := magnitudeRow(t)
	for iv, l := range p.MagnitudeLimits {
		t.Run("UC-040/BR-045 "+iv, func(t *testing.T) {
			row := reg.Get("interventions", l.Flag)
			if row == nil {
				t.Fatalf("no interventions row %s", l.Flag)
			}
			lo, hi, integer := row.Bounds.Range, row.Bounds.Range, row.Bounds.Integer
			if row.Bounds.Range == nil {
				// toughness_boost_seconds: "float lo hi" first segment of its pattern.
				f := strings.Fields(row.Bounds.Pattern[0][0])
				var a, b float64
				_ = json.Unmarshal([]byte(f[1]), &a)
				_ = json.Unmarshal([]byte(f[2]), &b)
				lo, hi = []float64{a, b}, []float64{a, b}
			}
			if l.Min != lo[0] || l.Max != hi[1] || l.Integer != integer {
				t.Fatalf("limit %+v, %s registry [%v, %v] integer %v", l, l.Flag, lo[0], hi[1], integer)
			}
		})
	}
	var names []string
	for _, p := range reg.Policies() {
		if p.MagnitudeLimits != nil {
			names = append(names, id(p.Set, p.Key))
		}
	}
	if len(names) != 1 || names[0] != "agent/decision.magnitude_bounds" {
		t.Fatalf("range tables %v", names)
	}
}

// UC: UC-040 BR-045
func TestParseRejectsBadMagnitudeLimits(t *testing.T) {
	b, err := baseline.Load(filepath.Join(root(t), "..", "flagd", "ci"), baseline.DefaultFiles)
	if err != nil {
		t.Fatal(err)
	}
	// The default variant names every intervention, so each case keeps the
	// others and replaces grant_shield or set_player_handicap only.
	const others = `adjust_music_tempo: {min: 1.0, max: 1.3, flag: music_tempo_override}, adjust_volume: {min: 0.3, max: 1.0, flag: volume_override}, adjust_global_sensitivity: {min: 0, max: 4, integer: true, flag: global_sensitivity_override}, adjust_player_sensitivity: {min: 0.2, max: 2.0, neutral: 1.0, flag: player_sensitivity_factor}, adjust_global_difficulty: {min: 0.5, max: 2.0, neutral: 1.0, flag: global_difficulty_factor}, toughness_boost: {min: 0.1, max: 30, flag: toughness_boost_seconds}`
	const handicap = `set_player_handicap: {min: 0.5, max: 2.0, neutral: 1.0, flag: player_handicap_factor}`
	const shield = `grant_shield: {min: 1, max: 10, flag: shield_seconds}`
	row := func(limits string) string {
		return `flags: [{set: agent, key: decision.magnitude_bounds, kind: overlay, scope: [stack], magnitude_limits: {` + others + `, ` + limits + `}}]`
	}
	t.Run("UC-040/BR-045 the full table loads", func(t *testing.T) {
		if _, err := Parse([]byte(row(handicap+", "+shield)), b); err != nil {
			t.Fatal(err)
		}
	})
	for _, tc := range []struct{ name, limits, want string }{
		{"min not below max", handicap + `, grant_shield: {min: 10, max: 1, flag: shield_seconds}`, "min must be below max"},
		{"neutral outside the limit", shield + `, set_player_handicap: {min: 0.5, max: 2.0, neutral: 3, flag: player_handicap_factor}`, "neutral outside"},
		{"no interventions flag", handicap + `, grant_shield: {min: 1, max: 10, flag: no_such_flag}`, "no interventions flag"},
		{"the default variant fails the table", handicap + `, grant_shield: {min: 1, max: 5, flag: shield_seconds}`, "default variant fails"},
	} {
		t.Run("UC-040/BR-045 "+tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(row(tc.limits)), b); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
	t.Run("UC-040/BR-045 a misspelt limit key is refused by the strict decode", func(t *testing.T) {
		if _, err := decodeRegistry([]byte(row(handicap+`, grant_shield: {min: 1, max: 10, flag: shield_seconds, hasneutral: true}`)), true); err == nil {
			t.Fatal("accepted")
		}
	})
}

func TestPlayerSensitivityFactorFloor(t *testing.T) {
	reg := loadReal(t, "ci")
	p := reg.Get("interventions", "player_sensitivity_factor")
	flag := baselineFlag(t, "ci", "interventions", "player_sensitivity_factor")
	t.Run("UC-040/BR-045 0.2 is accepted", func(t *testing.T) {
		if _, err := CheckValue(p, flag, "", json.RawMessage("0.2")); err != nil {
			t.Fatalf("refused 0.2: %v", err)
		}
	})
	t.Run("UC-040/BR-045 0.1 and 0 are refused", func(t *testing.T) {
		for _, v := range []string{"0.1", "0"} {
			if _, err := CheckValue(p, flag, "", json.RawMessage(v)); err == nil || err.Reason != fserr.OutOfBounds {
				t.Fatalf("%s: err %v", v, err)
			}
		}
	})
}
