package decide

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
)

// magnitude.go: the per-request magnitude bounds (#2696, FR-098, UC-040 BR-045,
// decide-contract.md §2.1). The operator flag decision.magnitude_bounds (agent
// set, human-only, C-034) moves an intervention's Min and Max inside the hard
// limits below; the agent parses it once per decide request into a
// MagnitudeTable and every consumer of that request reads the same table.

// Sources of a request's magnitude ranges (decide.magnitude_bounds.source).
const (
	MagnitudeSourceDefault = "default"
	MagnitudeSourceFlag    = "flag"
)

// MagnitudeInvalidValue names, in MagnitudeTable.Invalid, a flag value that is not
// a JSON object at all (every intervention then keeps its code default).
const MagnitudeInvalidValue = "*"

// Limit is the hard limit of one intervention's magnitude: the range the flag
// service's interventions registry accepts for the flag the intervention writes,
// itself inside the game coordinator's clamp. An operator range must lie inside it.
type Limit struct {
	Min, Max float64
	// Integer requires whole-number bounds (an int-valued flag).
	Integer bool
	// ContainsNeutral requires the range to keep the lever's neutral value (the
	// two-way levers: leaving out "no change" would make one side unreachable).
	ContainsNeutral bool
}

// hardMagnitudeLimits are the absolute limits, kept in code as the safety floor
// (a test checks them against services/flag-service/registry.yaml and the code
// default table). They equal the registry ranges of the interventions flags:
//
//	adjust_music_tempo         music_tempo_override        1.0..1.3 (SLOW/FAST_MUSIC_SPEED)
//	adjust_volume              volume_override             0.3..1.0
//	adjust_global_sensitivity  global_sensitivity_override 0..4 (int)
//	adjust_player_sensitivity  player_sensitivity_factor   0.2..2.0 (floor 0.2 since #2696)
//	adjust_global_difficulty   global_difficulty_factor    0.5..2.0
//	set_player_handicap        player_handicap_factor      0.5..2.0
//	grant_shield               shield_seconds              1..10
//	toughness_boost            toughness_boost_seconds     0.1..30
var hardMagnitudeLimits = map[string]Limit{
	"adjust_music_tempo":        {Min: 1.0, Max: 1.3},
	"adjust_volume":             {Min: 0.3, Max: 1.0},
	"adjust_global_sensitivity": {Min: 0, Max: 4, Integer: true},
	"adjust_player_sensitivity": {Min: 0.2, Max: 2.0, ContainsNeutral: true},
	"adjust_global_difficulty":  {Min: 0.5, Max: 2.0, ContainsNeutral: true},
	"set_player_handicap":       {Min: 0.5, Max: 2.0, ContainsNeutral: true},
	"grant_shield":              {Min: 1, Max: 10},
	"toughness_boost":           {Min: 0.1, Max: 30},
}

// HardMagnitudeLimits returns intervention's hard limit, and false when it takes
// no numeric magnitude.
func HardMagnitudeLimits(intervention string) (Limit, bool) {
	l, ok := hardMagnitudeLimits[intervention]
	return l, ok
}

// MagnitudeTable is the magnitude bounds of one decide request: the code default
// table with the operator's valid decision.magnitude_bounds entries applied. A nil
// *MagnitudeTable is the code default table, so every older caller and test is
// unchanged.
type MagnitudeTable struct {
	bounds map[string]Bounds
	// Invalid are the interventions whose entry fell back to the code default
	// (or MagnitudeInvalidValue for a value that is no object), sorted.
	Invalid []string
}

// Lookup returns intervention's bounds in t, and false when it takes no numeric
// magnitude.
func (t *MagnitudeTable) Lookup(intervention string) (Bounds, bool) {
	if t == nil || t.bounds == nil {
		b, ok := magnitudeBounds[intervention]
		return b, ok
	}
	b, ok := t.bounds[intervention]
	return b, ok
}

// Interventions returns the interventions with numeric bounds, sorted.
func (t *MagnitudeTable) Interventions() []string { return MagnitudeInterventions() }

// Source is MagnitudeSourceFlag when any range differs from the code default
// table, else MagnitudeSourceDefault.
func (t *MagnitudeTable) Source() string {
	if t == nil || t.bounds == nil {
		return MagnitudeSourceDefault
	}
	for iv, def := range magnitudeBounds {
		if t.bounds[iv] != def {
			return MagnitudeSourceFlag
		}
	}
	return MagnitudeSourceDefault
}

// Resolve maps a normalized ffa.magnitude score (0 = Min, 1 = Max) linearly onto
// intervention's bounds in t (see ResolveMagnitude).
func (t *MagnitudeTable) Resolve(intervention string, score float64) (value float64, ok bool) {
	b, ok := t.Lookup(intervention)
	if !ok {
		return 0, false
	}
	if !finite(score) {
		score = 0
	}
	score = clamp01(score)
	v := b.Min + score*(b.Max-b.Min)
	if b.Integer {
		v = math.Round(v)
	} else {
		// Drop float noise from the normalize/resolve round trip (0.7 + 0.25*0.8
		// is 0.8999999999999999), so Decision.Value reads like the flag variant.
		v = math.Round(v*1e9) / 1e9
	}
	return v, true
}

// Normalize is the inverse of Resolve: the normalized score of value within
// intervention's bounds in t (clamped to [0,1]).
func (t *MagnitudeTable) Normalize(intervention string, value float64) (score float64, ok bool) {
	b, ok := t.Lookup(intervention)
	if !ok {
		return 0, false
	}
	if b.Max == b.Min {
		return 0, true
	}
	return clamp01((value - b.Min) / (b.Max - b.Min)), true
}

// Clamp returns value clamped into intervention's range in t (ok false without
// bounds).
func (t *MagnitudeTable) Clamp(intervention string, value float64) (float64, bool) {
	b, ok := t.Lookup(intervention)
	if !ok {
		return value, false
	}
	return math.Max(b.Min, math.Min(b.Max, value)), true
}

// ParseMagnitudeOverride builds a request's MagnitudeTable from a
// decision.magnitude_bounds value (decide-contract.md §2.1): an object of
// intervention -> {"min": x, "max": y}. Each entry is applied only when it is
// exactly those two finite numbers, min < max, both inside the intervention's hard
// limit, whole for an Integer limit and around the neutral for a ContainsNeutral
// limit; otherwise that intervention keeps its code default and is named in
// Invalid. An unknown intervention is named in Invalid and ignored. A nil value
// (an unreadable flag) is the code default table with nothing invalid. The
// Default of an applied range is the code default clamped into it; Neutral,
// Integer and Unit never change.
func ParseMagnitudeOverride(value any) *MagnitudeTable {
	t := &MagnitudeTable{bounds: make(map[string]Bounds, len(magnitudeBounds))}
	for iv, b := range magnitudeBounds {
		t.bounds[iv] = b
	}
	if value == nil {
		return t
	}
	obj, ok := asObject(value)
	if !ok {
		t.Invalid = []string{MagnitudeInvalidValue}
		return t
	}
	for iv, raw := range obj {
		def, known := magnitudeBounds[iv]
		lo, hi, valid := parseRange(raw)
		if !known || !valid || !withinLimit(iv, def, lo, hi) {
			t.Invalid = append(t.Invalid, iv)
			continue
		}
		b := def
		b.Min, b.Max = lo, hi
		b.Default = math.Max(lo, math.Min(hi, def.Default))
		t.bounds[iv] = b
	}
	sort.Strings(t.Invalid)
	return t
}

// InvalidString is Invalid joined with commas ("" when none).
func (t *MagnitudeTable) InvalidString() string {
	if t == nil {
		return ""
	}
	return strings.Join(t.Invalid, ",")
}

func withinLimit(iv string, def Bounds, lo, hi float64) bool {
	l, ok := hardMagnitudeLimits[iv]
	if !ok || lo >= hi || lo < l.Min || hi > l.Max {
		return false
	}
	if l.Integer && (lo != math.Trunc(lo) || hi != math.Trunc(hi)) {
		return false
	}
	if l.ContainsNeutral && def.HasNeutral && (def.Neutral < lo || def.Neutral > hi) {
		return false
	}
	return true
}

// asObject reads a flag value as a JSON object: a map (what the OpenFeature
// providers return for an object flag) or a JSON text of one.
func asObject(value any) (map[string]any, bool) {
	switch v := value.(type) {
	case map[string]any:
		return v, true
	case string:
		var m map[string]any
		if err := json.Unmarshal([]byte(v), &m); err != nil || m == nil {
			return nil, false
		}
		return m, true
	case []byte:
		return asObject(string(v))
	}
	return nil, false
}

// parseRange reads one entry: exactly the keys min and max, both finite numbers.
func parseRange(raw any) (lo, hi float64, ok bool) {
	m, isObj := raw.(map[string]any)
	if !isObj || len(m) != 2 {
		return 0, 0, false
	}
	lo, okLo := number(m["min"])
	hi, okHi := number(m["max"])
	return lo, hi, okLo && okHi
}

func number(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case float32:
		f = float64(n)
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case json.Number:
		x, err := n.Float64()
		if err != nil {
			return 0, false
		}
		f = x
	default:
		return 0, false
	}
	return f, finite(f)
}
