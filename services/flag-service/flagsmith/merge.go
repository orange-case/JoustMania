package flagsmith

import "github.com/joustmania/flag-service/baseline"

// The 3-way merge of one key (architecture note §6, FR-210, UC-054 A2): R the
// last state seen from Flagsmith, D the document value, L the local value.

// action is what a round does with one key.
type action int

const (
	// actNone: all three agree; the key is in sync.
	actNone action = iota
	// actSkip: the document still holds a value the flag service refused;
	// nothing is applied and the local value is not pushed over it.
	actSkip
	// actPush: a local change not yet in Flagsmith; queue it for the out side.
	actPush
	// actApply: a change made in Flagsmith; write it locally.
	actApply
	// actAdopt: both sides moved to the same value; remote := D.
	actAdopt
	// actKeepLocal: the local value wins a conflict; remote := D and the
	// local value is queued for the out side.
	actKeepLocal
)

func (a action) String() string {
	return [...]string{"none", "skip", "push", "apply", "adopt", "keep_local"}[a]
}

// refusal is a document value the flag service refused, with the local value
// it kept (the last state both sides agreed on, for the next merge).
type refusal struct {
	d, l Value
}

// mergeIn is one key's input to decide.
type mergeIn struct {
	R    Value
	HasR bool
	D, L Value
	// Kill is the key's kill-switch rule (nil: not a kill switch). Only stack
	// keys carry one.
	Kill *killRule
	// Base resolves variant names for the kill-switch order.
	Base *baseline.Flag
	// Refused is the last refusal of this key, nil when none.
	Refused *refusal
}

// decision is decide's verdict: the action and, for a conflict, its
// resolution (ResolutionFlagsmith, ResolutionTightened, ResolutionLocal).
type decision struct {
	action     action
	resolution string
}

func (d decision) conflict() bool { return d.resolution != "" }

// decide is the merge table of architecture note §6 with the kill-switch rule
// of UC-054 BR-005 (flags.md §13.6):
//
//	R = D, R = L              -> none
//	R = D, R ≠ L              -> push (skip while D is the refused value; not re-sent while Flagsmith refused that local value, LinkState.Reoffer, UC-055 A4)
//	R ≠ D, R = L              -> apply (a deliberate change, also one that loosens a kill switch)
//	R ≠ D, R ≠ L, D = L       -> adopt
//	R ≠ D, R ≠ L, D ≠ L       -> conflict: Flagsmith wins, a kill switch takes the more restrictive value
//	R absent, D = L           -> adopt
//	R absent, D ≠ L           -> kill switch: conflict (#2590 R1); D absent: push; else apply
//
// A refused key merges against the local value it kept, so a new document
// value is a deliberate change and the refused one is skipped.
func decide(in mergeIn) decision {
	if in.Refused != nil {
		if in.Refused.d.Equal(in.D) {
			return decision{action: actSkip}
		}
		in.R, in.HasR = in.Refused.l, true
	}
	if !in.HasR {
		switch {
		case in.D.Equal(in.L):
			return decision{action: actAdopt}
		case in.Kill != nil:
			return conflictDecision(in)
		case !in.D.Present:
			// Nothing in Flagsmith to apply: the local value is sent.
			return decision{action: actPush}
		}
		return decision{action: actApply}
	}
	rd, rl := in.R.Equal(in.D), in.R.Equal(in.L)
	switch {
	case rd && rl:
		return decision{action: actNone}
	case rd:
		return decision{action: actPush}
	case rl:
		return decision{action: actApply}
	case in.D.Equal(in.L):
		return decision{action: actAdopt}
	}
	return conflictDecision(in)
}

func conflictDecision(in mergeIn) decision {
	if in.Kill == nil {
		return decision{action: actApply, resolution: ResolutionFlagsmith}
	}
	side, ok := in.Kill.tighter(in.Base, in.L, in.D)
	switch {
	case !ok:
		return decision{action: actKeepLocal, resolution: ResolutionLocal}
	case side == SideLocal:
		return decision{action: actKeepLocal, resolution: ResolutionTightened}
	}
	return decision{action: actApply, resolution: ResolutionTightened}
}
