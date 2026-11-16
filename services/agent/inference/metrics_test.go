package inference

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/joustmania/agent/internal/genaimetrics"
	"github.com/joustmania/agent/llm"
)

// withMetricReader installs a manual-reader meter provider for one test and
// rebuilds the package instruments against it (and again afterwards).
func withMetricReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	metricsOnce = sync.Once{}
	t.Cleanup(func() {
		otel.SetMeterProvider(previous)
		metricsOnce = sync.Once{}
		_ = provider.Shutdown(context.Background())
	})
	return reader
}

type tokenPoint struct {
	sum    int64
	count  uint64
	bounds []float64
	attrs  attribute.Set
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) (tokens map[string]tokenPoint, durations []metricdata.HistogramDataPoint[float64]) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	tokens = map[string]tokenPoint{}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != instrumentationName {
			t.Errorf("scope = %q, want %q", sm.Scope.Name, instrumentationName)
		}
		for _, md := range sm.Metrics {
			switch md.Name {
			case "gen_ai.client.token.usage":
				for _, dp := range md.Data.(metricdata.Histogram[int64]).DataPoints {
					typ, _ := dp.Attributes.Value("gen_ai.token.type")
					tokens[typ.AsString()] = tokenPoint{sum: dp.Sum, count: dp.Count, bounds: dp.Bounds, attrs: dp.Attributes}
				}
			case "gen_ai.client.operation.duration":
				durations = md.Data.(metricdata.Histogram[float64]).DataPoints
			}
		}
	}
	return tokens, durations
}

// UC: UC-035 BR-004
func TestClaude_RecordsGenAIClientMetrics(t *testing.T) {
	t.Run("UC-035/BR-004 answered call records tokens and duration", func(t *testing.T) {
		reader := withMetricReader(t)
		f := &fakeMessages{body: messageBody(`{"ok":true}`, "end_turn")}
		c := newTestClaude(t, f, Config{})
		if _, err := c.Infer(context.Background(), llm.Prompt{User: "u"}); err != nil {
			t.Fatalf("Infer: %v", err)
		}

		tokens, durations := collect(t, reader)
		if got := tokens["input"]; got.sum != 120 || got.count != 1 {
			t.Errorf("input tokens = %d (count %d), want 120 (1)", got.sum, got.count)
		}
		if got := tokens["output"]; got.sum != 30 || got.count != 1 {
			t.Errorf("output tokens = %d (count %d), want 30 (1)", got.sum, got.count)
		}
		input := tokens["input"]
		for key, want := range map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.provider.name":  "anthropic",
			"gen_ai.request.model":  DefaultModel,
			"gen_ai.response.model": "claude-opus-5-5",
		} {
			if v, _ := input.attrs.Value(attribute.Key(key)); v.AsString() != want {
				t.Errorf("token usage %s = %q, want %q", key, v.AsString(), want)
			}
		}
		if len(durations) != 1 || durations[0].Count != 1 {
			t.Fatalf("duration points = %+v, want one call", durations)
		}
		if _, ok := durations[0].Attributes.Value("error.type"); ok {
			t.Error("error.type set on a successful call")
		}
		// BR-004: the semconv advisory buckets, not the SDK defaults (#1434).
		if !slices.Equal(durations[0].Bounds, genaimetrics.OperationDurationBuckets) {
			t.Errorf("duration bounds = %v, want %v", durations[0].Bounds, genaimetrics.OperationDurationBuckets)
		}
		if !slices.Equal(input.bounds, genaimetrics.TokenUsageBuckets) {
			t.Errorf("token bounds = %v, want %v", input.bounds, genaimetrics.TokenUsageBuckets)
		}
	})

	t.Run("UC-035/BR-004 refusal still records tokens, duration carries error.type", func(t *testing.T) {
		reader := withMetricReader(t)
		f := &fakeMessages{body: messageBody("", "refusal")}
		c := newTestClaude(t, f, Config{})
		if _, err := c.Infer(context.Background(), llm.Prompt{User: "u"}); err == nil {
			t.Fatal("want refusal error")
		}
		tokens, durations := collect(t, reader)
		if tokens["input"].sum != 120 || tokens["output"].sum != 30 {
			t.Errorf("tokens = %+v, want the billed usage of the refused call", tokens)
		}
		if len(durations) != 1 {
			t.Fatalf("duration points = %d, want 1", len(durations))
		}
		if v, _ := durations[0].Attributes.Value("error.type"); v.AsString() != "refusal" {
			t.Errorf("error.type = %q, want refusal", v.AsString())
		}
		if v, _ := durations[0].Attributes.Value("gen_ai.response.model"); v.AsString() != "claude-opus-5-5" {
			t.Errorf("duration gen_ai.response.model = %q, want claude-opus-5-5 (the answered call's model)", v.AsString())
		}
	})

	t.Run("UC-035/BR-004 HTTP error records duration only", func(t *testing.T) {
		reader := withMetricReader(t)
		f := &fakeMessages{status: http.StatusInternalServerError, body: `{"type":"error","error":{"type":"api_error","message":"boom"}}`}
		c := newTestClaude(t, f, Config{})
		if _, err := c.Infer(context.Background(), llm.Prompt{User: "u"}); err == nil {
			t.Fatal("want HTTP error")
		}
		tokens, durations := collect(t, reader)
		if len(tokens) != 0 {
			t.Errorf("tokens = %+v, want none without a reply", tokens)
		}
		if len(durations) != 1 {
			t.Fatalf("duration points = %d, want 1", len(durations))
		}
		if v, _ := durations[0].Attributes.Value("error.type"); v.AsString() != "500" {
			t.Errorf("error.type = %q, want 500", v.AsString())
		}
	})

	t.Run("UC-035/BR-004 no API key records nothing", func(t *testing.T) {
		reader := withMetricReader(t)
		c := NewClaude(Config{})
		_, _ = c.Infer(context.Background(), llm.Prompt{User: "u"})
		tokens, durations := collect(t, reader)
		if len(tokens) != 0 || len(durations) != 0 {
			t.Errorf("tokens = %+v, durations = %+v, want nothing", tokens, durations)
		}
	})
}
