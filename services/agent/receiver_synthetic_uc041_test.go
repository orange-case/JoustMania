package main

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"

	"github.com/joustmania/agent/decision"
	"github.com/joustmania/agent/flags"
	"github.com/joustmania/agent/gamecontext"
)

// receiver_synthetic_uc041_test.go: the agent's backup drop of synthetic
// telemetry (UC-041 BR-011, #2577). The collector never routes it to the agent;
// these tests play the collector's agent route opened by mistake.

type syntheticHarness struct {
	mux       *gamecontext.Multiplexer
	rules     *recordingRules
	pipe      *pipeline
	flagReads atomic.Int32
	observe   atomic.Bool
}

func newSyntheticHarness(t *testing.T) *syntheticHarness {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	clock := time.Unix(5000, 0)
	h := &syntheticHarness{rules: &recordingRules{}}
	h.mux = gamecontext.NewMultiplexer(func(string) *gamecontext.Store {
		return gamecontext.NewStore(time.Hour, time.Hour, func() time.Time { return clock })
	})
	loops := decision.NewLoopSet(func(string) *decision.Loop {
		l := decision.NewLoop(staticFlags{flags.Snapshot{
			Enabled:              true,
			InterventionsAllowed: []string{"grant_shield"},
			Policy:               flags.Policy{MaxInterventionsPerMinute: 100},
		}}, logger)
		l.Rules = h.rules
		return l
	})
	h.pipe = newPipeline(h.mux, loops, time.Hour).withObserveSynthetic(func(context.Context) bool {
		h.flagReads.Add(1)
		return h.observe.Load()
	})
	h.pipe.now = func() time.Time { return clock }
	return h
}

func (h *syntheticHarness) metrics(t *testing.T, md pmetric.Metrics) {
	t.Helper()
	r := &metricsReceiver{pipe: h.pipe}
	if _, err := r.Export(context.Background(), pmetricotlp.NewExportRequestFromMetrics(md)); err != nil {
		t.Fatalf("metrics export: %v", err)
	}
}

func (h *syntheticHarness) traces(t *testing.T, td ptrace.Traces) {
	t.Helper()
	r := &traceReceiver{pipe: h.pipe}
	if _, err := r.Export(context.Background(), ptraceotlp.NewExportRequestFromTraces(td)); err != nil {
		t.Fatalf("traces export: %v", err)
	}
}

func (h *syntheticHarness) logs(t *testing.T, ld plog.Logs) {
	t.Helper()
	r := &logsReceiver{pipe: h.pipe}
	if _, err := r.Export(context.Background(), plogotlp.NewExportRequestFromLogs(ld)); err != nil {
		t.Fatalf("logs export: %v", err)
	}
}

func (h *syntheticHarness) hasGame(gameID string) bool {
	return slices.Contains(h.mux.Partitions(), gameID)
}

// markResources sets orangecase.synthetic on every resource appended after from.
func markMetricResources(md pmetric.Metrics, from int, v any) {
	for i := from; i < md.ResourceMetrics().Len(); i++ {
		putSynthetic(md.ResourceMetrics().At(i).Resource().Attributes(), v)
	}
}

func putSynthetic(m interface {
	PutBool(string, bool)
	PutStr(string, string)
}, v any) {
	switch x := v.(type) {
	case bool:
		m.PutBool(gamecontext.AttrSynthetic, x)
	case string:
		m.PutStr(gamecontext.AttrSynthetic, x)
	}
}

// syntheticGameMetrics is a gate-eligible game in its own resources, marked v on
// the resource, or on every data point when onPoints.
func syntheticGameMetrics(md pmetric.Metrics, gameID string, v any, onPoints bool) {
	from := md.ResourceMetrics().Len()
	metricGauge(md, "game_active", 1, map[string]string{"game_id": gameID})
	metricGauge(md, "game_player_alive", 1, map[string]string{"game_id": gameID, "serial": "SYN1"})
	if !onPoints {
		markMetricResources(md, from, v)
		return
	}
	for i := from; i < md.ResourceMetrics().Len(); i++ {
		dps := md.ResourceMetrics().At(i).ScopeMetrics().At(0).Metrics().At(0).Gauge().DataPoints()
		for k := 0; k < dps.Len(); k++ {
			putSynthetic(dps.At(k).Attributes(), v)
		}
	}
}

// UC: UC-041 BR-011
func TestReceiver_SyntheticTelemetryNeverReachesAGameContext(t *testing.T) {
	for _, tc := range []struct {
		name     string
		v        any
		onPoints bool
	}{
		{"resource bool", true, false},
		{"resource string", "true", false},
		{"data point bool", true, true},
		{"data point string", "true", true},
	} {
		t.Run("UC-041/BR-011 metrics marked on the "+tc.name+" open no context and wake no evaluation", func(t *testing.T) {
			h := newSyntheticHarness(t)
			md := pmetric.NewMetrics()
			syntheticGameMetrics(md, "game-S", tc.v, tc.onPoints)
			h.metrics(t, md)
			if h.hasGame("game-S") {
				t.Fatalf("partitions = %v, want no game-S", h.mux.Partitions())
			}
			if n := len(h.rules.evaluations()); n != 0 {
				t.Fatalf("evaluations = %d, want 0", n)
			}
		})
	}

	t.Run("UC-041/BR-011 spans marked on the resource or the span change no context", func(t *testing.T) {
		h := newSyntheticHarness(t)
		td := ptrace.NewTraces()
		gameIDSpan(td, "SYN1", "game-S")
		putSynthetic(td.ResourceSpans().At(0).Resource().Attributes(), true)
		gameIDSpan(td, "SYN2", "game-T")
		putSynthetic(td.ResourceSpans().At(1).ScopeSpans().At(0).Spans().At(0).Attributes(), "true")
		h.traces(t, td)
		if h.hasGame("game-S") || h.hasGame("game-T") {
			t.Fatalf("partitions = %v, want none", h.mux.Partitions())
		}
	})

	t.Run("UC-041/BR-011 a synthetic game.start opens no context", func(t *testing.T) {
		h := newSyntheticHarness(t)
		onResource := agentStartLog("game-S")
		putSynthetic(onResource.ResourceLogs().At(0).Resource().Attributes(), true)
		onRecord := agentStartLog("game-T")
		putSynthetic(onRecord.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).Attributes(), "true")
		h.logs(t, onResource)
		h.logs(t, onRecord)
		if h.hasGame("game-S") || h.hasGame("game-T") {
			t.Fatalf("partitions = %v, want none", h.mux.Partitions())
		}
	})
}

// UC: UC-041 BR-011
func TestReceiver_SyntheticLoadChangesNoDecision(t *testing.T) {
	t.Run("UC-041/BR-011 a real game with synthetic load in the same batches evaluates as without it", func(t *testing.T) {
		plain, loaded := newSyntheticHarness(t), newSyntheticHarness(t)
		for i := 0; i < 3; i++ {
			plain.metrics(t, liveGameMetrics("game-A", "PA"))

			md := liveGameMetrics("game-A", "PA")
			syntheticGameMetrics(md, "game-A", true, true) // same game id, marked points
			syntheticGameMetrics(md, "game-S", true, false)
			loaded.metrics(t, md)
		}
		if got, want := len(loaded.rules.evaluations()), len(plain.rules.evaluations()); got != want || want == 0 {
			t.Fatalf("evaluations = %d, want %d (> 0)", got, want)
		}
		if got, want := loaded.mux.Partitions(), plain.mux.Partitions(); !slices.Equal(got, want) {
			t.Fatalf("partitions = %v, want %v", got, want)
		}
		last := loaded.rules.evaluations()[len(loaded.rules.evaluations())-1]
		if _, ok := last.Players["SYN1"]; ok {
			t.Fatalf("players = %v, want no synthetic player", last.Players)
		}
	})
}

// UC: UC-041 BR-011
func TestReceiver_ObserveSyntheticFlag(t *testing.T) {
	t.Run("UC-041/BR-011 with ai.observe_synthetic on the agent ingests synthetic telemetry", func(t *testing.T) {
		h := newSyntheticHarness(t)
		h.observe.Store(true)
		md := pmetric.NewMetrics()
		syntheticGameMetrics(md, "game-S", true, false)
		h.metrics(t, md)
		if !h.hasGame("game-S") {
			t.Fatalf("partitions = %v, want game-S", h.mux.Partitions())
		}
	})

	t.Run("UC-041/BR-011 the flag is read only for a batch that holds synthetic telemetry", func(t *testing.T) {
		h := newSyntheticHarness(t)
		h.metrics(t, liveGameMetrics("game-A", "PA"))
		td := ptrace.NewTraces()
		gameIDSpan(td, "PA", "game-A")
		h.traces(t, td)
		h.logs(t, agentStartLog("game-A"))
		if n := h.flagReads.Load(); n != 0 {
			t.Fatalf("flag reads = %d, want 0 for unmarked batches", n)
		}
		md := pmetric.NewMetrics()
		syntheticGameMetrics(md, "game-S", true, false)
		h.metrics(t, md)
		if n := h.flagReads.Load(); n != 1 {
			t.Fatalf("flag reads = %d, want 1", n)
		}
	})

	t.Run("UC-041/BR-011 a pipeline without a flag source drops synthetic telemetry", func(t *testing.T) {
		h := newSyntheticHarness(t)
		h.pipe.observeSynthetic = nil
		md := pmetric.NewMetrics()
		syntheticGameMetrics(md, "game-S", true, false)
		h.metrics(t, md)
		if h.hasGame("game-S") {
			t.Fatalf("partitions = %v, want no game-S", h.mux.Partitions())
		}
	})
}

// agentStartLog is a game.start log event marked for the agent (UC-041 A8).
func agentStartLog(gameID string) plog.Logs {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "game-coordinator")
	lr := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.SetEventName("game.start")
	lr.Attributes().PutStr("orangecase.audience", "agent")
	lr.Attributes().PutStr("game.id", gameID)
	lr.Attributes().PutStr("game.kind", "real")
	lr.Attributes().PutStr("game.mode", "JoustFFA")
	return ld
}
