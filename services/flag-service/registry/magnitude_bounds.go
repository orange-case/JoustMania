package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/fserr"
)

// MagnitudeLimit is the hard limit of one intervention's magnitude range in the
// agent flag decision.magnitude_bounds (#2696, UC-040 BR-045, flags.md §13.2). Min
// and Max are the registry range of the interventions flag the intervention
// writes (a test keeps them equal); an operator range must lie inside them.
type MagnitudeLimit struct {
	Min, Max float64
	// Integer requires whole-number bounds (an int-valued flag).
	Integer bool
	// Neutral, when HasNeutral, must stay inside the range (a two-way lever).
	Neutral    float64
	HasNeutral bool
	// Flag is the interventions flag whose registry range the limit equals.
	Flag string
}

// magnitudeLimits are the range tables of the registry ("set/key" -> intervention
// -> limit). A Go table, not a registry.yaml attribute, because released
// flag-service images parse the checkout's registry.yaml strictly and would refuse
// a new key; such an image accepts only the declared variants of the row.
var magnitudeLimits = map[string]map[string]MagnitudeLimit{
	"agent/decision.magnitude_bounds": {
		"adjust_music_tempo":        {Min: 1.0, Max: 1.3, Flag: "music_tempo_override"},
		"adjust_volume":             {Min: 0.3, Max: 1.0, Flag: "volume_override"},
		"adjust_global_sensitivity": {Min: 0, Max: 4, Integer: true, Flag: "global_sensitivity_override"},
		"adjust_player_sensitivity": {Min: 0.2, Max: 2.0, Neutral: 1.0, HasNeutral: true, Flag: "player_sensitivity_factor"},
		"adjust_global_difficulty":  {Min: 0.5, Max: 2.0, Neutral: 1.0, HasNeutral: true, Flag: "global_difficulty_factor"},
		"set_player_handicap":       {Min: 0.5, Max: 2.0, Neutral: 1.0, HasNeutral: true, Flag: "player_handicap_factor"},
		"grant_shield":              {Min: 1, Max: 10, Flag: "shield_seconds"},
		"toughness_boost":           {Min: 0.1, Max: 30, Flag: "toughness_boost_seconds"},
	},
}

// MagnitudeLimits returns the range table of a policy (nil when it has none).
func (p *Policy) MagnitudeLimits() map[string]MagnitudeLimit {
	return magnitudeLimits[p.Set+"/"+p.Key]
}

// validMagnitudeRow checks a row with a range table: no yaml bounds (the table is
// the bound) and a baseline default variant that passes the table itself.
func validMagnitudeRow(p *Policy, flag *baseline.Flag) error {
	limits := p.MagnitudeLimits()
	if limits == nil {
		return nil
	}
	b := p.Bounds
	if len(b.Variants) > 0 || b.Range != nil || b.Pattern != nil || b.FreeString || b.Payload != nil || b.Shape {
		return fmt.Errorf("a range-table row takes no other bounds")
	}
	if msg := checkMagnitudeTable(limits, flag.Variants[flag.DefaultVariant]); msg != "" {
		return fmt.Errorf("default variant fails the range table: %s", msg)
	}
	return nil
}

// checkMagnitudeValue validates a decision.magnitude_bounds write.
func checkMagnitudeValue(p *Policy, value json.RawMessage) *fserr.Error {
	if msg := checkMagnitudeTable(p.MagnitudeLimits(), value); msg != "" {
		return fserr.Unprocessable(fserr.OutOfBounds, "%s: %s", p.Key, msg)
	}
	return nil
}

// checkMagnitudeTable returns why value is not a valid range table, or "": a JSON
// object whose keys are interventions of limits, each exactly {"min": n, "max": n},
// finite, min < max, within the limit, whole for an Integer limit and around the
// neutral where the limit has one. A subset of the interventions is fine.
func checkMagnitudeTable(limits map[string]MagnitudeLimit, value json.RawMessage) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(value, &obj); err != nil || obj == nil {
		return "takes an object of intervention -> {\"min\", \"max\"}"
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, iv := range keys {
		l, ok := limits[iv]
		if !ok {
			return fmt.Sprintf("unknown intervention %q", iv)
		}
		dec := json.NewDecoder(bytes.NewReader(obj[iv]))
		dec.DisallowUnknownFields()
		var e struct {
			Min *float64 `json:"min"`
			Max *float64 `json:"max"`
		}
		if err := dec.Decode(&e); err != nil || e.Min == nil || e.Max == nil {
			return fmt.Sprintf("%s takes exactly {\"min\": number, \"max\": number}", iv)
		}
		lo, hi := *e.Min, *e.Max
		switch {
		case math.IsNaN(lo) || math.IsNaN(hi) || math.IsInf(lo, 0) || math.IsInf(hi, 0):
			return fmt.Sprintf("%s: min and max must be finite", iv)
		case lo >= hi:
			return fmt.Sprintf("%s: min %v must be below max %v", iv, lo, hi)
		case lo < l.Min || hi > l.Max:
			return fmt.Sprintf("%s must be within %v to %v (the hard limit of %s)", iv, l.Min, l.Max, l.Flag)
		case l.Integer && (lo != math.Trunc(lo) || hi != math.Trunc(hi)):
			return fmt.Sprintf("%s takes whole numbers", iv)
		case l.HasNeutral && (l.Neutral < lo || l.Neutral > hi):
			return fmt.Sprintf("%s must contain its neutral %v", iv, l.Neutral)
		}
	}
	return ""
}

// magnitudeLimitNames lists the range-table rows ("set/key"), sorted (tests).
func magnitudeLimitNames() []string {
	out := make([]string, 0, len(magnitudeLimits))
	for k := range magnitudeLimits {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
