package main

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"

	"github.com/joustmania/agent/decision"
	"github.com/joustmania/agent/gamecontext"
	"github.com/joustmania/agent/gate"
)

// Full OTLP gRPC service names, recorded as rpc.service (semconv) on the
// agent.signal_received root span.
const (
	otlpTraceService   = "opentelemetry.proto.collector.trace.v1.TraceService"
	otlpMetricsService = "opentelemetry.proto.collector.metrics.v1.MetricsService"
	otlpLogsService    = "opentelemetry.proto.collector.logs.v1.LogsService"
)

// pipeline ties the context stores and decision loops together. On each signal
// update it snapshots the relevant partition(s) and, if the gate allows, runs the
// per-game loop for that partition: the GameContext multiplexer (#845 PR B) holds
// one Store partition per game_id, fallback partition "" for unlabeled signals,
// and the per-game LoopSet (#845 PR C) one decision.Loop per partition.
//
// PER-GAME BUDGET (#845 PR C): each partition gets its OWN decision.Loop via the
// LoopSet, lazily created on first evaluation. Each Loop owns its weighted rate
// limiter (per-minute budget), its log/span throttle slot, and its per-cycle
// LayerState — so interventions from one game no longer draw down another game's
// budget, and one game's throttle no longer suppresses another's spans/logs. In
// single-game mode every signal lands on the fallback partition's single loop, so
// behavior collapses to exactly the single-store agent's.
type pipeline struct {
	mux   *gamecontext.Multiplexer
	loops *decision.LoopSet
	// playerTTL is the static fallback freshness window for the evaluate gate; used
	// only when playerTTLSource is nil (tests).
	playerTTL time.Duration
	// playerTTLSource, when non-nil, is the LIVE lifecycle.player_ttl_seconds source
	// (#927): signalUpdated reads it each gate check so the freshness window tracks
	// the same hot-reloaded TTL the store eviction uses. main.go passes the shared
	// flags.LifecycleHolder.PlayerTTL (a lock-free atomic load). Nil keeps the static
	// playerTTL, preserving the read-at-startup behavior in tests.
	playerTTLSource func() time.Duration

	now func() time.Time
}

func newPipeline(mux *gamecontext.Multiplexer, loops *decision.LoopSet, playerTTL time.Duration) *pipeline {
	return &pipeline{
		mux:       mux,
		loops:     loops,
		playerTTL: playerTTL,
		now:       time.Now,
	}
}

// withPlayerTTLSource installs the LIVE player-TTL source for the evaluate gate
// (#927) so the freshness window hot-reloads in lockstep with the store eviction
// TTL. Returns the pipeline for chaining at construction.
func (p *pipeline) withPlayerTTLSource(src func() time.Duration) *pipeline {
	p.playerTTLSource = src
	return p
}

// effectivePlayerTTL returns the live player-TTL when a source is wired and
// reports a positive duration, else the static construction value.
func (p *pipeline) effectivePlayerTTL() time.Duration {
	if p.playerTTLSource != nil {
		if d := p.playerTTLSource(); d > 0 {
			return d
		}
	}
	return p.playerTTL
}

// signalUpdated is called after a received signal mutates one or more partitions.
// gameIDs is the deduped set of partitions an Apply pass touched. For each, the
// partition is snapshotted and, if the gate allows, evaluated through THAT game's
// own Loop (per-game budget/throttle, #845 PR C). The caller's gRPC context and
// EvalTrigger flow through so the decision loop can emit its audit trace (issue
// #724) with accurate timing and rpc.* attributes.
func (p *pipeline) signalUpdated(ctx context.Context, gameIDs []string, trig decision.EvalTrigger) {
	now := p.now
	if now == nil {
		now = time.Now
	}
	t := now()
	for _, gameID := range gameIDs {
		// A partition can be evicted between Apply and here; Snapshot reports !ok and
		// we simply skip it (a resumed signal recreates the partition next batch).
		snap, ok := p.mux.Snapshot(gameID)
		if !ok {
			continue
		}
		if gate.ShouldEvaluate(snap, t, p.effectivePlayerTTL()) {
			// loops.For lazily creates this game's Loop on first touch, so its budget
			// and throttle state are isolated from every other game's.
			p.loops.For(gameID).OnEvaluate(ctx, snap, trig)
		}
	}
}

// traceReceiver implements the OTLP trace gRPC service.
type traceReceiver struct {
	ptraceotlp.UnimplementedGRPCServer
	pipe *pipeline
}

// Export ingests a batch of traces into the game multiplexer, which recognizes
// only its own span kinds and skips the agent's own spans.
func (r *traceReceiver) Export(ctx context.Context, req ptraceotlp.ExportRequest) (ptraceotlp.ExportResponse, error) {
	t0 := time.Now()
	td := req.Traces()
	if ids := r.pipe.mux.ApplySpans(td); len(ids) > 0 {
		r.pipe.signalUpdated(ctx, ids, decision.EvalTrigger{
			Signal:     "traces",
			RPCService: otlpTraceService,
			T0:         t0,
		})
	}
	return ptraceotlp.NewExportResponse(), nil
}

// metricsReceiver implements the OTLP metrics gRPC service.
type metricsReceiver struct {
	pmetricotlp.UnimplementedGRPCServer
	pipe *pipeline
}

// Export ingests a batch of metrics.
func (r *metricsReceiver) Export(ctx context.Context, req pmetricotlp.ExportRequest) (pmetricotlp.ExportResponse, error) {
	t0 := time.Now()
	if ids := r.pipe.mux.ApplyMetrics(req.Metrics()); len(ids) > 0 {
		r.pipe.signalUpdated(ctx, ids, decision.EvalTrigger{
			Signal:     "metrics",
			RPCService: otlpMetricsService,
			T0:         t0,
		})
	}
	return pmetricotlp.NewExportResponse(), nil
}

// logsReceiver implements the OTLP logs gRPC service: moment log events and the
// agent-only game.start / game.phase.start / game.end log events (UC-041 A7, A8,
// FR-218, FR-251, FR-252). The
// collector forwards nothing else (filter/agent_moments) and the extractor ignores
// any other record. A new death wakes the game's evaluation with otlp.signal =
// logs; the gate still needs live players, so log records alone never evaluate
// (BR-010).
type logsReceiver struct {
	plogotlp.UnimplementedGRPCServer
	pipe *pipeline
}

// Export ingests a batch of log records.
func (r *logsReceiver) Export(ctx context.Context, req plogotlp.ExportRequest) (plogotlp.ExportResponse, error) {
	t0 := time.Now()
	if ids := r.pipe.mux.ApplyLogs(req.Logs()); len(ids) > 0 {
		r.pipe.signalUpdated(ctx, ids, decision.EvalTrigger{
			Signal:     "logs",
			RPCService: otlpLogsService,
			T0:         t0,
		})
	}
	return plogotlp.NewExportResponse(), nil
}
