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
	l.start, l.out = inOnlyStarter{l: l}, inOnlyOut{}
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

type inOnlyOut struct{}

func (inOnlyOut) Changed(state.Change) {}

func (inOnlyOut) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
