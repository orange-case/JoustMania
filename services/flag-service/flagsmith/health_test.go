package flagsmith

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// UC: UC-054 A4
// The health posts run in their own goroutine; Poll returns only after it has
// been joined (the post running at shutdown ends first), and a post cut short
// by the shutdown stays queued and is sent after resume (#2596 leftover).
func TestPollJoinsHealthDrain(t *testing.T) {
	l, err := New(Config{Enabled: true, Poll: time.Hour, HealthWebhookURL: "http://health.invalid/"}, Deps{Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	p := newIn(l).(*puller)
	p.after = func(time.Duration) <-chan time.Time { return nil }
	started := make(chan struct{})
	var once sync.Once
	var finished atomic.Bool
	p.health.send = func(ctx context.Context, _, _ string, _ healthPost) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // a post that takes a moment to give up
		finished.Store(true)
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	polled := make(chan error, 1)
	go func() { polled <- p.Poll(ctx) }()
	p.health.refuse("decision.backend", Key{Set: "agent", Flag: "decision.backend", Scope: ScopeStack}, "refused")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the health post never started")
	}
	cancel()
	select {
	case <-polled:
	case <-time.After(5 * time.Second):
		t.Fatal("Poll did not return")
	}
	if !finished.Load() {
		t.Fatal("UC-054/A4 Poll returned while the health post goroutine was still running (not joined)")
	}
	sent := make(chan healthPost, 1)
	p.health.send = func(_ context.Context, _, _ string, hp healthPost) error {
		sent <- hp
		return nil
	}
	p.health.resume()
	select {
	case hp := <-sent:
		if hp.status != HealthUnhealthy {
			t.Fatalf("UC-054/A4 resumed post %+v, want unhealthy", hp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UC-054/A4 the post cut short by the shutdown was not sent after resume")
	}
	p.health.stop()
}
