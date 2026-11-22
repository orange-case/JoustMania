/**
 * Stage view data (UC-035 A8, BR-017, #2721): what the talk shows full screen.
 *
 * Only OpenTelemetry data from the stack's own stores, read through envoy:
 * VictoriaMetrics for the acceleration, thresholds and alive state, Jaeger for
 * the agent's decisions. Every query is relative to the server's own now (range
 * selectors and subqueries, the v3 search bounds of jaeger.ts), never a browser
 * clock time: the stage Pi may run without NTP. All times below are epoch ms in
 * the Pi's clock.
 */
import { BASE_URL } from "./baseUrl.js";
import { searchTraces } from "./jaeger.js";
import type { JaegerTracesResponse } from "./jaeger.js";
import { DECIDE_OPERATION, parseDecideTraces } from "./agentDecide.js";

/** Seconds of history the stage view shows. */
export const STAGE_WINDOW_S = 60;
/** Seconds a recording keeps. */
export const RECORD_WINDOW_S = 180;
/** Newest decide traces searched per poll (one per game, the live one first). */
export const STAGE_DECIDE_TRACES = 2;

export interface Pt {
  /** Epoch ms, Pi clock. */
  t: number;
  v: number;
}

export interface PlayerSeries {
  serial: string;
  name: string;
  points: Pt[];
}

export interface StageOut {
  serial: string;
  t: number;
}

export interface StageDecision {
  t: number;
  action: string;
  /** "" for a session-wide action (tempo, pacing, a cue for everyone). */
  targetSerial: string;
  targetName: string;
  rationale: string;
  backend: string;
  confidence: number | null;
}

/** Everything one frame needs; also the recording format (`&record`, `&replay=`). */
export interface StageSnapshot {
  version: 1;
  players: PlayerSeries[];
  warning: Pt[];
  death: Pt[];
  outs: StageOut[];
  decisions: StageDecision[];
}

export function emptySnapshot(): StageSnapshot {
  return { version: 1, players: [], warning: [], death: [], outs: [], decisions: [] };
}

// --- queries ---------------------------------------------------------------

/** Names of the connected real controllers (mocks, incl. the shadow bots, left out). */
export const NAMES_QUERY =
  'topk by (serial) (1, controller_info unless on(serial) controller_backend_info{backend="mock"})';

export const accelQuery = (windowS: number): string => `controller_accel_magnitude[${windowS}s]`;
export const warningQuery = (windowS: number): string =>
  `max(game_effective_warning_threshold{game_kind=~"real|primary"})[${windowS}s:1s]`;
export const deathQuery = (windowS: number): string =>
  `max(game_effective_death_threshold{game_kind=~"real|primary"})[${windowS}s:1s]`;
export const aliveQuery = (windowS: number): string =>
  `max by (serial) (game_player_alive{game_kind=~"real|primary"})[${windowS}s:500ms]`;

/** A Prometheus API answer: an instant vector or a matrix (range selector, subquery). */
export interface PromAnswer {
  status: string;
  data?: {
    resultType: string;
    result: { metric: Record<string, string>; value?: [number, string]; values?: [number, string][] }[];
  };
}

/** One instant query against VictoriaMetrics; null when it cannot be read. */
export async function promQuery(query: string, signal?: AbortSignal): Promise<PromAnswer | null> {
  try {
    const params = new URLSearchParams({ query, nocache: "1" });
    const response = await fetch(`${BASE_URL}/victoria-metrics/api/v1/query?${params.toString()}`, {
      headers: { Accept: "application/json" },
      signal,
    });
    if (!response.ok) return null;
    const body = (await response.json()) as PromAnswer;
    return body.status === "success" ? body : null;
  } catch {
    return null;
  }
}

// --- folds (pure) ----------------------------------------------------------

/** serial → player name, from the names query. */
export function namesFrom(answer: PromAnswer | null): Map<string, string> {
  const names = new Map<string, string>();
  for (const r of answer?.data?.result ?? []) {
    const serial = r.metric.serial;
    if (serial) names.set(serial, r.metric.name || serial);
  }
  return names;
}

function pointsOf(values: [number, string][] | undefined): Pt[] {
  const out: Pt[] = [];
  for (const [ts, raw] of values ?? []) {
    const v = Number(raw);
    if (Number.isFinite(v)) out.push({ t: Math.round(ts * 1000), v });
  }
  return out;
}

/** One series per named controller; a serial without a name (a mock) is left out. */
export function playersFrom(answer: PromAnswer | null, names: Map<string, string>): PlayerSeries[] {
  const bySerial = new Map<string, PlayerSeries>();
  for (const r of answer?.data?.result ?? []) {
    const serial = r.metric.serial;
    const name = serial ? names.get(serial) : undefined;
    if (!serial || name === undefined) continue;
    const points = pointsOf(r.values);
    const prev = bySerial.get(serial);
    // A process restart starts a new series for the same serial: merge them.
    if (prev) prev.points = [...prev.points, ...points].sort((a, b) => a.t - b.t);
    else bySerial.set(serial, { serial, name, points });
  }
  return [...bySerial.values()].sort((a, b) => a.name.localeCompare(b.name) || a.serial.localeCompare(b.serial));
}

/** The single series of a max() subquery. */
export function lineFrom(answer: PromAnswer | null): Pt[] {
  return pointsOf(answer?.data?.result?.[0]?.values);
}

/** The moments a player went out: their alive state turned from 1 to 0. */
export function outsFrom(answer: PromAnswer | null): StageOut[] {
  const outs: StageOut[] = [];
  for (const r of answer?.data?.result ?? []) {
    const serial = r.metric.serial;
    if (!serial) continue;
    const pts = pointsOf(r.values);
    for (let i = 1; i < pts.length; i++) {
      if (pts[i - 1].v >= 1 && pts[i].v < 1) outs.push({ serial, t: pts[i].t });
    }
  }
  return outs.sort((a, b) => a.t - b.t);
}

/**
 * The applied decisions of real games, oldest first: every agent.decision span
 * that was written (not blocked, not left unapplied), with the decide's
 * rationale (else the decision's own reason), backend and confidence.
 */
export function decisionsFrom(body: JaegerTracesResponse): StageDecision[] {
  const out: StageDecision[] = [];
  for (const row of parseDecideTraces(body).rows) {
    if (row.gameKind === "shadow") continue;
    for (const d of row.decisions) {
      if (d.blocked || d.applied === false || !d.action || d.action === "noop") continue;
      out.push({
        t: d.tsMs,
        action: d.action,
        targetSerial: d.targetSerial,
        targetName: d.targetName,
        rationale: (row.rationale || d.reason || "").trim(),
        backend: row.backend,
        confidence: row.confidence,
      });
    }
  }
  return out.sort((a, b) => a.t - b.t);
}

/** The newest sample time of the snapshot (the right edge of the chart), or null. */
export function newestTime(s: StageSnapshot): number | null {
  let newest: number | null = null;
  for (const p of s.players) {
    const last = p.points[p.points.length - 1];
    if (last && (newest === null || last.t > newest)) newest = last.t;
  }
  return newest;
}

/** The part of a snapshot inside [from, to]. */
export function sliceSnapshot(s: StageSnapshot, from: number, to: number): StageSnapshot {
  const inside = (t: number): boolean => t >= from && t <= to;
  return {
    version: 1,
    players: s.players.map((p) => ({ ...p, points: p.points.filter((q) => inside(q.t)) })),
    warning: s.warning.filter((q) => inside(q.t)),
    death: s.death.filter((q) => inside(q.t)),
    outs: s.outs.filter((o) => inside(o.t)),
    decisions: s.decisions.filter((d) => inside(d.t)),
  };
}

/** Time span covered by a recording: [first, last] sample, or null when empty. */
export function snapshotRange(s: StageSnapshot): [number, number] | null {
  let lo = Infinity;
  let hi = -Infinity;
  for (const p of s.players) {
    for (const q of p.points) {
      if (q.t < lo) lo = q.t;
      if (q.t > hi) hi = q.t;
    }
  }
  return lo <= hi ? [lo, hi] : null;
}

/**
 * Replay clock: the virtual "now" of a recording `elapsedMs` after the replay
 * started, at real speed, looping. It starts one window into the recording so
 * the first frame is full.
 */
export function replayNow(range: [number, number], elapsedMs: number, windowMs: number): number {
  const [lo, hi] = range;
  const start = Math.min(lo + windowMs, hi);
  const span = hi - start;
  if (span <= 0) return hi;
  // Each loop holds the last frame for REPLAY_PAUSE_MS before starting again.
  const e = Math.max(0, elapsedMs) % (span + REPLAY_PAUSE_MS);
  return start + Math.min(e, span);
}

/** How long a replay holds its last frame before it loops. */
export const REPLAY_PAUSE_MS = 2000;

/** Is a value a usable recording? (A replay URL can point anywhere.) */
export function isSnapshot(x: unknown): x is StageSnapshot {
  const s = x as StageSnapshot;
  return (
    !!s &&
    s.version === 1 &&
    Array.isArray(s.players) &&
    Array.isArray(s.warning) &&
    Array.isArray(s.death) &&
    Array.isArray(s.outs) &&
    Array.isArray(s.decisions)
  );
}

// --- live reads ------------------------------------------------------------

export interface MetricsRead {
  players: PlayerSeries[];
  warning: Pt[];
  death: Pt[];
  outs: StageOut[];
  /** False when the acceleration could not be read. */
  ok: boolean;
}

/** Read the metric half of a snapshot over the last `windowS` seconds. */
export async function readMetrics(windowS: number, signal?: AbortSignal): Promise<MetricsRead> {
  const [names, accel, warn, death, alive] = await Promise.all([
    promQuery(NAMES_QUERY, signal),
    promQuery(accelQuery(windowS), signal),
    promQuery(warningQuery(windowS), signal),
    promQuery(deathQuery(windowS), signal),
    promQuery(aliveQuery(windowS), signal),
  ]);
  return {
    players: playersFrom(accel, namesFrom(names)),
    warning: lineFrom(warn),
    death: lineFrom(death),
    outs: outsFrom(alive),
    ok: accel !== null && names !== null,
  };
}

/** Read the agent's applied decisions; null when Jaeger cannot be read. */
export async function readDecisions(signal?: AbortSignal): Promise<StageDecision[] | null> {
  try {
    const body = await searchTraces({ operation: DECIDE_OPERATION, limit: STAGE_DECIDE_TRACES, startMs: null }, signal);
    return decisionsFrom(body);
  } catch {
    return null;
  }
}
