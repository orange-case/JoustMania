package gamecontext

import (
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// AttrSynthetic marks benchmark load (#2576). The agent's backup to the
// collector route (UC-041 BR-011): the receiver drops marked telemetry before
// the multiplexer unless ai.observe_synthetic is on.
const AttrSynthetic = "orangecase.synthetic"

// isSynthetic matches what the collector filters match: the bool true or the
// string "true", nothing else.
func isSynthetic(attrs pcommon.Map) bool {
	v, ok := attrs.Get(AttrSynthetic)
	if !ok {
		return false
	}
	switch v.Type() {
	case pcommon.ValueTypeBool:
		return v.Bool()
	case pcommon.ValueTypeStr:
		return v.Str() == "true"
	default:
		return false
	}
}

// HasSyntheticSpans reports whether td holds a marked resource or span.
func HasSyntheticSpans(td ptrace.Traces) bool {
	rss := td.ResourceSpans()
	for i := 0; i < rss.Len(); i++ {
		rs := rss.At(i)
		if isSynthetic(rs.Resource().Attributes()) {
			return true
		}
		sss := rs.ScopeSpans()
		for j := 0; j < sss.Len(); j++ {
			spans := sss.At(j).Spans()
			for k := 0; k < spans.Len(); k++ {
				if isSynthetic(spans.At(k).Attributes()) {
					return true
				}
			}
		}
	}
	return false
}

// DropSyntheticSpans removes marked resources and spans from td in place.
func DropSyntheticSpans(td ptrace.Traces) {
	td.ResourceSpans().RemoveIf(func(rs ptrace.ResourceSpans) bool {
		if isSynthetic(rs.Resource().Attributes()) {
			return true
		}
		rs.ScopeSpans().RemoveIf(func(ss ptrace.ScopeSpans) bool {
			ss.Spans().RemoveIf(func(s ptrace.Span) bool { return isSynthetic(s.Attributes()) })
			return ss.Spans().Len() == 0
		})
		return rs.ScopeSpans().Len() == 0
	})
}

// HasSyntheticMetrics reports whether md holds a marked resource or data point.
func HasSyntheticMetrics(md pmetric.Metrics) bool {
	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		rm := rms.At(i)
		if isSynthetic(rm.Resource().Attributes()) {
			return true
		}
		sms := rm.ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			ms := sms.At(j).Metrics()
			for k := 0; k < ms.Len(); k++ {
				if metricHasSynthetic(ms.At(k)) {
					return true
				}
			}
		}
	}
	return false
}

// DropSyntheticMetrics removes marked resources and data points from md in
// place, and the metrics and scopes left empty.
func DropSyntheticMetrics(md pmetric.Metrics) {
	md.ResourceMetrics().RemoveIf(func(rm pmetric.ResourceMetrics) bool {
		if isSynthetic(rm.Resource().Attributes()) {
			return true
		}
		rm.ScopeMetrics().RemoveIf(func(sm pmetric.ScopeMetrics) bool {
			sm.Metrics().RemoveIf(dropSyntheticPoints)
			return sm.Metrics().Len() == 0
		})
		return rm.ScopeMetrics().Len() == 0
	})
}

func metricHasSynthetic(m pmetric.Metric) bool {
	found := false
	forEachPointAttrs(m, func(a pcommon.Map) {
		found = found || isSynthetic(a)
	})
	return found
}

func forEachPointAttrs(m pmetric.Metric, fn func(pcommon.Map)) {
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		for i := 0; i < m.Gauge().DataPoints().Len(); i++ {
			fn(m.Gauge().DataPoints().At(i).Attributes())
		}
	case pmetric.MetricTypeSum:
		for i := 0; i < m.Sum().DataPoints().Len(); i++ {
			fn(m.Sum().DataPoints().At(i).Attributes())
		}
	case pmetric.MetricTypeHistogram:
		for i := 0; i < m.Histogram().DataPoints().Len(); i++ {
			fn(m.Histogram().DataPoints().At(i).Attributes())
		}
	case pmetric.MetricTypeExponentialHistogram:
		for i := 0; i < m.ExponentialHistogram().DataPoints().Len(); i++ {
			fn(m.ExponentialHistogram().DataPoints().At(i).Attributes())
		}
	case pmetric.MetricTypeSummary:
		for i := 0; i < m.Summary().DataPoints().Len(); i++ {
			fn(m.Summary().DataPoints().At(i).Attributes())
		}
	}
}

// dropSyntheticPoints removes m's marked data points and reports whether m is
// left without any (a metric of an unknown type is kept).
func dropSyntheticPoints(m pmetric.Metric) bool {
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		dps := m.Gauge().DataPoints()
		dps.RemoveIf(func(p pmetric.NumberDataPoint) bool { return isSynthetic(p.Attributes()) })
		return dps.Len() == 0
	case pmetric.MetricTypeSum:
		dps := m.Sum().DataPoints()
		dps.RemoveIf(func(p pmetric.NumberDataPoint) bool { return isSynthetic(p.Attributes()) })
		return dps.Len() == 0
	case pmetric.MetricTypeHistogram:
		dps := m.Histogram().DataPoints()
		dps.RemoveIf(func(p pmetric.HistogramDataPoint) bool { return isSynthetic(p.Attributes()) })
		return dps.Len() == 0
	case pmetric.MetricTypeExponentialHistogram:
		dps := m.ExponentialHistogram().DataPoints()
		dps.RemoveIf(func(p pmetric.ExponentialHistogramDataPoint) bool { return isSynthetic(p.Attributes()) })
		return dps.Len() == 0
	case pmetric.MetricTypeSummary:
		dps := m.Summary().DataPoints()
		dps.RemoveIf(func(p pmetric.SummaryDataPoint) bool { return isSynthetic(p.Attributes()) })
		return dps.Len() == 0
	default:
		return false
	}
}

// HasSyntheticLogs reports whether ld holds a marked resource or log record.
func HasSyntheticLogs(ld plog.Logs) bool {
	rls := ld.ResourceLogs()
	for i := 0; i < rls.Len(); i++ {
		rl := rls.At(i)
		if isSynthetic(rl.Resource().Attributes()) {
			return true
		}
		sls := rl.ScopeLogs()
		for j := 0; j < sls.Len(); j++ {
			lrs := sls.At(j).LogRecords()
			for k := 0; k < lrs.Len(); k++ {
				if isSynthetic(lrs.At(k).Attributes()) {
					return true
				}
			}
		}
	}
	return false
}

// DropSyntheticLogs removes marked resources and log records from ld in place.
func DropSyntheticLogs(ld plog.Logs) {
	ld.ResourceLogs().RemoveIf(func(rl plog.ResourceLogs) bool {
		if isSynthetic(rl.Resource().Attributes()) {
			return true
		}
		rl.ScopeLogs().RemoveIf(func(sl plog.ScopeLogs) bool {
			sl.LogRecords().RemoveIf(func(lr plog.LogRecord) bool { return isSynthetic(lr.Attributes()) })
			return sl.LogRecords().Len() == 0
		})
		return rl.ScopeLogs().Len() == 0
	})
}
