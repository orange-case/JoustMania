package gamecontext

import (
	"testing"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// UC: UC-041 BR-011

func putMarker(m pcommon.Map, v any) {
	switch x := v.(type) {
	case bool:
		m.PutBool(AttrSynthetic, x)
	case string:
		m.PutStr(AttrSynthetic, x)
	case int:
		m.PutInt(AttrSynthetic, int64(x))
	}
}

func TestIsSynthetic(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want bool
	}{
		{true, true}, {"true", true},
		{false, false}, {"false", false}, {"True", false}, {"TRUE", false}, {1, false}, {"", false},
	} {
		t.Run("UC-041/BR-011 the marker matches the collector's spellings only", func(t *testing.T) {
			m := pcommon.NewMap()
			putMarker(m, tc.v)
			if got := isSynthetic(m); got != tc.want {
				t.Fatalf("isSynthetic(%#v) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
	t.Run("UC-041/BR-011 no marker is real telemetry", func(t *testing.T) {
		if isSynthetic(pcommon.NewMap()) {
			t.Fatal("unmarked map counted as synthetic")
		}
	})
}

func spanNames(td ptrace.Traces) []string {
	var out []string
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		sss := td.ResourceSpans().At(i).ScopeSpans()
		for j := 0; j < sss.Len(); j++ {
			for k := 0; k < sss.At(j).Spans().Len(); k++ {
				out = append(out, sss.At(j).Spans().At(k).Name())
			}
		}
	}
	return out
}

func TestDropSyntheticSpans(t *testing.T) {
	t.Run("UC-041/BR-011 marked resources and spans go, real spans stay", func(t *testing.T) {
		td := ptrace.NewTraces()
		synRes := td.ResourceSpans().AppendEmpty()
		putMarker(synRes.Resource().Attributes(), true)
		synRes.ScopeSpans().AppendEmpty().Spans().AppendEmpty().SetName("syn-resource")

		mixed := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans()
		mixed.AppendEmpty().SetName("real")
		s := mixed.AppendEmpty()
		s.SetName("syn-span")
		putMarker(s.Attributes(), "true")
		f := mixed.AppendEmpty()
		f.SetName("real-false")
		putMarker(f.Attributes(), false)

		onlySyn := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
		onlySyn.SetName("syn-alone")
		putMarker(onlySyn.Attributes(), true)

		if !HasSyntheticSpans(td) {
			t.Fatal("HasSyntheticSpans = false")
		}
		DropSyntheticSpans(td)
		if got := spanNames(td); len(got) != 2 || got[0] != "real" || got[1] != "real-false" {
			t.Fatalf("spans = %v, want [real real-false]", got)
		}
		if n := td.ResourceSpans().Len(); n != 1 {
			t.Fatalf("resources = %d, want 1 (emptied ones removed)", n)
		}
		if HasSyntheticSpans(td) {
			t.Fatal("HasSyntheticSpans after drop = true")
		}
	})
	t.Run("UC-041/BR-011 an unmarked batch is untouched", func(t *testing.T) {
		td := ptrace.NewTraces()
		td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty().SetName("real")
		if HasSyntheticSpans(td) {
			t.Fatal("HasSyntheticSpans = true")
		}
	})
}

func metricPoints(md pmetric.Metrics) map[string]int {
	out := map[string]int{}
	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		sms := md.ResourceMetrics().At(i).ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			for k := 0; k < sms.At(j).Metrics().Len(); k++ {
				m := sms.At(j).Metrics().At(k)
				forEachPointAttrs(m, func(pcommon.Map) { out[m.Name()]++ })
			}
		}
	}
	return out
}

func TestDropSyntheticMetrics(t *testing.T) {
	t.Run("UC-041/BR-011 marked resources and data points go in every metric type", func(t *testing.T) {
		md := pmetric.NewMetrics()
		synRes := md.ResourceMetrics().AppendEmpty()
		putMarker(synRes.Resource().Attributes(), "true")
		synRes.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty().SetName("syn_resource")
		synRes.ScopeMetrics().At(0).Metrics().At(0).SetEmptyGauge().DataPoints().AppendEmpty()

		ms := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics()
		g := ms.AppendEmpty()
		g.SetName("gauge")
		g.SetEmptyGauge().DataPoints().AppendEmpty()
		putMarker(g.Gauge().DataPoints().AppendEmpty().Attributes(), true)
		sum := ms.AppendEmpty()
		sum.SetName("sum")
		sum.SetEmptySum().DataPoints().AppendEmpty()
		putMarker(sum.Sum().DataPoints().AppendEmpty().Attributes(), "true")
		h := ms.AppendEmpty()
		h.SetName("histogram")
		h.SetEmptyHistogram().DataPoints().AppendEmpty()
		putMarker(h.Histogram().DataPoints().AppendEmpty().Attributes(), true)
		e := ms.AppendEmpty()
		e.SetName("exp")
		e.SetEmptyExponentialHistogram().DataPoints().AppendEmpty()
		putMarker(e.ExponentialHistogram().DataPoints().AppendEmpty().Attributes(), true)
		s := ms.AppendEmpty()
		s.SetName("summary")
		s.SetEmptySummary().DataPoints().AppendEmpty()
		putMarker(s.Summary().DataPoints().AppendEmpty().Attributes(), true)
		only := ms.AppendEmpty()
		only.SetName("syn_only")
		putMarker(only.SetEmptyGauge().DataPoints().AppendEmpty().Attributes(), true)

		if !HasSyntheticMetrics(md) {
			t.Fatal("HasSyntheticMetrics = false")
		}
		DropSyntheticMetrics(md)
		got := metricPoints(md)
		want := map[string]int{"gauge": 1, "sum": 1, "histogram": 1, "exp": 1, "summary": 1}
		if len(got) != len(want) {
			t.Fatalf("points = %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("points = %v, want %v", got, want)
			}
		}
		if HasSyntheticMetrics(md) {
			t.Fatal("HasSyntheticMetrics after drop = true")
		}
	})
}

func TestDropSyntheticLogs(t *testing.T) {
	t.Run("UC-041/BR-011 marked resources and records go, real records stay", func(t *testing.T) {
		ld := plog.NewLogs()
		synRes := ld.ResourceLogs().AppendEmpty()
		putMarker(synRes.Resource().Attributes(), true)
		synRes.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().SetEventName("game.start")
		lrs := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords()
		lrs.AppendEmpty().SetEventName("real")
		putMarker(lrs.AppendEmpty().Attributes(), "true")

		if !HasSyntheticLogs(ld) {
			t.Fatal("HasSyntheticLogs = false")
		}
		DropSyntheticLogs(ld)
		if ld.ResourceLogs().Len() != 1 {
			t.Fatalf("resources = %d, want 1", ld.ResourceLogs().Len())
		}
		got := ld.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords()
		if got.Len() != 1 || got.At(0).EventName() != "real" {
			t.Fatalf("records = %d, want only real", got.Len())
		}
	})
}
