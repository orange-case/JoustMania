/** invoke_agent feed: parsing, follow anchor and the store. Split out of agent.ts (#2388). */

import { spanStartMs, spanTags, tagBool, tagBoolOrNull, tagNumber, tagString, parentSpanId } from "./jaeger.js";
import type { JaegerSpan, JaegerTracesResponse } from "./jaeger.js";
import { GoalStore, parseGoalSummaries } from "./agentGoal.js";
import type { GameGoal } from "./agentGoal.js";
import { isShadowGame, pickAnchor } from "./agentAnchor.js";
import type { DecideFollow } from "./agentAnchor.js";

/* ======================================================================== */
/* invoke_agent feed (#1233, epic #1225)                                    */
/* ======================================================================== */
/*
 * The decide contract (#1226) records every decision on an `invoke_agent` span:
 * which backend answered (and which one the decision.backend flag requested),
 * the model, the answers + confidence, the confidence-threshold verdict, the
 * outcome and the model's one-line rationale. The agent keeps its newest decide
 * spans with their neighbours in memory and serves them from GET
 * /experiments/state (agentState.ts) in the shape of a Jaeger answer, so the
 * same fold reads them; the row links open the trace in Jaeger.
 *
 * TRACE SHAPE (services/agent/decision/decide_hook.go)
 *   rules path:  agent.signal_received ─┬─ invoke_agent
 *                                        └─ agent.decision ─ agent.action   (0..2)
 *   remote path: invoke_agent ─┬─ chat {model}            (backend CLIENT span)
 *                              └─ agent.decision ─ agent.action
 *                (in a game, invoke_agent is a remote child of game_cycle.)
 *
 * Every string here (rationale above all) can be model output: it is untrusted
 * and the view escapes it before rendering.
 */
/** Jaeger operation the decide feed searches for. */
export const DECIDE_OPERATION = "invoke_agent joustmania-agent";

/** Max decide rows kept (newest first): the agent keeps 30 per game-kind class (real and shadow). */
const MAX_DECIDE_ROWS = 60;

/** Max prediction outcome spans kept per class (shadow, other) for the per-game strip, as the agent sends them. */
const MAX_OUTCOMES = 400;

/** The rules path's per-cycle parent span (decide + decisions are its children). */
const SIGNAL_OPERATION = "agent.signal_received";

/** decide.outcome values (DecideOutcome* in decide_hook.go). */
export type DecideOutcome =
  | "act"
  | "held"
  | "noop"
  | "fallback"
  | "discarded";

const KNOWN_OUTCOMES: ReadonlySet<string> = new Set([
  "act",
  "held",
  "noop",
  "fallback",
  "discarded",
]);

/** decide.discarded_reason, plus the check a stale discard failed ("stale: target_gone"). */
function discardedReason(tags: Map<string, unknown>): string {
  const reason = tagString(tags, "decide.discarded_reason");
  const detail = tagString(tags, "decide.discarded_detail");
  return reason && detail ? `${reason}: ${detail}` : reason;
}

/** An agent.decision span linked to a decide row via the trace shape. */
export interface LinkedDecision {
  spanId: string;
  /** Span start, epoch ms (agent clock). */
  tsMs: number;
  action: string;
  blocked: boolean;
  /** decision.reason: why the agent proposed it. */
  reason: string;
  /** decision.block_reason ("" unless blocked). */
  blockReason: string;
  /**
   * decision.applied (#2093): false when a permitted decision was not written,
   * true when it was, null when the span predates the attribute or the decision
   * was blocked / failed.
   */
  applied: boolean | null;
  /** decision.not_applied_reason ("" unless applied is false), e.g. watch. */
  notAppliedReason: string;
  /** player.serial of a player-targeted decision ("" when session-scoped), #1295. */
  targetSerial: string;
  /** player.name of the targeted player ("" when the agent knew no name), #1295. */
  targetName: string;
}

/** The targeted player as shown in the Decisions row: name, else serial, else "". */
export function decisionTargetLabel(d: Pick<LinkedDecision, "targetSerial" | "targetName">): string {
  return d.targetName || d.targetSerial;
}

/**
 * The label of an ffa.target choice that names a group (UC-048 A13, BR-014, #2529):
 * "Group: all" for group:all, "Group: all but one" for group:all_except:<serial>
 * (the serial is not shown). null for any other choice, which keeps its raw text.
 */
export function groupTargetLabel(target: string): string | null {
  if (target === "group:all") return "Group: all";
  if (target.startsWith("group:all_except:")) return "Group: all but one";
  if (target.startsWith("group:")) return "Group";
  return null;
}

/**
 * The badge tooltip: the group choice without its excepted serial ("group:all",
 * "group:all_except"), so the serial stays out of the UI (UC-048 BR-014).
 */
export function groupTargetShape(target: string): string {
  return target.split(":").slice(0, 2).join(":");
}

/** One invoke_agent span, folded for the feed. */
export interface DecideRow {
  traceId: string;
  spanId: string;
  /** Span start, epoch ms. */
  tsMs: number;
  /** decide.latency_ms, else the span duration. */
  latencyMs: number;
  /** decide.backend: the backend whose answers were used ("rules" after a fallback). */
  backend: string;
  /** decide.backend.requested: the decision.backend flag value. */
  requested: string;
  fallbackReason: string;
  /** gen_ai.request.model, else the child chat span's model ("" for rules). */
  model: string;
  /** ffa.intervention choice ("" when no answer was recorded). */
  intervention: string;
  /** ffa.target choice ("none" for session-scoped, "" when absent). */
  target: string;
  /** decide.confidence (the routing confidence), null when absent. */
  confidence: number | null;
  /** decide.act_confidence_threshold, null when the router did not run. */
  threshold: number | null;
  /** decide.outcome; unknown values are passed through as "unknown". */
  outcome: DecideOutcome | "unknown";
  /** decide.discarded_reason, with decide.discarded_detail after a colon when set. */
  outcomeReason: string;
  /** decide.rationale: untrusted model text. */
  rationale: string;
  /** error.type, when the backend call failed. */
  errorType: string;
  gameId: string;
  gameKind: string;
  /** agent.decision spans this decide produced (remote) or ran beside (rules). */
  decisions: LinkedDecision[];
  /** The prediction this decision carried (#2479, UC-040 A44); null when it had none. */
  prediction: DecidePrediction | null;
}

/** Jaeger operation of a scored prediction (decide-contract §14, UC-040 A44). */
export const PREDICTION_OUTCOME_OPERATION = "agent.prediction.outcome";

/** hit / miss / unscorable come from the outcome span; pending = no outcome span (yet). */
export type PredictionState = "hit" | "miss" | "unscorable" | "pending";

/** The prediction a decision carried (decide.prediction.* tags) and its scored state. */
export interface DecidePrediction {
  id: string;
  template: string;
  characteristic: string;
  scope: string;
  horizonS: number | null;
  /** The stated probability that it comes true. */
  p: number | null;
  /** rules | lever_default | model. */
  source: string;
  /** The rules-table row ("R4") or "". */
  rule: string;
  /** Joined from the agent.prediction.outcome span of the same trace by id. */
  outcome: PredictionState;
  /** prediction.unscorable_reason of the joined span ("" otherwise). */
  unscorableReason: string;
}

/** One agent.prediction.outcome span: a scored decision prediction or a control. */
export interface PredictionOutcomeSpan {
  id: string;
  traceId: string;
  tsMs: number;
  gameId: string;
  gameKind: string;
  backend: string;
  /** rules | lever_default | model | control. */
  source: string;
  template: string;
  p: number | null;
  /** Not a control (only applied decisions register a prediction). */
  acted: boolean;
  outcome: "hit" | "miss" | "unscorable";
  unscorableReason: string;
}

/** The "current backend" strip: derived from the newest invoke_agent span. */
export interface DecideCurrent {
  tsMs: number;
  backend: string;
  requested: string;
  model: string;
  /** Threshold of the newest span, else the newest one that carried it. */
  threshold: number | null;
  /** Game the strip describes (see currentFrom, UC-070 BR-005). */
  gameId: string;
  gameKind: string;
  /** True when `threshold` came from an older span than the newest one. */
  thresholdFromOlder: boolean;
}

export interface DecideFeed {
  rows: DecideRow[];
  current: DecideCurrent | null;
  /** Every agent.prediction.outcome span found (decisions and controls); parse results only. */
  outcomes?: PredictionOutcomeSpan[];
  /** The followed game's hit rate vs control base rate per source (#2479); null without outcomes. */
  predictionStrip?: PredictionStrip | null;
  /** The followed game's goal profile and attainment (#2498); null until its summary exists. */
  goal?: GameGoal | null;
}

/** One source's acted predictions of a game against the controls' base rate. */
export interface PredictionStripSource {
  source: string;
  hits: number;
  misses: number;
  unscorable: number;
  /** hits / (hits + misses); null when nothing was scored. */
  hitRate: number | null;
  /**
   * Hit rate the controls reach on the same templates, weighted by this source's
   * template mix; null while no control of those templates was scored.
   */
  baseRate: number | null;
}

/** The per-game prediction strip: decision predictions, controls as the base rate. */
export interface PredictionStrip {
  gameId: string;
  gameKind: string;
  sources: PredictionStripSource[];
}

/**
 * Pull the ffa.intervention / ffa.target choices out of decide.answers. The
 * attribute is size-capped JSON (it may end in "...(truncated)"), so a parse
 * failure is expected and yields blanks rather than an error.
 */
export function parseDecideAnswers(raw: string): { intervention: string; target: string } {
  const out = { intervention: "", target: "" };
  if (!raw) return out;
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return out;
  }
  if (!Array.isArray(parsed)) return out;
  for (const a of parsed) {
    if (!a || typeof a !== "object") continue;
    const ans = a as { question_id?: unknown; choice?: unknown };
    if (typeof ans.choice !== "string") continue;
    if (ans.question_id === "ffa.intervention") out.intervention = ans.choice;
    else if (ans.question_id === "ffa.target") out.target = ans.choice;
  }
  return out;
}

function isChatSpan(span: JaegerSpan, tags: Map<string, unknown>): boolean {
  return span.operationName.startsWith("chat ") || tags.get("gen_ai.operation.name") === "chat";
}

/**
 * The decide.prediction.* tags of an invoke_agent span, with the outcome joined from
 * the agent.prediction.outcome span of the same trace by prediction id. No outcome
 * span (yet) reads as pending. null when the decision carried no prediction.
 */
function predictionFrom(
  tags: Map<string, unknown>,
  outcomes: Map<string, PredictionOutcomeSpan>,
): DecidePrediction | null {
  const id = tagString(tags, "decide.prediction.id");
  if (!id) return null;
  const scored = outcomes.get(id);
  return {
    id,
    template: tagString(tags, "decide.prediction.template"),
    characteristic: tagString(tags, "decide.prediction.characteristic"),
    scope: tagString(tags, "decide.prediction.scope"),
    horizonS: tagNumber(tags, "decide.prediction.horizon_s"),
    p: tagNumber(tags, "decide.prediction.p"),
    source: tagString(tags, "decide.prediction.source"),
    rule: tagString(tags, "decide.prediction.rule"),
    outcome: scored ? scored.outcome : "pending",
    unscorableReason: scored ? scored.unscorableReason : "",
  };
}

const KNOWN_PREDICTION_OUTCOMES: ReadonlySet<string> = new Set(["hit", "miss", "unscorable"]);

/** Fold one agent.prediction.outcome span; null for an unknown outcome or a missing id. */
function outcomeSpanFrom(span: JaegerSpan): PredictionOutcomeSpan | null {
  const tags = spanTags(span);
  const id = tagString(tags, "prediction.id");
  const outcome = tagString(tags, "prediction.outcome");
  if (!id || !KNOWN_PREDICTION_OUTCOMES.has(outcome)) return null;
  return {
    id,
    traceId: span.traceID,
    tsMs: spanStartMs(span),
    gameId: tagString(tags, "game.id"),
    gameKind: tagString(tags, "game.kind"),
    backend: tagString(tags, "prediction.backend"),
    source: tagString(tags, "prediction.source"),
    template: tagString(tags, "prediction.template"),
    p: tagNumber(tags, "prediction.p"),
    acted: tagBool(tags, "prediction.acted"),
    outcome: outcome as PredictionOutcomeSpan["outcome"],
    unscorableReason: tagString(tags, "prediction.unscorable_reason"),
  };
}

/**
 * Hit rate per source against the controls' base rate (#2479). Unscorable is neither a hit
 * nor a miss. The base rate of a source is the controls' hit rate on
 * the templates that source predicted, weighted by its own template mix, so a source
 * that only picks easy templates is compared with how easy they are.
 */
export function predictionStripFrom(
  outcomes: Iterable<PredictionOutcomeSpan>,
  gameId: string,
): PredictionStrip | null {
  const game = [...outcomes].filter((o) => o.gameId === gameId);
  if (game.length === 0) return null;
  const control = new Map<string, { hits: number; scored: number }>();
  for (const o of game) {
    if (o.source !== "control" || o.outcome === "unscorable") continue;
    const c = control.get(o.template) ?? { hits: 0, scored: 0 };
    c.scored++;
    if (o.outcome === "hit") c.hits++;
    control.set(o.template, c);
  }
  const bySource = new Map<string, PredictionStripSource & { mix: Map<string, number> }>();
  for (const o of game) {
    if (o.source === "control") continue;
    let src = bySource.get(o.source);
    if (!src) {
      src = { source: o.source, hits: 0, misses: 0, unscorable: 0, hitRate: null, baseRate: null, mix: new Map() };
      bySource.set(o.source, src);
    }
    if (o.outcome === "unscorable") {
      src.unscorable++;
      continue;
    }
    if (o.outcome === "hit") src.hits++;
    else src.misses++;
    src.mix.set(o.template, (src.mix.get(o.template) ?? 0) + 1);
  }
  const sources: PredictionStripSource[] = [];
  for (const src of [...bySource.values()].sort((a, b) => a.source.localeCompare(b.source))) {
    const scored = src.hits + src.misses;
    let weight = 0;
    let expected = 0;
    for (const [template, n] of src.mix) {
      const c = control.get(template);
      if (!c || c.scored === 0) continue;
      weight += n;
      expected += n * (c.hits / c.scored);
    }
    sources.push({
      source: src.source,
      hits: src.hits,
      misses: src.misses,
      unscorable: src.unscorable,
      hitRate: scored > 0 ? src.hits / scored : null,
      baseRate: weight > 0 ? expected / weight : null,
    });
  }
  const gameKind = game[0].gameKind;
  return { gameId, gameKind, sources };
}

/** Fold one invoke_agent span (with its trace neighbours) into a feed row. */
function decideRowFrom(
  span: JaegerSpan,
  byParent: Map<string, JaegerSpan[]>,
  byId: Map<string, JaegerSpan>,
  outcomes: Map<string, PredictionOutcomeSpan>,
): DecideRow {
  const tags = spanTags(span);
  const parent = parentSpanId(span);

  // Remote path: decision + chat spans are children of the decide span.
  // Rules path: decisions are siblings under the same agent.signal_received.
  const children = byParent.get(span.spanID) ?? [];
  let decisionSpans = children.filter((s) => s.operationName === "agent.decision");
  // Only the rules path shares a parent with its decisions. Without game ids that
  // parent is the cycle's agent.signal_received. In a game (#2648) there is no such
  // span: the decide and its decisions are siblings under game_cycle, and the
  // decisions of one cycle carry the decide's decide.state.digest. A remote decide's
  // parent is the game's game_cycle too, whose other children are unrelated: those
  // are never linked (a remote decide has its decisions as children, above).
  if (decisionSpans.length === 0) {
    const siblings = (byParent.get(parent) ?? []).filter((s) => s.operationName === "agent.decision");
    if (byId.get(parent)?.operationName === SIGNAL_OPERATION) {
      decisionSpans = siblings;
    } else {
      const digest = tagString(tags, "decide.state.digest");
      if (digest) {
        decisionSpans = siblings.filter(
          (s) => tagString(spanTags(s), "decide.state.digest") === digest,
        );
      }
    }
  }

  let model = tagString(tags, "gen_ai.request.model");
  if (!model) {
    for (const c of children) {
      const ct = spanTags(c);
      if (!isChatSpan(c, ct)) continue;
      model =
        tagString(ct, "gen_ai.response.model") ||
        tagString(ct, "gen_ai.request.model") ||
        c.operationName.replace(/^chat /, "");
      if (model) break;
    }
  }

  const answers = parseDecideAnswers(tagString(tags, "decide.answers"));
  const rawOutcome = tagString(tags, "decide.outcome");
  const latency = tagNumber(tags, "decide.latency_ms");

  return {
    traceId: span.traceID,
    spanId: span.spanID,
    tsMs: spanStartMs(span),
    latencyMs: latency ?? Math.round(span.duration / 1000),
    backend: tagString(tags, "decide.backend"),
    requested: tagString(tags, "decide.backend.requested"),
    fallbackReason: tagString(tags, "decide.fallback_reason"),
    model,
    intervention: answers.intervention,
    target: answers.target,
    confidence: tagNumber(tags, "decide.confidence"),
    threshold: tagNumber(tags, "decide.act_confidence_threshold"),
    outcome: KNOWN_OUTCOMES.has(rawOutcome) ? (rawOutcome as DecideOutcome) : "unknown",
    outcomeReason: discardedReason(tags),
    rationale: tagString(tags, "decide.rationale"),
    errorType: tagString(tags, "error.type"),
    gameId: tagString(tags, "game.id"),
    gameKind: tagString(tags, "game.kind"),
    prediction: predictionFrom(tags, outcomes),
    decisions: decisionSpans
      .slice()
      .sort((a, b) => a.startTime - b.startTime)
      .map((d) => {
        const dt = spanTags(d);
        return {
          spanId: d.spanID,
          tsMs: spanStartMs(d),
          action: tagString(dt, "decision.action"),
          blocked: tagBool(dt, "decision.blocked"),
          reason: tagString(dt, "decision.reason"),
          blockReason: tagString(dt, "decision.block_reason"),
          applied: tagBoolOrNull(dt, "decision.applied"),
          notAppliedReason: tagString(dt, "decision.not_applied_reason"),
          targetSerial: tagString(dt, "player.serial"),
          targetName: tagString(dt, "player.name"),
        };
      }),
  };
}

/**
 * Fold a Jaeger search response (jaeger.ts fromOtlp) into the decide feed.
 *
 * Pure. Rows are deduplicated by (traceID, spanID): Jaeger can return the same
 * trace more than once when it is still being written to, and the remote path
 * puts many decide spans in one per-game trace. Newest first, capped.
 */
export function parseDecideTraces(body: JaegerTracesResponse): DecideFeed {
  const rows: DecideRow[] = [];
  const seen = new Set<string>();
  const allOutcomes: PredictionOutcomeSpan[] = [];

  for (const trace of body.data ?? []) {
    const spans = trace.spans ?? [];
    // The outcome span sits in the game trace beside the decide span: join by id.
    const traceOutcomes = new Map<string, PredictionOutcomeSpan>();
    for (const s of spans) {
      if (s.operationName !== PREDICTION_OUTCOME_OPERATION) continue;
      const o = outcomeSpanFrom(s);
      if (!o) continue;
      traceOutcomes.set(o.id, o);
      allOutcomes.push(o);
    }
    const byParent = new Map<string, JaegerSpan[]>();
    const byId = new Map<string, JaegerSpan>();
    for (const s of spans) {
      byId.set(s.spanID, s);
      const p = parentSpanId(s);
      if (!p) continue;
      const list = byParent.get(p);
      if (list) list.push(s);
      else byParent.set(p, [s]);
    }
    for (const s of spans) {
      if (s.operationName !== DECIDE_OPERATION) continue;
      const key = `${s.traceID}/${s.spanID}`;
      if (seen.has(key)) continue;
      seen.add(key);
      rows.push(decideRowFrom(s, byParent, byId, traceOutcomes));
    }
  }

  rows.sort(newestFirst);
  const capped = rows.slice(0, MAX_DECIDE_ROWS);
  const current = currentFrom(rows);
  // The outcome fields are only present when the traces carried outcome spans.
  const strip = current ? predictionStripFrom(allOutcomes, current.gameId) : null;
  return {
    rows: capped,
    current,
    ...(allOutcomes.length > 0 ? { outcomes: allOutcomes } : {}),
    ...(strip ? { predictionStrip: strip } : {}),
  };
}

/**
 * The current-backend strip. By default it describes the newest NON-shadow game
 * (the one on stage): a shadow decide must never flip the strip while the real
 * game is on. When no real game is live (the screen-only fallback, UC-070 A9) it
 * describes the followed shadow game, else the game of the newest row of any
 * kind, so an older real game's rows do not hold the strip and an older shadow
 * row does not beat a newer real one. Without any non-shadow row it falls back to
 * the game of the newest row. `rows` must be newest first.
 */
export function currentFrom(rows: DecideRow[], follow?: DecideFollow): DecideCurrent | null {
  const anchor = pickAnchor(rows, follow);
  if (!anchor) return null;
  const gameRows = rows.filter((r) => r.gameId === anchor.gameId);
  const newest = gameRows[0] ?? anchor;
  let threshold = newest.threshold;
  let thresholdFromOlder = false;
  if (threshold === null) {
    const older = gameRows.find((r) => r.threshold !== null) ?? rows.find((r) => r.threshold !== null);
    if (older) {
      threshold = older.threshold;
      thresholdFromOlder = true;
    }
  }
  return {
    tsMs: newest.tsMs,
    backend: newest.backend,
    requested: newest.requested,
    model: newest.model,
    threshold,
    thresholdFromOlder,
    gameId: newest.gameId,
    gameKind: newest.gameKind,
  };
}

function newestFirst(a: DecideRow, b: DecideRow): number {
  return b.tsMs - a.tsMs || b.spanId.localeCompare(a.spanId);
}

/**
 * The decide rows of the agent's last answer (GET /experiments/state). The agent
 * holds the newest decides per game-kind class, so the answer replaces the rows; a
 * prediction outcome or game goal stays held after the agent dropped its span.
 */
export class DecideStore {
  private rows: DecideRow[] = [];
  private follow: DecideFollow = { realGameLive: null, followGameId: "" };
  private readonly outcomes = new Map<string, PredictionOutcomeSpan>();
  private readonly goals = new GoalStore();

  constructor(private readonly max = MAX_DECIDE_ROWS) {}

  /** Load an answer's decide spans: rows, scored predictions and game goals. */
  load(body: JaegerTracesResponse): void {
    const parsed = parseDecideTraces(body);
    this.mergeOutcomes(parsed.outcomes ?? []);
    this.goals.merge(parseGoalSummaries(body));
    this.setRows(parsed.rows);
  }

  /** Replace the rows (newest first after sorting, capped). */
  setRows(rows: DecideRow[]): void {
    this.rows = [...rows].sort(newestFirst).slice(0, this.max).map((r) => this.withHeldOutcome(r));
  }

  /**
   * Merge outcome spans (keyed by prediction id, newest kept), bounded per class like
   * the agent (shadow, everything else), so a shadow burst never pushes out the stage
   * game's strip.
   */
  mergeOutcomes(outcomes: PredictionOutcomeSpan[]): void {
    for (const o of outcomes) this.outcomes.set(o.id, o);
    if (this.outcomes.size <= MAX_OUTCOMES) return;
    const newest = [...this.outcomes.values()].sort((a, b) => b.tsMs - a.tsMs);
    const keep = [
      ...newest.filter((o) => isShadowGame(o.gameKind)).slice(0, MAX_OUTCOMES),
      ...newest.filter((o) => !isShadowGame(o.gameKind)).slice(0, MAX_OUTCOMES),
    ];
    this.outcomes.clear();
    for (const o of keep) this.outcomes.set(o.id, o);
  }

  /** The followed game's goal (#2498), null until its summary arrived. */
  goalFor(gameId: string): GameGoal | null {
    return this.goals.forGame(gameId);
  }

  /** The follow state the next feed() uses. */
  get followState(): DecideFollow {
    return { ...this.follow };
  }

  get size(): number {
    return this.rows.length;
  }

  /** The game the dashboard follows (UC-070 BR-005); used by the next feed(). */
  setFollow(follow: DecideFollow): void {
    this.follow = { ...follow };
  }

  feed(): DecideFeed {
    const rows = [...this.rows];
    const current = currentFrom(rows, this.follow);
    return {
      rows,
      current,
      predictionStrip: current ? predictionStripFrom(this.outcomes.values(), current.gameId) : null,
      goal: current ? this.goals.forGame(current.gameId) : null,
    };
  }

  /** A pending prediction whose outcome span is held from an earlier answer reads that outcome. */
  private withHeldOutcome(row: DecideRow): DecideRow {
    if (row.prediction?.outcome !== "pending") return row;
    const scored = this.outcomes.get(row.prediction.id);
    if (!scored) return row;
    return { ...row, prediction: { ...row.prediction, outcome: scored.outcome, unscorableReason: scored.unscorableReason } };
  }
}