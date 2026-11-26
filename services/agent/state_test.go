package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/joustmania/agent/decision"
	"github.com/joustmania/agent/experiment"
	"github.com/joustmania/agent/gamecontext"
)

func recentTracer(t *testing.T) (*recentSpans, trace.Tracer) {
	t.Helper()
	rs := newRecentSpans()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rs))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return rs, tp.Tracer("test")
}

func names(spans []sdktrace.ReadOnlySpan) map[string]int {
	out := map[string]int{}
	for _, s := range spans {
		out[s.Name()]++
	}
	return out
}

// UC: UC-035 3
func TestRecentSpans_KeepsTheDecideSpansBoundedPerGameKind(t *testing.T) {
	rs, tr := recentTracer(t)
	_, real := tr.Start(context.Background(), spanDecide, trace.WithAttributes(attribute.String("game.kind", "real")))
	real.End()
	for i := 0; i < recentSpanCaps[spanDecide]+5; i++ {
		_, s := tr.Start(context.Background(), spanDecide, trace.WithAttributes(attribute.String("game.kind", "shadow")))
		s.End()
	}
	_, other := tr.Start(context.Background(), "game.cycle")
	other.End()

	got := rs.snapshot()
	if n := names(got)[spanDecide]; n != recentSpanCaps[spanDecide]+1 {
		t.Fatalf("UC-035/3 decides kept = %d, want %d shadow plus the real one", n, recentSpanCaps[spanDecide])
	}
	if names(got)["game.cycle"] != 0 {
		t.Error("a span the dashboard does not read is kept")
	}
	found := false
	for _, s := range got {
		if s.SpanContext().SpanID() == real.SpanContext().SpanID() {
			found = true
		}
	}
	if !found {
		t.Error("UC-035/3 a burst of shadow decides evicted the real game's decide")
	}
}

// UC: UC-035 3
func TestRecentSpans_SignalReceivedOnlyAsAKeptDecidesParent(t *testing.T) {
	rs, tr := recentTracer(t)
	ctx, parent := tr.Start(context.Background(), spanSignalReceived)
	_, d := tr.Start(ctx, spanDecide)
	d.End()
	parent.End()
	_, idle := tr.Start(context.Background(), spanSignalReceived)
	idle.End()
	if n := names(rs.snapshot())[spanSignalReceived]; n != 1 {
		t.Fatalf("signal_received kept = %d, want only the decide's parent", n)
	}
}

// UC: UC-035 3, UC-035 4
func TestStateEndpoint_ServesDecidesFlagsAndExperiments(t *testing.T) {
	rs, tr := recentTracer(t)
	ctx, d := tr.Start(context.Background(), spanDecide, trace.WithAttributes(
		attribute.String("game.id", "game_s1"),
		attribute.Float64("decide.confidence", 0.75),
		attribute.Int("decide.latency_ms", 42),
		attribute.Bool("decide.blocked", false),
		attribute.String("decide.questions", "{big}"),
	))
	_, dec := tr.Start(ctx, spanDecision, trace.WithAttributes(attribute.StringSlice("fitness.evaluated", []string{"a", "b"})))
	dec.End()
	d.End()

	board := decision.NewFlagBoard()
	loop := decision.NewLoop(nil, nil)
	loop.SetFlagBoard(board)
	loop.OnEvaluate(context.Background(), gamecontext.GameContext{SessionID: "game_s1", GameKind: gamecontext.GameKindReal}, decision.EvalTrigger{})
	snap := experiment.ProgressSnapshot{ID: "exp_1", Status: "running", Arms: []experiment.ProgressArm{}}
	reader := &stateReader{spans: rs, flags: board, progress: func() []experiment.ProgressSnapshot {
		return []experiment.ProgressSnapshot{snap}
	}}

	mux := http.NewServeMux()
	registerDossierRoutes(mux, &dossierReader{root: t.TempDir()})
	mux.HandleFunc("/experiments/state", reader.handle)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/experiments/state", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200: the state route wins over /experiments/{id}", rec.Code, rec.Body)
	}
	var got struct {
		Decides struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						TraceID, SpanID, ParentSpanID, Name string
						StartTimeUnixNano                   string
						Attributes                          []struct {
							Key   string
							Value map[string]any
						}
					}
				}
			}
		}
		Flags       []decision.FlagState
		Experiments []experiment.ProgressSnapshot
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	spans := got.Decides.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 2 {
		t.Fatalf("UC-035/3 decides = %d spans, want the decide and its decision", len(spans))
	}
	byName := map[string]map[string]map[string]any{}
	for _, s := range spans {
		attrs := map[string]map[string]any{}
		for _, a := range s.Attributes {
			attrs[a.Key] = a.Value
		}
		byName[s.Name] = attrs
		if s.Name == spanDecision && s.ParentSpanID != d.SpanContext().SpanID().String() {
			t.Errorf("decision parent = %q, want the decide %s", s.ParentSpanID, d.SpanContext().SpanID())
		}
		if s.Name == spanDecide && (s.TraceID != d.SpanContext().TraceID().String() || s.StartTimeUnixNano == "") {
			t.Errorf("decide ids = %+v, want its trace id (the Jaeger drill-down)", s)
		}
	}
	decide := byName[spanDecide]
	for key, want := range map[string]any{
		"game.id": map[string]any{"stringValue": "game_s1"}, "decide.confidence": map[string]any{"doubleValue": 0.75},
		"decide.latency_ms": map[string]any{"intValue": "42"}, "decide.blocked": map[string]any{"boolValue": false},
	} {
		if fmt.Sprint(decide[key]) != fmt.Sprint(want) {
			t.Errorf("%s = %v, want %v (OTLP/JSON)", key, decide[key], want)
		}
	}
	if _, ok := decide["decide.questions"]; ok {
		t.Error("decide.questions is served; it is dropped")
	}
	if arr := byName[spanDecision]["fitness.evaluated"]; arr["arrayValue"] == nil {
		t.Errorf("a string slice = %v, want an arrayValue", arr)
	}
	if len(got.Flags) != 1 || got.Flags[0].GameID != "game_s1" {
		t.Errorf("UC-035/3 flags = %+v, want game_s1's", got.Flags)
	}
	if len(got.Experiments) != 1 || got.Experiments[0].ID != "exp_1" {
		t.Errorf("UC-035/4 experiments = %+v, want exp_1", got.Experiments)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("the state answer may be cached")
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/experiments/state", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405 (read-only, C-005)", rec.Code)
	}
}

// UC: UC-035 4
func TestStateEndpoint_EmptyWithoutSources(t *testing.T) {
	rec := httptest.NewRecorder()
	(&stateReader{}).handle(rec, httptest.NewRequest(http.MethodGet, "/experiments/state", nil))
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if string(got["flags"]) != "[]" || string(got["experiments"]) != "[]" {
		t.Errorf("flags / experiments = %s / %s, want empty lists, never null", got["flags"], got["experiments"])
	}
}
