package llm

// retro_catalog.go renders the retro's intervention catalog (#2695, UC-042 A13,
// BR-010): for each intervention of the live allow-list, what it does in the game
// (the decide catalog's effect text, the same text the neutral decide prompt's
// Levers block shows), its value range, unit and neutral value, which direction
// helps or hinders a player, and the policy caps the snapshot carries. Every text
// comes from the decide package; the bounds come only through retroBounds.

import (
	"strconv"
	"strings"

	"github.com/joustmania/agent/decide"
	"github.com/joustmania/agent/flags"
)

// retroBounds is the retro's ONE read of an intervention's value bounds. Per-game
// bounds (an operator flag replacing the hard-coded table) plug in here.
func retroBounds(intervention string) (decide.Bounds, bool) {
	return decide.MagnitudeBounds(intervention)
}

// writeInterventionCatalog writes one line per allowed intervention (allow-list
// order, duplicates and empty names dropped), then the game-side factor clamp when
// a factor lever is allowed. An empty allow-list renders "(none)". A name outside
// the decide catalog renders as the bare name.
func writeInterventionCatalog(b *strings.Builder, allowed []string, policy flags.Policy) {
	seen := make(map[string]bool, len(allowed))
	factor := false
	n := 0
	for _, name := range allowed {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if n > 0 {
			b.WriteString("\n")
		}
		n++
		b.WriteString("  - " + catalogLine(name, policy))
		factor = factor || decide.FactorLever(name)
	}
	if n == 0 {
		b.WriteString("  " + joinInterventions(nil))
		return
	}
	if factor {
		b.WriteString("\n  " + decide.FactorClampNote)
	}
}

// catalogLine is one intervention's catalog entry, without the leading "  - ".
func catalogLine(name string, policy flags.Policy) string {
	iv, ok := decide.ByName(name)
	if !ok {
		return name
	}
	var b strings.Builder
	b.WriteString(name + " (" + leverScope(iv) + "): " + iv.Description(decide.PromptProfileNeutral))
	if bd, ok := retroBounds(name); ok {
		b.WriteString(" Value " + retroNum(bd.Min) + " to " + retroNum(bd.Max) + " " + bd.Unit)
		if bd.HasNeutral {
			b.WriteString(", neutral " + retroNum(bd.Neutral))
		}
		b.WriteString(".")
		if iv.ValueEffect != "" {
			b.WriteString(" " + iv.ValueEffect)
		}
	}
	b.WriteString(policyCaps(name, policy))
	return b.String()
}

// leverScope says whom an intervention acts on.
func leverScope(iv decide.Intervention) string {
	switch {
	case iv.PlayerTargeted:
		return "one player"
	case iv.GroupTargeted:
		return "several players"
	default:
		return "everyone"
	}
}

// policyCaps is the snapshot's caps of the shield levers (UC-040 BR-022, BR-033),
// "" for every other lever. 0 means no cap (flags.Policy).
func policyCaps(name string, p flags.Policy) string {
	switch name {
	case decide.IntvGrantShield:
		caps := "no per-game cap"
		if p.MaxShieldsPerPlayerPerGame > 0 {
			caps = "at most " + strconv.Itoa(p.MaxShieldsPerPlayerPerGame) + " per player per game"
		}
		if p.ShieldCooldownSeconds > 0 {
			caps += ", " + strconv.Itoa(p.ShieldCooldownSeconds) + " s cooldown after a player's shield ends"
		}
		return " Policy: " + caps + "."
	case decide.IntvGrantGroupShield:
		if p.GroupGrantMaxTargets > 0 {
			return " Policy: up to " + strconv.Itoa(p.GroupGrantMaxTargets) + " players per grant."
		}
	}
	return ""
}

func retroNum(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
