package flagsmith

import (
	"bytes"
	"encoding/json"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/registry"
)

// killSwitch is registry.IsKillSwitch (flags.md §13.6); tests swap it. A round
// applies kill switches first, so one switched off in Flagsmith is in force
// before the write budget defers anything.
var killSwitch = registry.IsKillSwitch

// isKillKey: k is the stack value of a kill switch.
func isKillKey(k Key) bool {
	return k.Scope == ScopeStack && killSwitch(k.Set, k.Flag)
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
