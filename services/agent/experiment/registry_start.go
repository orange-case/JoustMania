package experiment

// registry_start.go holds the Registry's START phase (#2635 split of registry.go):
// PROPOSED → RUNNING, the targeting write, the late-write strip after an abort and
// the experiment root span.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/joustmania/agent/experiment/journal"
	"github.com/joustmania/agent/telemetry/keys"
)

// Start moves a PROPOSED experiment to RUNNING and writes its experiment-scoped
// shadow targeting via the TargetingWriter (#977). After Start the experiment may
// be allocated shadow capacity. A no-op (already RUNNING) returns nil. If the
// targeting write fails the experiment stays PROPOSED (it never accrues games it
// cannot scope) and the error is returned.
//
// DEGRADED (nil TargetingWriter): Start still transitions the experiment to
// RUNNING, but no shadow-scoped flag rule is written — its games resolve the flag
// DEFAULT, making it a silent, non-experimental no-op experiment (no arm
// differentiation). Production always injects #977's writer; a nil writer is a
// test/degraded shape only (see RegistryConfig).
func (r *Registry) Start(ctx context.Context, id string) error {
	intent, proceed, err := r.startableIntent(id)
	if err != nil || !proceed {
		return err
	}
	if err := r.applyStartTargeting(ctx, id, intent); err != nil {
		return err
	}
	return r.commitStart(ctx, id, intent)
}

// startableIntent checks that experiment id may start and returns its intent. It
// takes and releases r.mu itself. proceed is false with a nil error when the
// experiment is already RUNNING (Start is a no-op).
func (r *Registry) startableIntent(id string) (intent journal.Intent, proceed bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[id]
	if e == nil {
		return intent, false, fmt.Errorf("registry: start: unknown experiment %q", id)
	}
	if e.status == StatusRunning {
		return intent, false, nil
	}
	if e.status != StatusProposed {
		return intent, false, fmt.Errorf("registry: start: experiment %q is %s, not proposed", id, e.status)
	}
	if r.shuttingDown() {
		// UC-043 A20 (#2332): no targeting write once the stop began; the caller aborts
		// the PROPOSED experiment (start_failed), or the shutdown AbortAll does.
		return intent, false, fmt.Errorf("registry: start %q: %w", id, ErrShuttingDown)
	}
	return e.journal.Intent(), true, nil
}

// applyStartTargeting writes the experiment-scoped shadow targeting (#977). It runs
// outside r.mu; it locks only for the #2340 re-check after a failed Apply.
func (r *Registry) applyStartTargeting(ctx context.Context, id string, intent journal.Intent) error {
	if r.target == nil {
		return nil
	}
	p := Proposal{
		FlagKey:           intent.FlagKey,
		ExperimentID:      id,
		ExperimentalValue: intent.ExperimentalValue,
		Rationale:         intent.Hypothesis,
	}
	err := r.target.Apply(ctx, p)
	if err == nil {
		return nil
	}
	applyErr := fmt.Errorf("registry: start: write targeting for %q: %w", id, err)
	// #2340: an Apply error (the client's request cap, the shutdown-cancelled
	// ctx) does not prove the write missed the flag service: it may land after
	// a concurrent teardown's strip. Strip again, best effort, when a teardown
	// began; an experiment still PROPOSED is aborted by the caller, which strips.
	r.mu.Lock()
	e := r.entries[id]
	if !startAbortedLocked(e) {
		r.mu.Unlock()
		return applyErr
	}
	jrnl := journalOf(e)
	r.mu.Unlock()
	if stripErr := r.stripAfterAbortedStart(ctx, id, intent.FlagKey, jrnl); stripErr != nil {
		return errors.Join(applyErr, stripErr)
	}
	return applyErr
}

// commitStart re-fetches the experiment after the targeting write and moves it to
// RUNNING, or strips the targeting again when a teardown began meanwhile (#2340).
func (r *Registry) commitStart(ctx context.Context, id string, intent journal.Intent) error {
	r.mu.Lock()
	// Re-fetch: a concurrent Abort may have removed/terminated it while unlocked.
	e := r.entries[id]
	if r.target != nil && startAbortedLocked(e) {
		// #2340: a teardown (Abort / AbortAll: kill switch, experiments_enabled off,
		// shutdown) began while the Apply above was in flight, so its strip may have
		// run before the Apply wrote the branch. Strip it again: the teardown does
		// not strip a second time, and the experiment never becomes RUNNING.
		jrnl := journalOf(e)
		r.mu.Unlock()
		return r.stripAfterAbortedStart(ctx, id, intent.FlagKey, jrnl)
	}
	defer r.mu.Unlock()
	if e == nil || e.status != StatusProposed {
		// Gone, tearing down or terminal without a targeting writer; or a concurrent
		// Start won (RUNNING) and its targeting is the live one: keep it.
		return nil
	}
	r.markRunningLocked(e, intent)
	return nil
}

// markRunningLocked moves e to RUNNING. The caller holds r.mu.
func (r *Registry) markRunningLocked(e *entry, intent journal.Intent) {
	e.status = StatusRunning
	// #1188: start the long-lived agent.experiment ROOT span now that the experiment is
	// RUNNING (idempotent — a no-op if it is somehow already started). The analysis
	// spans re-parent under it; teardown ends it.
	r.startSpanLocked(e, intent)
	r.recordDecision(e.journal, StatusRunning, "experiment started")
	r.emitProgressLocked(e, ProgressTriggerStarted, "", e.spanCtx) // #2059
	r.log.Info("experiment.started", "experiment_id", e.id)
}

// startAbortedLocked reports whether a teardown removed, began on or ended e while
// its Start was unlocked (#2340). The caller holds r.mu.
func startAbortedLocked(e *entry) bool {
	return e == nil || e.teardownBegun || e.status.IsTerminal()
}

// journalOf returns e's journal, or nil when e is gone. The caller holds r.mu.
func journalOf(e *entry) *journal.Journal {
	if e == nil {
		return nil
	}
	return e.journal
}

// Strip attempts and the per-attempt bound of the compensating strip (#2340).
const (
	abortedStartStripAttempts = 3
	abortedStartStripTimeout  = 5 * time.Second
	abortedStartStripBackoff  = 100 * time.Millisecond
)

// noteStartStripFailed is the journal note (a KindDecision without a status change)
// of a compensating strip that failed after every attempt (#2340).
const noteStartStripFailed = "start_targeting_strip_failed"

// noteReassertStripFailed is the journal note of a compensating strip after an
// arm-value re-assert (#2362) that failed after every attempt.
const noteReassertStripFailed = "reassert_targeting_strip_failed"

// ErrStartTargetingLeaked is returned by Start when the experiment was aborted
// while its targeting write was in flight and the compensating strip failed: the
// shadow targeting branch may remain on the flag service (#2340).
var ErrStartTargetingLeaked = errors.New("targeting written after abort could not be stripped")

// ErrReassertTargetingLeaked is returned by reassertArmValue when the experiment
// was torn down while its arm-value re-assert was in flight and the compensating
// strip failed (#2362).
var ErrReassertTargetingLeaked = errors.New("targeting re-asserted after abort could not be stripped")

// errReassertTornDown is returned by reassertArmValue when a teardown began
// before or during the re-assert: the pair must not spawn (#2362).
var errReassertTornDown = errors.New("experiment torn down during arm-value re-assert")

// lateTargetingWrite names the writer whose targeting landed after a teardown
// began: its log event, journal note and leak error.
type lateTargetingWrite struct {
	op     string // "start" | "reassert"
	note   string
	leaked error
}

var (
	lateWriteStart    = lateTargetingWrite{op: "start", note: noteStartStripFailed, leaked: ErrStartTargetingLeaked}
	lateWriteReassert = lateTargetingWrite{op: "reassert", note: noteReassertStripFailed, leaked: ErrReassertTargetingLeaked}
)

// stripAfterAbortedStart removes the targeting a Start wrote after a concurrent
// teardown had already stripped it (#2340).
func (r *Registry) stripAfterAbortedStart(ctx context.Context, id, flagKey string, jrnl *journal.Journal) error {
	return r.stripLateTargetingWrite(ctx, lateWriteStart, id, flagKey, jrnl)
}

// stripLateTargetingWrite removes targeting written (by a Start, #2340, or an
// arm-value re-assert, #2362) after a concurrent teardown had already stripped
// it, so the kill switch, experiments_enabled off and the shutdown leave no agent
// targeting behind. It runs outside r.mu (file I/O) on a context detached from the
// caller's cancellation: the shutdown cancels the writer's context, and the strip
// must still run. A strip that fails after every attempt is logged at ERROR,
// journaled on the experiment and returned.
func (r *Registry) stripLateTargetingWrite(ctx context.Context, w lateTargetingWrite, id, flagKey string, jrnl *journal.Journal) error {
	var err error
	for attempt := 1; attempt <= abortedStartStripAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(time.Duration(attempt-1) * abortedStartStripBackoff)
		}
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortedStartStripTimeout)
		err = r.target.Strip(sctx, flagKey, id)
		cancel()
		if err == nil {
			r.log.Info("experiment."+w.op+"_targeting_stripped", "experiment_id", id, "flag_key", flagKey)
			return nil
		}
	}
	r.log.Error("experiment."+w.op+"_targeting_strip_failed", "experiment_id", id, "flag_key", flagKey,
		"attempts", abortedStartStripAttempts, "error", err)
	if jrnl != nil {
		r.mu.Lock()
		r.appendEvent(jrnl, journal.Event{
			Kind: journal.KindDecision,
			Note: w.note + ": " + err.Error(),
		})
		r.mu.Unlock()
	}
	return fmt.Errorf("registry: %s %q: %w: %w", w.op, id, w.leaked, err)
}

// startSpanLocked starts the experiment's long-lived agent.experiment ROOT span
// (#1188) and records its SpanContext on the entry. It is idempotent (a no-op when
// the span already exists), nil-tracer-safe (no-op), and assumes r.mu is held.
//
// The span is started from context.Background() — it is the trace ROOT, with no
// inbound parent (the experiment is an autonomous, long-lived activity, not a
// request). intent supplies the identifying attributes; on a partial/zero intent the
// per-field guards simply skip the empty attrs.
func (r *Registry) startSpanLocked(e *entry, intent journal.Intent) {
	if r.tracer == nil || e == nil || e.span != nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String(keys.AttrExperimentID, e.id),
	}
	// #1184: stamp the explicit THESIS (the immutable Intent.Hypothesis — what idea +
	// expected effect) on the root span so the trace carries the experiment's WHY, not
	// just the flag/value mechanics. Same string the journal persists + the dossier
	// surfaces. Guarded so a thesis-less (legacy/test) intent adds no empty attr.
	if intent.Hypothesis != "" {
		attrs = append(attrs, attribute.String(keys.AttrExperimentThesis, intent.Hypothesis))
	}
	if intent.FlagKey != "" {
		attrs = append(attrs, attribute.String(keys.AttrExperimentFlagKey, intent.FlagKey))
	}
	if intent.Objective != "" {
		attrs = append(attrs, attribute.String(keys.AttrExperimentObjective, intent.Objective))
	}
	if intent.TargetNPerArm > 0 {
		attrs = append(attrs, attribute.Int(keys.AttrExperimentTargetN, intent.TargetNPerArm))
	}
	if len(intent.Arms) > 0 {
		attrs = append(attrs, attribute.String(keys.AttrExperimentArms, strings.Join(intent.Arms, ",")))
	}
	_, span := r.tracer.Start(context.Background(), keys.SpanExperiment, trace.WithAttributes(attrs...))
	e.span = span
	e.spanCtx = span.SpanContext()
}

// endSpanLocked ends the experiment's root span exactly once (#1188). It is the
// inverse of startSpanLocked, called from teardown — the single terminal chokepoint —
// so the span never leaks regardless of which terminal transition fired. Idempotent
// (clears e.span) and nil-safe. Assumes r.mu is held.
func (r *Registry) endSpanLocked(e *entry) {
	if e == nil || e.span == nil {
		return
	}
	e.span.End()
	e.span = nil
	// Clear the recorded SpanContext too so ExperimentSpanContext reports unavailable
	// after teardown: a torn-down experiment is no longer a re-parent target (its span
	// has ended, so a late analysis span must fall back to its own-root behavior).
	e.spanCtx = trace.SpanContext{}
}
