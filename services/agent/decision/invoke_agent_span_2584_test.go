package decision

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// startAttrRecorder keeps the attributes each span carried when it started.
type startAttrRecorder struct {
	mu    sync.Mutex
	attrs map[string][]map[attribute.Key]attribute.Value
}

func (r *startAttrRecorder) OnStart(_ context.Context, s sdktrace.ReadWriteSpan) {
	m := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes() {
		m[kv.Key] = kv.Value
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attrs == nil {
		r.attrs = map[string][]map[attribute.Key]attribute.Value{}
	}
	r.attrs[s.Name()] = append(r.attrs[s.Name()], m)
}
func (r *startAttrRecorder) OnEnd(sdktrace.ReadOnlySpan)       {}
func (r *startAttrRecorder) Shutdown(context.Context) error   { return nil }
func (r *startAttrRecorder) ForceFlush(context.Context) error { return nil }

func (r *startAttrRecorder) at(name string) []map[attribute.Key]attribute.Value {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attrs[name]
}

// recordStarts points l's tracer at a provider that feeds sr and a start recorder.
func recordStarts(t *testing.T, l *Loop, sr *tracetest.SpanRecorder) *startAttrRecorder {
	t.Helper()
	st := &startAttrRecorder{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithSpanProcessor(st))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	l.Tracer = tp.Tracer("test")
	return st
}

// UC: UC-040 main
// UC: UC-040 A1
// UC: UC-035 main
//
// #2584: the decide span follows the GenAI semconv (semantic-conventions-genai
// v0.1.0) "Invoke agent internal span" on both paths: name
// `{gen_ai.operation.name} {gen_ai.agent.name}`, kind INTERNAL, the required
// gen_ai.operation.name=invoke_agent, and the sampling-relevant attributes
// present at span creation. The name is asserted as a literal so a typo in
// SpanDecide (which the collector filter, Grafana, the dashboard and the talk
// pre-flight all match) fails here.
func TestInvokeAgentSpan_SemconvNameAndOperation(t *testing.T) {
	const wantName = "invoke_agent joustmania-agent"
	if SpanDecide != wantName {
		t.Fatalf("SpanDecide = %q, want %q", SpanDecide, wantName)
	}

	check := func(t *testing.T, span sdktrace.ReadOnlySpan, st *startAttrRecorder, remote bool) {
		t.Helper()
		if span.Name() != wantName {
			t.Errorf("span name = %q, want %q", span.Name(), wantName)
		}
		if span.SpanKind() != trace.SpanKindInternal {
			t.Errorf("span kind = %v, want INTERNAL", span.SpanKind())
		}
		if got := strAttr(t, span, "gen_ai.operation.name"); got != "invoke_agent" {
			t.Errorf("gen_ai.operation.name = %q, want invoke_agent", got)
		}
		if got := strAttr(t, span, "gen_ai.agent.name"); got != "joustmania-agent" {
			t.Errorf("gen_ai.agent.name = %q, want joustmania-agent", got)
		}
		starts := st.at(wantName)
		if len(starts) != 1 {
			t.Fatalf("%q starts = %d, want 1", wantName, len(starts))
		}
		start := starts[0]
		if v := start["gen_ai.operation.name"]; v.AsString() != "invoke_agent" {
			t.Errorf("at start: gen_ai.operation.name = %q, want invoke_agent", v.AsString())
		}
		if v := start["gen_ai.agent.name"]; v.AsString() != "joustmania-agent" {
			t.Errorf("at start: gen_ai.agent.name = %q, want joustmania-agent", v.AsString())
		}
		out, ok := attrValue(span, "gen_ai.output.type")
		if remote {
			if v := start["gen_ai.request.model"]; v.AsString() == "" {
				t.Error("at start: gen_ai.request.model missing on the remote path")
			}
			if !ok || out.AsString() != "json" {
				t.Errorf("gen_ai.output.type = %q (present %v), want json on the remote path", out.AsString(), ok)
			}
		} else if ok {
			t.Errorf("gen_ai.output.type = %q on the rules path, want absent (no request)", out.AsString())
		}
	}

	t.Run("UC-040/A1 rules path", func(t *testing.T) {
		fx := decideFixtures()[0]
		clock := time.Unix(10_000, 0)
		l, sr := decideRecordingLoop(t, rulesSnapshot())
		st := recordStarts(t, l, sr)
		l.Rules = newTestEngine(fx.cfg, &clock)
		l.OnEvaluate(context.Background(), fx.ctx, testTrigger())
		ds := spansByName(sr.Ended(), wantName)
		if len(ds) != 1 {
			t.Fatalf("%q spans = %d, want 1", wantName, len(ds))
		}
		check(t, ds[0], st, false)
	})

	t.Run("UC-040/main remote path", func(t *testing.T) {
		be := newFakeDecideBackend(ffaAnswer("grant_shield", 0.9, "A", 0.5, "shield"))
		h := newRemoteHarness(t, remoteSnapshot(time.Second), be)
		st := recordStarts(t, h.loop, h.sr)
		check(t, h.run(t), st, true)
	})
}
