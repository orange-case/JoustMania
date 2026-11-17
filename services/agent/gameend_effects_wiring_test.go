package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/joustmania/agent/decision"
	"github.com/joustmania/agent/flags"
	"github.com/joustmania/agent/gamecontext"
	"github.com/joustmania/agent/gamesummary"
	"github.com/joustmania/agent/prediction"
)

// storeProvider serves one partition's live store as the loop's context provider.
type storeProvider struct{ s *gamecontext.Store }

func (p storeProvider) Current(string) (gamecontext.GameContext, bool) { return p.s.Snapshot(), true }

// effectRules dispatches one shield decision and reports an endurance fitness
// evaluation, so the dispatch schedules an effect sample.
type effectRules struct{}

func (effectRules) Evaluate(context.Context, gamecontext.GameContext) []decision.Decision {
	return []decision.Decision{{Intervention: "grant_shield", ObjectiveServed: decision.ObjectiveEndurance, Reason: "test"}}
}

func (effectRules) LastFitness() decision.FitnessEvaluation {
	return decision.FitnessEvaluation{Results: map[string]decision.FitnessResult{
		decision.ObjectiveEndurance: {Objective: decision.ObjectiveEndurance, Evaluated: true, Values: map[string]float64{
			"endurance.min_session_seconds": 120,
			"endurance.session_seconds":     30,
			"endurance.session_progress":    0.25,
		}},
	}}
}

// UC: UC-040 BR-013
// TestNewStore_GameEndClosesEffectWindows pins the production wiring (#2668): the
// partition store main builds (components.newStore) closes the game's open effect
// windows on its game end, so a sample is read at game end instead of after
// effect.window_seconds.
func TestNewStore_GameEndClosesEffectWindows(t *testing.T) {
	t.Run("UC-040/BR-013 main's partition store closes the game's effect windows at game end", func(t *testing.T) {
		const gameID = "game_wired"
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		ctx, cancel := context.WithCancel(context.Background())
		book := prediction.New(ctx, nil)
		t.Cleanup(func() { cancel(); book.Await() })

		sr := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
		t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

		var loop *decision.Loop
		c := &components{
			ctx:         ctx,
			logger:      logger,
			loops:       decision.NewLoopSet(func(string) *decision.Loop { return loop }),
			predictions: book,
			retro:       decision.NewRetroCoordinator(nil, logger),
			summaries:   gamesummary.NewCoordinator(logger),
		}
		staleEnds, _ := metricnoop.NewMeterProvider().Meter("test").Int64Counter("stale")
		store := c.newStore(staleEnds)(gameID)

		loop = decision.NewLoop(staticFlagSource{snap: flags.Snapshot{
			Enabled:              true,
			InterventionsAllowed: []string{"grant_shield"},
			Policy:               flags.Policy{MaxInterventionsPerMinute: 100},
		}}, logger)
		loop.Tracer = tp.Tracer("test")
		loop.Rules = effectRules{}
		loop.SetGameID(gameID)
		loop.SetContextProvider(storeProvider{s: store})
		loop.SetRootContext(ctx)
		loop.SetEffectWindow(time.Hour) // only the game end can close it in time
		c.loops.For(gameID)

		store.SetGameActive(true)
		store.SetSessionDuration(30)
		loop.OnEvaluate(context.Background(), store.Snapshot(), decision.EvalTrigger{Signal: "metrics", T0: time.Now()})
		store.SetSessionDuration(34)
		store.SetGameActive(false)

		done := make(chan struct{})
		go func() { loop.AwaitEffects(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the effect sample is still open after the game end: newStore does not wire the effect close")
		}

		var reasons []string
		for _, s := range sr.Ended() {
			if s.Name() != decision.SpanInterventionEffect {
				continue
			}
			for _, kv := range s.Attributes() {
				if string(kv.Key) == decision.AttrEffectWindowEndReason {
					reasons = append(reasons, kv.Value.AsString())
				}
			}
		}
		if len(reasons) != 1 || reasons[0] != decision.EffectWindowGameEnded {
			t.Fatalf("effect window end reasons = %v, want [%s]", reasons, decision.EffectWindowGameEnded)
		}
	})
}
