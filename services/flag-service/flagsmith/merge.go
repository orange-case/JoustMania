package flagsmith

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
	// Refused is the last refusal of this key, nil when none.
	Refused *refusal
	// Discard: L is a stack-scope local change made while the link was not
	// connected, or a stack value Flagsmith refused (#2621); Flagsmith is the
	// source of truth once online, so L is not sent: D is applied, or the
	// baseline when Flagsmith holds no value (admin settings too). Game and
	// player values are never discarded.
	Discard bool
}

// decision is decide's verdict: the action and, for a conflict, its
// resolution (ResolutionFlagsmith).
type decision struct {
	action     action
	resolution string
}

func (d decision) conflict() bool { return d.resolution != "" }

// decide is the merge table of architecture note §6 (UC-054 BR-005: Flagsmith
// wins a conflict, a kill switch too, #2621):
//
//	R = D, R = L              -> none
//	R = D, R ≠ L              -> push (skip while D is the refused value; not re-sent while Flagsmith refused that local value, LinkState.Reoffer, UC-055 A4);
//	                             Discard: apply D (D absent: the baseline), a conflict
//	R ≠ D, R = L              -> apply (a deliberate change in Flagsmith)
//	R ≠ D, R ≠ L, D = L       -> adopt
//	R ≠ D, R ≠ L, D ≠ L       -> conflict: Flagsmith wins
//	R absent, D = L           -> adopt
//	R absent, D ≠ L           -> D absent: push (Discard: apply, back to the baseline); else apply
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
		case !in.D.Present && !in.Discard:
			// Nothing in Flagsmith to apply: the local value is sent.
			return decision{action: actPush}
		}
		return decision{action: actApply}
	}
	rd, rl := in.R.Equal(in.D), in.R.Equal(in.L)
	switch {
	case rd && rl:
		return decision{action: actNone}
	case rd && in.Discard:
		return decision{action: actApply, resolution: ResolutionFlagsmith}
	case rd:
		return decision{action: actPush}
	case rl:
		return decision{action: actApply}
	case in.D.Equal(in.L):
		return decision{action: actAdopt}
	}
	return decision{action: actApply, resolution: ResolutionFlagsmith}
}
