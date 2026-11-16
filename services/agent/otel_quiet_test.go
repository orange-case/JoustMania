package main

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// UC: UC-040 BR-013
func TestQuietEvaluatorProvider_DropsOnlyTheFlagdEvaluatorScope(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	p := quietEvaluatorProvider{TracerProvider: tp}

	_, quiet := p.Tracer("jsonEvaluator").Start(context.Background(), "resolveBoolean")
	quiet.End()
	_, kept := p.Tracer("joustmania/agent").Start(context.Background(), "agent.decide")
	kept.End()

	ended := sr.Ended()
	if len(ended) != 1 || ended[0].Name() != "agent.decide" {
		names := []string{}
		for _, s := range ended {
			names = append(names, s.Name())
		}
		t.Fatalf("ended spans = %v, want only agent.decide: the flagd evaluator's spans are dropped", names)
	}
}
