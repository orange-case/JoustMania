package flags

import (
	"context"
	"testing"

	"github.com/open-feature/go-sdk/openfeature"
	"github.com/open-feature/go-sdk/openfeature/memprovider"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func challengeFlag(variant string) map[string]memprovider.InMemoryFlag {
	return map[string]memprovider.InMemoryFlag{
		"stowaway.challenge": {
			State: memprovider.Enabled, DefaultVariant: variant,
			Variants: map[string]any{"off": "off", "swing": "swing"},
		},
		"decision.backend": {
			State: memprovider.Enabled, DefaultVariant: "jev",
			Variants: map[string]any{"jev": "jev"},
		},
	}
}

func eventKeys(span sdktrace.ReadOnlySpan) []string {
	var keys []string
	for _, ev := range span.Events() {
		if ev.Name != "feature_flag.evaluation" {
			continue
		}
		key, _ := attr(attribute.NewSet(ev.Attributes...), "feature_flag.key")
		variant, _ := attr(attribute.NewSet(ev.Attributes...), "feature_flag.result.variant")
		keys = append(keys, key+"="+variant)
	}
	return keys
}

// UC: UC-035 BR-018
// A challenge flag records a span event only when its outcome changed for the game; other flags
// keep one event per evaluation, and the metrics count every evaluation.
func TestChallengeTracesHook_ChangesOnlyPerGame(t *testing.T) {
	const domain = "challenge-test"
	reader := sdkmetric.NewManualReader()
	hooks, err := newOTelHooks(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatal(err)
	}
	provider := memprovider.NewInMemoryProvider(challengeFlag("off"))
	if err := openfeature.SetNamedProviderAndWait(domain, provider); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = openfeature.SetNamedProviderAndWait(domain, openfeature.NoopProvider{}) })
	client := openfeature.NewClient(domain)
	client.AddHooks(hooks...)

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	ctx, span := tp.Tracer("test").Start(context.Background(), "invoke_agent joustmania-agent")
	gameA := openfeature.NewEvaluationContext("k", map[string]any{"gameId": "game_a"})
	gameB := openfeature.NewEvaluationContext("k", map[string]any{"gameId": "game_b"})

	eval := func(ec openfeature.EvaluationContext, key string) {
		if _, err := client.StringValue(ctx, key, "off", ec); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}
	t.Run("UC-035/BR-018 repeats in one game record one event", func(t *testing.T) {
		eval(gameA, "stowaway.challenge")
		eval(gameA, "stowaway.challenge")
		eval(gameA, "stowaway.challenge")
	})
	t.Run("UC-035/BR-018 another game records its own first event", func(t *testing.T) {
		eval(gameB, "stowaway.challenge")
	})
	t.Run("UC-035/BR-018 a flip records one event", func(t *testing.T) {
		provider.UpdateFlags(challengeFlag("swing"))
		eval(gameA, "stowaway.challenge")
		eval(gameA, "stowaway.challenge")
	})
	t.Run("UC-035/BR-018 other flags keep one event per evaluation", func(t *testing.T) {
		eval(gameA, "decision.backend")
		eval(gameA, "decision.backend")
	})
	span.End()

	got := eventKeys(recorder.Ended()[0])
	want := []string{
		"stowaway.challenge=off", "stowaway.challenge=off", "stowaway.challenge=swing",
		"decision.backend=jev", "decision.backend=jev",
	}
	if len(got) != len(want) {
		t.Fatalf("UC-035 BR-018: events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("UC-035 BR-018: events = %v, want %v", got, want)
		}
	}

	var requests int64
	for _, dp := range collectMetrics(t, reader)["feature_flag.evaluation_requests_total"] {
		if key, _ := attr(dp.Attributes, "feature_flag.key"); key == "stowaway.challenge" {
			requests += dp.Value
		}
	}
	if requests != 6 {
		t.Errorf("UC-035 BR-018: stowaway.challenge requests = %d, want 6 (metrics count every evaluation)", requests)
	}
}

// UC: UC-035 BR-018
func TestChallengeTracesHook_BoundedGames(t *testing.T) {
	h := newChallengeTracesHook(openfeature.UnimplementedHook{})
	details := openfeature.InterfaceEvaluationDetails{Value: "swing"}
	for i := 0; i <= maxChallengeGames; i++ {
		ec := openfeature.NewEvaluationContext("", map[string]any{"gameId": i})
		hCtx := openfeature.NewHookContext("challenge.x", openfeature.String, "off",
			openfeature.NewClientMetadata("d"), openfeature.Metadata{}, ec)
		if !h.changed(hCtx, details) {
			t.Fatalf("UC-035 BR-018: first evaluation of game %d not recorded", i)
		}
	}
	if len(h.last) > maxChallengeGames {
		t.Errorf("UC-035 BR-018: %d games kept, want at most %d", len(h.last), maxChallengeGames)
	}
}
