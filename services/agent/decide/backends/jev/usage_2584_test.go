package jev

import (
	"context"
	"reflect"
	"testing"

	"go.opentelemetry.io/otel"

	"github.com/joustmania/agent/decide"
)

// UC: UC-040 A13
//
// #2584: each HTTP call of the two-stage round reports its usage to the
// invocation's accumulator, so the invoke_agent span's totals equal the sum of
// the two chat spans.
func TestUsage_TwoStageRoundSumsBothCalls(t *testing.T) {
	api := newScriptedAPI(t, ok(loadFixture(t, fxIntervention)), ok(loadFixture(t, fxGateDetails)))
	usage := &decide.Usage{}
	ctx, parent := otel.Tracer("test").Start(decide.WithUsage(context.Background(), usage), "invoke_agent joustmania-agent")
	if _, err := api.backend(fixtureModel, false).Decide(ctx, testRequest()); err != nil {
		t.Fatal(err)
	}
	parent.End()

	spans := api.chatSpans()
	if len(spans) != 2 {
		t.Fatalf("%d chat spans, want 2", len(spans))
	}
	var in, out int64
	for _, s := range spans {
		a := attrs(s)
		in += a["gen_ai.usage.input_tokens"].AsInt64()
		out += a["gen_ai.usage.output_tokens"].AsInt64()
	}
	if in == 0 || out == 0 {
		t.Fatalf("fixtures report no usage (in %d, out %d): the test proves nothing", in, out)
	}
	got := usage.Totals()
	if got.Calls != 2 {
		t.Errorf("calls = %d, want 2", got.Calls)
	}
	if !got.InputTokens.OK || int64(got.InputTokens.N) != in {
		t.Errorf("input tokens = %+v, want %d (sum of the chat spans)", got.InputTokens, in)
	}
	if !got.OutputTokens.OK || int64(got.OutputTokens.N) != out {
		t.Errorf("output tokens = %+v, want %d (sum of the chat spans)", got.OutputTokens, out)
	}
	if got.CacheReadInputTokens.OK || got.ReasoningOutputTokens.OK {
		t.Errorf("jev reports no cache or reasoning tokens, got %+v", got)
	}
}

// UC: UC-040 A2
//
// #2584: Jev reports no finish reason; a call without a response is "error" on
// the chat span and in the totals, an answered call "stop".
func TestUsage_JevFinishReasons(t *testing.T) {
	t.Run("UC-040/A2 status failure", func(t *testing.T) {
		api := newScriptedAPI(t, fakeReply{status: 500, body: []byte(`{"detail":"boom"}`)})
		usage := &decide.Usage{}
		ctx := decide.WithUsage(context.Background(), usage)
		if _, err := api.backend(fixtureModel, false).Decide(ctx, testRequest()); err == nil {
			t.Fatal("Decide succeeded")
		}
		got := usage.Totals()
		if got.InputTokens.OK || !reflect.DeepEqual(got.FinishReasons, []string{"error"}) {
			t.Errorf("totals = %+v, want no tokens and [error]", got)
		}
		spans := api.chatSpans()
		if fr := attrs(spans[len(spans)-1])["gen_ai.response.finish_reasons"].AsStringSlice(); !reflect.DeepEqual(fr, []string{"error"}) {
			t.Errorf("chat span finish_reasons = %v, want [error]", fr)
		}
	})
	t.Run("UC-040/A13 two answered stages", func(t *testing.T) {
		api := newScriptedAPI(t, ok(loadFixture(t, fxIntervention)), ok(loadFixture(t, fxGateDetails)))
		usage := &decide.Usage{}
		if _, err := api.backend(fixtureModel, false).Decide(decide.WithUsage(context.Background(), usage), testRequest()); err != nil {
			t.Fatal(err)
		}
		if got := usage.Totals().FinishReasons; !reflect.DeepEqual(got, []string{"stop", "stop"}) {
			t.Errorf("finish reasons = %v, want [stop stop] (positional, one per call)", got)
		}
	})
}
