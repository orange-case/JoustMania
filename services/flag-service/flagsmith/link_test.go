package flagsmith

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/state"
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

// scriptedIn records the reconcile triggers, fails the first failReconciles
// reconciles and lets Poll fail the first drops times; drop i first advances
// clock (when set) by online[i] (the time the link stayed online).
type scriptedIn struct {
	mu             sync.Mutex
	triggers       []string
	failReconciles int
	polls          int
	drops          int
	clock          *fakeClock
	online         []time.Duration
}

func (s *scriptedIn) Reconcile(_ context.Context, trigger string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.triggers = append(s.triggers, trigger)
	if len(s.triggers) <= s.failReconciles {
		return ReasonError{Reason: ReasonTimeout}
	}
	return nil
}

func (s *scriptedIn) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.triggers...)
}

func (s *scriptedIn) Poll(ctx context.Context) error {
	s.mu.Lock()
	s.polls++
	n := s.polls
	drop := n <= s.drops
	if drop && s.clock != nil && n <= len(s.online) {
		s.clock.Advance(s.online[n-1])
	}
	s.mu.Unlock()
	if drop {
		return &HTTPError{Status: 503}
	}
	<-ctx.Done()
	return ctx.Err()
}

// idleOut is an out side that sends nothing; its Run returns linger after
// ctx ends and records that it returned.
type idleOut struct {
	linger   time.Duration
	returned atomic.Bool
}

func (*idleOut) Changed(state.Change) {}

func (o *idleOut) Run(ctx context.Context) error {
	<-ctx.Done()
	time.Sleep(o.linger)
	o.returned.Store(true)
	return ctx.Err()
}

// pingOnly is a start sequence that only fetches the document (the link's
// own client against the fake): a failing Flagsmith keeps the link offline.
type pingOnly struct{ l *Link }

func (p pingOnly) Start(ctx context.Context) error {
	_, _, _, err := p.l.Client().EnvironmentDocument(ctx, "")
	return err
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newTestLink builds a link whose three phases are test doubles (a start
// that succeeds, an in side that reconciles and polls until cancelled, an out
// side that sends nothing): the foundation's tests never depend on the
// start.go, in.go and out.go that #2589, #2590 and #2591 replace.
func newTestLink(t *testing.T, cfg Config, sl *sleeper) *Link {
	t.Helper()
	return newTestLinkAt(t, cfg, sl, nil)
}

func newTestLinkAt(t *testing.T, cfg Config, sl *sleeper, now func() time.Time) *Link {
	t.Helper()
	l, err := New(cfg, Deps{Logger: quietLogger(), Sleep: sl.Sleep, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	l.start, l.in, l.out = &countingStarter{}, &scriptedIn{}, &idleOut{}
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
		l.start = pingOnly{l}
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
	t.Run("UC-054/A1 reachable: online with test sides", func(t *testing.T) {
		f := flagsmithtest.New(t)
		ctx, cancel := context.WithCancel(context.Background())
		l := newTestLink(t, testConfig(f), &sleeper{max: 1, cancel: cancel})
		l.start = pingOnly{l}
		done := runLink(ctx, l)
		waitMode(t, l, ModeOnline)
		cancel()
		waitDone(t, done)
	})
	t.Run("UC-054/A1 Start runs once; quick drops keep backing off", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sl := &sleeper{max: 4, cancel: cancel}
		l := newTestLink(t, Config{Enabled: true}, sl)
		st := &countingStarter{fail: 2}
		in := &scriptedIn{drops: 2}
		l.start, l.in = st, in
		waitDone(t, runLink(ctx, l))
		// start fails twice (1 s, 2 s), succeeds, drop (4 s), drop (8 s): a
		// side failing right after each reconnect does not reset the backoff.
		got := sl.got()
		want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("sleeps %v, want %v", got, want)
			}
		}
		if st.calls != 3 {
			t.Fatalf("Start called %d times, want 3 (2 failures, then once)", st.calls)
		}
		if tr := in.got(); len(tr) != 3 || tr[0] != TriggerStart || tr[1] != TriggerReconnect || tr[2] != TriggerReconnect {
			t.Fatalf("reconcile triggers %v, want start, reconnect, reconnect", tr)
		}
	})
	// #2594 review finding 11.
	t.Run("UC-054/A1 the backoff starts again at 1 s after a time online", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		clock := &fakeClock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
		sl := &sleeper{max: 3, cancel: cancel}
		l := newTestLinkAt(t, Config{Enabled: true}, sl, clock.Now)
		l.in = &scriptedIn{drops: 3, clock: clock, online: []time.Duration{0, BackoffResetAfter - time.Second, BackoffResetAfter}}
		waitDone(t, runLink(ctx, l))
		got := sl.got()
		want := []time.Duration{time.Second, 2 * time.Second, time.Second}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("sleeps %v, want %v", got, want)
			}
		}
	})
	// #2594 review finding 3: the first round after start is special
	// (architecture note §5.1), so a failed first reconcile retries as start.
	t.Run("UC-054/A1 a failed first reconcile retries with trigger start", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sl := &sleeper{max: 2, cancel: cancel}
		l := newTestLink(t, Config{Enabled: true}, sl)
		st := &countingStarter{}
		in := &scriptedIn{failReconciles: 1, drops: 1}
		l.start, l.in = st, in
		waitDone(t, runLink(ctx, l))
		if tr := in.got(); len(tr) != 3 || tr[0] != TriggerStart || tr[1] != TriggerStart || tr[2] != TriggerReconnect {
			t.Fatalf("reconcile triggers %v, want start, start, reconnect", tr)
		}
		if st.calls != 1 {
			t.Fatalf("Start called %d times, want once", st.calls)
		}
	})
	// #2594 review finding 10: runSides joins both sides.
	t.Run("UC-054/A1 a link-down error from one side stops the other before the backoff", func(t *testing.T) {
		l := newTestLink(t, Config{Enabled: true}, &sleeper{})
		out := &idleOut{linger: 50 * time.Millisecond}
		l.in, l.out = &scriptedIn{drops: 1}, out
		err := l.runSides(context.Background())
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != 503 {
			t.Fatalf("err %v, want the 503 of the in side", err)
		}
		if !out.returned.Load() {
			t.Fatal("runSides returned before the out side's Run returned")
		}
	})
	// #2594 F4, F6: a game registration wakes the in side and opens GamesSeen.
	t.Run("UC-054/A1 a game registration wakes the in side and closes GamesSeen once", func(t *testing.T) {
		l := newTestLink(t, Config{Enabled: true}, &sleeper{})
		select {
		case <-l.GamesSeen():
			t.Fatal("GamesSeen closed before a game registered")
		default:
		}
		l.StateChanged(state.Change{Kind: state.ChangeValue})
		select {
		case <-l.WakeC():
			t.Fatal("a value change woke the in side")
		default:
		}
		for i := 0; i < 2; i++ {
			l.StateChanged(state.Change{Kind: state.ChangeGameRegistered})
		}
		select {
		case <-l.GamesSeen():
		default:
			t.Fatal("GamesSeen still open after a game registered")
		}
		select {
		case <-l.WakeC():
		default:
			t.Fatal("a game registration did not wake the in side")
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
