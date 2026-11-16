package main

import (
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// flagdEvaluatorScope is the instrumentation scope of flagd core's in-process JSON
// evaluator (the flagd provider embeds it for every local flag read).
const flagdEvaluatorScope = "jsonEvaluator"

// quietEvaluatorProvider wraps the global TracerProvider so the flagd in-process
// evaluator gets a no-op tracer (#2648). The evaluator takes its tracer from the
// global provider (otel.Tracer) and cannot be handed another, and its resolveBoolean /
// resolveAll spans (a few microseconds each, one per flag read) are noise next to the
// game's own spans. Every other scope is passed through unchanged.
type quietEvaluatorProvider struct {
	trace.TracerProvider
}

// Tracer returns a no-op tracer for the flagd evaluator's scope, the wrapped
// provider's tracer otherwise.
func (p quietEvaluatorProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	if name == flagdEvaluatorScope {
		return tracenoop.NewTracerProvider().Tracer(name, opts...)
	}
	return p.TracerProvider.Tracer(name, opts...)
}
