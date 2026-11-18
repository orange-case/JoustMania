package main

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"

	"github.com/joustmania/agent/gamecontext"
)

// moments.go counts the game moments the agent reads (#2658 slice 3, UC-041 A7,
// BR-009, telemetry.md §6): agent_moments_received_total{moment,shape,outcome}, one
// increment per moment record, from the span ingest (player_death span events) and
// the log ingest (moment log events). The only deterministic evidence that the log
// path delivered a moment: a NonStop respawn death changes no game context, and a
// disconnect death is masked by the elimination-order gauge.

// newMomentsReceivedCounter builds agent_moments_received_total; a noop counter when
// the meter refuses it. The name is a literal so the telemetry inventory finds it.
func newMomentsReceivedCounter(mp otelmetric.MeterProvider) otelmetric.Int64Counter {
	c, err := mp.Meter(gameEndMeterName).Int64Counter(
		"agent_moments_received_total",
		otelmetric.WithDescription("Game moment records the agent read, by moment, shape (span_event, log_event) and outcome (counted, duplicate, ignored, dropped) (#2658, UC-041 A7, BR-009)"),
	)
	if err != nil {
		c, _ = metricnoop.NewMeterProvider().Meter(gameEndMeterName).Int64Counter("agent_moments_received_total")
	}
	return c
}

// countMoments returns the multiplexer's moment observer: one increment of counter
// per moment record. The labels are bounded by gamecontext (two moments, two shapes,
// four outcomes; 7 reachable series).
func countMoments(counter otelmetric.Int64Counter) gamecontext.MomentObserver {
	return func(moment, shape, outcome string) {
		counter.Add(context.Background(), 1, otelmetric.WithAttributes(
			attribute.String("moment", moment),
			attribute.String("shape", shape),
			attribute.String("outcome", outcome),
		))
	}
}
