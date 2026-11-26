package flagsmith

import "github.com/joustmania/flag-service/registry"

// killSwitch is registry.IsKillSwitch (flags.md §13.6); tests swap it. A round
// applies kill switches first, so one switched off in Flagsmith is in force
// before the write budget defers anything.
var killSwitch = registry.IsKillSwitch

// isKillKey: k is the stack value of a kill switch.
func isKillKey(k Key) bool {
	return k.Scope == ScopeStack && killSwitch(k.Set, k.Flag)
}
