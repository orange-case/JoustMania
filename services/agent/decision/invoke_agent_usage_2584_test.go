package decision

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/joustmania/agent/decide"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// usageBackend is a remote backend whose Decide reports one CallUsage per model
// call it "makes" (like jev's two-stage round), then answers or fails.
type usageBackend struct {
	calls    []decide.CallUsage
	answer   func(decide.Request) (decide.Response, error)
	settings *decide.RequestSettings
}

func (b *usageBackend) Name() string    { return "claude" }
func (b *usageBackend) Available() bool { return true }
func (b *usageBackend) Decide(ctx context.Context, req decide.Request) (decide.Response, error) {
	for _, c := range b.calls {
		decide.RecordCall(ctx, c)
	}
	return b.answer(req)
}

// settingsBackend adds the optional RequestSettingsReporter.
type settingsBackend struct{ usageBackend }

func (b *settingsBackend) RequestSettings() decide.RequestSettings { return *b.settings }

func intAttr(t *testing.T, span sdktrace.ReadOnlySpan, key string) (int64, bool) {
	t.Helper()
	v, ok := attrValue(span, key)
	return v.AsInt64(), ok
}

var usageKeys = []string{
	"gen_ai.usage.input_tokens",
	"gen_ai.usage.output_tokens",
	"gen_ai.usage.cache_read.input_tokens",
	"gen_ai.usage.cache_write.input_tokens",
	"gen_ai.usage.reasoning.output_tokens",
	"gen_ai.response.finish_reasons",
}

// UC: UC-040 main
// UC: UC-040 A2
// UC: UC-040 A1
//
// #2584: the invoke_agent span carries the summed usage of the invocation's model
// calls (semantic-conventions-genai v0.1.0 names), set at span end; a total only
// when at least one call reported it; the calls of a decision that fell back
// still count; the rules path has none.
func TestInvokeAgentSpan_SummedUsage(t *testing.T) {
	answer := ffaAnswer("grant_shield", 0.9, "A", 0.5, "shield")

	t.Run("UC-040/main two calls are summed, finish reasons distinct in call order", func(t *testing.T) {
		be := &usageBackend{answer: answer, calls: []decide.CallUsage{
			{
				InputTokens: decide.Tokens(100), OutputTokens: decide.Tokens(20),
				CacheReadInputTokens: decide.Tokens(30), CacheWriteInputTokens: decide.Tokens(5),
				FinishReasons: []string{"stop"},
			},
			{
				InputTokens: decide.Tokens(50), OutputTokens: decide.Tokens(7),
				CacheReadInputTokens: decide.Tokens(10), ReasoningOutputTokens: decide.Tokens(3),
				FinishReasons: []string{"length", "stop"},
			},
		}}
		h := newRemoteHarness(t, remoteSnapshot(time.Second), be)
		span := h.run(t)
		for key, want := range map[string]int64{
			"gen_ai.usage.input_tokens":             150,
			"gen_ai.usage.output_tokens":            27,
			"gen_ai.usage.cache_read.input_tokens":  40,
			"gen_ai.usage.cache_write.input_tokens": 5,
			"gen_ai.usage.reasoning.output_tokens":  3,
		} {
			if got, ok := intAttr(t, span, key); !ok || got != want {
				t.Errorf("%s = %d (present %v), want %d", key, got, ok, want)
			}
		}
		v, _ := attrValue(span, "gen_ai.response.finish_reasons")
		if got := v.AsStringSlice(); !reflect.DeepEqual(got, []string{"stop", "length"}) {
			t.Errorf("gen_ai.response.finish_reasons = %v, want [stop length]", got)
		}
	})

	t.Run("UC-040/main a count no call reported stays absent, never 0", func(t *testing.T) {
		be := &usageBackend{answer: answer, calls: []decide.CallUsage{
			{InputTokens: decide.Tokens(11), OutputTokens: decide.Tokens(4)},
			{},
		}}
		span := newRemoteHarness(t, remoteSnapshot(time.Second), be).run(t)
		if got, ok := intAttr(t, span, "gen_ai.usage.input_tokens"); !ok || got != 11 {
			t.Errorf("gen_ai.usage.input_tokens = %d (present %v), want 11", got, ok)
		}
		for _, key := range []string{
			"gen_ai.usage.cache_read.input_tokens", "gen_ai.usage.cache_write.input_tokens",
			"gen_ai.usage.reasoning.output_tokens", "gen_ai.response.finish_reasons",
		} {
			if v, ok := attrValue(span, key); ok {
				t.Errorf("%s = %v, want absent (no call reported it)", key, v.Emit())
			}
		}
	})

	t.Run("UC-040/main no call reported usage: no usage attributes", func(t *testing.T) {
		be := &usageBackend{answer: answer}
		span := newRemoteHarness(t, remoteSnapshot(time.Second), be).run(t)
		for _, key := range usageKeys {
			if v, ok := attrValue(span, key); ok {
				t.Errorf("%s = %v, want absent", key, v.Emit())
			}
		}
	})

	t.Run("UC-040/A2 a failed call that fell back keeps the made calls' tokens", func(t *testing.T) {
		be := &usageBackend{
			answer: func(decide.Request) (decide.Response, error) { return decide.Response{}, errors.New("boom") },
			calls: []decide.CallUsage{
				{InputTokens: decide.Tokens(80), OutputTokens: decide.Tokens(9), FinishReasons: []string{"stop"}},
				{FinishReasons: []string{"error"}},
			},
		}
		span := newRemoteHarness(t, remoteSnapshot(time.Second), be).run(t)
		if got := strAttr(t, span, AttrDecideOutcome); got != DecideOutcomeFallback {
			t.Fatalf("decide.outcome = %q, want fallback", got)
		}
		if got, ok := intAttr(t, span, "gen_ai.usage.input_tokens"); !ok || got != 80 {
			t.Errorf("gen_ai.usage.input_tokens = %d (present %v), want 80 (billed before the fallback)", got, ok)
		}
		if got, ok := intAttr(t, span, "gen_ai.usage.output_tokens"); !ok || got != 9 {
			t.Errorf("gen_ai.usage.output_tokens = %d (present %v), want 9", got, ok)
		}
		v, _ := attrValue(span, "gen_ai.response.finish_reasons")
		if got := v.AsStringSlice(); !reflect.DeepEqual(got, []string{"stop", "error"}) {
			t.Errorf("gen_ai.response.finish_reasons = %v, want [stop error]", got)
		}
		if strAttr(t, span, "gen_ai.request.model") == "" {
			t.Error("gen_ai.request.model missing on the fallback span (#2718 filters on it)")
		}
	})

	t.Run("UC-040/A1 rules path: no usage, no request settings", func(t *testing.T) {
		fx := decideFixtures()[0]
		clock := time.Unix(10_000, 0)
		l, sr := decideRecordingLoop(t, rulesSnapshot())
		l.Rules = newTestEngine(fx.cfg, &clock)
		l.OnEvaluate(context.Background(), fx.ctx, testTrigger())
		ds := spansByName(sr.Ended(), SpanDecide)
		if len(ds) != 1 {
			t.Fatalf("invoke_agent spans = %d, want 1", len(ds))
		}
		for _, key := range append(usageKeys, "gen_ai.request.max_tokens", "gen_ai.request.temperature") {
			if v, ok := attrValue(ds[0], key); ok {
				t.Errorf("%s = %v on the rules path, want absent", key, v.Emit())
			}
		}
	})
}

// UC: UC-040 main
//
// #2584: gen_ai.request.max_tokens and gen_ai.request.temperature of the chosen
// backend are on the invoke_agent span from its start; absent when the backend
// sets none.
func TestInvokeAgentSpan_RequestSettingsAtStart(t *testing.T) {
	answer := ffaAnswer("grant_shield", 0.9, "A", 0.5, "shield")
	zero := 0.0

	t.Run("UC-040/main backend with settings", func(t *testing.T) {
		be := &settingsBackend{usageBackend{answer: answer, settings: &decide.RequestSettings{MaxTokens: 2048, Temperature: &zero}}}
		h := newRemoteHarness(t, remoteSnapshot(time.Second), be)
		st := recordStarts(t, h.loop, h.sr)
		span := h.run(t)
		start := st.at(SpanDecide)
		if len(start) != 1 {
			t.Fatalf("starts = %d, want 1", len(start))
		}
		if v := start[0]["gen_ai.request.max_tokens"]; v.AsInt64() != 2048 {
			t.Errorf("at start: gen_ai.request.max_tokens = %d, want 2048", v.AsInt64())
		}
		if v, ok := start[0]["gen_ai.request.temperature"]; !ok || v.AsFloat64() != 0 {
			t.Errorf("at start: gen_ai.request.temperature = %v (present %v), want 0", v.AsFloat64(), ok)
		}
		if got, ok := intAttr(t, span, "gen_ai.request.max_tokens"); !ok || got != 2048 {
			t.Errorf("gen_ai.request.max_tokens = %d (present %v), want 2048", got, ok)
		}
	})

	t.Run("UC-040/main backend without settings (jev)", func(t *testing.T) {
		be := &usageBackend{answer: answer}
		span := newRemoteHarness(t, remoteSnapshot(time.Second), be).run(t)
		for _, key := range []string{"gen_ai.request.max_tokens", "gen_ai.request.temperature"} {
			if v, ok := attrValue(span, key); ok {
				t.Errorf("%s = %v, want absent", key, v.Emit())
			}
		}
	})
}
