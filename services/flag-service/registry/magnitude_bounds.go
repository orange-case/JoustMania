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
	Min float64 `yaml:"min"`
	Max float64 `yaml:"max"`
	// Integer requires whole-number bounds (an int-valued flag).
	Integer bool `yaml:"integer,omitempty"`
	// Neutral, when set, must stay inside the range (a two-way lever). A
	// pointer: 0 is a legal neutral.
	Neutral *float64 `yaml:"neutral,omitempty"`
	// Flag is the interventions flag whose registry range the limit equals.
	Flag string `yaml:"flag"`
}

// validMagnitudeRow checks a row with a range table: no yaml bounds (the table is
// the bound), well-formed limits and a baseline default variant that passes the
// table itself.
func validMagnitudeRow(p *Policy, flag *baseline.Flag, b *baseline.Baseline) error {
	limits := p.MagnitudeLimits
	if limits == nil {
		return nil
	}
	if len(limits) == 0 {
		return fmt.Errorf("magnitude_limits is empty")
	}
	bd := p.Bounds
	if len(bd.Variants) > 0 || bd.Range != nil || bd.Pattern != nil || bd.FreeString || bd.Payload != nil || bd.Shape {
		return fmt.Errorf("a range-table row takes no other bounds")
	}
	for iv, l := range limits {
		switch {
		case math.IsNaN(l.Min) || math.IsNaN(l.Max) || l.Min >= l.Max:
			return fmt.Errorf("magnitude_limits %s: min must be below max", iv)
		case l.Neutral != nil && (*l.Neutral < l.Min || *l.Neutral > l.Max):
			return fmt.Errorf("magnitude_limits %s: neutral outside min to max", iv)
		case b.Flag("interventions", l.Flag) == nil:
			return fmt.Errorf("magnitude_limits %s: flag %q is no interventions flag", iv, l.Flag)
		}
	}
	if msg := checkMagnitudeTable(limits, flag.Variants[flag.DefaultVariant]); msg != "" {
		return fmt.Errorf("default variant fails the range table: %s", msg)
	}
	return nil
}

// checkMagnitudeValue validates a decision.magnitude_bounds write.
func checkMagnitudeValue(p *Policy, value json.RawMessage) *fserr.Error {
	if msg := checkMagnitudeTable(p.MagnitudeLimits, value); msg != "" {
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
		case l.Neutral != nil && (*l.Neutral < lo || *l.Neutral > hi):
			return fmt.Sprintf("%s must contain its neutral %v", iv, *l.Neutral)
		}
	}
	return ""
}
