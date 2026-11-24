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

// cooldownKeys are the ledger keys of cooldownNames (all game-wide).
var cooldownKeys = func() []string {
	keys := make([]string, len(cooldownNames))
	for i, name := range cooldownNames {
		keys[i] = decide.LevelKey(name, "")
	}
	return keys
}()

// leverCooling is step 3a of the permission chain: whether d's lever is cooling,
// and whether this is the first refusal since its last write. A decision that
// passes holds the lever until the caller releases the hold. Only while suppression
// is on, as the ledger is.
func (l *Loop) leverCooling(d Decision, now time.Time) (cooling, first bool, hold *cooldownHold) {
	if !l.suppressReapplied || !cooldownLevers[d.Intervention] {
		return false, false, nil
	}
	return l.applied.cooling(appliedKey(d), normalisedValue(d.Value), now, l.effectWindow())
}

// CoolingValues is the rules engine's view of the cooldown (A53 step 5), taken once
// per ranking: ledger key -> cooling lever. nil when none or while suppression is off.
func (l *Loop) CoolingValues() map[string]coolingLever {
	if !l.suppressReapplied {
		return nil
	}
	return l.applied.coolingView(cooldownKeys, l.nowOrDefault()(), l.effectWindow())
}

// CoolingLevers returns the cooldown levers step 3a would refuse a new value for at
// now (the decide state's lever_cooldown, A53 step 4), sorted; nil when none or
// while suppression is off.
func (l *Loop) CoolingLevers() []string {
	view := l.CoolingValues()
	var out []string
	for i, name := range cooldownNames {
		if _, ok := view[cooldownKeys[i]]; ok {
			out = append(out, name)
		}
	}
	return out
}
