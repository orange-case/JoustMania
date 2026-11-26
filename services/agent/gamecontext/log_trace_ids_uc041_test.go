package gamecontext

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/otel/trace"
)

const (
	logTraceHex = "4bf92f3577b34da6a3ce929d0e0e4736"
	logRootHex  = "051581bf3cb55c13"
	logPhaseHex = "00f067aa0ba902b7"
	logCycleHex = "b7ad6b7169203331"
)

func hexTraceID(t *testing.T, s string) pcommon.TraceID {
	t.Helper()
	var id pcommon.TraceID
	if n, err := hex.Decode(id[:], []byte(s)); err != nil || n != len(id) {
		t.Fatalf("trace id %q", s)
	}
	return id
}

func hexSpanID(t *testing.T, s string) pcommon.SpanID {
	t.Helper()
	var id pcommon.SpanID
	if n, err := hex.Decode(id[:], []byte(s)); err != nil || n != len(id) {
		t.Fatalf("span id %q", s)
	}
	return id
}

// withTrace sets the only record's trace context; empty ids stay zero (invalid).
func withTrace(t *testing.T, ld plog.Logs, traceHex, spanHex string, sampled bool) plog.Logs {
	t.Helper()
	lr := ld.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0)
	if traceHex != "" {
		lr.SetTraceID(hexTraceID(t, traceHex))
	}
	if spanHex != "" {
		lr.SetSpanID(hexSpanID(t, spanHex))
	}
	if sampled {
		lr.SetFlags(plog.DefaultLogRecordFlags.WithIsSampled(true))
	}
	return ld
}

// phaseStartLog is an agent-only game.phase.start record of gameID for span.
func phaseStartLog(gameID, span string) plog.Logs {
	return logEvent("game-coordinator", "game.phase.start", map[string]string{
		"game.id": gameID, "game.kind": "real", "game.phase.span": span, "orangecase.audience": "agent",
	})
}

type traceIDs struct{ trace, root, phase, cycle string }

func traceIDsOf(t *testing.T, mux *Multiplexer, gameID string) traceIDs {
	t.Helper()
	snap, ok := mux.Snapshot(gameID)
	if !ok {
		t.Fatalf("no %s context", gameID)
	}
	return traceIDs{snap.GameTraceID, snap.GameTraceSpanID, snap.GameplayPhaseSpanID, snap.GameCycleSpanID}
}

// UC: UC-041 A8
// UC: UC-041 BR-004
func TestApplyLogs_TraceIDsFromLogContext(t *testing.T) {
	newMux := func() *Multiplexer {
		return newTestMux(&muxClock{t: time.Unix(4000, 0)}, 5*time.Second, 15*time.Second, nil, nil)
	}

	t.Run("UC-041/A8 a sampled game.start sets the trace id and root span id", func(t *testing.T) {
		mux := newMux()
		mux.ApplyLogs(withTrace(t, lifecycleLog("game.start", "game-B", "real", "", time.Time{}), logTraceHex, logRootHex, true))
		got := traceIDsOf(t, mux, "game-B")
		if want := (traceIDs{trace: logTraceHex, root: logRootHex}); got != want {
			t.Fatalf("ids = %+v, want %+v", got, want)
		}
		snap, _ := mux.Snapshot("game-B")
		if len(snap.Players) != 0 || (snap.Session.GameActive != nil && *snap.Session.GameActive) {
			t.Fatal("game.start added a player or set the game active")
		}
		if !snap.UpdatedAt.IsZero() {
			t.Fatalf("UpdatedAt = %v, want zero: a log record is never a live signal (BR-010)", snap.UpdatedAt)
		}
	})

	t.Run("UC-041/A8 a sampled game.phase.start sets the game.phase.gameplay and game.cycle span ids", func(t *testing.T) {
		mux := newMux()
		mux.ApplyLogs(withTrace(t, phaseStartLog("game-B", "game.phase.gameplay"), logTraceHex, logPhaseHex, true))
		if got, want := traceIDsOf(t, mux, "game-B"), (traceIDs{trace: logTraceHex, phase: logPhaseHex}); got != want {
			t.Fatalf("after game.phase.gameplay ids = %+v, want %+v", got, want)
		}
		mux.ApplyLogs(withTrace(t, phaseStartLog("game-B", "game.cycle"), logTraceHex, logCycleHex, true))
		if got, want := traceIDsOf(t, mux, "game-B"), (traceIDs{trace: logTraceHex, phase: logPhaseHex, cycle: logCycleHex}); got != want {
			t.Fatalf("after game.cycle ids = %+v, want %+v", got, want)
		}
		snap, _ := mux.Snapshot("game-B")
		if len(snap.Players) != 0 || (snap.Session.GameActive != nil && *snap.Session.GameActive) {
			t.Fatal("game.phase.start added a player or set the game active")
		}
		if !snap.UpdatedAt.IsZero() {
			t.Fatalf("UpdatedAt = %v, want zero: a log record is never a live signal (BR-010)", snap.UpdatedAt)
		}
	})

	t.Run("UC-041/BR-010 a context opened by game.phase.start is held for 60 s", func(t *testing.T) {
		clk := &muxClock{t: time.Unix(4000, 0)}
		mux := newTestMux(clk, 5*time.Second, 15*time.Second, nil, nil)
		mux.ApplyLogs(withTrace(t, phaseStartLog("game-B", "game.cycle"), logTraceHex, logCycleHex, true))

		clk.advance(logHoldFor - time.Second)
		if removed := mux.EvictStale(); len(removed) != 0 {
			t.Fatalf("removed %v within the hold", removed)
		}
		if got := traceIDsOf(t, mux, "game-B"); got.cycle != logCycleHex {
			t.Fatalf("cycle id = %q after the hold, want %q", got.cycle, logCycleHex)
		}
	})

	t.Run("UC-041/BR-004 an unsampled or invalid trace context or an unknown span sets no id", func(t *testing.T) {
		cases := map[string]plog.Logs{
			"unsampled start":     withTrace(t, lifecycleLog("game.start", "game-B", "real", "", time.Time{}), logTraceHex, logRootHex, false),
			"no trace id":         withTrace(t, lifecycleLog("game.start", "game-B", "real", "", time.Time{}), "", logRootHex, true),
			"no span id":          withTrace(t, lifecycleLog("game.start", "game-B", "real", "", time.Time{}), logTraceHex, "", true),
			"unsampled phase":     withTrace(t, phaseStartLog("game-B", "game.phase.gameplay"), logTraceHex, logPhaseHex, false),
			"invalid phase":       withTrace(t, phaseStartLog("game-B", "game.cycle"), "", logCycleHex, true),
			"unknown phase span":  withTrace(t, phaseStartLog("game-B", "round"), logTraceHex, logPhaseHex, true),
			"phase without trace": phaseStartLog("game-B", "game.phase.gameplay"),
		}
		for name, ld := range cases {
			mux := newMux()
			mux.ApplyLogs(ld)
			if got := traceIDsOf(t, mux, "game-B"); got != (traceIDs{}) {
				t.Fatalf("%s: ids = %+v, want none", name, got)
			}
		}
	})

	t.Run("UC-041/A8 an unmarked game.start or game.phase.start is ignored", func(t *testing.T) {
		mux := newMux()
		mux.ApplyLogs(withTrace(t, logEvent("game-coordinator", "game.start", map[string]string{"game.id": "game-B"}), logTraceHex, logRootHex, true))
		mux.ApplyLogs(withTrace(t, logEvent("game-coordinator", "game.phase.start", map[string]string{
			"game.id": "game-B", "game.phase.span": "game.phase.gameplay",
		}), logTraceHex, logPhaseHex, true))
		if _, ok := mux.Snapshot("game-B"); ok {
			t.Fatal("an unmarked record opened a context")
		}
	})

	t.Run("UC-041/A8 the agent's own game.phase.start is skipped", func(t *testing.T) {
		mux := newMux()
		mux.SetOwnService("agent")
		ld := withTrace(t, phaseStartLog("game-B", "game.phase.gameplay"), logTraceHex, logPhaseHex, true)
		ld.ResourceLogs().At(0).Resource().Attributes().PutStr("service.name", "agent")
		mux.ApplyLogs(ld)
		if _, ok := mux.Snapshot("game-B"); ok {
			t.Fatal("the agent's own record opened a context")
		}
	})

	t.Run("UC-041/BR-004 a newer game.cycle record replaces the cycle id", func(t *testing.T) {
		mux := newMux()
		mux.ApplyLogs(withTrace(t, phaseStartLog("game-B", "game.cycle"), logTraceHex, logRootHex, true))
		mux.ApplyLogs(withTrace(t, phaseStartLog("game-B", "game.cycle"), logTraceHex, logCycleHex, true))
		if got := traceIDsOf(t, mux, "game-B"); got.cycle != logCycleHex {
			t.Fatalf("cycle id = %q, want the newer %q", got.cycle, logCycleHex)
		}
	})
}

// UC: UC-041 BR-004
// UC: UC-040 BR-013
func TestApplyLogs_PhaseStartGivesTraceID(t *testing.T) {
	const otherTraceHex = "0af7651916cd43dd8448eb211c80319c"
	newMux := func() *Multiplexer {
		return newTestMux(&muxClock{t: time.Unix(4000, 0)}, 5*time.Second, 15*time.Second, nil, nil)
	}

	t.Run("UC-041/BR-004 a phase record alone gives the trace id, not the root span id", func(t *testing.T) {
		mux := newMux()
		mux.ApplyLogs(withTrace(t, phaseStartLog("game-B", "game.cycle"), logTraceHex, logCycleHex, true))
		if got, want := traceIDsOf(t, mux, "game-B"), (traceIDs{trace: logTraceHex, cycle: logCycleHex}); got != want {
			t.Fatalf("ids = %+v, want %+v", got, want)
		}
		snap, _ := mux.Snapshot("game-B")
		if !snap.UpdatedAt.IsZero() {
			t.Fatalf("UpdatedAt = %v, want zero: a log record is never a live signal (BR-010)", snap.UpdatedAt)
		}
	})

	t.Run("UC-041/BR-004 a later game.start fills the root span id", func(t *testing.T) {
		mux := newMux()
		mux.ApplyLogs(withTrace(t, phaseStartLog("game-B", "game.phase.gameplay"), logTraceHex, logPhaseHex, true))
		mux.ApplyLogs(withTrace(t, lifecycleLog("game.start", "game-B", "real", "", time.Time{}), logTraceHex, logRootHex, true))
		if got, want := traceIDsOf(t, mux, "game-B"), (traceIDs{trace: logTraceHex, root: logRootHex, phase: logPhaseHex}); got != want {
			t.Fatalf("ids = %+v, want %+v", got, want)
		}
	})

	t.Run("UC-041/BR-004 a phase record never overwrites a known trace id", func(t *testing.T) {
		mux := newMux()
		mux.ApplyLogs(withTrace(t, lifecycleLog("game.start", "game-B", "real", "", time.Time{}), logTraceHex, logRootHex, true))
		mux.ApplyLogs(withTrace(t, phaseStartLog("game-B", "game.cycle"), otherTraceHex, logCycleHex, true))
		if got, want := traceIDsOf(t, mux, "game-B"), (traceIDs{trace: logTraceHex, root: logRootHex, cycle: logCycleHex}); got != want {
			t.Fatalf("after game.start ids = %+v, want %+v", got, want)
		}

		mux = newMux()
		mux.ApplyLogs(withTrace(t, phaseStartLog("game-B", "game.phase.gameplay"), logTraceHex, logPhaseHex, true))
		mux.ApplyLogs(withTrace(t, phaseStartLog("game-B", "game.cycle"), otherTraceHex, logCycleHex, true))
		if got := traceIDsOf(t, mux, "game-B"); got.trace != logTraceHex {
			t.Fatalf("trace id = %q, want the first phase record's %q", got.trace, logTraceHex)
		}
	})

	t.Run("UC-040/BR-013 without the root span id the in-game parent is the cycle span and there is no Link", func(t *testing.T) {
		mux := newMux()
		mux.ApplyLogs(withTrace(t, phaseStartLog("game-B", "game.cycle"), logTraceHex, logCycleHex, true))
		snap, _ := mux.Snapshot("game-B")
		ctx, ok := RemoteParent(context.Background(), snap.GameTraceID, snap.GameCycleSpanID)
		if !ok {
			t.Fatal("no remote parent from the phase record's ids")
		}
		sc := trace.SpanContextFromContext(ctx)
		if sc.TraceID().String() != logTraceHex || sc.SpanID().String() != logCycleHex {
			t.Fatalf("parent = %s/%s, want %s/%s", sc.TraceID(), sc.SpanID(), logTraceHex, logCycleHex)
		}
		if opts := TraceLink(snap.GameTraceID, snap.GameTraceSpanID, "game.trace_id", "game.trace_span_id"); len(opts) != 0 {
			t.Fatalf("Link options = %d, want none without the root span id", len(opts))
		}
	})
}

// UC: UC-041 A8
// A coordinator from before #2837 still sends the old phase span values (telemetry.md §8).
func TestApplyLogs_LegacyPhaseSpanValues(t *testing.T) {
	mux := newTestMux(&muxClock{t: time.Unix(4000, 0)}, 5*time.Second, 15*time.Second, nil, nil)
	mux.ApplyLogs(withTrace(t, phaseStartLog("game-L", "gameplay_phase"), logTraceHex, logPhaseHex, true))
	mux.ApplyLogs(withTrace(t, phaseStartLog("game-L", "game_cycle"), logTraceHex, logCycleHex, true))
	if got, want := traceIDsOf(t, mux, "game-L"), (traceIDs{trace: logTraceHex, phase: logPhaseHex, cycle: logCycleHex}); got != want {
		t.Fatalf("ids = %+v, want %+v", got, want)
	}
}
