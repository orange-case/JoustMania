// state.go serves GET /experiments/state (docs/specs/services.md §5): the live
// state the dashboard's Agent and Experiments tabs render, read from the agent's own
// memory instead of reconstructed from Jaeger searches. Read-only and for humans; it
// changes nothing (C-005). Jaeger stays the drill-down: every decide span and
// snapshot carries the trace id it links to.
package main

import (
	"context"
	"net/http"
	"strconv"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/joustmania/agent/decision"
	"github.com/joustmania/agent/experiment"
)

const (
	spanDecide            = "invoke_agent joustmania-agent"
	spanDecision          = "agent.decision"
	spanSignalReceived    = "agent.signal_received"
	spanPredictionOutcome = "agent.prediction.outcome"
	spanGameSummary       = "agent.game.summary"
)

// recentSpanCaps bounds what recentSpans keeps per span name and per game-kind class
// (shadow, everything else), so a burst of shadow games never evicts the stage game.
var recentSpanCaps = map[string]int{
	spanDecide:            30,
	spanDecision:          90,
	spanSignalReceived:    30,
	spanPredictionOutcome: 400,
	spanGameSummary:       20,
}

// droppedAttrs are left out of the state answer: large and not shown.
var droppedAttrs = map[attribute.Key]bool{"decide.questions": true}

// recentSpans is a span processor that keeps the newest ended decide-related spans
// in memory. agent.signal_received is kept only as the parent of a kept decide (the
// rules path without game ids links its decisions through it).
type recentSpans struct {
	mu    sync.Mutex
	rings map[string][]sdktrace.ReadOnlySpan
}

func newRecentSpans() *recentSpans {
	return &recentSpans{rings: map[string][]sdktrace.ReadOnlySpan{}}
}

// agentRecentSpans is registered on the agent's tracer provider (otel.go).
var agentRecentSpans = newRecentSpans()

func spanClass(s sdktrace.ReadOnlySpan) string {
	for _, kv := range s.Attributes() {
		if kv.Key == "game.kind" && kv.Value.AsString() == "shadow" {
			return "shadow"
		}
	}
	return "other"
}

func (r *recentSpans) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

func (r *recentSpans) OnEnd(s sdktrace.ReadOnlySpan) {
	limit, ok := recentSpanCaps[s.Name()]
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.Name() == spanSignalReceived && !r.parentOfKeptDecideLocked(s) {
		return
	}
	key := s.Name() + "|" + spanClass(s)
	ring := append(r.rings[key], s)
	// Reslicing keeps at most about 2*limit spans reachable: append reallocates
	// (copying only the kept tail) once the front-trimmed capacity runs out.
	if len(ring) > limit {
		ring = ring[len(ring)-limit:]
	}
	r.rings[key] = ring
}

func (r *recentSpans) parentOfKeptDecideLocked(s sdktrace.ReadOnlySpan) bool {
	id := s.SpanContext().SpanID()
	for _, class := range []string{"shadow", "other"} {
		for _, d := range r.rings[spanDecide+"|"+class] {
			if d.Parent().SpanID() == id {
				return true
			}
		}
	}
	return false
}

func (r *recentSpans) Shutdown(context.Context) error   { return nil }
func (r *recentSpans) ForceFlush(context.Context) error { return nil }

func (r *recentSpans) snapshot() []sdktrace.ReadOnlySpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []sdktrace.ReadOnlySpan
	for _, ring := range r.rings {
		out = append(out, ring...)
	}
	return out
}

// OTLP/JSON shapes (hex ids, decimal-string nanoseconds and int64s), so the
// dashboard folds them with the same code it uses for Jaeger's v3 answers.
type otlpTraces struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpScopeSpans struct {
	Spans []otlpSpan `json:"spans"`
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes"`
	Links             []otlpLink     `json:"links,omitempty"`
}

type otlpLink struct {
	TraceID string `json:"traceId"`
	SpanID  string `json:"spanId"`
}

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

type otlpAnyValue struct {
	StringValue *string         `json:"stringValue,omitempty"`
	BoolValue   *bool           `json:"boolValue,omitempty"`
	IntValue    *string         `json:"intValue,omitempty"`
	DoubleValue *float64        `json:"doubleValue,omitempty"`
	ArrayValue  *otlpArrayValue `json:"arrayValue,omitempty"`
}

type otlpArrayValue struct {
	Values []otlpAnyValue `json:"values"`
}

func otlpValue(v attribute.Value) otlpAnyValue {
	switch v.Type() {
	case attribute.BOOL:
		b := v.AsBool()
		return otlpAnyValue{BoolValue: &b}
	case attribute.INT64:
		s := strconv.FormatInt(v.AsInt64(), 10)
		return otlpAnyValue{IntValue: &s}
	case attribute.FLOAT64:
		f := v.AsFloat64()
		return otlpAnyValue{DoubleValue: &f}
	case attribute.BOOLSLICE, attribute.INT64SLICE, attribute.FLOAT64SLICE, attribute.STRINGSLICE:
		var vals []otlpAnyValue
		switch v.Type() {
		case attribute.BOOLSLICE:
			for _, b := range v.AsBoolSlice() {
				vals = append(vals, otlpValue(attribute.BoolValue(b)))
			}
		case attribute.INT64SLICE:
			for _, i := range v.AsInt64Slice() {
				vals = append(vals, otlpValue(attribute.Int64Value(i)))
			}
		case attribute.FLOAT64SLICE:
			for _, f := range v.AsFloat64Slice() {
				vals = append(vals, otlpValue(attribute.Float64Value(f)))
			}
		default:
			for _, s := range v.AsStringSlice() {
				vals = append(vals, otlpValue(attribute.StringValue(s)))
			}
		}
		return otlpAnyValue{ArrayValue: &otlpArrayValue{Values: vals}}
	default:
		s := v.Emit()
		return otlpAnyValue{StringValue: &s}
	}
}

func toOTLP(spans []sdktrace.ReadOnlySpan) otlpTraces {
	out := make([]otlpSpan, 0, len(spans))
	for _, s := range spans {
		sc := s.SpanContext()
		span := otlpSpan{
			TraceID:           sc.TraceID().String(),
			SpanID:            sc.SpanID().String(),
			Name:              s.Name(),
			StartTimeUnixNano: strconv.FormatInt(s.StartTime().UnixNano(), 10),
			EndTimeUnixNano:   strconv.FormatInt(s.EndTime().UnixNano(), 10),
			Attributes:        []otlpKeyValue{},
		}
		if p := s.Parent(); p.IsValid() {
			span.ParentSpanID = p.SpanID().String()
		}
		for _, kv := range s.Attributes() {
			if droppedAttrs[kv.Key] {
				continue
			}
			span.Attributes = append(span.Attributes, otlpKeyValue{Key: string(kv.Key), Value: otlpValue(kv.Value)})
		}
		for _, l := range s.Links() {
			span.Links = append(span.Links, otlpLink{
				TraceID: l.SpanContext.TraceID().String(),
				SpanID:  l.SpanContext.SpanID().String(),
			})
		}
		out = append(out, span)
	}
	return otlpTraces{ResourceSpans: []otlpResourceSpans{{ScopeSpans: []otlpScopeSpans{{Spans: out}}}}}
}

// stateView is the GET /experiments/state answer.
type stateView struct {
	// Decides holds the newest decide spans with their agent.decision, the parents
	// that link them on the rules path, the scored predictions and the game summaries.
	Decides     otlpTraces                    `json:"decides"`
	Flags       []decision.FlagState          `json:"flags"`
	Experiments []experiment.ProgressSnapshot `json:"experiments"`
}

// stateReader assembles stateView from the agent's memory. Each source is nil-safe.
type stateReader struct {
	spans    *recentSpans
	flags    *decision.FlagBoard
	progress func() []experiment.ProgressSnapshot
}

func (s *stateReader) view() stateView {
	v := stateView{Flags: s.flags.List(), Experiments: []experiment.ProgressSnapshot{}}
	var spans []sdktrace.ReadOnlySpan
	if s.spans != nil {
		spans = s.spans.snapshot()
	}
	v.Decides = toOTLP(spans)
	if s.progress != nil {
		if p := s.progress(); p != nil {
			v.Experiments = p
		}
	}
	return v
}

func (s *stateReader) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.view())
}
