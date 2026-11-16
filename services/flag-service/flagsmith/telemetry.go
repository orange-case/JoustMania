package flagsmith

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
)

// Span names (telemetry.md §10.4).
const (
	SpanPull      = "flagservice.flagsmith.pull"
	SpanPush      = "flagservice.flagsmith.push"
	SpanReconcile = "flagservice.flagsmith.reconcile"
	EventConflict = "flagservice.flagsmith.conflict"
)

// Pull and reconcile triggers.
const (
	TriggerPoll      = "poll"
	TriggerRealtime  = "realtime"
	TriggerStart     = "start"
	TriggerReconnect = "reconnect"
)

// Push ops.
const (
	OpValue          = "value"
	OpOverride       = "override"
	OpDeleteOverride = "delete_override"
	OpIdentity       = "identity"
	OpDeleteIdentity = "delete_identity"
	OpFeature        = "feature"
	OpTag            = "tag"
)

// Conflict resolutions (FR-210): Flagsmith wins every conflict since #2621.
const (
	ResolutionFlagsmith = "flagsmith"
)

// Results.
const (
	ResultOK          = "ok"
	ResultChanged     = "changed"
	ResultNotModified = "not_modified"
	ResultRejected    = "rejected"
	ResultError       = "error"
)

// Attribute keys.
const (
	AttrTrigger     = "flagservice.flagsmith.trigger"
	AttrChanges     = "flagservice.flagsmith.changes"
	AttrConflicts   = "flagservice.flagsmith.conflicts"
	AttrOp          = "flagservice.flagsmith.op"
	AttrMerged      = "flagservice.flagsmith.merged"
	AttrApplied     = "flagservice.flagsmith.applied"
	AttrPushed      = "flagservice.flagsmith.pushed"
	AttrLocalValue  = "flagservice.flagsmith.local_value"
	AttrRemoteValue = "flagservice.flagsmith.remote_value"
	AttrResolution  = "flagservice.flagsmith.resolution"
	AttrResult      = "flagservice.result"
	AttrReason      = "flagservice.reason"
	AttrScope       = "flagservice.scope"
	AttrWriter      = "flagservice.writer"
	AttrFlagKey     = "feature_flag.key"
	AttrFlagSet     = "feature_flag.set.id"
	AttrHTTPStatus  = "http.response.status_code"
)

// Metric names (telemetry.md §10.4). NewMetrics spells them as literals, so
// the telemetry contract scanner (tests/contract/telemetry_inventory.py) finds
// the producers; TestMetricsRegistered keeps these constants equal to them.
const (
	MetricLink        = "flagservice_flagsmith_link"
	MetricPulls       = "flagservice_flagsmith_pulls_total"
	MetricPushes      = "flagservice_flagsmith_pushes_total"
	MetricPending     = "flagservice_flagsmith_pending"
	MetricOutOfSync   = "flagservice_flagsmith_out_of_sync"
	MetricConflicts   = "flagservice_flagsmith_conflicts_total"
	MetricChangeDelay = "flagservice_flagsmith_change_delay_seconds"
	MetricHealthPosts = "flagservice_flagsmith_health_posts_total"
)

// Metrics are the link's instruments. They are registered whether the link
// is on or off (the link gauge then shows off). A nil *Metrics records
// nothing. Labels never carry a game id, a serial or a key value.
type Metrics struct {
	pulls       metric.Int64Counter
	pushes      metric.Int64Counter
	conflicts   metric.Int64Counter
	healthPosts metric.Int64Counter
	changeDelay metric.Float64Histogram
	reg         metric.Registration
}

// NewMetrics creates the instruments on meter (nil: a no-op meter). mode and
// counts feed the observable gauges.
func NewMetrics(meter metric.Meter, mode func() Mode, counts func() (pending, outOfSync, refused int)) (*Metrics, error) {
	if meter == nil {
		meter = noop.NewMeterProvider().Meter("flagsmith")
	}
	m := &Metrics{}
	var err error
	if m.pulls, err = meter.Int64Counter("flagservice_flagsmith_pulls_total",
		metric.WithDescription("Flagsmith environment-document fetches by trigger and result")); err != nil {
		return nil, err
	}
	if m.pushes, err = meter.Int64Counter("flagservice_flagsmith_pushes_total",
		metric.WithDescription("Flagsmith Admin API calls by op, result and reason")); err != nil {
		return nil, err
	}
	if m.conflicts, err = meter.Int64Counter("flagservice_flagsmith_conflicts_total",
		metric.WithDescription("Conflicts between the stack and Flagsmith on reconnect, by resolution (FR-210)")); err != nil {
		return nil, err
	}
	if m.healthPosts, err = meter.Int64Counter("flagservice_flagsmith_health_posts_total",
		metric.WithDescription("Flagsmith feature-health webhook posts by status and result (FR-213)")); err != nil {
		return nil, err
	}
	if m.changeDelay, err = meter.Float64Histogram("flagservice_flagsmith_change_delay_seconds",
		metric.WithDescription("Flagsmith change time to the write accepted (NFR-058)"),
		metric.WithExplicitBucketBoundaries(0.5, 1, 2, 5, 10, 15, 20, 30, 45, 60, 120)); err != nil {
		return nil, err
	}
	link, err := meter.Int64ObservableGauge("flagservice_flagsmith_link",
		metric.WithDescription("1 for the Flagsmith link's current state (off, starting, online, offline), 0 for the others"))
	if err != nil {
		return nil, err
	}
	pendingG, err := meter.Int64ObservableGauge("flagservice_flagsmith_pending",
		metric.WithDescription("Local values waiting to be sent to Flagsmith"))
	if err != nil {
		return nil, err
	}
	outOfSyncG, err := meter.Int64ObservableGauge("flagservice_flagsmith_out_of_sync",
		metric.WithDescription("Flags and scopes whose value differs between the stack and Flagsmith"))
	if err != nil {
		return nil, err
	}
	m.reg, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		cur := ModeOff
		if mode != nil {
			cur = mode()
		}
		for _, s := range Modes {
			n := int64(0)
			if s == cur {
				n = 1
			}
			o.ObserveInt64(link, n, metric.WithAttributes(attribute.String("state", string(s))))
		}
		var p, oos int
		if counts != nil {
			p, oos, _ = counts()
		}
		o.ObserveInt64(pendingG, int64(p))
		o.ObserveInt64(outOfSyncG, int64(oos))
		return nil
	}, link, pendingG, outOfSyncG)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// RecordPull counts one document fetch.
func (m *Metrics) RecordPull(ctx context.Context, trigger, result string) {
	if m == nil {
		return
	}
	m.pulls.Add(ctx, 1, metric.WithAttributes(attribute.String("trigger", trigger), attribute.String("result", result)))
}

// RecordPush counts one Admin API call (reason "" on success).
func (m *Metrics) RecordPush(ctx context.Context, op, result, reason string) {
	if m == nil {
		return
	}
	m.pushes.Add(ctx, 1, metric.WithAttributes(attribute.String("op", op), attribute.String("result", result), attribute.String("reason", reason)))
}

// RecordConflict counts one conflict.
func (m *Metrics) RecordConflict(ctx context.Context, resolution string) {
	if m == nil {
		return
	}
	m.conflicts.Add(ctx, 1, metric.WithAttributes(attribute.String("resolution", resolution)))
}

// RecordHealthPost counts one feature-health webhook post.
func (m *Metrics) RecordHealthPost(ctx context.Context, status, result string) {
	if m == nil {
		return
	}
	m.healthPosts.Add(ctx, 1, metric.WithAttributes(attribute.String("status", status), attribute.String("result", result)))
}

// RecordChangeDelay records a Flagsmith change's delay to the accepted write.
func (m *Metrics) RecordChangeDelay(ctx context.Context, trigger string, d time.Duration) {
	if m == nil {
		return
	}
	m.changeDelay.Record(ctx, d.Seconds(), metric.WithAttributes(attribute.String("trigger", trigger)))
}

// StartPull starts a flagservice.flagsmith.pull span (client).
func StartPull(ctx context.Context, tr trace.Tracer, trigger string) (context.Context, trace.Span) {
	return tr.Start(ctx, SpanPull, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String(AttrTrigger, trigger)))
}

// StartPush starts a flagservice.flagsmith.push span (client), linked to the
// flagservice.write span that queued the value when link is valid.
func StartPush(ctx context.Context, tr trace.Tracer, k Key, op, writer string, merged int, link trace.SpanContext) (context.Context, trace.Span) {
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String(AttrFlagKey, k.Flag),
			attribute.String(AttrFlagSet, k.Set),
			attribute.String(AttrScope, k.Scope),
			attribute.String(AttrOp, op),
			attribute.String(AttrWriter, writer),
			attribute.Int(AttrMerged, merged),
		),
	}
	if link.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: link}))
	}
	return tr.Start(ctx, SpanPush, opts...)
}

// StartReconcile starts a flagservice.flagsmith.reconcile span (internal).
func StartReconcile(ctx context.Context, tr trace.Tracer, trigger string) (context.Context, trace.Span) {
	return tr.Start(ctx, SpanReconcile, trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attribute.String(AttrTrigger, trigger)))
}

// ConflictEvent adds a flagservice.flagsmith.conflict event to the reconcile
// span.
func ConflictEvent(span trace.Span, k Key, local, remote, resolution string) {
	span.AddEvent(EventConflict, trace.WithAttributes(
		attribute.String(AttrFlagKey, k.Flag),
		attribute.String(AttrScope, k.Scope),
		attribute.String(AttrLocalValue, local),
		attribute.String(AttrRemoteValue, remote),
		attribute.String(AttrResolution, resolution),
	))
}

// EndSpan ends span with result and reason ("" omitted); a result other than
// ok, changed or not_modified marks it an error.
func EndSpan(span trace.Span, result, reason string) {
	span.SetAttributes(attribute.String(AttrResult, result))
	if reason != "" {
		span.SetAttributes(attribute.String(AttrReason, reason))
	}
	if result != ResultOK && result != ResultChanged && result != ResultNotModified {
		span.SetStatus(codes.Error, reason)
	}
	span.End()
}
