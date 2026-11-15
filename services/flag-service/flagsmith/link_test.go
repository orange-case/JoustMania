package flagsmith

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
)

// sleeper records the backoff sleeps and cancels the run after max of them.
type sleeper struct {
	mu     sync.Mutex
	sleeps []time.Duration
	max    int
	cancel context.CancelFunc
}

func (s *sleeper) Sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sleeps = append(s.sleeps, d)
	if len(s.sleeps) >= s.max {
		s.cancel()
		return context.Canceled
	}
	return nil
}

func (s *sleeper) got() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.sleeps...)
}

type countingStarter struct {
	mu    sync.Mutex
	calls int
	fail  int // the first fail calls fail
}

func (c *countingStarter) Start(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls <= c.fail {
		return ReasonError{Reason: ReasonTimeout}
	}
	return nil
}

// scriptedIn reconciles with the given triggers recorded and lets Poll fail
// the first drops times.
type scriptedIn struct {
	mu       sync.Mutex
	triggers []string
	polls    int
	drops    int
}

func (s *scriptedIn) Reconcile(_ context.Context, trigger string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.triggers = append(s.triggers, trigger)
	return nil
}

func (s *scriptedIn) Poll(ctx context.Context) error {
	s.mu.Lock()
	s.polls++
	drop := s.polls <= s.drops
	s.mu.Unlock()
	if drop {
		return &HTTPError{Status: 503}
	}
	<-ctx.Done()
	return ctx.Err()
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestLink(t *testing.T, cfg Config, sl *sleeper) *Link {
	t.Helper()
	l, err := New(cfg, Deps{Logger: quietLogger(), Sleep: sl.Sleep})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func runLink(ctx context.Context, l *Link) chan struct{} {
	done := make(chan struct{})
	go func() {
		l.Run(ctx)
		close(done)
	}()
	return done
}

func waitDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

func waitMode(t *testing.T, l *Link, m Mode) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for l.Mode() != m {
		if time.Now().After(deadline) {
			t.Fatalf("mode %s, want %s", l.Mode(), m)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// UC: UC-054 A1
func TestLinkRun(t *testing.T) {
	t.Run("UC-054/A1 off: Run returns at once and the status is off", func(t *testing.T) {
		l := newTestLink(t, Config{ConfigError: []string{"FLAGSMITH_ADMIN_API_KEY"}}, &sleeper{})
		waitDone(t, runLink(context.Background(), l))
		st := l.Status()
		if st.Link != ModeOff || len(st.ConfigError) != 1 || st.RestartRule != "" {
			t.Fatalf("status %+v, want off with the config error", st)
		}
		var nilLink *Link
		if nilLink.Status().Link != ModeOff {
			t.Fatal("a nil link is not off")
		}
	})
	t.Run("UC-054/A1 unreachable: offline with its reason, backoff 1, 2, 4 ... 60, 60", func(t *testing.T) {
		f := flagsmithtest.New(t)
		f.Fail(502)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sl := &sleeper{max: 8, cancel: cancel}
		l := newTestLink(t, testConfig(f), sl)
		waitDone(t, runLink(ctx, l))
		want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
		got := sl.got()
		for i := range want {
			if got[i] != want[i]*time.Second {
				t.Fatalf("sleeps %v, want %v seconds", got, want)
			}
		}
		if st := l.Status(); st.Link != ModeOffline || st.OfflineReason != "http_502" {
			t.Fatalf("status %+v, want offline http_502", st)
		}
	})
	t.Run("UC-054/A1 reachable: online with the stub sides", func(t *testing.T) {
		f := flagsmithtest.New(t)
		ctx, cancel := context.WithCancel(context.Background())
		l := newTestLink(t, testConfig(f), &sleeper{max: 1, cancel: cancel})
		done := runLink(ctx, l)
		waitMode(t, l, ModeOnline)
		cancel()
		waitDone(t, done)
	})
	t.Run("UC-054/A1 Start runs once; a drop goes offline and the backoff starts again at 1 s", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sl := &sleeper{max: 4, cancel: cancel}
		l := newTestLink(t, Config{Enabled: true}, sl)
		st := &countingStarter{fail: 2}
		in := &scriptedIn{drops: 2}
		l.start, l.in = st, in
		waitDone(t, runLink(ctx, l))
		// start fails twice (1 s, 2 s), succeeds, drop (1 s), drop (1 s): max 4 sleeps.
		got := sl.got()
		want := []time.Duration{time.Second, 2 * time.Second, time.Second, time.Second}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("sleeps %v, want %v", got, want)
			}
		}
		if st.calls != 3 {
			t.Fatalf("Start called %d times, want 3 (2 failures, then once)", st.calls)
		}
		if len(in.triggers) != 2 || in.triggers[0] != TriggerStart || in.triggers[1] != TriggerReconnect {
			t.Fatalf("reconcile triggers %v, want start then reconnect", in.triggers)
		}
	})
	t.Run("UC-054/A1 a link-down error from one side stops the other before the backoff", func(t *testing.T) {
		l := newTestLink(t, Config{Enabled: true}, &sleeper{})
		in := &scriptedIn{drops: 1}
		l.in = in
		err := l.runSides(context.Background())
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != 503 {
			t.Fatalf("err %v, want the 503 of the in side", err)
		}
	})
	t.Run("UC-054/A1 the restart rule shows once reported", func(t *testing.T) {
		l := newTestLink(t, Config{Enabled: true}, &sleeper{})
		l.setMode(ModeOnline, "")
		if l.Status().RestartRule != "" {
			t.Fatal("restart rule before Start")
		}
		k := Key{Set: "game", Flag: "sensitivity", Scope: ScopeStack}
		l.SetRestartReport(RestartAuditLog, map[Key]Author{k: AuthorAPIKey, {Set: "agent", Flag: "enabled", Scope: ScopeStack}: AuthorPerson})
		rule, attribution, ok := l.RestartReport()
		if !ok || rule != RestartAuditLog || RestartCleared(attribution) != 1 || l.Status().RestartRule != RestartAuditLog {
			t.Fatalf("report %q %v %v", rule, attribution, ok)
		}
	})
}
