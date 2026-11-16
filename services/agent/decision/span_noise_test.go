package decision

import (
	"context"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/joustmania/agent/flags"
	"github.com/joustmania/agent/gamecontext"
)

// span_noise_test.go covers #2648: fewer agent spans in the game trace. The OTLP Export
// that triggers a cycle is an event, not a span; decide.questions is written once.

// requireSignalEvent fails unless span carries one agent.signal_received event for the
// given OTLP signal, with the rpc.* attributes the old span had.
func requireSignalEvent(t *testing.T, span sdktrace.ReadOnlySpan, signal string) {
	t.Helper()
	var found int
	for _, ev := range span.Events() {
		if ev.Name != SignalReceived {
			continue
		}
		found++
		got := map[string]string{}
		for _, kv := range ev.Attributes {
			got[string(kv.Key)] = kv.Value.Emit()
		}
		if got["otlp.signal"] != signal || got["rpc.method"] != "Export" || got["rpc.system"] != "grpc" || got["rpc.service"] == "" {
			t.Errorf("%s: signal event attrs = %v, want otlp.signal=%s rpc.method=Export rpc.system=grpc and an rpc.service", span.Name(), got, signal)
		}
	}
	if found != 1 {
		t.Fatalf("%s: %d %s events, want 1", span.Name(), found, SignalReceived)
	}
}

// UC: UC-040 BR-013
func TestSignalReceived_IsAnEventInGame(t *testing.T) {
	for _, signal := range []string{"metrics", "traces"} {
		t.Run("UC-040/BR-013 rules decide in a game: "+signal+" export is an event on agent.decide", func(t *testing.T) {
			fx := decideFixtures()[0]
			fx.ctx.GameKind = "real"
			fx.ctx.GameTraceID, fx.ctx.GameTraceSpanID = hubGameTrace, hubGameSpanID
			fx.ctx.GameplayPhaseSpanID, fx.ctx.GameCycleSpanID = hubGameplayPhaseSpanID, hubGameCycleSpanID
			clock := time.Unix(10_000, 0)
			l, sr := decideRecordingLoop(t, rulesSnapshot())
			l.Rules = newTestEngine(fx.cfg, &clock)
			trig := EvalTrigger{Signal: signal, RPCService: "svc." + signal, T0: time.Unix(9_999, 123)}
			l.OnEvaluate(context.Background(), fx.ctx, trig)

			spans := sr.Ended()
			if n := len(spansByName(spans, SignalReceived)); n != 0 {
				t.Fatalf("agent.signal_received spans in the game trace = %d, want 0", n)
			}
			ds := spansByName(spans, SpanDecide)
			if len(ds) != 1 {
				t.Fatalf("agent.decide spans = %d, want 1", len(ds))
			}
			if p := ds[0].Parent(); p.SpanID().String() != hubGameCycleSpanID || !p.IsRemote() {
				t.Fatalf("agent.decide parent = %v, want the remote game_cycle", p)
			}
			requireSignalEvent(t, ds[0], signal)
			if at := ds[0].Events()[0].Time; !at.Equal(trig.T0) {
				t.Errorf("event time = %v, want the Export's arrival %v", at, trig.T0)
			}
			for _, d := range spansByName(spans, SpanDecision) {
				if p := d.Parent(); p.SpanID().String() != hubGameCycleSpanID {
					t.Errorf("agent.decision parent = %v, want the remote game_cycle", p)
				}
			}
		})
	}
	t.Run("UC-040/BR-013 kill switch in a game: agent.disabled carries the event", func(t *testing.T) {
		l, sr := recordingLoop(t, flags.Snapshot{Enabled: false}, nil)
		c := gameCycleCtx("game_sn01")
		l.OnEvaluate(context.Background(), c, EvalTrigger{Signal: "traces", RPCService: "svc.traces", T0: time.Now()})
		spans := sr.Ended()
		disabled := spansByName(spans, SpanDisabled)
		if len(disabled) != 1 || len(spansByName(spans, SignalReceived)) != 0 {
			t.Fatalf("disabled=%d signal_received=%d, want 1 and 0", len(disabled), len(spansByName(spans, SignalReceived)))
		}
		requireSignalEvent(t, disabled[0], "traces")
	})
	t.Run("UC-040/BR-013 without game ids the cycle keeps its agent.signal_received root", func(t *testing.T) {
		fx := decideFixtures()[0]
		clock := time.Unix(10_000, 0)
		l, sr := decideRecordingLoop(t, rulesSnapshot())
		l.Rules = newTestEngine(fx.cfg, &clock)
		l.OnEvaluate(context.Background(), fx.ctx, testTrigger())
		spans := sr.Ended()
		if len(spansByName(spans, SignalReceived)) != 1 {
			t.Fatal("want the own-root agent.signal_received span when no game ids are known")
		}
		if ev := spansByName(spans, SpanDecide)[0].Events(); len(ev) != 0 {
			t.Errorf("agent.decide events = %v, want none (the span carries the trigger)", ev)
		}
	})
}

// UC: UC-040 BR-013
func TestDecideQuestions_OncePerGame(t *testing.T) {
	fx := decideFixtures()[0]
	clock := time.Unix(10_000, 0)
	l, sr := decideRecordingLoop(t, rulesSnapshot())
	l.Rules = newTestEngine(fx.cfg, &clock)
	_, rec := l.rulesDecide(context.Background(), rulesSnapshot(), l.Rules, fx.ctx, flags.DecisionBackendRules, "")
	if rec == nil {
		t.Fatal("no decide record")
	}
	emit := func(c gamecontext.GameContext) { l.emitDecideSpan(context.Background(), rec, c) }
	other := fx.ctx
	other.SessionID = "game_other"
	emit(fx.ctx)
	emit(fx.ctx)
	emit(fx.ctx)
	emit(other)

	ds := spansByName(sr.Ended(), SpanDecide)
	if len(ds) != 4 {
		t.Fatalf("agent.decide spans = %d, want 4", len(ds))
	}
	has := func(s sdktrace.ReadOnlySpan, key string) bool { _, ok := attrValue(s, key); return ok }
	wantSHA := questionsSHA(strAttr(t, ds[0], AttrDecideQuestions))
	if len(wantSHA) != questionsSHALen {
		t.Fatalf("sha %q is not %d chars", wantSHA, questionsSHALen)
	}
	for i, s := range ds {
		if got := strAttr(t, s, AttrDecideQuestionsSHA); got != wantSHA {
			t.Errorf("decide %d: %s = %q, want %q on every decide", i, AttrDecideQuestionsSHA, got, wantSHA)
		}
	}
	for i, wantQuestions := range []bool{true, false, false, true} {
		if got := has(ds[i], AttrDecideQuestions); got != wantQuestions {
			t.Errorf("decide %d: decide.questions present = %v, want %v (written once per game)", i, got, wantQuestions)
		}
	}
}

// UC: UC-040 BR-013
func TestClaimQuestions_ChangedSetIsWrittenAgain(t *testing.T) {
	l, _ := decideRecordingLoop(t, rulesSnapshot())
	for i, tc := range []struct {
		game, sha string
		want      bool
	}{
		{"g1", "aaa", true}, {"g1", "aaa", false}, {"g1", "bbb", true}, {"g1", "aaa", false}, {"g2", "aaa", true},
	} {
		if got := l.claimQuestions(tc.game, tc.sha); got != tc.want {
			t.Errorf("claim %d (%s,%s) = %v, want %v", i, tc.game, tc.sha, got, tc.want)
		}
	}
}

// UC: UC-040 BR-013
func TestRulesDecisions_ShareTheDecideStateDigest(t *testing.T) {
	fx := decideFixtures()[0]
	fx.ctx.GameTraceID, fx.ctx.GameTraceSpanID = hubGameTrace, hubGameSpanID
	fx.ctx.GameCycleSpanID = hubGameCycleSpanID
	clock := time.Unix(10_000, 0)
	l, sr := decideRecordingLoop(t, rulesSnapshot())
	l.Rules = newTestEngine(fx.cfg, &clock)
	l.OnEvaluate(context.Background(), fx.ctx, testTrigger())

	spans := sr.Ended()
	ds, dec := spansByName(spans, SpanDecide), spansByName(spans, SpanDecision)
	if len(ds) != 1 || len(dec) == 0 {
		t.Fatalf("decide=%d decision=%d", len(ds), len(dec))
	}
	want := strAttr(t, ds[0], AttrDecideStateDigest)
	for _, d := range dec {
		if got := strAttr(t, d, AttrDecideStateDigest); got != want {
			t.Errorf("agent.decision %s = %q, want the decide span's %q", AttrDecideStateDigest, got, want)
		}
	}
}
