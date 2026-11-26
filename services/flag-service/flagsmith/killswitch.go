package flagsmith

import (
	"bytes"
	"encoding/json"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/registry"
)

// The kill switches of flags.md §13.6 (FR-210, UC-054 BR-005). Since #2621
// Flagsmith wins a conflict for them like for every other flag: there is no
// "more restrictive" merge any more. A round still applies them first, so a
// kill switch switched off in Flagsmith is in force before the write budget
// (NFR-050) defers anything. flags.md §13.6 is the source of truth:
// TestKillSwitchTableInSync keeps this set equal to its table.
var killSwitches = map[string]bool{
	"agent/enabled":                                 true,
	"agent/interventions_allowed":                   true,
	"agent/experiment_proposer_enabled":             true,
	"agent/experiments_enabled":                     true,
	"agent/code_improvement.mode":                   true,
	"agent/code_improvement.target":                 true,
	"agent/code_improvement.min_interval_seconds":   true,
	"agent/llm.max_requests_per_minute":             true,
	"agent/decision.claude.max_requests_per_minute": true,
	"agent/experiment_max_games":                    true,
	"agent/experiment_dynamic_max_concurrent":       true,
}

// isKillKey: k is the stack value of a kill switch.
func isKillKey(k Key) bool {
	return k.Scope == ScopeStack && killSwitches[k.Set+"/"+k.Flag]
}

// agent/enabled boots at `watch`, so Flagsmith's own off (the feature switched
// off) and a bool false must still read as the stop, never as the baseline.
const stopFlag, stopVariant = "agent/enabled", "off"

// isFalse: a Flagsmith bool false, or the text "false".
func isFalse(fv FSValue) bool {
	switch fv.Type {
	case TypeBool:
		return string(bytes.TrimSpace(fv.Value)) == "false"
	case TypeUnicode:
		var s string
		return json.Unmarshal(fv.Value, &s) == nil && s == "false"
	}
	return false
}

// isStopKey: k is agent/enabled.
func isStopKey(k Key) bool { return k.Set+"/"+k.Flag == stopFlag }

// stopValue is the `off` of agent/enabled; ok is false for every other flag.
func stopValue(pol *registry.Policy, b *baseline.Flag) (Value, bool) {
	if pol.Set+"/"+pol.Key != stopFlag || b == nil {
		return Value{}, false
	}
	raw, ok := b.Variants[stopVariant]
	if !ok {
		return Value{}, false
	}
	return Value{Present: true, Variant: stopVariant, Value: compact(raw)}, true
}
