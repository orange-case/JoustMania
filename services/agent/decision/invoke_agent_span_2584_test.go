package decision

import (
	"context"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// UC: UC-040 main
// UC: UC-040 A1
// UC: UC-035 main
//
// #2584: the decide span follows the GenAI semconv (semantic-conventions-genai
// v0.1.0) "Invoke agent internal span" on both paths: name
// `{gen_ai.operation.name} {gen_ai.agent.name}`, kind INTERNAL and the required
// gen_ai.operation.name=invoke_agent. The name is asserted as a literal so a typo
// in SpanDecide (which the collector filter, Grafana, the dashboard and the talk
// pre-flight all match) fails here.
func TestInvokeAgentSpan_SemconvNameAndOperation(t *testing.T) {
	const wantName = "invoke_agent joustmania-agent"
	if SpanDecide != wantName {
		t.Fatalf("SpanDecide = %q, want %q", SpanDecide, wantName)
	}

	check := func(t *testing.T, span sdktrace.ReadOnlySpan) {
		t.Helper()
		if span.Name() != wantName {
			t.Errorf("span name = %q, want %q", span.Name(), wantName)
		}
		if span.SpanKind() != trace.SpanKindInternal {
			t.Errorf("span kind = %v, want INTERNAL", span.SpanKind())
		}
		if got := strAttr(t, span, "gen_ai.operation.name"); got != "invoke_agent" {
			t.Errorf("gen_ai.operation.name = %q, want invoke_agent", got)
		}
		if got := strAttr(t, span, "gen_ai.agent.name"); got != "joustmania-agent" {
			t.Errorf("gen_ai.agent.name = %q, want joustmania-agent", got)
		}
	}

	t.Run("UC-040/A1 rules path", func(t *testing.T) {
		fx := decideFixtures()[0]
		clock := time.Unix(10_000, 0)
		l, sr := decideRecordingLoop(t, rulesSnapshot())
		l.Rules = newTestEngine(fx.cfg, &clock)
		l.OnEvaluate(context.Background(), fx.ctx, testTrigger())
		ds := spansByName(sr.Ended(), wantName)
		if len(ds) != 1 {
			t.Fatalf("%q spans = %d, want 1", wantName, len(ds))
		}
		check(t, ds[0])
	})

	t.Run("UC-040/main remote path", func(t *testing.T) {
		be := newFakeDecideBackend(ffaAnswer("grant_shield", 0.9, "A", 0.5, "shield"))
		h := newRemoteHarness(t, remoteSnapshot(time.Second), be)
		check(t, h.run(t))
	})
}
