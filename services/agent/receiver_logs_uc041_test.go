package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/joustmania/agent/decision"
	"github.com/joustmania/agent/flags"
	"github.com/joustmania/agent/gamecontext"
)

// receiver_logs_uc041_test.go covers #2658 slice 2 (FR-218, UC-041 A7): the agent
// serves the OTLP logs service and a new death read from a moment log event wakes
// the game's evaluation with otlp.signal = logs.

// momentLogBatch is one moment log record of the game coordinator in game-A.
func momentLogBatch(eventName, serial, id string) plog.Logs {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "game-coordinator")
	lr := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.SetEventName(eventName)
	lr.Body().SetStr(eventName)
	lr.Attributes().PutStr("moment.id", id)
	lr.Attributes().PutStr("player.serial", serial)
	lr.Attributes().PutStr("game.id", "game-A")
	lr.Attributes().PutStr("game.kind", "real")
	return ld
}

// evaluationsBySignal reads agent_evaluations_total from reader as signal -> sum.
func evaluationsBySignal(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "agent_evaluations_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("agent_evaluations_total is %T", m.Data)
			}
			for _, dp := range sum.DataPoints {
				sig, _ := dp.Attributes.Value("signal")
				out[sig.AsString()] += dp.Value
			}
		}
	}
	return out
}

// UC: UC-041 A7
func TestLogsReceiver_MomentLogWakesEvaluation(t *testing.T) {
	// The decision Loop builds agent_evaluations_total from the global meter
	// provider at NewLoop, so a recording provider is installed for this test.
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
		_ = mp.Shutdown(context.Background())
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	clock := time.Unix(5000, 0)
	mux := gamecontext.NewMultiplexer(func(string) *gamecontext.Store {
		return gamecontext.NewStore(time.Hour, time.Hour, func() time.Time { return clock })
	})
	rules := &recordingRules{}
	loops := decision.NewLoopSet(func(string) *decision.Loop {
		l := decision.NewLoop(staticFlags{flags.Snapshot{
			Enabled:              true,
			InterventionsAllowed: []string{"grant_shield"},
			Policy:               flags.Policy{MaxInterventionsPerMinute: 100},
		}}, logger)
		l.Rules = rules
		return l
	})
	pipe := newPipeline(mux, loops, time.Hour)
	pipe.now = func() time.Time { return clock }
	metrics := &metricsReceiver{pipe: pipe}
	logs := &logsReceiver{pipe: pipe}

	md := liveGameMetrics("game-A", "PA")
	metricGauge(md, "game_player_alive", 1, map[string]string{"game_id": "game-A", "serial": "PB"})
	if _, err := metrics.Export(context.Background(), pmetricotlp.NewExportRequestFromMetrics(md)); err != nil {
		t.Fatalf("metrics export: %v", err)
	}

	t.Run("UC-041/A7 a death_warning log event wakes no evaluation", func(t *testing.T) {
		before := len(rules.evaluations())
		if _, err := logs.Export(context.Background(), plogotlp.NewExportRequestFromLogs(momentLogBatch("death_warning", "PB", "game-A/PB/1"))); err != nil {
			t.Fatalf("logs export: %v", err)
		}
		if got := len(rules.evaluations()); got != before {
			t.Fatalf("evaluations = %d, want %d", got, before)
		}
	})

	t.Run("UC-041/A7 a player_death log event wakes one evaluation with signal logs", func(t *testing.T) {
		before := len(rules.evaluations())
		if _, err := logs.Export(context.Background(), plogotlp.NewExportRequestFromLogs(momentLogBatch("player_death", "PB", "game-A/PB/2"))); err != nil {
			t.Fatalf("logs export: %v", err)
		}
		evals := rules.evaluations()
		if len(evals) != before+1 {
			t.Fatalf("evaluations = %d, want %d", len(evals), before+1)
		}
		if seq := evals[len(evals)-1].Session.EliminationSequence; len(seq) != 1 || seq[0] != "PB" {
			t.Fatalf("evaluated elimination order = %v, want [PB]", seq)
		}
		if n := evaluationsBySignal(t, reader)["logs"]; n != 1 {
			t.Fatalf("agent_evaluations_total{signal=logs} = %d, want 1", n)
		}
	})
}

// UC: UC-041 A7
func TestRun_ServesOTLPLogs(t *testing.T) {
	t.Run("UC-041/A7 the agent's gRPC server serves the OTLP logs service", func(t *testing.T) {
		c := newShutdownComponents(t)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = c.run(ctx)
		}()
		t.Cleanup(func() {
			cancel()
			<-done
		})
		waitDial(t, c.listenAddr)

		conn, err := grpc.NewClient(c.listenAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer callCancel()
		_, err = plogotlp.NewGRPCClient(conn).Export(callCtx, plogotlp.NewExportRequestFromLogs(momentLogBatch("player_death", "PB", "game-A/PB/2")))
		if status.Code(err) == codes.Unimplemented {
			t.Fatalf("logs Export = Unimplemented, want the logs receiver registered")
		}
		if err != nil {
			t.Fatalf("logs Export: %v", err)
		}
	})
}

// UC: UC-041 BR-010
func TestLogsReceiver_LogOpenedContextNeverEvaluates(t *testing.T) {
	t.Run("UC-041/BR-010 game.start and a death of an unknown game open its context and wake no evaluation", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		clock := time.Unix(5000, 0)
		mux := gamecontext.NewMultiplexer(func(string) *gamecontext.Store {
			return gamecontext.NewStore(time.Hour, time.Hour, func() time.Time { return clock })
		})
		rules := &recordingRules{}
		loops := decision.NewLoopSet(func(string) *decision.Loop {
			l := decision.NewLoop(staticFlags{flags.Snapshot{
				Enabled:              true,
				InterventionsAllowed: []string{"grant_shield"},
				Policy:               flags.Policy{MaxInterventionsPerMinute: 100},
			}}, logger)
			l.Rules = rules
			return l
		})
		pipe := newPipeline(mux, loops, time.Hour)
		pipe.now = func() time.Time { return clock }
		logs := &logsReceiver{pipe: pipe}

		ld := plog.NewLogs()
		rl := ld.ResourceLogs().AppendEmpty()
		rl.Resource().Attributes().PutStr("service.name", "game-coordinator")
		records := rl.ScopeLogs().AppendEmpty().LogRecords()
		start := records.AppendEmpty()
		start.SetEventName("game.start")
		start.Attributes().PutStr("game.id", "game-B")
		start.Attributes().PutStr("game.kind", "real")
		start.Attributes().PutStr("game.mode", "Joust")
		start.Attributes().PutStr("orangecase.audience", "agent")
		death := records.AppendEmpty()
		death.SetEventName("player_death")
		death.Attributes().PutStr("moment.id", "game-B/PB/1")
		death.Attributes().PutStr("player.serial", "PB")
		death.Attributes().PutStr("game.id", "game-B")
		death.Attributes().PutStr("game.kind", "real")
		death.Attributes().PutStr("orangecase.audience", "agent")
		if _, err := logs.Export(context.Background(), plogotlp.NewExportRequestFromLogs(ld)); err != nil {
			t.Fatalf("logs export: %v", err)
		}
		if got := len(rules.evaluations()); got != 0 {
			t.Fatalf("evaluations = %d, want 0 on log records alone", got)
		}
		snap, ok := mux.Snapshot("game-B")
		if !ok || snap.GameKind != gamecontext.GameKindReal || len(snap.Session.EliminationSequence) != 1 {
			t.Fatalf("game-B context = %v (kind %q, seq %v), want opened with the death", ok, snap.GameKind, snap.Session.EliminationSequence)
		}
	})
}
