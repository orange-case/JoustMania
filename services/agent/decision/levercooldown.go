package decision

import (
	"time"

	"github.com/joustmania/agent/decide"
)

// The lever cooldown (UC-040 A53, BR-046; decide-contract §4.5 step 3a; #2641). A
// magnitude score maps to an absolute value, so two close scores can resolve to two
// values; the already-applied check (A35) blocks only an exact repeat, and a high
// budget does not damp the alternation. A game-wide lever therefore changes at most
// once per effect window, so each change has its effect window to itself. The check
// is check-only and reads the write times the applied-value ledger keeps.

// cooldownLevers are the game-wide levers with a cooldown (decide.Intervention.Cooldown).
var cooldownLevers = decide.Set(func(iv decide.Intervention) bool { return iv.Cooldown })

// cooldownNames are cooldownLevers, sorted.
var cooldownNames = decide.Names(func(iv decide.Intervention) bool { return iv.Cooldown })

// leverCooling is step 3a of the permission chain: whether d's lever is cooling, and
// whether this is the first refusal since its last write. A same-value repeat never
// gets here (step 3 blocks it first). Only while suppression is on, as the ledger is.
func (l *Loop) leverCooling(d Decision, now time.Time) (cooling, first bool) {
	if !l.suppressReapplied || !cooldownLevers[d.Intervention] {
		return false, false
	}
	return l.applied.cooling(appliedKey(d), now, l.effectWindow())
}

// CoolingLevers returns the cooldown levers step 3a would refuse a new value for at
// now (the decide state's lever_cooldown, A53 step 4), sorted; nil when none or
// while suppression is off.
func (l *Loop) CoolingLevers() []string {
	if !l.suppressReapplied {
		return nil
	}
	now, window := l.nowOrDefault()(), l.effectWindow()
	var out []string
	for _, name := range cooldownNames {
		if l.applied.isCooling(decide.LevelKey(name, ""), now, window) {
			out = append(out, name)
		}
	}
	return out
}
