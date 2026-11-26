package decision

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/joustmania/agent/gamecontext"
)

// gameplay_phase_parent_test.go is the #1195 assertion suite (refines #1187): the
// agent's IN-GAME decision chain (signal_received -> decision -> action; the async
// infer.call + apply spans) re-parents under the coordinator's game.phase.gameplay span
// when its span_id is present, with a STRICT two-tier fallback so correlation is
// never lost:
//
//  1. game.phase.gameplay span_id present  -> child of game.phase.gameplay (the new target).
//  2. game.phase.gameplay absent, game root present -> child of game root (#1187 behavior).
//  3. both absent -> own-root (pre-#1187 shape).
//
// game.phase.gameplay is in the SAME trace as the game root (GameTraceID is identical),
// so the only thing that changes between tiers 1 and 2 is the parent span_id.

// A distinct, valid game.phase.gameplay span id within the same game trace as hubGameSpanID.
const hubGameplayPhaseSpanID = "00f067aa0ba902b7" // valid 8-byte hex span id

// gameplayPhaseCtx is gameTraceCtx additionally carrying the game.phase.gameplay span id.
func gameplayPhaseCtx(gameID string) gamecontext.GameContext {
	c := gameTraceCtx(gameID)
	c.GameplayPhaseSpanID = hubGameplayPhaseSpanID
	return c
}

// TestSignalReceived_ChildOfGameplayPhaseSpan: the sync cycle-root agent.signal_received
// span (and thus the whole sync decision->action subtree) parents under the game.phase.gameplay
// span when its id is present — NOT the game root span.
func TestSignalReceived_ChildOfGameplayPhaseSpan(t *testing.T) {
	l, sr, _ := meteredTestLoop(t)
	l.Rules = fakeRules{out: []Decision{{Intervention: "noop", Reason: "x"}}}
	l.Actions = &fakeSink{}

	l.OnEvaluate(context.Background(), gameplayPhaseCtx("game_gp01"),
		EvalTrigger{Signal: "metrics", T0: time.Now(), RPCService: "svc"})

	// #2648: in a game there is no signal_received span; the decision is the child.
	if n := len(spansByName(sr.Ended(), SignalReceived)); n != 0 {
		t.Fatalf("agent.signal_received spans in the game trace = %d, want 0", n)
	}
	root := spansByName(sr.Ended(), SpanDecision)[0]
	wantTID, _ := trace.TraceIDFromHex(hubGameTrace)
	wantSID, _ := trace.SpanIDFromHex(hubGameplayPhaseSpanID)
	rootSID, _ := trace.SpanIDFromHex(hubGameSpanID)

	if root.Parent().SpanID() != wantSID {
		t.Errorf("signal_received parent span_id = %s, want game.phase.gameplay %s", root.Parent().SpanID(), wantSID)
	}
	if root.Parent().SpanID() == rootSID {
		t.Errorf("signal_received parented under the GAME ROOT span %s; want game.phase.gameplay %s", rootSID, wantSID)
	}
	if root.Parent().TraceID() != wantTID {
		t.Errorf("signal_received parent trace_id = %s, want game trace %s", root.Parent().TraceID(), wantTID)
	}
	if !root.Parent().IsRemote() {
		t.Error("signal_received parent must be the REMOTE game.phase.gameplay span")
	}
	if root.SpanContext().TraceID() != wantTID {
		t.Errorf("signal_received must live in the game trace; got %s want %s", root.SpanContext().TraceID(), wantTID)
	}
}

// TestSignalReceived_FallsBackToGameRootWhenGameplayPhaseAbsent: with the game.phase.gameplay
// id empty but the game root id present, the span parents under the game ROOT (tier 2 —
// the #1187 behavior). This proves the fallback never loses the existing correlation.
func TestSignalReceived_FallsBackToGameRootWhenGameplayPhaseAbsent(t *testing.T) {
	l, sr, _ := meteredTestLoop(t)
	l.Rules = fakeRules{out: []Decision{{Intervention: "noop", Reason: "x"}}}
	l.Actions = &fakeSink{}

	// gameTraceCtx has GameplayPhaseSpanID == "" (tier-2 input).
	l.OnEvaluate(context.Background(), gameTraceCtx("game_gp02"),
		EvalTrigger{Signal: "metrics", T0: time.Now(), RPCService: "svc"})

	root := spansByName(sr.Ended(), SpanDecision)[0]
	wantSID, _ := trace.SpanIDFromHex(hubGameSpanID)
	if root.Parent().SpanID() != wantSID {
		t.Errorf("signal_received parent span_id = %s, want game ROOT %s (fallback)", root.Parent().SpanID(), wantSID)
	}
	if !root.Parent().IsRemote() {
		t.Error("signal_received parent must be the REMOTE game root span on fallback")
	}
}

// TestSignalReceived_OwnRootWhenBothAbsent: tier 3 — both ids absent -> own-root span,
// the pre-#1187 shape.
func TestSignalReceived_OwnRootWhenBothAbsent(t *testing.T) {
	l, sr, _ := meteredTestLoop(t)
	l.Rules = fakeRules{out: []Decision{{Intervention: "noop", Reason: "x"}}}
	l.Actions = &fakeSink{}

	l.OnEvaluate(context.Background(), gamecontext.GameContext{SessionID: "s"},
		EvalTrigger{Signal: "metrics", T0: time.Now(), RPCService: "svc"})

	root := spansByName(sr.Ended(), SignalReceived)[0]
	if root.Parent().IsValid() {
		t.Errorf("signal_received must be own-root when no ids; parent=%v", root.Parent())
	}
}

// TestInGameRemoteParent_TwoTierFallback exercises the resolver directly: game.phase.gameplay
// wins when valid; game root is used when game.phase.gameplay is empty/invalid; own-root when
// both are empty/invalid.
func TestInGameRemoteParent_TwoTierFallback(t *testing.T) {
	parentSpanID := func(ctx context.Context) trace.SpanID {
		return trace.SpanContextFromContext(ctx).SpanID()
	}

	t.Run("game.phase.gameplay wins", func(t *testing.T) {
		ctx, ok := inGameRemoteParent(context.Background(), hubGameTrace, hubGameSpanID, hubGameplayPhaseSpanID, "")
		want, _ := trace.SpanIDFromHex(hubGameplayPhaseSpanID)
		if !ok || parentSpanID(ctx) != want {
			t.Errorf("want game.phase.gameplay parent %s (ok=%v), got %s", want, ok, parentSpanID(ctx))
		}
	})

	t.Run("falls back to game root when game.phase.gameplay empty", func(t *testing.T) {
		ctx, ok := inGameRemoteParent(context.Background(), hubGameTrace, hubGameSpanID, "", "")
		want, _ := trace.SpanIDFromHex(hubGameSpanID)
		if !ok || parentSpanID(ctx) != want {
			t.Errorf("want game-root parent %s (ok=%v), got %s", want, ok, parentSpanID(ctx))
		}
	})

	t.Run("falls back to game root when game.phase.gameplay invalid", func(t *testing.T) {
		ctx, ok := inGameRemoteParent(context.Background(), hubGameTrace, hubGameSpanID, "not-hex", "")
		want, _ := trace.SpanIDFromHex(hubGameSpanID)
		if !ok || parentSpanID(ctx) != want {
			t.Errorf("want game-root parent %s (ok=%v), got %s", want, ok, parentSpanID(ctx))
		}
	})

	t.Run("own-root when both empty", func(t *testing.T) {
		base := context.Background()
		ctx, ok := inGameRemoteParent(base, hubGameTrace, "", "", "")
		if ok || trace.SpanContextFromContext(ctx).IsValid() {
			t.Errorf("want own-root (ok=false), got ok=%v parent=%v", ok, trace.SpanContextFromContext(ctx))
		}
	})

	t.Run("own-root when trace id empty", func(t *testing.T) {
		ctx, ok := inGameRemoteParent(context.Background(), "", hubGameSpanID, hubGameplayPhaseSpanID, "")
		if ok || trace.SpanContextFromContext(ctx).IsValid() {
			t.Errorf("want own-root when trace id empty (ok=false), got ok=%v", ok)
		}
	})
}
