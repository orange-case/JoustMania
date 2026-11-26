package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/joustmania/agent/flags"
)

// experiment_env_removal_1907_test.go covers #1907 PR 2b: with the experiment env
// fallbacks gone, the first refill does not wait the compiled 30 s tick.

// fakeLoopTicker records the intervals run() arms and lets the test fire ticks.
type fakeLoopTicker struct {
	c       chan time.Time
	mu      sync.Mutex
	armed   []time.Duration
	stopped bool
}

func (f *fakeLoopTicker) Chan() <-chan time.Time { return f.c }
func (f *fakeLoopTicker) Reset(d time.Duration) {
	f.mu.Lock()
	f.armed = append(f.armed, d)
	f.mu.Unlock()
}
func (f *fakeLoopTicker) Stop() {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
}
func (f *fakeLoopTicker) intervals() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.armed...)
}

// The first refill comes within firstTickCap even when the resolved tick is the
// compiled 30 s (flagd not yet synced); after that tick the ticker moves to the
// live experiment_tick_seconds, and a tick shorter than the cap is used as is.
// UC: UC-043 A1
func TestExperimentRun_FirstTickCappedThenLiveCadence(t *testing.T) {
	cases := []struct {
		name  string
		tick  time.Duration
		armed []time.Duration // first arm, then each Reset
	}{
		{"UC-043/A1 compiled 30s: first tick capped at 5s, then 30s", 30 * time.Second, []time.Duration{firstTickCap, 30 * time.Second}},
		{"UC-043/A1 live 3s: below the cap, no re-arm", 3 * time.Second, []time.Duration{3 * time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loop, _, lc := loopForConfigTest(t, 8)
			lc.set(flags.ExperimentConfig{Tick: tc.tick}) // gates closed: every tick is inert
			ft := &fakeLoopTicker{c: make(chan time.Time)}
			loop.newTicker = func(d time.Duration) loopTicker {
				ft.Reset(d)
				return ft
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				loop.run(ctx)
				close(done)
			}()
			ft.c <- time.Now() // the second tick; the first ran inline
			ft.c <- time.Now() // a third tick proves the re-arm happened once
			cancel()
			<-done
			got := ft.intervals()
			if len(got) != len(tc.armed) {
				t.Fatalf("armed intervals = %v, want %v", got, tc.armed)
			}
			for i := range got {
				if got[i] != tc.armed[i] {
					t.Fatalf("armed intervals = %v, want %v", got, tc.armed)
				}
			}
		})
	}
}
