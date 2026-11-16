package flagsmith

import (
	"context"

	"github.com/joustmania/flag-service/state"
)

// The in side's journeys (in_test.go, in_rounds_test.go) run the real in side
// only: the start sequence is a ping and the out side sends nothing, so they
// pass alone and with #2589's out side and #2591's start sequence merged.

// UseInSideOnly replaces l's start sequence by a ping and its out side by an
// idle one. Call it before l.Run.
func UseInSideOnly(l *Link) {
	l.start, l.out = inOnlyStarter{l: l}, inOnlyOut{l: l}
}

// UsePingStart replaces only l's start sequence by a ping and keeps the real
// out side: the in and out sides run together (inout_test.go, #2604). Call it
// before l.Run.
func UsePingStart(l *Link) {
	l.start = inOnlyStarter{l: l}
}

type inOnlyStarter struct{ l *Link }

func (s inOnlyStarter) Start(ctx context.Context) error {
	_, _, _, err := s.l.client.EnvironmentDocument(ctx, "")
	return err
}

// inOnlyOut queues local values like the real out side's observer and never
// sends them.
type inOnlyOut struct{ l *Link }

func (o inOnlyOut) Changed(c state.Change) {
	if c.Kind == state.ChangeValue || c.Kind == state.ChangeCleared {
		if k, v, ok := ValueOfChange(c); ok {
			o.l.Links().OfferFrom(k, v, c.At, c.Version, k.Scope == ScopeStack && !o.l.connected.Load())
		}
	}
}

func (inOnlyOut) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// SettleControllers marks every controller settled for its owner, as the out
// side's settle would (the in-only journeys have no out side, #2624).
func SettleControllers(l *Link) {
	c := l.ctrl
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		if e.owner != "" && e.settled != e.owner {
			e.settled, e.readable, e.verified = e.owner, true, true
			c.bumpLocked(e)
		}
	}
}

// NoteControllerRemoved records that a removal took value (a JSON literal) of
// flag off serial's controller identity, as the out side's settle does.
func NoteControllerRemoved(l *Link, serial, flag string, featureID int, value string) {
	l.ctrl.noteRemoved(serial, []FeatureState{{Feature: Feature{ID: featureID}, Enabled: true, FeatureStateValue: []byte(value)}},
		func(int) string { return flag }, l.Now())
}
