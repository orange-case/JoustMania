package decision

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otelmetric "go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/joustmania/agent/decide"
	"github.com/joustmania/agent/decide/llmio"
	"github.com/joustmania/agent/flags"
	"github.com/joustmania/agent/gamecontext"
	"github.com/joustmania/agent/goal"
	"github.com/joustmania/agent/telemetry/keys"
)

// decide_hook.go wires the decide contract (epic #1225) into the decision loop
// (#1226). decision.backend selects who decides:
//
//   - rules (default): the loop keeps calling the rules engine exactly as before —
//     ObjectiveRules.EvaluateDecide is Evaluate plus the request and prediction from
//     the SAME ranking (one rankLocked call, one chaos RNG draw per evaluation), so
//     the emitted decisions are byte-for-byte unchanged. The record of the first
//     decision, without confidence, rides an invoke_agent span; like every other
//     span on the sync path it is emitted only on a cycle that produces decisions
//     (idle cycles stay span-free).
//   - anything else: the remote backend OWNS the decision. The call runs off the
//     loop on the existing async machinery (inflight CAS, inferWG, rootCtx,
//     ContextProvider re-read at landing), bounded by the backend's timeout flag.
//     While a call is in flight for a game the rules engine does NOT also act for
//     it. An error, timeout or invalid response (decide.Validate) falls back to the
//     rules engine for the CURRENT context, recorded as decide.fallback_reason.
//
// Every remote call (and every rules decision) is recorded on one invoke_agent
// span: OTel GenAI semantic conventions where a name exists
// (gen_ai.operation.name, gen_ai.agent.name, gen_ai.conversation.id,
// gen_ai.request.model, error.type, and a gen_ai.evaluation.result event for the
// confidence-threshold judgement), decide.* attributes for the rest.

// SpanDecide is the per-decision decide-contract span (#1226), named after the
// GenAI semconv (semantic-conventions-genai v0.1.0) "Invoke agent internal span":
// `{gen_ai.operation.name} {gen_ai.agent.name}` = "invoke_agent joustmania-agent",
// kind INTERNAL, on both paths (#2584; it was agent.decide before). On the remote path
// it is the root of the async trace (a remote child of the game's game_cycle span
// when known, else gameplay_phase / the game root, UC-040 BR-013; Linked to the fire
// cycle) and wraps the backend call — the
// backend's own `chat {model}` CLIENT span nests under it — and the resulting
// agent.decision -> agent.action spans. On the rules path it is a sibling of the
// agent.decision spans under agent.signal_received, so the existing trace shape is
// unchanged.
const SpanDecide = "invoke_agent " + AgentName

// invokeAgentStartAttributes are the GenAI attributes the convention wants at span
// creation time (sampling-relevant: gen_ai.operation.name, gen_ai.agent.name and,
// when known, gen_ai.request.model), on both paths (#2584). gen_ai.output.type=json
// is set on the remote path only: its request asks the backend for JSON, while the
// rules path sends no request at all.
func invokeAgentStartAttributes(model string, remote bool, settings decide.RequestSettings) []attribute.KeyValue {
	// gen_ai.operation.name is Required; also on the rules path, where no model is
	// called (a documented deviation, telemetry.md §4.2).
	attrs := []attribute.KeyValue{
		semconv.GenAIOperationNameInvokeAgent,
		semconv.GenAIAgentName(AgentName),
	}
	if model != "" {
		attrs = append(attrs, semconv.GenAIRequestModel(model))
	}
	if remote {
		attrs = append(attrs, semconv.GenAIOutputTypeJSON)
	}
	if settings.MaxTokens > 0 {
		attrs = append(attrs, semconv.GenAIRequestMaxTokens(settings.MaxTokens))
	}
	if settings.Temperature != nil {
		attrs = append(attrs, semconv.GenAIRequestTemperature(*settings.Temperature))
	}
	return attrs
}

// requestSettingsOf is the backend's request settings, zero when it reports none.
func requestSettingsOf(b decide.Backend) decide.RequestSettings {
	if r, ok := b.(decide.RequestSettingsReporter); ok {
		return r.RequestSettings()
	}
	return decide.RequestSettings{}
}

// usageAttributes omits any count no call reported.
func usageAttributes(t decide.UsageTotals) []attribute.KeyValue {
	var attrs []attribute.KeyValue
	if t.InputTokens.OK {
		attrs = append(attrs, semconv.GenAIUsageInputTokens(t.InputTokens.N))
	}
	if t.OutputTokens.OK {
		attrs = append(attrs, semconv.GenAIUsageOutputTokens(t.OutputTokens.N))
	}
	if t.CacheReadInputTokens.OK {
		attrs = append(attrs, semconv.GenAIUsageCacheReadInputTokens(t.CacheReadInputTokens.N))
	}
	if t.CacheWriteInputTokens.OK {
		attrs = append(attrs, attribute.Int(llmio.AttrUsageCacheWriteInputTokens, t.CacheWriteInputTokens.N))
	}
	if t.ReasoningOutputTokens.OK {
		attrs = append(attrs, semconv.GenAIUsageReasoningOutputTokens(t.ReasoningOutputTokens.N))
	}
	if len(t.FinishReasons) > 0 {
		attrs = append(attrs, semconv.GenAIResponseFinishReasons(t.FinishReasons...))
	}
	return attrs
}

// invoke_agent span attributes without a GenAI semconv name (#1226).
const (
	// AttrDecideBackend is the backend that produced the answers ("rules" after a
	// fallback).
	AttrDecideBackend = "decide.backend"
	// AttrDecideBackendRequested is the decision.backend flag value this cycle.
	AttrDecideBackendRequested = "decide.backend.requested"
	// AttrDecideFallbackReason is why rules decided although another backend was
	// requested ("" when the requested backend decided).
	AttrDecideFallbackReason = "decide.fallback_reason"
	// AttrDecideStateDigest is decide.State.Digest. It changes on every snapshot
	// (the narrative includes time): use it to correlate traces, not as a cache key.
	AttrDecideStateDigest = "decide.state.digest"
	// AttrDecideQuestions / AttrDecideAnswers are size-capped JSON renderings of the
	// questions (id, kind, options, range; the prose text is omitted) and answers.
	AttrDecideQuestions = "decide.questions"
	// AttrDecideQuestionsSHA is the short SHA-256 of the decide.questions JSON, on
	// every invoke_agent span. The JSON itself rides only the first decide of a game
	// carrying that hash (#2648): it is the same ~500 bytes on every decide.
	AttrDecideQuestionsSHA = "decide.questions_sha"
	AttrDecideAnswers      = "decide.answers"
	// AttrDecideConfidence is the routing confidence: the ffa.intervention answer's.
	AttrDecideConfidence = "decide.confidence"
	// AttrDecideConfidenceMin is the minimum confidence over all answers.
	AttrDecideConfidenceMin = "decide.confidence.min"
	// AttrDecideActThreshold is the answering backend's
	// decision.<backend>.act_confidence_threshold as read when the answer landed
	// (#1287). Present only when the confidence router ran (outcome act or held): the
	// rules path and rules fallbacks do not route by confidence.
	AttrDecideActThreshold = "decide.act_confidence_threshold"
	// AttrDecideLatencyMs is the backend call latency (ranking time for rules).
	AttrDecideLatencyMs = "decide.latency_ms"
	// AttrDecideOutcome is one of the DecideOutcome* values.
	AttrDecideOutcome = "decide.outcome"
	// AttrDecideRationale is the backend's one-line rationale, truncated.
	AttrDecideRationale = "decide.rationale"
	// AttrDecideDiscardedReason names why nothing acted although the call returned:
	// stale (re-validation against the current context failed), kill_switch /
	// backend_changed (live flags at landing) or shutdown. It accompanies
	// outcome=discarded.
	AttrDecideDiscardedReason = "decide.discarded_reason"
	// AttrDecideDiscardedDetail names the failed check of a stale discard
	// (DiscardStaleContext .. DiscardStateTooOld).
	AttrDecideDiscardedDetail = "decide.discarded_detail"
	// AttrDecideBlocked / AttrDecideBlockReason (#1295) report what the permission
	// chain did with the decision the answers describe (the cycle's first emitted
	// decision): blocked true/false, and the BlockReason when blocked. Present
	// exactly when that decision went through the permission chain (outcome act, or
	// a rules fallback that emitted a decision); decide.outcome stays the routing
	// outcome.
	AttrDecideBlocked     = "decide.blocked"
	AttrDecideBlockReason = "decide.block_reason"
	// AttrDecidePromptProfile is the normalized decision.prompt_profile a remote
	// request was rendered with (v1 or neutral; #2467 PR 5, decide-contract.md §12).
	// Absent on the rules path.
	AttrDecidePromptProfile = "decide.prompt_profile"
)

// decide.outcome values.
const (
	// DecideOutcomeAct: the answer was routed to the permission chain (runDecision);
	// whether a gate then blocked it is on the agent.decision child.
	DecideOutcomeAct = "act"
	// DecideOutcomeHeld: a valid intervention answer below the act threshold; no action.
	DecideOutcomeHeld = "held"
	// DecideOutcomeNoop: the backend chose noop; no action.
	DecideOutcomeNoop = "noop"
	// DecideOutcomeFallback: the requested backend could not decide; rules did.
	DecideOutcomeFallback = "fallback"
	// DecideOutcomeDiscarded: an answer, or the rules fallback of a failed call,
	// dropped when it landed (live flags, shutdown, or a stale context); no action.
	DecideOutcomeDiscarded = "discarded"
)

// Evaluation recorded as a gen_ai.evaluation.result event on invoke_agent.
const (
	evaluationEventName    = "gen_ai.evaluation.result"
	evaluationActThreshold = "act_confidence_threshold"
	evaluationLabelPass    = "pass"
	evaluationLabelFail    = "fail"
)

// Size caps for model-supplied or unbounded strings on spans and decisions.
const (
	maxDecideJSONBytes  = 4096
	maxRationaleRunes   = 256
	truncatedJSONMarker = "...(truncated)"
)

// decideEngine is the optional rules-engine seam the hook uses: Evaluate plus the
// decide answers from one ranking. *ObjectiveRules satisfies it; Noop/Probe/test
// engines do not, and then the loop calls plain Evaluate and emits no span.
type decideEngine interface {
	EvaluateDecide(ctx context.Context, c gamecontext.GameContext, build func(charged, noShield map[string]bool) decide.Request) ([]Decision, decide.Request, decide.Response, bool)
}

// actedRecorder is the optional seam that arms the rules engine's variance
// cooldown for a decision another backend made (*ObjectiveRules.RecordActed).
type actedRecorder interface {
	RecordActed(Decision)
}

// PromptProfileSource returns the decision.prompt_profile of one game, normalized
// (decide.NormalizePromptProfile). main.go wires flags.Flags.PromptProfile, which
// reads it in the game's flag context and gives v1 on any error.
type PromptProfileSource func(ctx context.Context, gameID string) string

// SetPromptProfileSource injects the prompt-profile source of remote decide
// requests (#2467 PR 5, UC-040 BR-036). nil (the default and every older test)
// leaves Request.PromptProfile empty, which every backend renders as v1. Set at
// construction, before the loop serves.
func (l *Loop) SetPromptProfileSource(src PromptProfileSource) { l.promptProfile = src }

// GoalSource resolves one game's goal profile (#2479, decide-contract.md §14.4).
// main.go wires flags.Flags.GoalProfile in the game's flag context.
type GoalSource func(ctx context.Context, c gamecontext.GameContext) goal.Profile

// SetGoalSource injects the goal-profile source of the prompt's goal lines. nil
// (the default and every older test) renders no goal line. Set at construction,
// before the loop serves.
func (l *Loop) SetGoalSource(src GoalSource) { l.goalSource = src }

// SetDecideRegistry injects the SHARED decision-backend registry (#1226). main.go
// builds one and calls this on every per-game loop. A nil registry (the default)
// knows no remote backend, so any decision.backend other than rules falls back to
// rules with unknown_backend. Set at construction, before the loop serves.
func (l *Loop) SetDecideRegistry(r *DecideRegistry) { l.decideBackends = r }

// decideRecord is everything the invoke_agent span reports for one decision.
type decideRecord struct {
	// trig is the OTLP Export that triggered this decide when the cycle has no
	// agent.signal_received span (in a game, #2648): emitted as an event on the span.
	trig *EvalTrigger
	// usage is nil on the rules path, so its span carries no usage.
	usage          *decide.Usage
	requested      string
	used           string
	fallbackReason string
	model          string
	req            decide.Request
	resp           decide.Response
	hasResp        bool
	start, end     time.Time
	outcome        string
	threshold      float64
	// routed is true when the confidence router ran (outcome act or held): only
	// then is the gen_ai.evaluation.result event recorded.
	routed          bool
	confidence      float64
	err             error
	discardedReason string
	discardedDetail string
	// stateAt is the fire-time GameContext.UpdatedAt: when the state the request
	// was built on last changed (#2123, decision.max_state_age_ms).
	stateAt time.Time
	// permissionKnown is true when the decision the answers describe went through
	// the permission chain; blocked / blockReason are its result (#1295).
	permissionKnown bool
	blocked         bool
	blockReason     BlockReason
	// applied is true when that decision's write was applied (#2479).
	applied bool

	// Prediction (#2479, UC-040 A44, BR-034; prediction_hook.go): the decision's
	// prediction, the context its baseline is taken from (nil = do not register),
	// whether decision.predictions_enabled was off, and what registration returned.
	prediction     *decide.Prediction
	predictionCtx  *gamecontext.GameContext
	predictionsOff bool
	predictionID   string
}

// discardStale records a failed re-validation against the current context.
func (rec *decideRecord) discardStale(detail string) {
	rec.outcome, rec.discardedReason, rec.discardedDetail = DecideOutcomeDiscarded, DiscardStale, detail
}

// notePermission records what the permission chain did with the first decision
// run since state held `before` outcomes (the decision the answers describe).
// Nothing is recorded when no decision ran.
func (rec *decideRecord) notePermission(state *LayerState, before int) {
	if len(state.Outcomes) <= before {
		return
	}
	first := state.Outcomes[before]
	rec.permissionKnown = true
	rec.blocked = !first.Dispatched
	rec.blockReason = first.BlockReason
	rec.applied = slices.Contains(state.appliedOutcomes, before)
}

// buildDecideRequest builds the FFA decide request for c with the shield-gate
// view (one ShieldSerials snapshot: charged and no-shield, #1870, #2066) and the
// applied levels (#2066) at this moment. withContext attaches the raw snapshot (for
// the in-process rules adapter only); remote requests never carry it.
func (l *Loop) buildDecideRequest(c gamecontext.GameContext, allowed []string, withContext bool) decide.Request {
	return l.buildDecideRequestCtx(context.Background(), c, allowed, withContext)
}

// buildDecideRequestCtx is buildDecideRequest with the game's magnitude ranges read
// in ctx (decision.magnitude_bounds, #2696; the remote path passes the invoke_agent
// span's context, so the flag's evaluation event lands on it).
func (l *Loop) buildDecideRequestCtx(ctx context.Context, c gamecontext.GameContext, allowed []string, withContext bool) decide.Request {
	charged, noShield := l.ShieldSerials(c)
	return decideRequestWith(c, allowed, withContext, decide.Annotations{
		Charged:  charged,
		NoShield: noShield,
		Levels:   l.AppliedValues(),
		Cooling:  l.CoolingLevers(),
		Bounds:   l.magnitudeBoundsFor(ctx, c.SessionID),
	})
}

// rulesRequestBuilder is the rules path's lazy request builder for EvaluateDecide:
// the state gets the ranking's own shield-gate snapshot (charged, no-shield) plus
// the applied levels at build time (#2066).
func (l *Loop) rulesRequestBuilder(c gamecontext.GameContext, allowed []string) func(charged, noShield map[string]bool) decide.Request {
	return l.rulesRequestBuilderCtx(context.Background(), c, allowed)
}

// rulesRequestBuilderCtx is rulesRequestBuilder with the game's magnitude ranges
// read in ctx when the request is built (#2696).
func (l *Loop) rulesRequestBuilderCtx(ctx context.Context, c gamecontext.GameContext, allowed []string) func(charged, noShield map[string]bool) decide.Request {
	return func(charged, noShield map[string]bool) decide.Request {
		return decideRequestWith(c, allowed, true, decide.Annotations{
			Charged:  charged,
			NoShield: noShield,
			Levels:   l.AppliedValues(),
			Cooling:  l.CoolingLevers(),
			Bounds:   l.magnitudeBoundsFor(ctx, c.SessionID),
		})
	}
}

// decideRequestWith builds the FFA decide request for c whose state marks the
// charged players shield_charged, the other no-shield players shield_limited, the
// applied per-player levels and, in a hold game, the players' energy (#2096,
// #2066). The rules path passes the view its ranking used, so the state and the
// charge policy agree.
func decideRequestWith(c gamecontext.GameContext, allowed []string, withContext bool, ann decide.Annotations) decide.Request {
	st := decide.BuildStateWith(c, ann)
	req := decide.Request{State: st, Questions: decide.FFAQuestionsWith(st, allowed, ann.Bounds), Bounds: ann.Bounds}
	if withContext {
		cc := c
		req.Context = &cc
	}
	return req
}

// requestedBackend is the normalized decision.backend value ("" counts as rules).
func requestedBackend(snapshot flags.Snapshot) string {
	if snapshot.Decision.IsRules() {
		return flags.DecisionBackendRules
	}
	return snapshot.Decision.Backend
}

// rulesDecide runs the rules engine through the decide hook: the decisions are
// exactly Evaluate's, and the invoke_agent record (nil on a throttled or
// span-less evaluation) carries the answers from the same ranking. requested and
// fallbackReason describe why rules decided (requested=rules, reason "" on the
// default path).
func (l *Loop) rulesDecide(ctx context.Context, snapshot flags.Snapshot, rules RulesEngine, c gamecontext.GameContext, requested, fallbackReason string) ([]Decision, *decideRecord) {
	eng, ok := rules.(decideEngine)
	if !ok {
		return rules.Evaluate(ctx, c), nil
	}
	now := l.nowOrDefault()
	start := now()
	out, req, resp, ranked := eng.EvaluateDecide(ctx, c, l.rulesRequestBuilderCtx(ctx, c, l.withoutClosedGroupLever(snapshot.Policy, c, snapshot.InterventionsAllowed)))
	if !ranked {
		return out, nil
	}
	rec := &decideRecord{
		requested:      requested,
		used:           RulesBackendName,
		fallbackReason: fallbackReason,
		req:            req,
		resp:           resp,
		hasResp:        true,
		start:          start,
		end:            now(),
		outcome:        DecideOutcomeAct,
		prediction:     resp.Prediction,
		predictionCtx:  &c,
		predictionsOff: snapshot.Decision.PredictionsOff,
	}
	switch {
	case fallbackReason != "":
		rec.outcome = DecideOutcomeFallback
	case len(out) == 0:
		// Defensive: OnEvaluate emits no span on a zero-decision cycle, but a
		// noop record must never read act.
		rec.outcome = DecideOutcomeNoop
	}
	return out, rec
}

// decideRemote is decide() for decision.backend != rules. It never blocks: it
// either fires the remote call asynchronously and returns no decision (the async
// path owns this context), returns nothing because a call is in flight or the
// per-game cadence (decision.min_interval_ms from the previous call's start,
// decision.min_gap_after_answer_ms from its end) has not elapsed (the remote backend
// owns the decision; rules do not also act) or the allow-list is empty (nothing
// could act, #1298), or falls back to rules synchronously
// when the backend cannot be called at all (unknown/unavailable backend, async not
// wired, game kind not eligible, global budget exhausted).
func (l *Loop) decideRemote(ctx context.Context, snapshot flags.Snapshot, rules RulesEngine, c gamecontext.GameContext, trig EvalTrigger) ([]Decision, *decideRecord) {
	// Claim the per-game inflight slot FIRST: a cycle that loses the claim (a call
	// is in flight) builds no backend and charges neither cadence nor budget.
	if !l.inflight.CompareAndSwap(false, true) {
		return nil, nil
	}
	fired := false
	defer func() {
		if !fired {
			l.inflight.Store(false)
		}
	}()

	requested := requestedBackend(snapshot)
	backend, reason := l.decideBackends.Resolve(snapshot.Decision)
	misconfigured := reason
	if backend != nil && !l.asyncEnabled() {
		misconfigured = DecideFallbackNotWired
	}
	l.llmGate.Lock()
	l.noteMisconfigured(c, requested, misconfigured)
	l.llmGate.Unlock()
	if backend == nil {
		// Counted every cycle, like the eligibility and budget gates, so a
		// misconfigured backend (e.g. JEV_API_KEY missing) shows up in metrics
		// even while the rules engine is quiet.
		l.recordGated(ctx, reason)
		return l.rulesDecide(ctx, snapshot, rules, c, requested, reason)
	}
	if !l.asyncEnabled() {
		l.recordGated(ctx, DecideFallbackNotWired)
		return l.rulesDecide(ctx, snapshot, rules, c, requested, DecideFallbackNotWired)
	}
	switch gate := l.gateRemoteDecide(ctx, snapshot, c); gate {
	case "":
	case DecideGateInterval:
		// Not a failure: the remote backend decides at its cadence and rules stay
		// quiet in between. No span per blocked cycle; the gated counter and a
		// debug log say why.
		l.Log.Debug("agent.decide.cadence_wait", "session_id", c.SessionID, "backend", requested,
			"min_interval", snapshot.Decision.MinInterval,
			"min_gap_after_answer", snapshot.Decision.MinGapAfterAnswer)
		return nil, nil
	case DecideGateNoInterventions:
		// Nothing is allowed, so neither the backend nor rules could act: no call,
		// no rules run, not a fallback (#1298). The gated counter says why.
		return nil, nil
	default:
		return l.rulesDecide(ctx, snapshot, rules, c, requested, gate)
	}
	fired = true
	l.launchDecideAsync(ctx, snapshot, backend, requested, c, trig)
	return nil, nil
}

// isConfigFallback reports whether reason is a configuration fallback: the
// requested backend can never decide until its configuration changes
// (unknown_backend, backend_unavailable, async_not_wired). llm_not_eligible and
// llm_budget_exhausted are normal operation (shadow games, spend cap) and stay
// counter-only on a quiet cycle.
func isConfigFallback(reason string) bool {
	switch reason {
	case DecideFallbackUnknownBackend, DecideFallbackUnavailable, DecideFallbackNotWired:
		return true
	}
	return false
}

// noteMisconfigured logs when this game enters or leaves a configuration
// fallback (reason "" = none), not once per cycle: a WARN on entering or
// changing reason, an INFO when it clears. The caller holds llmGate.
func (l *Loop) noteMisconfigured(c gamecontext.GameContext, requested, reason string) {
	prev := l.misconfiguredReason
	if prev == reason {
		return
	}
	l.misconfiguredReason = reason
	if reason == "" {
		l.Log.Info("agent.decide.backend_misconfigured", "session_id", c.SessionID,
			"backend", requested, "state", "cleared", "previous_reason", prev)
		return
	}
	l.Log.Warn("agent.decide.backend_misconfigured", "session_id", c.SessionID,
		"backend", requested, "state", "entered", "reason", reason)
}

// emitQuietFallback records a configuration fallback (isConfigFallback) on which
// the rules engine emitted nothing (UC-040 A3, decide contract §7): an
// invoke_agent span with outcome=fallback and the fallback reason, under its own
// agent.signal_received root like a decided cycle's. A decided cycle already
// carries that span; without this a quiet cycle left no trace, so for example a
// missing API key was invisible until a rule fired. Throttled per reason to one
// per throttle window (decision.throttle_seconds) because it can fire on every
// 10-60 Hz cycle; the agent_llm_gated_total counter still counts every cycle.
func (l *Loop) emitQuietFallback(ctx context.Context, rec *decideRecord, c gamecontext.GameContext, trig EvalTrigger) {
	if !l.throttleAllows(l.quietFallbackClock(rec.fallbackReason)) {
		return
	}
	rootCtx, ev, endCycle := l.startCycle(ctx, c, trig)
	rec.trig = ev
	l.emitDecideSpan(rootCtx, rec, c)
	endCycle()
}

// quietFallbackClock returns the per-reason throttle timestamp for
// emitQuietFallback, allocating it on first use. The pointer stays valid; the
// timestamp itself is read and written under mu by throttleAllows.
func (l *Loop) quietFallbackClock(reason string) *time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastQuietFallback == nil {
		l.lastQuietFallback = map[string]*time.Time{}
	}
	t, ok := l.lastQuietFallback[reason]
	if !ok {
		t = new(time.Time)
		l.lastQuietFallback[reason] = t
	}
	return t
}

// DecideGateInterval is the agent_llm_gated_total reason (and debug log) for a
// remote decide cycle held back by the per-game cadence: decision.min_interval_ms
// (from the previous call's admission) or decision.min_gap_after_answer_ms (from
// the previous call's end, #2123).
const DecideGateInterval = "decide_interval"

// DecideGateNoInterventions is the agent_llm_gated_total reason for a remote
// decide cycle skipped because the allow-list is empty (#1298): the only
// ffa.intervention option would be noop, so a paid call could not change anything.
const DecideGateNoInterventions = "no_interventions_allowed"

// hasAllowedInterventions reports whether allowed offers at least one
// intervention besides noop, filtered exactly like the ffa.intervention options
// (decide.FFAQuestions: empty ids and noop dropped). It is the single seam for
// "is there anything a remote decide call could choose" (later also: every
// allowed intervention currently unusable).
func hasAllowedInterventions(allowed []string) bool {
	for _, a := range allowed {
		if a != "" && a != decide.OptionNoop {
			return true
		}
	}
	return false
}

// noteNoInterventions records the empty-allow-list state for this game and logs
// at debug level only when it changes, not once per 10-60 Hz cycle.
// experimentFiltered is true when the experiment-flag filter (A40 step 5) emptied
// an allow-list that offered something. The caller holds llmGate.
func (l *Loop) noteNoInterventions(c gamecontext.GameContext, skipping, experimentFiltered bool) {
	if l.skippingNoInterventions == skipping {
		return
	}
	l.skippingNoInterventions = skipping
	l.Log.Debug("agent.decide.no_interventions_allowed", "session_id", c.SessionID, "skipping", skipping, "experiment_filtered", experimentFiltered)
}

// gateRemoteDecide is the call gate for remote decide calls: eligibility
// (llm.eligible_game_kinds, so shadow games stay rules-only), a non-empty
// allow-list (hasAllowedInterventions, #1298), the per-game cadence
// (decision.min_interval_ms from the previous admission and, #2123,
// decision.min_gap_after_answer_ms from the previous call's end), and the GLOBAL
// per-minute budget
// llm.max_requests_per_minute shared with the post-game retro (the spend bound),
// tightened for claude by its own decision.<backend>.
// max_requests_per_minute cap (#2519).
// Cadence and budget are charged only on admission, and only after the caller
// won the inflight claim. It returns "" when admitted.
func (l *Loop) gateRemoteDecide(ctx context.Context, snapshot flags.Snapshot, c gamecontext.GameContext) string {
	if !snapshot.LLMGate.EligibleFor(c.GameKind) {
		l.recordGated(ctx, FallbackNotEligible)
		return FallbackNotEligible
	}
	l.llmGate.Lock()
	defer l.llmGate.Unlock()
	// Before cadence and budget: an empty allow-list charges neither
	// (UC-040 BR-005: no paid call without a possible action).
	// UC-040 A40 step 5: in an experiment game the levers of the flag under test
	// are not offered, so they do not count either.
	if !hasAllowedInterventions(l.offeredInterventions(c, snapshot)) {
		l.noteNoInterventions(c, true, hasAllowedInterventions(snapshot.InterventionsAllowed))
		l.recordGated(ctx, DecideGateNoInterventions)
		return DecideGateNoInterventions
	}
	l.noteNoInterventions(c, false, false)
	t := l.nowOrDefault()()
	if iv := snapshot.Decision.MinInterval; iv > 0 && !l.lastDecideAttempt.IsZero() && t.Sub(l.lastDecideAttempt) < iv {
		l.recordGated(ctx, DecideGateInterval)
		return DecideGateInterval
	}
	// The gap runs from the END of the previous call (#2123): a timeout or a
	// failure ends a call like an answer does. lastDecideEnded is written before
	// the in-flight slot is released, and this cycle already won that slot, so
	// it always sees the end of the call before it.
	if gap := snapshot.Decision.MinGapAfterAnswer; gap > 0 && !l.lastDecideEnded.IsZero() && t.Sub(l.lastDecideEnded) < gap {
		l.recordGated(ctx, DecideGateInterval)
		return DecideGateInterval
	}
	// #2519 FR-152 (c): a paid backend's own cap (decision.<backend>.
	// max_requests_per_minute) tightens the global budget for its decide calls.
	// One window, one critical section: the global total and the backend's
	// count are checked and charged atomically.
	requested := requestedBackend(snapshot)
	backendCap := -1
	if cfg, ok := snapshot.Decision.Remote(requested); ok && cfg.HasMaxRequestsPerMinute {
		backendCap = max(cfg.MaxRequestsPerMinute, 0)
	}
	if l.budget == nil || !l.budget.allowBackend(t, l.budgetCap(snapshot.LLMGate.MaxRequestsPerMinute), requested, backendCap) {
		l.recordGated(ctx, FallbackBudgetExhausted)
		return FallbackBudgetExhausted
	}
	l.lastDecideAttempt = t
	return ""
}

// launchDecideAsync launches one remote decide call off the loop (#1226), reusing
// the #917 async machinery: the per-game inflight slot (already claimed by the
// caller, released when the goroutine ends), inferWG (AwaitInflight joins it),
// rootCtx (shutdown cancels it) and the fire-cycle anchor span the async root
// Links to.
func (l *Loop) launchDecideAsync(ctx context.Context, snapshot flags.Snapshot, backend decide.Backend, requested string, c gamecontext.GameContext, trig EvalTrigger) {
	// In a game the async span needs no anchor: it is a child of game_cycle and carries
	// the trigger as an event (#2648). Without game ids it Links to an anchor span.
	var fireSC trace.SpanContext
	_, ev, endCycle := l.startCycle(ctx, c, trig)
	if ev == nil {
		_, anchor := l.startSignalReceivedSpan(ctx, c, trig)
		fireSC = anchor.SpanContext()
		anchor.End()
	}
	endCycle()

	root := l.rootCtx
	if root == nil {
		root = context.Background()
	}
	// The remote call runs off the evaluation path; nothing waits for it
	// (UC-040 BR-001: the game never waits).
	l.inferWG.Add(1)
	go func() {
		defer l.inferWG.Done()
		defer l.inflight.Store(false)
		// runDecideRemote starts the gap clock (noteDecideEnded) when the answer
		// lands, while this goroutine still holds the slot (#2123).
		l.runDecideRemote(root, snapshot, backend, requested, c, fireSC, ev)
	}()
}

// noteDecideEnded starts the decision.min_gap_after_answer_ms clock (#2123) at
// t, the moment the remote call of this game returned, whatever its outcome
// (answer, timeout, failure; a later discard or shutdown does not move it). The
// caller still holds the in-flight slot, so the next cycle that wins the slot
// always sees it.
func (l *Loop) noteDecideEnded(t time.Time) {
	l.llmGate.Lock()
	l.lastDecideEnded = t
	l.llmGate.Unlock()
}

// Discard reasons specific to the remote decide path (decide.discarded_reason).
const (
	// decideDiscardShutdown: the agent shut down while the call was in flight.
	decideDiscardShutdown = "shutdown"
	// DecideDiscardKillSwitch: the enabled kill switch was off when the answer
	// landed. Nothing acts, not even the rules fallback.
	DecideDiscardKillSwitch = "kill_switch"
	// DecideDiscardBackendChanged: decision.backend changed while the call was in
	// flight; the answer of the old backend is dropped.
	DecideDiscardBackendChanged = "backend_changed"
)

// runDecideRemote is the fired goroutine: call the backend under its timeout,
// validate, re-read the LIVE flags, then route the answer (act / held / noop) or
// fall back to rules for the CURRENT context. Everything is recorded
// on one invoke_agent span.
//
// Every safety gate is evaluated against the flags as they are when the answer
// LANDS, not when the call fired: the enabled kill switch, decision.backend, the
// act threshold, interventions_allowed and the policy gates all come from a fresh
// Flags.Evaluate, so flipping a flag mid-flight takes effect on the in-flight
// answer too.
func (l *Loop) runDecideRemote(root context.Context, fired flags.Snapshot, backend decide.Backend, requested string, c gamecontext.GameContext, fireSC trace.SpanContext, ev *EvalTrigger) {
	now := l.nowOrDefault()
	start := now()
	cfg, _ := fired.Decision.Remote(requested)
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = flags.DefaultDecisionTimeoutMs * time.Millisecond
	}

	parent := root
	if p, ok := inGameRemoteParent(root, c.GameTraceID, c.GameTraceSpanID, c.GameplayPhaseSpanID, c.GameCycleSpanID); ok {
		parent = p
	}
	opts := []trace.SpanStartOption{
		trace.WithTimestamp(start),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(invokeAgentStartAttributes(cfg.Model, true, requestSettingsOf(backend))...),
	}
	opts = append(opts, fireCycleLink(fireSC)...)
	spanCtx, span := l.Tracer.Start(parent, SpanDecide, opts...)

	rec := &decideRecord{
		trig:      ev,
		requested: requested,
		used:      backend.Name(),
		model:     cfg.Model,
		req:       l.buildDecideRequestCtx(spanCtx, c, l.offeredInterventions(c, fired), false),
		start:     start,
		stateAt:   c.UpdatedAt,
	}
	// The prompt profile is read per request in the game's flag context (#2467 PR 5);
	// it changes only the wording a backend renders, never the questions or the state.
	if l.promptProfile != nil {
		rec.req.PromptProfile = decide.NormalizePromptProfile(l.promptProfile(root, c.SessionID))
	}
	// The operator brief (#2687, UC-040 BR-044): read per request, claude
	// only; "" (no brief, jev, rules) leaves the request byte-identical.
	rec.req.OperatorBrief = l.operatorBriefFor(root, rec.used, c.SessionID)
	// #2695 (UC-042 A13): the retro tells the analyst whether the in-game model saw it.
	if rec.req.OperatorBrief != "" {
		l.briefLog.Note(c.SessionID)
	}
	// The prediction ask (#2479, UC-040 A45): only with decision.predictions.ask_model
	// and decision.predictions_enabled on (fire-time flags) and a claude
	// backend; otherwise the request stays v1.
	if askModelPrediction(fired, rec.used) {
		rec.req.PredictionAsk = decide.PredictionQuestion(rec.req.State)
	}
	// The goal lines (#2479, decide-contract.md §14.4): only under
	// decision.goal_in_prompt, and only for claude or jev (jev's goal state
	// and gate criterion, #2498); otherwise the request stays byte-identical.
	rec.req.Goal = l.goalLines(root, fired, rec.used, c)
	// Fire-time threshold: jev's gate acts at it; the landing re-read below
	// overwrites it with the LIVE threshold, which is what routing uses.
	rec.threshold, _ = fired.Decision.ActThreshold(rec.used)
	rec.req.ActThreshold = rec.threshold
	defer func() {
		l.endDecideSpan(span, rec, c)
		if rec.discardedReason != "" {
			l.decideDiscarded.Add(root, 1, otelmetric.WithAttributes(attribute.String("reason", rec.discardedReason)))
		}
		l.countPredictionDropped(root, rec)
	}()

	// Calls of a decision that then falls back still count: they were billed.
	rec.usage = &decide.Usage{}
	callCtx, cancel := context.WithTimeout(decide.WithUsage(spanCtx, rec.usage), timeout)
	resp, err := backend.Decide(callCtx, rec.req)
	timedOut := err != nil && (errors.Is(err, context.DeadlineExceeded) || callCtx.Err() == context.DeadlineExceeded)
	cancel()
	rec.end = now()
	l.noteDecideEnded(rec.end)

	// Shutdown first: a call cancelled by the agent's root context is neither a
	// backend error nor a fallback. Record it, act on nothing.
	if root.Err() != nil {
		rec.outcome, rec.discardedReason = DecideOutcomeDiscarded, decideDiscardShutdown
		return
	}

	switch {
	case timedOut:
		rec.fallbackReason, rec.err = DecideFallbackTimeout, err
	case errors.Is(err, decide.ErrInvalid):
		// The remote backends validate their own output (llmio.Parse runs
		// decide.Validate) and return an error wrapping decide.ErrInvalid for a
		// malformed or invalid answer: that is an invalid answer, not a
		// transport error (#1230).
		rec.fallbackReason, rec.err = DecideFallbackInvalidAnswer, err
	case err != nil:
		rec.fallbackReason, rec.err = DecideFallbackError, err
	default:
		if verr := decide.Validate(rec.req, resp); verr != nil {
			rec.fallbackReason, rec.err = DecideFallbackInvalidAnswer, verr
		} else {
			rec.resp, rec.hasResp = resp, true
			// Record the answer's confidence even when the live re-read below
			// discards it (kill switch, backend change), so the span reports
			// what the backend answered rather than 0.
			if iv, found := resp.Find(decide.QuestionFFAIntervention); found {
				rec.confidence = iv.Confidence
			}
		}
	}
	if rec.fallbackReason != "" {
		// error.type only, never the error text: a backend error can echo a
		// vendor body (and a masked key in it), and logs are exported too.
		l.Log.Warn("agent.decide.fallback",
			"session_id", l.gameID,
			"backend", requested,
			"reason", rec.fallbackReason,
			"digest", rec.req.State.Digest,
			"error.type", decideErrorType(rec))
	}

	// Re-read the LIVE flags: the safety gates apply as they are now.
	live := l.Flags.Evaluate(root)
	// The answering backend's threshold from the LIVE flags: a flip mid-flight
	// applies to the in-flight answer.
	rec.threshold, _ = live.Decision.ActThreshold(rec.used)
	rec.predictionsOff = live.Decision.PredictionsOff
	if !live.Enabled {
		rec.outcome, rec.discardedReason = DecideOutcomeDiscarded, DecideDiscardKillSwitch
		return
	}
	if requestedBackend(live) != requested {
		rec.outcome, rec.discardedReason = DecideOutcomeDiscarded, DecideDiscardBackendChanged
		return
	}

	// Re-read the CURRENT context: the call took up to the timeout, so the game may
	// have ended or the target may be gone (#917 re-validation).
	current, ok := l.ctxProvider.Current(l.gameID)
	state := newLayerState(live)
	// The fitness baseline of the landing (UC-040 step 14, #1699): the synchronous
	// path lifts it from the rules engine's cycle evaluation; here it is evaluated
	// on the current context with the live thresholds, the same functions and
	// thresholds runDecision hands the effect sampler, so a dispatched remote
	// decision (and its rules fallback) gets an effect sample and an id.
	if ok {
		if vals := EvaluateFitness(current, fitnessThresholds(live.Fitness)).Evaluated(); len(vals) > 0 {
			state.FitnessEvaluated = vals
		}
	}
	if rec.fallbackReason != "" {
		l.decideRemoteFallback(spanCtx, live, l.rulesFor(live), current, ok, rec, &state)
		return
	}
	l.decideRemoteApply(spanCtx, live, current, ok, rec, &state)
}

// decideRemoteFallback runs the given rules engine (rulesFor of the live flags,
// so the probe flag applies here too) for the current context after the
// remote backend could not decide, recording rules as the backend that answered.
func (l *Loop) decideRemoteFallback(ctx context.Context, snapshot flags.Snapshot, rules RulesEngine, current gamecontext.GameContext, ok bool, rec *decideRecord, state *LayerState) {
	rec.used = RulesBackendName
	rec.outcome = DecideOutcomeFallback
	rec.hasResp = false
	rec.resp = decide.Response{}
	state.DecideBackend = RulesBackendName
	if !ok {
		rec.discardStale(DiscardStaleContext)
		return
	}
	if !gameActive(current) {
		rec.discardStale(DiscardGameEnded)
		return
	}
	var decisions []Decision
	if eng, isDecide := rules.(decideEngine); isDecide {
		out, req, resp, ranked := eng.EvaluateDecide(ctx, current, l.rulesRequestBuilderCtx(ctx, current, l.withoutClosedGroupLever(snapshot.Policy, current, snapshot.InterventionsAllowed)))
		decisions = out
		if ranked {
			rec.req, rec.resp, rec.hasResp = req, resp, true
			rec.prediction, rec.predictionCtx = resp.Prediction, &current
		}
	} else {
		decisions = rules.Evaluate(ctx, current)
	}
	decisions = l.stampShieldSeconds(ctx, snapshot, decisions, rec.req.Bounds)
	rec.answerRules(decisions)
	now := l.nowOrDefault()
	before := len(state.Outcomes)
	for _, d := range decisions {
		l.runDecision(ctx, snapshot, current, d, now(), state)
	}
	rec.notePermission(state, before)
}

// decideRemoteApply maps a validated remote answer to a Decision and routes it by
// confidence: at or above the act threshold (rec.threshold: the backend's
// decision.<backend>.act_confidence_threshold) it goes through the
// normal permission chain (runDecision: allow-list, battery, rate limit) and the
// action sink; below it is held.
func (l *Loop) decideRemoteApply(ctx context.Context, snapshot flags.Snapshot, current gamecontext.GameContext, ok bool, rec *decideRecord, state *LayerState) {
	state.DecideBackend = rec.used
	d, outcome := mapAnswers(rec.req, rec.resp)
	if iv, found := rec.resp.Find(decide.QuestionFFAIntervention); found {
		rec.confidence = iv.Confidence
	}
	if outcome == DecideOutcomeNoop {
		rec.outcome = DecideOutcomeNoop
		l.noteNoopAnswer(current, rec.used)
		return
	}
	rec.routed = true
	// #2479: the remote answer's prediction (lever default while no model states
	// one), registered when the record ends, only if the decision acted. Not when
	// the game is gone: nothing could be scored.
	if ok && gameActive(current) {
		rec.prediction, rec.predictionCtx = remotePrediction(rec.req, rec.resp, d, current), &current
	}
	if rec.confidence < rec.threshold {
		rec.outcome = DecideOutcomeHeld
		l.noteIntervention(current, d, NotedHeld, rec.used, l.nowOrDefault()())
		return
	}
	rec.outcome = DecideOutcomeAct
	switch {
	case !ok:
		rec.discardStale(DiscardStaleContext)
		return
	case !gameActive(current):
		rec.discardStale(DiscardGameEnded)
		return
	case d.TargetSerial != "" && !targetPresent(current, d.TargetSerial):
		rec.discardStale(DiscardTargetGone)
		return
	case len(d.TargetSerials) > 0 && !groupPresent(current, d.TargetSerials):
		// A group shield is all or nothing (#2467, UC-048 A14): one target gone drops it.
		rec.discardStale(DiscardTargetGone)
		return
	case decide.NeedsFreshReadings(d.Intervention) && !decide.TargetReadingsFresh(current, d.TargetSerial):
		// #2514: a lever whose rule needs fresh readings (R3, R4, R8) is not
		// applied to a player whose readings are stale now (UC-040 BR-039).
		rec.discardStale(DiscardTargetStale)
		return
	case stateTooOld(rec.stateAt, rec.end, snapshot.Decision.MaxStateAge):
		rec.discardStale(DiscardStateTooOld)
		return
	}
	before := len(state.Outcomes)
	l.runDecision(ctx, snapshot, current, d, l.nowOrDefault()(), state)
	rec.notePermission(state, before)
	if len(state.Outcomes) > before && state.Outcomes[len(state.Outcomes)-1].Dispatched {
		// A dispatched remote difficulty intervention invalidates the variance
		// baseline exactly like a rules one: arm the rules engine's cooldown.
		if r, isRecorder := l.Rules.(actedRecorder); isRecorder {
			r.RecordActed(d)
		}
	}
}

// stateTooOld reports whether the state a remote request was built on (stateAt,
// the fire-time GameContext.UpdatedAt) is older than maxAge when the answer
// landed (#2123, decision.max_state_age_ms, read live). A zero maxAge disables
// the check; a zero stateAt (never observed) never discards, like targetPresent's
// lenient treatment of an unknown signal.
func stateTooOld(stateAt, landed time.Time, maxAge time.Duration) bool {
	return maxAge > 0 && !stateAt.IsZero() && landed.Sub(stateAt) > maxAge
}

// playerTargetedInterventions are the interventions the action sink scopes to one
// player (services/agent/actions/writer.go uses TargetSerial for exactly these).
// Every other intervention is session-scoped and takes target "none".
var playerTargetedInterventions = decide.Set(func(iv decide.Intervention) bool { return iv.PlayerTargeted })

// mapAnswers turns a VALIDATED FFA response into a Decision: ffa.intervention is
// the intervention, ffa.target the TargetSerial ("none" -> session-scoped), and
// ffa.magnitude resolves through decide.ResolveMagnitude into Decision.Value (left
// empty for interventions without numeric bounds, so the writer default applies).
// Reason is the truncated rationale. It returns outcome noop for noop and act
// otherwise; decide.Validate already rejected inconsistent answers, and confidence
// routing is the caller's job.
//
// A group lever (decide.GroupTargeted) takes a group option, resolved against the
// request's alive players into Decision.TargetSerials (#2467, UC-048 A13); req is
// the request the answers belong to.
func mapAnswers(req decide.Request, resp decide.Response) (Decision, string) {
	iv, _ := resp.Find(decide.QuestionFFAIntervention)
	if iv.Choice == decide.OptionNoop || iv.Choice == "" {
		return Decision{}, DecideOutcomeNoop
	}
	target := decide.TargetNone
	if t, ok := resp.Find(decide.QuestionFFATarget); ok {
		target = t.Choice
	}
	var group []string
	if decide.GroupTargeted(iv.Choice) {
		group, _ = decide.ResolveGroupTarget(target, req.State.AliveSerials())
	}
	playerScoped := playerTargetedInterventions[iv.Choice]
	d := Decision{Intervention: iv.Choice, Reason: truncateRationale(resp.Rationale), TargetSerials: group}
	if playerScoped {
		d.TargetSerial = target
	}
	if d.Reason == "" {
		d.Reason = "decided by " + resp.Backend
		d.ReasonSynthesized = true
	}
	if m, ok := resp.Find(decide.QuestionFFAMagnitude); ok {
		if v, ok := req.Bounds.Resolve(iv.Choice, m.Score); ok {
			d.Value = strconv.FormatFloat(v, 'g', -1, 64)
		}
	}
	return d, DecideOutcomeAct
}

// truncateRationale makes untrusted model text safe for a span attribute and a
// Decision.Reason: one line, at most maxRationaleRunes runes.
func truncateRationale(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= maxRationaleRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxRationaleRunes]) + "..."
}

// emitDecideSpan emits a recorded rules-path invoke_agent span under parent (the
// cycle's agent.signal_received), backdated to the ranking it reports.
func (l *Loop) emitDecideSpan(parent context.Context, rec *decideRecord, c gamecontext.GameContext) {
	_, span := l.Tracer.Start(parent, SpanDecide,
		trace.WithTimestamp(rec.start),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(invokeAgentStartAttributes(rec.model, false, decide.RequestSettings{})...))
	l.registerPrediction(span, rec)
	l.setDecideAttributes(span, rec, c)
	span.End(trace.WithTimestamp(rec.end))
}

// endDecideSpan finishes the remote-path invoke_agent span.
func (l *Loop) endDecideSpan(span trace.Span, rec *decideRecord, c gamecontext.GameContext) {
	if rec.end.IsZero() {
		rec.end = l.nowOrDefault()()
	}
	l.registerPrediction(span, rec)
	l.setDecideAttributes(span, rec, c)
	span.End()
}

// setDecideAttributes stamps the invoke_agent schema onto span.
func (l *Loop) setDecideAttributes(span trace.Span, rec *decideRecord, c gamecontext.GameContext) {
	attrs := []attribute.KeyValue{
		// gen_ai.operation.name and gen_ai.agent.name are set at span start
		// (invokeAgentStartAttributes, #2584).
		// The game is the "conversation" the agent's decisions belong to.
		semconv.GenAIConversationID(c.SessionID),
		attribute.String(AttrGameID, c.SessionID),
		attribute.String(AttrSessionID, c.SessionID),
		attribute.String(AttrGameKind, c.GameKind),
		attribute.String(AttrDecideBackend, rec.used),
		attribute.String(AttrDecideBackendRequested, rec.requested),
		attribute.String(AttrDecideFallbackReason, rec.fallbackReason),
		attribute.String(AttrDecideStateDigest, rec.req.State.Digest),
		attribute.Int64(AttrDecideLatencyMs, rec.end.Sub(rec.start).Milliseconds()),
		attribute.String(AttrDecideOutcome, rec.outcome),
	}
	qJSON := questionsJSON(rec.req.Questions)
	sha := questionsSHA(qJSON)
	attrs = append(attrs, attribute.String(AttrDecideQuestionsSHA, sha))
	if l.claimQuestions(c.SessionID, sha) {
		attrs = append(attrs, attribute.String(AttrDecideQuestions, qJSON))
	}
	signalEvent(span, rec.trig)
	if rec.routed {
		attrs = append(attrs, attribute.Float64(AttrDecideActThreshold, rec.threshold))
	}
	if rec.model != "" {
		attrs = append(attrs, semconv.GenAIRequestModel(rec.model))
	}
	if rec.usage != nil {
		attrs = append(attrs, usageAttributes(rec.usage.Totals())...)
	}
	if rec.hasResp {
		attrs = append(attrs,
			attribute.String(AttrDecideAnswers, capJSON(rec.resp.Answers)),
			attribute.String(AttrDecideRationale, truncateRationale(rec.resp.Rationale)),
		)
		if rec.used != RulesBackendName {
			attrs = append(attrs,
				attribute.Float64(AttrDecideConfidence, rec.confidence),
				attribute.Float64(AttrDecideConfidenceMin, minConfidence(rec.resp.Answers)))
		}
	}
	if rec.discardedReason != "" {
		attrs = append(attrs, attribute.String(AttrDecideDiscardedReason, rec.discardedReason))
	}
	if rec.discardedDetail != "" {
		attrs = append(attrs, attribute.String(AttrDecideDiscardedDetail, rec.discardedDetail))
	}
	if rec.permissionKnown {
		attrs = append(attrs, attribute.Bool(AttrDecideBlocked, rec.blocked))
		if rec.blocked {
			attrs = append(attrs, attribute.String(AttrDecideBlockReason, string(rec.blockReason)))
		}
	}
	if rec.req.PromptProfile != "" {
		attrs = append(attrs, attribute.String(AttrDecidePromptProfile, rec.req.PromptProfile))
	}
	attrs = append(attrs, operatorBriefAttributes(rec.req.OperatorBrief)...)
	attrs = append(attrs, magnitudeBoundsAttributes(rec.req.Bounds)...)
	if rec.req.Goal != nil {
		attrs = append(attrs, attribute.String(keys.AttrDecideGoalProfile, rec.req.Goal.Profile),
			attribute.String(keys.AttrDecideGoalSource, rec.req.Goal.Source))
	}
	attrs = append(attrs, predictionAttributes(rec)...)
	attrs = append(attrs, droppedPredictionAttributes(rec)...)
	if rec.err != nil {
		attrs = append(attrs, semconv.ErrorTypeKey.String(decideErrorType(rec)))
		span.SetStatus(codes.Error, rec.fallbackReason)
	}
	span.SetAttributes(attrs...)
	if rec.routed {
		label, cmp := evaluationLabelFail, "<"
		if rec.confidence >= rec.threshold {
			label, cmp = evaluationLabelPass, ">="
		}
		span.AddEvent(evaluationEventName, trace.WithAttributes(
			semconv.GenAIEvaluationName(evaluationActThreshold),
			semconv.GenAIEvaluationScoreValue(rec.confidence),
			semconv.GenAIEvaluationScoreLabel(label),
			semconv.GenAIEvaluationExplanation("confidence "+fmtFloat(rec.confidence)+" "+cmp+
				" threshold "+fmtFloat(rec.threshold)+" (decision."+rec.used+".act_confidence_threshold)"),
		))
	}
}

// decideErrorType is the low-cardinality error.type: the fallback reason for a
// timeout or an invalid answer, the Go error type otherwise.
func decideErrorType(rec *decideRecord) string {
	switch rec.fallbackReason {
	case DecideFallbackTimeout, DecideFallbackInvalidAnswer:
		return rec.fallbackReason
	}
	return semconv.ErrorType(rec.err).Value.AsString()
}

// questionJSON is the compact span view of a decide.Question (no prose text).
type questionJSON struct {
	ID      string      `json:"id"`
	Kind    string      `json:"kind"`
	Options []string    `json:"options,omitempty"`
	Range   *[2]float64 `json:"range,omitempty"`
}

func questionsJSON(qs []decide.Question) string {
	out := make([]questionJSON, 0, len(qs))
	for _, q := range qs {
		v := questionJSON{ID: q.ID, Kind: string(q.Kind), Options: q.Options}
		if q.Kind == decide.KindScore {
			r := q.Range
			v.Range = &r
		}
		out = append(out, v)
	}
	return capJSON(out)
}

// questionsSHALen is the length of decide.questions_sha: 12 hex chars of SHA-256.
const questionsSHALen = 12

// questionsSHA is the short hash of the rendered decide.questions JSON.
func questionsSHA(questionsJSON string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(questionsJSON)))[:questionsSHALen]
}

// questionsSeenMax bounds the per-loop memory of recorded question sets.
const questionsSeenMax = 64

// claimQuestions reports whether the decide.questions JSON with this hash still has
// to be written on a span for this game: true on the first decide of the game and
// whenever the question set changes (the offered interventions change it), false for
// the repeats (#2648).
func (l *Loop) claimQuestions(gameID, sha string) bool {
	l.questionsMu.Lock()
	defer l.questionsMu.Unlock()
	if l.questionsSeen == nil || len(l.questionsSeen) >= questionsSeenMax {
		l.questionsSeen = map[string]struct{}{}
	}
	k := gameID + "|" + sha
	if _, ok := l.questionsSeen[k]; ok {
		return false
	}
	l.questionsSeen[k] = struct{}{}
	return true
}

// capJSON marshals v and caps it at maxDecideJSONBytes (cut on a rune boundary,
// marked with truncatedJSONMarker).
func capJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	if len(b) <= maxDecideJSONBytes {
		return string(b)
	}
	cut := maxDecideJSONBytes - len(truncatedJSONMarker)
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return string(b[:cut]) + truncatedJSONMarker
}

func minConfidence(answers []decide.Answer) float64 {
	if len(answers) == 0 {
		return 0
	}
	m := answers[0].Confidence
	for _, a := range answers[1:] {
		if a.Confidence < m {
			m = a.Confidence
		}
	}
	return m
}

func fmtFloat(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }
