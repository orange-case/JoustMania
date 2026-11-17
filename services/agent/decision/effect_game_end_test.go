package decision

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/goleak"

	"github.com/joustmania/agent/gamecontext"
)

// effect_game_end_test.go covers #2668: a game that ends inside an intervention's
// follow-up window closes that window at once. The follow-up is read at game end,
// the agent.intervention.effect span ends then and carries the window that actually
// elapsed plus intervention.window_end_reason=game_ended; a window that runs out
// carries window_end_reason=elapsed. The sample is emitted exactly once even when the
// timer and the game end race.

// fakeClock is a settable clock for the Loop's now seam.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// endedContext is the game's context after its end: game_active false, duration frozen.
func endedContext(gameID string, durSeconds float64) gamecontext.GameContext {
	c := durationContext(gameID, durSeconds)
	c.Session.GameActive = bp(false)
	return c
}

// awaitEffectsWithin joins the loop's effect goroutines, failing (not hanging) when a
// sample is still open after d: a game end that does not close the window leaves the
// manual scheduler's sample waiting forever.
func awaitEffectsWithin(t *testing.T, l *Loop, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		l.AwaitEffects()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("effect sample still open %v after the game end: the window was not closed", d)
	}
}

// endedEffectSpans returns the ended agent.intervention.effect spans.
func endedEffectSpans(sr *tracetest.SpanRecorder) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == SpanInterventionEffect {
			out = append(out, s)
		}
	}
	return out
}

// UC: UC-040 BR-013
func TestEffect_GameEndClosesWindowEarly(t *testing.T) {
	t.Run("UC-040/BR-013 game end inside the window reads the follow-up at game end", func(t *testing.T) {
		provider := newFakeContextProvider()
		const gameID = "game_ends_early"
		l, sr, reader, sched := effectLoop(t, provider, gameID)
		clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
		l.now = clock.now

		// Dispatched at 30 s of a 120 s endurance target (baseline progress 0.25).
		id := l.schedule(shieldDecision(), enduranceBaseline(), defaultFit())
		if id == "" {
			t.Fatal("expected a scheduled sample")
		}

		// The game ends 4 s later, at 34 s, well inside the 20 s window.
		clock.advance(4 * time.Second)
		provider.set(gameID, endedContext(gameID, 34))
		if n := l.CloseEffectsAtGameEnd(); n != 1 {
			t.Fatalf("CloseEffectsAtGameEnd closed %d samples, want 1", n)
		}
		awaitEffectsWithin(t, l, 2*time.Second)

		spans := endedEffectSpans(sr)
		if len(spans) != 1 {
			t.Fatalf("got %d effect spans, want 1", len(spans))
		}
		attrs := attrMap(spans[0].Attributes())
		if got := attrs[AttrEffectWindowEndReason]; got != EffectWindowGameEnded {
			t.Errorf("window_end_reason = %v, want %q", got, EffectWindowGameEnded)
		}
		if got := attrs[AttrEffectWindowSeconds]; got != float64Attr(4) {
			t.Errorf("window_seconds = %v, want 4 (dispatch to game end), not the configured 20", got)
		}
		if got := attrs[AttrEffectInterventionID]; got != id {
			t.Errorf("intervention id = %v, want %q", got, id)
		}
		if attrs[AttrEffectAborted] == true {
			t.Error("a sample read at game end while the partition exists must not be aborted")
		}
		// Follow-up at game end: 34/120 progress, delta 34/120 - 0.25.
		wantDelta := 34.0/120.0 - 0.25
		if got, ok := attrs[AttrEffectDelta].(float64); !ok || got-wantDelta > 1e-9 || wantDelta-got > 1e-9 {
			t.Errorf("fitness_delta = %v, want %v (follow-up read at game end)", attrs[AttrEffectDelta], wantDelta)
		}
		// The metric records the same delta, over the same shortened window.
		if got := readEffectDelta(t, reader, "endurance.session_progress"); got-wantDelta > 1e-9 || wantDelta-got > 1e-9 {
			t.Errorf("metric delta = %v, want %v", got, wantDelta)
		}

		// The window elapsing afterwards emits nothing more, and a second game end
		// closes nothing.
		sched.fire()
		l.AwaitEffects()
		if n := l.CloseEffectsAtGameEnd(); n != 0 {
			t.Errorf("second CloseEffectsAtGameEnd closed %d samples, want 0", n)
		}
		if got := len(endedEffectSpans(sr)); got != 1 {
			t.Errorf("got %d effect spans after the timer fired too, want exactly 1", got)
		}
	})

	t.Run("UC-040/BR-013 a window that runs out records elapsed and the configured window", func(t *testing.T) {
		provider := newFakeContextProvider()
		const gameID = "game_runs_on"
		provider.set(gameID, durationContext(gameID, 90))
		l, sr, _, sched := effectLoop(t, provider, gameID)

		if id := l.schedule(shieldDecision(), enduranceBaseline(), defaultFit()); id == "" {
			t.Fatal("expected a scheduled sample")
		}
		sched.fire()
		l.AwaitEffects()

		spans := endedEffectSpans(sr)
		if len(spans) != 1 {
			t.Fatalf("got %d effect spans, want 1", len(spans))
		}
		attrs := attrMap(spans[0].Attributes())
		if got := attrs[AttrEffectWindowEndReason]; got != EffectWindowElapsed {
			t.Errorf("window_end_reason = %v, want %q", got, EffectWindowElapsed)
		}
		if got := attrs[AttrEffectWindowSeconds]; got != float64Attr(DefaultEffectWindow.Seconds()) {
			t.Errorf("window_seconds = %v, want %v", got, DefaultEffectWindow.Seconds())
		}
		// The game end after the sample completed has nothing left to close.
		if n := l.CloseEffectsAtGameEnd(); n != 0 {
			t.Errorf("CloseEffectsAtGameEnd after the window closed %d samples, want 0", n)
		}
	})

	t.Run("UC-040/BR-013 a sample scheduled after the game end is not closed by it", func(t *testing.T) {
		provider := newFakeContextProvider()
		const gameID = "game_next"
		provider.set(gameID, durationContext(gameID, 90))
		l, sr, _, sched := effectLoop(t, provider, gameID)

		l.CloseEffectsAtGameEnd() // the previous game of this (fallback) loop ended
		if id := l.schedule(shieldDecision(), enduranceBaseline(), defaultFit()); id == "" {
			t.Fatal("expected a scheduled sample")
		}
		sched.fire()
		l.AwaitEffects()
		spans := endedEffectSpans(sr)
		if len(spans) != 1 || attrMap(spans[0].Attributes())[AttrEffectWindowEndReason] != EffectWindowElapsed {
			t.Fatalf("want one elapsed effect span, got %d", len(spans))
		}
	})
}

// UC: UC-040 BR-013
func TestEffect_GameEndRacingTimerEmitsOnce(t *testing.T) {
	t.Run("UC-040/BR-013 timer and game end together emit the sample exactly once", func(t *testing.T) {
		provider := newFakeContextProvider()
		const gameID = "game_race"
		provider.set(gameID, endedContext(gameID, 34))
		l, sr, _, sched := effectLoop(t, provider, gameID)

		const rounds = 200
		for i := 0; i < rounds; i++ {
			if id := l.schedule(shieldDecision(), enduranceBaseline(), defaultFit()); id == "" {
				t.Fatalf("round %d: expected a scheduled sample", i)
			}
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); sched.fire() }()
			go func() { defer wg.Done(); l.CloseEffectsAtGameEnd() }()
			wg.Wait()
			l.AwaitEffects()
		}
		if got := len(endedEffectSpans(sr)); got != rounds {
			t.Fatalf("got %d effect spans for %d samples, want exactly one each", got, rounds)
		}
		l.pendingMu.Lock()
		pending, open := l.pending, len(l.gameEnd)
		l.pendingMu.Unlock()
		if pending != 0 || open != 0 {
			t.Errorf("after all samples: pending=%d open game-end channels=%d, want 0 and 0", pending, open)
		}
	})
}

// UC: UC-040 BR-013
func TestEffect_GameEndCloseWithRealTimerNoLeak(t *testing.T) {
	t.Run("UC-040/BR-013 a game-end close stops the real timer and leaks no goroutine", func(t *testing.T) {
		defer goleak.VerifyNone(t,
			goleak.IgnoreTopFunction("github.com/open-feature/go-sdk/openfeature.newEventExecutor.(*eventExecutor).startEventListener.func1.1"),
			goleak.IgnoreTopFunction("github.com/open-feature/go-sdk/openfeature.(*eventExecutor).startEventListener.func1.1"),
			goleak.IgnoreAnyFunction("github.com/open-feature/go-sdk/openfeature.(*eventExecutor).startEventListener.func1"),
			goleak.IgnoreAnyFunction("github.com/open-feature/go-sdk/openfeature.(*eventExecutor).startListeningAndShutdownOld.func1"),
			goleak.IgnoreAnyFunction("github.com/open-feature/go-sdk/openfeature.(*eventExecutor).startListeningAndShutdownOld.(*eventExecutor).startEventListener.func3.1"),
		)

		provider := newFakeContextProvider()
		const gameID = "game_real_timer"
		provider.set(gameID, endedContext(gameID, 34))

		sr := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
		defer func() { _ = tp.Shutdown(context.Background()) }()

		l := NewLoop(&settableFlags{snap: permissiveEffectSnapshot()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		l.Tracer = tp.Tracer("test")
		l.SetGameID(gameID)
		l.SetContextProvider(provider)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		l.SetRootContext(ctx)

		// Real 20 s timer (no afterFunc seam): the game end must close it now.
		if id := l.schedule(shieldDecision(), enduranceBaseline(), defaultFit()); id == "" {
			t.Fatal("expected a scheduled sample")
		}
		l.CloseEffectsAtGameEnd()
		awaitEffectsWithin(t, l, 2*time.Second)

		spans := endedEffectSpans(sr)
		if len(spans) != 1 || attrMap(spans[0].Attributes())[AttrEffectWindowEndReason] != EffectWindowGameEnded {
			t.Fatalf("want one game_ended effect span, got %d", len(spans))
		}
	})
}

// UC: UC-040 BR-013
func TestEffect_ShutdownThenGameEndEmitsNothing(t *testing.T) {
	t.Run("UC-040/BR-013 shutdown inside the window still emits nothing, a later game end too", func(t *testing.T) {
		provider := newFakeContextProvider()
		const gameID = "game_shutdown_end"
		provider.set(gameID, endedContext(gameID, 34))
		l, sr, _, _ := effectLoop(t, provider, gameID)
		ctx, cancel := context.WithCancel(context.Background())
		l.SetRootContext(ctx)

		if id := l.schedule(shieldDecision(), enduranceBaseline(), defaultFit()); id == "" {
			t.Fatal("expected a scheduled sample")
		}
		cancel()
		l.AwaitEffects()
		l.CloseEffectsAtGameEnd()
		if got := len(endedEffectSpans(sr)); got != 0 {
			t.Fatalf("got %d effect spans after shutdown, want 0", got)
		}
	})
}

// UC: UC-040 BR-013
func TestLoopSet_EffectsOnGameEnd(t *testing.T) {
	t.Run("UC-040/BR-013 the store's game end closes the game's open effect windows", func(t *testing.T) {
		provider := newFakeContextProvider()
		const gameID = "game_store_end"
		provider.set(gameID, endedContext(gameID, 34))
		l, sr, _, _ := effectLoop(t, provider, gameID)
		ls := NewLoopSet(func(string) *Loop { return l })
		ls.For(gameID)

		// A real store partition whose game_active 1 -> 0 fires the hook.
		store := gamecontext.NewStore(time.Minute, time.Minute, nil)
		store.OnGameEnd = ls.EffectsOnGameEnd(gameID)
		store.SetGameActive(true)

		if id := l.schedule(shieldDecision(), enduranceBaseline(), defaultFit()); id == "" {
			t.Fatal("expected a scheduled sample")
		}
		store.SetGameActive(false)
		awaitEffectsWithin(t, l, 2*time.Second)

		spans := endedEffectSpans(sr)
		if len(spans) != 1 || attrMap(spans[0].Attributes())[AttrEffectWindowEndReason] != EffectWindowGameEnded {
			t.Fatalf("want one game_ended effect span, got %d", len(spans))
		}
	})

	t.Run("UC-040/BR-013 a game the agent never evaluated creates no loop", func(t *testing.T) {
		created := 0
		ls := NewLoopSet(func(string) *Loop { created++; return nil })
		ls.EffectsOnGameEnd("never_seen")(gamecontext.GameContext{})
		if created != 0 || ls.Len() != 0 {
			t.Errorf("game-end hook created %d loops (Len %d), want none", created, ls.Len())
		}
		var nilSet *LoopSet
		if nilSet.EffectsOnGameEnd("x") != nil {
			t.Error("a nil LoopSet must yield a nil hook")
		}
	})
}
