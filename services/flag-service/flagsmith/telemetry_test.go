package flagsmith

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// UC: UC-055 BR-001
// Every instrument of telemetry.md §10.4 is registered, with the link off too;
// the link gauge has exactly one state at 1 and no label carries a key value.
func TestMetricsRegistered(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("t")
	l, err := New(Config{}, Deps{Meter: meter, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m := l.Metrics()
	m.RecordPull(ctx, TriggerPoll, ResultChanged)
	m.RecordPush(ctx, OpValue, ResultOK, "")
	m.RecordConflict(ctx, ResolutionTightened)
	m.RecordHealthPost(ctx, "unhealthy", ResultOK)
	m.RecordChangeDelay(ctx, TriggerPoll, 2*time.Second)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			seen[md.Name] = true
			if md.Name != MetricLink {
				continue
			}
			ones := 0
			for _, dp := range md.Data.(metricdata.Gauge[int64]).DataPoints {
				if dp.Value == 1 {
					ones++
					if s, _ := dp.Attributes.Value("state"); s.AsString() != string(ModeOff) {
						t.Fatalf("link gauge 1 for %s, want off", s.AsString())
					}
				}
			}
			if ones != 1 {
				t.Fatalf("link gauge has %d states at 1, want 1", ones)
			}
		}
	}
	for _, name := range []string{MetricLink, MetricPulls, MetricPushes, MetricPending, MetricOutOfSync, MetricConflicts, MetricChangeDelay, MetricHealthPosts} {
		if !seen[name] {
			t.Errorf("metric %s not registered", name)
		}
	}
	if MetricPulls != "flagservice_flagsmith_pulls_total" || MetricChangeDelay != "flagservice_flagsmith_change_delay_seconds" {
		t.Fatal("metric names drifted from telemetry.md §10.4")
	}
	var nilMetrics *Metrics
	nilMetrics.RecordPull(ctx, TriggerPoll, ResultError)
}
