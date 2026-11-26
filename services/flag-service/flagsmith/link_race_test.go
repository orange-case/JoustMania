package flagsmith

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
)

// newTestState is a State over the CI baseline and the service's registry.
func newTestState(t *testing.T) *state.State {
	t.Helper()
	base, err := baseline.Load("../../flagd/ci", baseline.DefaultFiles)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.LoadFile("../registry.yaml", base)
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := state.New(base, reg, state.Options{Epoch: "e1", Served: base.Order, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := st.Subscribe("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	return st
}

// blockedOut is an out side whose Run blocks (a push stuck on the network)
// until ctx ends; Changed does the map work the real out side does.
type blockedOut struct {
	l       *Link
	once    sync.Once
	entered chan struct{}
	changes atomic.Int64
}

func (b *blockedOut) Changed(c state.Change) {
	if k, v, ok := ValueOfChange(c); ok {
		b.l.Links().Offer(k, v, b.l.Now(), c.Version)
	}
	b.changes.Add(1)
}

func (b *blockedOut) Run(ctx context.Context) error {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	return ctx.Err()
}

// UC: UC-055 BR-001
// The observer runs under State.mu (#2588 plan D6): a blocked link must not
// hold up a write (NFR-057, NFR-050).
func TestWritesWhileRunIsBlocked(t *testing.T) {
	t.Run("UC-055/BR-001 writes and status reads go on while the out side's Run is blocked", func(t *testing.T) {
		st := newTestState(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		l := newTestLink(t, Config{Enabled: true}, &sleeper{max: 1, cancel: cancel})
		out := &blockedOut{l: l, entered: make(chan struct{})}
		l.start, l.in, l.out = &countingStarter{}, &scriptedIn{}, out
		st.SetObserver(l)
		done := runLink(ctx, l)
		select {
		case <-out.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the out side's Run never started")
		}

		const writers, each = 4, 25
		variants := []string{"jev", "claude", "rules"}
		var wg sync.WaitGroup
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < each; i++ {
					req := state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: variants[(w+i)%len(variants)], Writer: "test"}
					if _, ferr := st.Put(req); ferr != nil {
						t.Errorf("write %d/%d: %v", w, i, ferr)
						return
					}
					_ = l.Status()
				}
			}(w)
		}
		finished := make(chan struct{})
		go func() {
			wg.Wait()
			close(finished)
		}()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatal("writes did not finish while the link's Run was blocked")
		}
		if m := l.Mode(); m != ModeOnline {
			t.Fatalf("mode %s, want online (Run still blocked in the out side)", m)
		}
		if out.changes.Load() == 0 || l.Status().Pending == 0 {
			t.Fatalf("changes %d, pending %d: the observer did not reach the out side", out.changes.Load(), l.Status().Pending)
		}
		cancel()
		waitDone(t, done)
	})
}
