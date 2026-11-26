package flagsmith

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
	"agent/interventions_enabled":                   true,
	"agent/experiments_enabled":                     true,
	"agent/code_improvement.mode":                   true,
	"agent/code_improvement.target":                 true,
	"agent/code_improvement.min_interval_seconds":   true,
	"agent/llm.max_requests_per_minute":             true,
	"agent/decision.claude.max_requests_per_minute": true,
	"agent/experiment_dynamic_max_concurrent":       true,
}

// isKillKey: k is the stack value of a kill switch.
func isKillKey(k Key) bool {
	return k.Scope == ScopeStack && killSwitches[k.Set+"/"+k.Flag]
}
