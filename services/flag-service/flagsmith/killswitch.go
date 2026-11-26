package flagsmith

import (
	"bytes"
	"encoding/json"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/registry"
)

// killSwitch reads the row's kill_switch attribute (flags.md §13.6); tests
// swap it. A round applies kill switches first, so one switched off in
// Flagsmith is in force before the write budget defers anything.
var killSwitch = func(l *Link, set, key string) bool { return l.Policy(set, key).IsKillSwitch() }

// isKillKey: k is the stack value of a kill switch.
func (l *Link) isKillKey(k Key) bool {
	return k.Scope == ScopeStack && killSwitch(l, k.Set, k.Flag)
}

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

// isStopKey: k's row has a stop variant (agent/enabled). It boots at `watch`,
// so Flagsmith's own off (the feature switched off) and a bool false must
// still read as the stop, never as the baseline.
func (l *Link) isStopKey(k Key) bool { return l.Policy(k.Set, k.Flag).StopVariant() != "" }

// stopValue is the row's stop variant; ok is false for a row without one.
func stopValue(pol *registry.Policy, b *baseline.Flag) (Value, bool) {
	stop := pol.StopVariant()
	if stop == "" || b == nil {
		return Value{}, false
	}
	raw, ok := b.Variants[stop]
	if !ok {
		return Value{}, false
	}
	return Value{Present: true, Variant: stop, Value: compact(raw)}, true
}
