package decision

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/joustmania/agent/gamecontext"
	"github.com/joustmania/agent/telemetry/keys"
)

// newInterventionID returns a fresh dispatch-unique id: the intervention id a decision
// is written with (UC-101 BR-003, #2665), which ties its effect record (#918) and the
// game's intervention moment to the decision that produced it. A UUID v4 hex string —
// opaque, collision-resistant across agent restarts, never compared for ordering.
func newInterventionID() string { return uuid.NewString() }

// effect.go closes the game-path intervention-effect MEASUREMENT loop (#918). The
// agent attributes a decision's reasoning OUTBOUND (objectives / LayerState onto the
// decision span, #729), but until now it never measured whether the intervention
// HELPED: the infra path has fitness->rollback (#828), the game path had nothing.
//
// THE LOOP (measurement only — it never reverts an intervention):
//
//  1. At dispatch (runDecision, the applied-decision path) the loop STAMPS a baseline
//     of the game's fitness/objective signals — the SAME dotted-key quantities the
//     decision span carries as fitness.evaluated — tagged with a dispatch-unique
//     intervention id.
//  2. After a follow-up window (DefaultEffectWindow, env-tunable) the sampler reads the
//     SAME signals for the SAME game again and computes the per-objective DELTA.
//  3. It emits an effect record attributed to the intervention id: an
//     agent.intervention.effect span (one per intervention, #2640) AND an
//     agent_intervention_effect_delta metric, both carrying baseline/follow-up/delta +
//     window, so the M5 dashboard (#791/#792) and the #844 retro can consume it.
//
// LEAK-FREENESS / LIFECYCLE SAFETY (#923 goleak): every scheduled follow-up runs in a
// goroutine tracked on effectWG and bounded by rootCtx. The goroutine selects on the
// follow-up timer AND rootCtx.Done(); on shutdown (rootCtx cancelled) or when the
// game's partition is gone at follow-up time it emits a clearly-marked ABORTED record
// (or, on shutdown, simply returns) — it never blocks shutdown and never leaks a timer
// (the timer is always stopped). AwaitInflight joins effectWG alongside the #917
// inference goroutines.
//
// PER-GAME ISOLATION (#845): the sampler re-reads the game's CURRENT signals through
// the SAME ContextProvider (MuxContextProvider) the #917 async path uses, keyed by THIS
// loop's gameID — so an effect sample for game A reads game A's signals only.
//
// BOUNDED MEMORY: the in-flight pending-effect set is capped (maxPendingEffects). When
// the cap is reached a dispatch records no baseline (the decision still dispatches
// normally — measurement is best-effort, never on the hot decision path's critical
// route). The set is drained as samples complete and emptied when the loop is dropped.

// DefaultEffectWindow is the follow-up window: how long after an intervention is
// dispatched the loop samples the game's fitness signals again to measure the effect
// (#918). 20s sits in the issue's 15-30s band — long enough for an intervention
// (shield, sensitivity nudge, audio cue) to move the objective signals, short enough to
// still attribute the change to that intervention. Tuned live by the
// effect.window_seconds agent flag (#1788, see SetEffectWindowSource / tuning.go).
const DefaultEffectWindow = 20 * time.Second

// maxPendingEffects bounds the in-flight follow-up samples PER LOOP (per game) so a
// burst of dispatched interventions cannot launch unbounded goroutines/timers. At the
// per-game weighted rate limit (max_interventions_per_minute) a 20s window holds only a
// handful of samples in flight; 64 is comfortably above that while still a hard ceiling.
const maxPendingEffects = 64

// effectSampler is the per-Loop intervention-effect machinery (#918), embedded in Loop
// like asyncInferState. It owns the follow-up window, the in-flight goroutine tracking,
// the bounded pending set, the effect metric, and the test seams (now / afterFunc).
type effectSampler struct {
	// window is the follow-up delay; zero means DefaultEffectWindow. Set by
	// SetEffectWindow at construction.
	window time.Duration
	// windowSource, when set, resolves the window at use time (#1788) and wins
	// over window.
	windowSource func() time.Duration
	// effectWG tracks every in-flight follow-up goroutine so AwaitInflight can join
	// them at shutdown (no leak) and tests can wait for a sample to land deterministically.
	effectWG sync.WaitGroup
	// pendingMu guards pending; the concurrent trace+metrics Export handlers both drive
	// runDecision and so both schedule samples.
	pendingMu sync.Mutex
	// pending counts in-flight follow-up samples for THIS loop (bounded by
	// maxPendingEffects). A scheduled sample increments it; the goroutine decrements it
	// when it completes. Never goes negative.
	pending int
	// effectDelta records agent_intervention_effect_delta. Never nil (NewLoop seeds a
	// no-op), so the record path is always safe.
	effectDelta otelmetric.Float64Histogram
	// afterFunc schedules fn to run after d, returning a stop function. It is the test
	// seam for the follow-up timer: production uses realAfterFunc (a context-cancellable
	// time.Timer); tests inject a manual scheduler to fire the window deterministically
	// with no real sleep. Nil means realAfterFunc.
	afterFunc func(d time.Duration, fn func()) (stop func() bool)
}

// SetEffectWindowSource makes the loop resolve the follow-up window at use time, on
// every dispatched intervention (effect.window_seconds, #1788). It wins over
// SetEffectWindow; a non-positive result falls back to DefaultEffectWindow. Set at
// construction, before the loop serves.
func (l *Loop) SetEffectWindowSource(src func() time.Duration) { l.windowSource = src }

// SetEffectWindow overrides the follow-up window (#918). A non-positive value leaves
// DefaultEffectWindow in place. main.go resolves the effect.window_seconds flag and passes it
// here. Set at construction, before the loop serves; not safe to call concurrently with
// OnEvaluate.
func (l *Loop) SetEffectWindow(d time.Duration) {
	if d <= 0 {
		return
	}
	l.window = d
}

// effectWindow returns the configured follow-up window, defaulting to
// DefaultEffectWindow when unset.
func (l *Loop) effectWindow() time.Duration {
	if l.windowSource != nil {
		if d := l.windowSource(); d > 0 {
			return d
		}
		return DefaultEffectWindow
	}
	if l.window <= 0 {
		return DefaultEffectWindow
	}
	return l.window
}

// AwaitEffects blocks until every in-flight follow-up sample for this loop has
// completed (#918), mirroring AwaitInflight for the #917 inference goroutines. Tests
// call it to make a follow-up land deterministically before asserting; shutdown joins
// it via AwaitInflight. Safe to call when nothing is pending (returns immediately).
func (l *Loop) AwaitEffects() { l.effectWG.Wait() }

// scheduleEffectSample stamps the baseline fitness signals for a just-DISPATCHED
// intervention and schedules a follow-up sample after the window (#918). It is called
// from runDecision on the applied-decision path ONLY (blocked decisions never moved the
// game, so there is nothing to measure). The sample's `intervention.id` is the
// decision's own InterventionID, made before the write and already on the decision
// span (UC-101 BR-003, #2665); only a decision without one (a probe-mode noop, which
// writes nothing, or a direct caller) gets a fresh id here. It returns the id used, or
// "" when no sample was scheduled (effect measurement not wired, no baseline signals,
// or the pending cap reached); the caller stamps it on the decision span only when the
// decision had no id of its own.
//
// It NEVER blocks the hot decision path: the baseline is a cheap map copy and the
// follow-up runs in its own goroutine. Without a context provider (the re-validation
// source, same as the #917 async path) there is no way to read the game's future
// signals, so it no-ops — an un-wired Loop is behavior-unchanged.
func (l *Loop) scheduleEffectSample(decisionCtx context.Context, c gamecontext.GameContext, d Decision, baselineVals map[string]float64, thresholds FitnessThresholds) string {
	// No re-validation source => cannot read the future context => nothing to measure.
	if l.ctxProvider == nil {
		return ""
	}
	if len(baselineVals) == 0 {
		// The cycle evaluated no fitness signals (e.g. the engine had no signals, or a
		// Probe/Noop engine): there is no baseline to compare against, so skip — emitting
		// an all-zero delta would be misleading, not measurement.
		return ""
	}

	// Bound the in-flight set so a burst of dispatches cannot launch unbounded
	// goroutines/timers. Over the cap we skip scheduling (the dispatch itself still
	// happens; only the measurement is dropped, best-effort by design).
	l.pendingMu.Lock()
	if l.pending >= maxPendingEffects {
		l.pendingMu.Unlock()
		return ""
	}
	l.pending++
	l.pendingMu.Unlock()

	now := l.now
	if now == nil {
		now = time.Now
	}
	// Reuse the id the decision was written with (UC-101 BR-003, #2665): the effect's
	// intervention.id is the decision's decision.intervention_id and the write's
	// decision_id. A decision without one (a direct caller) gets a fresh id.
	id := d.InterventionID
	if id == "" {
		id = newInterventionID()
	}
	dispatchedAt := now()
	objective := d.ObjectiveServed
	if objective == "" {
		objective = DefaultObjectives
	}

	// Defensive copy: the baseline is read by the follow-up goroutine seconds later, so
	// it must not alias a map the caller may reuse/mutate on a later cycle.
	baseline := make(map[string]float64, len(baselineVals))
	for k, v := range baselineVals {
		baseline[k] = v
	}

	root := l.rootCtx
	if root == nil {
		root = context.Background()
	}
	window := l.effectWindow()

	sample := pendingEffect{
		id:           id,
		intervention: d.Intervention,
		objective:    objective,
		baseline:     baseline,
		thresholds:   thresholds,
		dispatchedAt: dispatchedAt,
		window:       window,
		// Capture the game-session trace ids from the GameContext in scope at schedule
		// time (#1133), stamped as searchable attributes on the deferred effect span.
		// nil-safe: empty ids => no attribute.
		gameTraceID:     c.GameTraceID,
		gameTraceSpanID: c.GameTraceSpanID,
		// Capture the CAUSING agent.decision span's SpanContext while it is still live
		// (decisionCtx is the decision span's context — it calls schedule). #1178: the effect
		// span (emitted seconds later) is started as a remote CHILD of this decision span, so
		// decision->effect is structural (nested) and lands inside the game trace transitively
		// (the decision is itself under the game span). Invalid SpanContext (no-op tracer /
		// no active span) => no remote parent => the effect span stays its own root.
		decisionSC: trace.SpanContextFromContext(decisionCtx),
	}

	l.effectWG.Add(1)
	fired := make(chan struct{})

	after := l.afterFunc
	if after == nil {
		after = realAfterFunc(root)
	}
	stop := after(window, func() { close(fired) })

	go func() {
		defer l.effectWG.Done()
		defer l.releasePending()
		select {
		case <-root.Done():
			// Shutdown / loop dropped before the window elapsed. Stop the timer (no leak)
			// and emit nothing: a measurement we never took is not a result. The decision
			// itself was already audited on its own trace.
			stop()
			return
		case <-fired:
			l.emitEffectSample(root, sample)
		}
	}()
	return id
}

// releasePending decrements the in-flight pending counter, floored at zero.
func (l *Loop) releasePending() {
	l.pendingMu.Lock()
	if l.pending > 0 {
		l.pending--
	}
	l.pendingMu.Unlock()
}

// pendingEffect is one scheduled follow-up sample's captured state (#918): the baseline
// signals stamped at dispatch, the thresholds to recompute the SAME fitness functions
// against the follow-up context, and the attribution (id / intervention / objective).
type pendingEffect struct {
	id           string
	intervention string
	objective    string
	baseline     map[string]float64
	thresholds   FitnessThresholds
	dispatchedAt time.Time
	window       time.Duration
	// gameTraceID/gameTraceSpanID are the game-session trace ids captured at schedule
	// time (#1133), stamped as searchable attributes on the effect span. Empty => no attr.
	gameTraceID     string
	gameTraceSpanID string
	// decisionSC is the SpanContext of the agent.decision span that caused this effect,
	// captured live at schedule time (#1178). The effect root is started as a remote CHILD
	// of it (nested under the decision in the game trace). Invalid => effect stays own-root.
	decisionSC trace.SpanContext
}

// emitEffectSample is the follow-up half (#918): it re-reads the game's CURRENT signals
// through the ContextProvider (same per-game source the #917 async path uses), recomputes
// the SAME fitness functions with the dispatch-time thresholds, computes the per-signal
// delta, and emits ONE agent.intervention.effect span + the agent_intervention_effect_delta
// metric (progress-type signals only, see effectMetricSignal). If the partition is
// gone (game ended/evicted before the window elapsed) the span is marked ABORTED — no
// delta, no metric — so the cancellation is visible rather than silent.
//
// Span shape (#2640): one span per intervention. Every signal evaluated at dispatch is
// an entry of the intervention.signals JSON attribute; the objective's primary signal
// (primaryEffectSignal) is also stamped as intervention.signal / fitness_baseline /
// fitness_followup / fitness_delta, so the headline number reads without parsing JSON.
// Before #2640 there was one span per signal (~12 per intervention), which buried the
// decisions in a game's trace.
//
// Parenting (#1325, UC-040 BR-013): the span is a DIRECT remote child of the CAUSING
// agent.decision span (#1178), backdated to dispatch time so it covers the follow-up
// window. The effect outlives its decision (the window is 20 s, the decision span
// milliseconds). That is valid OTel (a child may end after its parent) and keeps the
// cause one click away. Graceful fallback: an invalid decisionSC (no-op tracer / no
// active decision span) yields no remote parent, so the effect span is its own root.
func (l *Loop) emitEffectSample(ctx context.Context, s pendingEffect) {
	current, ok := l.ctxProvider.Current(l.gameID)

	effectParent := ctx
	if s.decisionSC.IsValid() {
		effectParent = trace.ContextWithRemoteSpanContext(ctx, s.decisionSC)
	}
	// game.id + the intervention id (#1174) make a span found in isolation attributable
	// to its game and cause; the SAME key the decision span uses joins decision<->effect
	// even on backends that don't surface Link attrs. The game trace ids stay as
	// searchable attributes.
	attrs := []attribute.KeyValue{
		attribute.String(AttrSessionID, l.gameID),
		attribute.String(AttrGameID, l.gameID),
		attribute.String(AttrEffectInterventionID, s.id),
		attribute.String(AttrDecisionInterventionID, s.id),
		attribute.String(AttrEffectIntervention, s.intervention),
		attribute.String(AttrEffectObjective, s.objective),
		attribute.Float64(AttrEffectWindowSeconds, s.window.Seconds()),
	}
	if s.gameTraceID != "" {
		attrs = append(attrs, attribute.String(AttrGameTraceID, s.gameTraceID))
	}
	if s.gameTraceSpanID != "" {
		attrs = append(attrs, attribute.String(AttrGameTraceSpanID, s.gameTraceSpanID))
	}

	var followUp map[string]float64
	if ok {
		followUp = EvaluateFitness(current, s.thresholds).Evaluated()
		attrs = append(attrs, effectSignalAttributes(s.objective, s.baseline, followUp)...)
	} else {
		// Game ended / partition evicted before the window elapsed (#918 cancellation):
		// there is no current context to sample, so the effect is unmeasurable. The
		// span is marked aborted — no fabricated delta, no metric.
		attrs = append(attrs, attribute.Bool(AttrEffectAborted, true))
	}
	_, span := l.Tracer.Start(effectParent, SpanInterventionEffect,
		trace.WithTimestamp(s.dispatchedAt),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	span.End()
	if !ok {
		return
	}

	// The metric keeps one point per progress-type signal still present at follow-up,
	// in a stable order (#1444).
	for _, key := range sortedKeys(s.baseline) {
		fu, present := followUp[key]
		if !present || !effectMetricSignal(key) {
			// A vanished signal has no honest delta; elapsed time and constant
			// thresholds stay on the span only (#1444).
			continue
		}
		l.effectDelta.Record(ctx, fu-s.baseline[key], otelmetric.WithAttributes(
			attribute.String(AttrEffectIntervention, s.intervention),
			attribute.String(AttrEffectObjective, s.objective),
			attribute.String(AttrEffectSignal, key),
		))
	}
}

// primaryEffectSignal is the signal each objective's fitness binds on (fitness.go):
// its Progress. The effect span stamps it as the top-level
// intervention.fitness_delta (#2640). Chaos and "unset" have none.
var primaryEffectSignal = map[string]string{
	ObjectiveEndurance:  "endurance.session_progress",
	ObjectiveBalanced:   "balanced.skill_gap_progress",
	ObjectiveAccelerate: "accelerate.session_progress",
}

// effectSignalEntry is one intervention.signals JSON entry. FollowUp and Delta are
// absent for a signal that was evaluable at dispatch but not at follow-up (Aborted).
// A non-finite value (NaN, Inf) has no JSON encoding: it is left out and the entry
// is marked Aborted, so one bad signal never drops the whole attribute.
type effectSignalEntry struct {
	Baseline *float64 `json:"baseline,omitempty"`
	FollowUp *float64 `json:"followup,omitempty"`
	Delta    *float64 `json:"delta,omitempty"`
	Aborted  bool     `json:"aborted,omitempty"`
}

// effectSignalAttributes builds the measured-sample attributes of the single effect
// span (#2640): intervention.signals (every baseline signal, values rounded to 4
// decimals, keys sorted by encoding/json), the primary signal's exact
// baseline/follow-up/delta when the objective has one, and effect_aborted when no
// baseline signal survived to the follow-up.
func effectSignalAttributes(objective string, baseline, followUp map[string]float64) []attribute.KeyValue {
	entries := make(map[string]effectSignalEntry, len(baseline))
	measured := 0
	for key, base := range baseline {
		var e effectSignalEntry
		if finite(base) {
			b := round4(base)
			e.Baseline = &b
		}
		if fu, present := followUp[key]; present && finite(fu) && e.Baseline != nil {
			f, d := round4(fu), round4(fu-base)
			e.FollowUp, e.Delta = &f, &d
			measured++
		} else {
			e.Aborted = true
		}
		entries[key] = e
	}
	var attrs []attribute.KeyValue
	if raw, err := json.Marshal(entries); err == nil {
		attrs = append(attrs, attribute.String(keys.AttrEffectSignals, string(raw)))
	}
	if measured == 0 {
		attrs = append(attrs, attribute.Bool(AttrEffectAborted, true))
	}
	key, ok := primaryEffectSignal[objective]
	if !ok {
		return attrs
	}
	base, ok := baseline[key]
	if !ok {
		return attrs
	}
	attrs = append(attrs,
		attribute.String(AttrEffectSignal, key),
		attribute.Float64(AttrEffectBaseline, base),
	)
	if fu, present := followUp[key]; present {
		attrs = append(attrs,
			attribute.Float64(AttrEffectFollowUp, fu),
			attribute.Float64(AttrEffectDelta, fu-base),
		)
	}
	return attrs
}

// finite reports whether v has a JSON encoding (not NaN, not +-Inf).
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// round4 rounds v to 4 decimals for the compact JSON attribute.
func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }

// effectMetricSignal reports whether a fitness signal's delta is recorded on the
// agent_intervention_effect_delta metric (#1444). Only the 0..1 progress signals
// (`*_progress`, `*_ratio`) and the skill gap qualify: their deltas share a unit and
// fit EffectDeltaBuckets, so a quantile across signals means something. The other
// Evaluated() keys stay on the agent.intervention.effect spans only:
//   - `*_seconds` keys are elapsed time (endurance.session_seconds), whose delta is
//     roughly the follow-up window, or constant thresholds
//     (endurance.min_session_seconds, accelerate.target_session_seconds);
//   - threshold keys (balanced.max_skill_gap, balanced.spike_survival_threshold)
//     are flag values, so their delta is always 0.
//
// An allow-list, so a new threshold or time key never leaks into the metric;
// TestEffect_MetricSignalsClassifyEveryFitnessKey makes a new key a conscious choice.
func effectMetricSignal(key string) bool {
	return strings.HasSuffix(key, "_progress") ||
		strings.HasSuffix(key, "_ratio") ||
		strings.HasSuffix(key, ".skill_gap")
}

// sortedKeys returns the map keys sorted, for deterministic span/metric emission order.
func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// realAfterFunc is the production follow-up scheduler: a time.Timer whose firing is
// suppressed (and stopped) if root is cancelled first. The returned stop function stops
// the timer (idempotent), so a shutdown-cancelled sample never leaves a timer running.
// It is the default when no afterFunc test seam is injected.
func realAfterFunc(root context.Context) func(d time.Duration, fn func()) func() bool {
	return func(d time.Duration, fn func()) func() bool {
		timer := time.NewTimer(d)
		go func() {
			select {
			case <-timer.C:
				fn()
			case <-root.Done():
				// Cancelled before firing: stop the timer and do NOT call fn. The
				// scheduling goroutine observes root.Done() on its own and returns.
				timer.Stop()
			}
		}()
		return timer.Stop
	}
}
