package registry

// latchedAtGameStart lists the flags the game reads once per game, so a write in
// scope real never changes a running game (UC-040 BR-027, flags.md §13, #2810).
// Every row with scope real must be listed and true; TestRealScopeIsLatched
// enforces it. A Go set, not a registry.yaml key: released images still load the
// checkout's registry.yaml with KnownFields.
//
// shield.activation: the latch holds for the agent's only promotable value,
// hold. An instant or trigger value written mid-game reaches an instant or
// trigger game at its next grant (accepted, #2810).
var latchedAtGameStart = map[string]bool{
	// Menu game config, built at game start (UC-011).
	"game/sensitivity":                  true,
	"game/nonstop.time_limit_seconds":   true,
	"game/invincibility_seconds":        true,
	"game/fight_club.min_rounds":        true,
	"game/werewolf.reveal_time_seconds": true,
	// Coordinator init flags, read once when the game is built.
	"game/death_grace_period_seconds":              true,
	"game/nonstop.respawn_seconds":                 true,
	"game/nonstop.spawn_protection_seconds":        true,
	"game/fight_club.round_seconds":                true,
	"game/tournament.match_seconds":                true,
	"game/tournament.time_between_matches_seconds": true,
	"game/werewolf.werewolf_fraction":              true,
	"game/zombie.initial_count":                    true,
	"game/zombie.respawn_min_seconds":              true,
	"game/zombie.respawn_max_seconds":              true,
	"game/windows":                                 true,
	// Shield hold latch (UC-040 BR-027).
	"game/shield.activation":             true,
	"game/shield.energy_max_seconds":     true,
	"game/shield.energy_budget_seconds":  true,
	"game/shield.energy_initial_seconds": true,
	"game/shield.recharge_per_second":    true,
	"game/shield.min_burst_seconds":      true,
	"game/shield.hold_rumble_intensity":  true,
	"game/shield.empty_pulse_ms":         true,
}

// LatchedAtGameStart reports whether the game reads this flag once per game.
func (p *Policy) LatchedAtGameStart() bool { return latchedAtGameStart[id(p.Set, p.Key)] }
