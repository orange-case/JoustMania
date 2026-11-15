package flagsmith

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/joustmania/flag-service/baseline"
)

// The kill switches and their order (UC-054 BR-005, FR-210, flags.md §13.6):
// on a conflict, and on any difference while no last state from Flagsmith is
// known (after a restart, a first sighting), these flags take the more
// restrictive of the two values; every other flag takes the Flagsmith value.
// flags.md §13.6 is the source of truth: TestKillSwitchTableInSync keeps this
// map equal to its table.

// killKind is how a kill switch orders its values.
type killKind int

const (
	// killOff: off (false) before on (true).
	killOff killKind = iota
	// killLadder: the earlier entry of ladder first; an unknown string counts
	// as fallback, as the agent reads it.
	killLadder
	// killLower: the lower number first (after norm).
	killLower
	// killHigher: the higher number first (after norm).
	killHigher
	// killActions: the variant whose allowed actions are a subset of the
	// other's (agent/interventions_allowed).
	killActions
)

// killRule orders the values of one kill switch. Values are resolved through
// the baseline (a variant name is its declared value; no value is the
// baseline default) and normalised exactly as the agent reads them.
type killRule struct {
	kind     killKind
	ladder   []string
	fallback string
	norm     func(float64) float64
}

// killSwitchFlags is flags.md §13.6 by "set/key".
var killSwitchFlags = map[string]killRule{
	"agent/enabled":                     {kind: killOff},
	"agent/interventions_allowed":       {kind: killActions},
	"agent/experiment_proposer_enabled": {kind: killOff},
	"agent/interventions_enabled":       {kind: killOff},
	"agent/experiments_enabled":         {kind: killOff},
	"agent/rollout_enabled":             {kind: killOff},
	"rollout/remediation_allowed":       {kind: killOff},
	"agent/code_improvement.mode":       {kind: killLadder, ladder: []string{"issue", "pr", "autonomous"}, fallback: "issue"},
	"agent/code_improvement.target":     {kind: killLadder, ladder: []string{"local", "github"}, fallback: "local"},
	// ≤ 0 counts as the agent's default interval of 300 s; a longer interval
	// is more restrictive.
	"agent/code_improvement.min_interval_seconds": {kind: killHigher, norm: func(x float64) float64 {
		if x <= 0 {
			return 300
		}
		return x
	}},
	// ≤ 0 means closed: the lowest.
	"agent/llm.max_requests_per_minute": {kind: killLower, norm: func(x float64) float64 {
		if x <= 0 {
			return 0
		}
		return x
	}},
	"agent/decision.claude.max_requests_per_minute": {kind: killLower, norm: perBackendRPM},
	"agent/decision.openai.max_requests_per_minute": {kind: killLower, norm: perBackendRPM},
	// unlimited (0, any value ≤ 0) counts as the highest.
	"agent/experiment_max_games": {kind: killLower, norm: func(x float64) float64 {
		if x <= 0 {
			return math.Inf(1)
		}
		return x
	}},
	// ≤ 0 resolves to the compiled default 3.
	"agent/experiment_dynamic_max_concurrent": {kind: killLower, norm: func(x float64) float64 {
		if x <= 0 {
			return 3
		}
		return x
	}},
}

// perBackendRPM: -1 (inherit) counts as the highest, any other negative value
// as 0, as in the agent.
func perBackendRPM(x float64) float64 {
	switch {
	case x == -1:
		return math.Inf(1)
	case x < 0:
		return 0
	}
	return x
}

// killRuleOf returns the kill-switch rule of set/key, or nil.
func killRuleOf(set, key string) *killRule {
	r, ok := killSwitchFlags[set+"/"+key]
	if !ok {
		return nil
	}
	return &r
}

// resolveValue returns the JSON value a Value stands for: a declared variant
// is its baseline value, no value the baseline default.
func resolveValue(b *baseline.Flag, v Value) (json.RawMessage, bool) {
	if !v.Present {
		if b == nil {
			return nil, false
		}
		raw, ok := b.Variants[b.DefaultVariant]
		return raw, ok
	}
	if v.Variant != "" && b != nil {
		if raw, ok := b.Variants[v.Variant]; ok {
			return raw, true
		}
	}
	if len(v.Value) == 0 {
		return nil, false
	}
	return v.Value, true
}

// tighter says which of the local and the remote (document) value is the more
// restrictive. ok is false when they cannot be ordered (an unresolvable value,
// two action sets neither of which contains the other): the local value then
// stays. Values of equal rank answer SideRemote (nothing is loosened).
func (r killRule) tighter(b *baseline.Flag, local, remote Value) (Side, bool) {
	lr, ok1 := resolveValue(b, local)
	rr, ok2 := resolveValue(b, remote)
	if !ok1 || !ok2 {
		return SideLocal, false
	}
	c, ok := r.compare(lr, rr)
	if !ok {
		return SideLocal, false
	}
	if c < 0 {
		return SideLocal, true
	}
	return SideRemote, true
}

// compare orders two resolved values: -1 when a is more restrictive than b,
// 1 when b is, 0 for the same rank.
func (r killRule) compare(a, b json.RawMessage) (int, bool) {
	switch r.kind {
	case killOff:
		x, ok1 := boolOf(a)
		y, ok2 := boolOf(b)
		if !ok1 || !ok2 {
			return 0, false
		}
		return cmpRank(rankBool(x), rankBool(y)), true
	case killLadder:
		x, ok1 := r.ladderRank(a)
		y, ok2 := r.ladderRank(b)
		if !ok1 || !ok2 {
			return 0, false
		}
		return cmpRank(float64(x), float64(y)), true
	case killLower, killHigher:
		x, ok1 := numberOf(a)
		y, ok2 := numberOf(b)
		if !ok1 || !ok2 {
			return 0, false
		}
		if r.norm != nil {
			x, y = r.norm(x), r.norm(y)
		}
		if r.kind == killHigher {
			x, y = -x, -y
		}
		return cmpRank(x, y), true
	case killActions:
		return compareActions(a, b)
	}
	return 0, false
}

func cmpRank(x, y float64) int {
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

func rankBool(on bool) float64 {
	if on {
		return 1
	}
	return 0
}

// boolOf reads true/false, or the strings on/off.
func boolOf(raw json.RawMessage) (bool, bool) {
	var x any
	if json.Unmarshal(raw, &x) != nil {
		return false, false
	}
	switch v := x.(type) {
	case bool:
		return v, true
	case string:
		switch v {
		case "on", "true":
			return true, true
		case "off", "false":
			return false, true
		}
	}
	return false, false
}

func numberOf(raw json.RawMessage) (float64, bool) {
	var x any
	if json.Unmarshal(raw, &x) != nil {
		return 0, false
	}
	switch v := x.(type) {
	case float64:
		return v, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	}
	return 0, false
}

func (r killRule) ladderRank(raw json.RawMessage) (int, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return 0, false
	}
	for _, name := range []string{s, r.fallback} {
		for i, l := range r.ladder {
			if l == name {
				return i, true
			}
		}
	}
	return 0, false
}

// actionsOf parses an interventions_allowed value: the comma-separated action
// list, with and without noop (noop counts as no action).
func actionsOf(raw json.RawMessage) (actions, tokens map[string]bool, ok bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil, nil, false
	}
	actions, tokens = map[string]bool{}, map[string]bool{}
	for _, t := range strings.Split(s, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		tokens[t] = true
		if t != "noop" {
			actions[t] = true
		}
	}
	return actions, tokens, true
}

// compareActions: a proper subset of actions is more restrictive; equal
// action sets are ordered by their raw tokens (none before probe); otherwise
// they cannot be ordered.
func compareActions(a, b json.RawMessage) (int, bool) {
	xa, xt, ok1 := actionsOf(a)
	ya, yt, ok2 := actionsOf(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	if c, ok := subsetOrder(xa, ya); ok && c != 0 {
		return c, true
	} else if !ok {
		return 0, false
	}
	return subsetOrder(xt, yt)
}

// subsetOrder: -1 when x ⊂ y, 1 when y ⊂ x, 0 when equal, ok false otherwise.
func subsetOrder(x, y map[string]bool) (int, bool) {
	xin, yin := subset(x, y), subset(y, x)
	switch {
	case xin && yin:
		return 0, true
	case xin:
		return -1, true
	case yin:
		return 1, true
	}
	return 0, false
}

func subset(x, y map[string]bool) bool {
	for k := range x {
		if !y[k] {
			return false
		}
	}
	return true
}
