package main

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/joustmania/agent/gamecontext"
)

// momentCounterValues reads agent_moments_received_total as {moment/shape/outcome: value}.
func momentCounterValues(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "agent_moments_received_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("agent_moments_received_total is %T, want a Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				get := func(k string) string {
					v, _ := dp.Attributes.Value(attribute.Key(k))
					return v.AsString()
				}
				out[get("moment")+"/"+get("shape")+"/"+get("outcome")] = dp.Value
			}
		}
	}
	return out
}

// UC: UC-041 A7
// UC: UC-041 BR-009
func TestMomentsReceivedCounter(t *testing.T) {
	t.Run("UC-041/BR-009 the multiplexer's moment records reach agent_moments_received_total", func(t *testing.T) {
		reader := sdkmetric.NewManualReader()
		provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
		observe := countMoments(newMomentsReceivedCounter(provider))

		observe("player_death", gamecontext.MomentShapeLogEvent, gamecontext.MomentCounted)
		observe("player_death", gamecontext.MomentShapeSpanEvent, gamecontext.MomentDuplicate)
		observe("player_death", gamecontext.MomentShapeLogEvent, gamecontext.MomentCounted)
		observe("death_warning", gamecontext.MomentShapeLogEvent, gamecontext.MomentIgnored)

		got := momentCounterValues(t, reader)
		want := map[string]int64{
			"player_death/log_event/counted":    2,
			"player_death/span_event/duplicate": 1,
			"death_warning/log_event/ignored":   1,
		}
		if len(got) != len(want) {
			t.Fatalf("series %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %d, want %d", k, got[k], v)
			}
		}
	})
}
