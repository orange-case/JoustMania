/**
 * Stage view (UC-035 A8, BR-017, #2721): `/?view=stage` shows one full-screen
 * picture for the talk instead of the dashboard. One line per player, the
 * thresholds, a marker when a player goes out, and the agent's applied
 * decisions with their rationale. `&replay=<url>` plays a recording,
 * `&record` adds a button that saves the last 3 min, `&theme=dark|light`
 * forces a theme (the slide passes its own).
 */
import { html, setHtml } from "./html.js";
import type { SafeHtml } from "./html.js";
import { applyTheme } from "./theme.js";
import {
  RECORD_WINDOW_S,
  STAGE_WINDOW_S,
  emptySnapshot,
  isSnapshot,
  newestTime,
  readDecisions,
  readMetrics,
  replayNow,
  sliceSnapshot,
  snapshotRange,
} from "./stage.js";
import type { PlayerSeries, Pt, StageDecision, StageSnapshot } from "./stage.js";

export const STAGE_METRICS_POLL_MS = 1000;
export const STAGE_DECIDE_POLL_MS = 2000;
export const STAGE_REPLAY_FRAME_MS = 250;
/** Decision cards under the chart. */
export const STAGE_CARDS = 3;

const W = 1600;
const H = 760;
const L = 64;
const R = 190;
const T = 36;
const B = 40;
const PLOT_W = W - L - R;
const PLOT_H = H - T - B;

/** Line colours per player (sorted by name), readable on light and dark. */
export const PLAYER_COLORS = ["#e8701a", "#3b82f6", "#16a34a", "#a855f7", "#0891b2", "#db2777", "#ca8a04", "#64748b"];

const ACTION_ICONS: Record<string, string> = {
  grant_shield: "🛡️",
  grant_group_shield: "🛡️",
  toughness_boost: "💪",
  adjust_player_sensitivity: "🎚",
  set_player_handicap: "⚖️",
  revive_player: "❤️",
  eliminate_player: "☠️",
  soft_penalty: "⚠️",
  adjust_music_tempo: "🎵",
  set_pacing_profile: "🎵",
  play_audio_cue: "🔊",
  controller_effect: "💡",
};

export function actionIcon(action: string): string {
  return ACTION_ICONS[action] ?? "🤖";
}

export function actionLabel(action: string): string {
  return action.replace(/_/g, " ");
}

export function isStageView(search: string): boolean {
  return new URLSearchParams(search).get("view") === "stage";
}

/** The value of a series at time t (the newest sample at or before t), or null. */
export function valueAt(points: Pt[], t: number): number | null {
  let v: number | null = null;
  for (const p of points) {
    if (p.t > t) break;
    v = p.v;
  }
  return v ?? (points.length > 0 ? points[0].v : null);
}

/** Spread label y positions so no two are closer than `gap` (stable order by y). */
export function spreadLabels(ys: number[], gap: number, lo: number, hi: number): number[] {
  const order = ys.map((y, i) => ({ y, i })).sort((a, b) => a.y - b.y);
  const out = new Array<number>(ys.length);
  let prev = -Infinity;
  for (const o of order) {
    const y = Math.max(o.y, prev + gap, lo);
    out[o.i] = y;
    prev = y;
  }
  // Pushed past the bottom: shift the stack up.
  const over = Math.max(0, prev - hi);
  return over > 0 ? out.map((y) => Math.max(lo, y - over)) : out;
}

function clip(text: string, max: number): string {
  return text.length <= max ? text : `${text.slice(0, max - 1).trimEnd()}…`;
}

function agoLabel(now: number, t: number): string {
  const s = Math.max(0, Math.round((now - t) / 1000));
  return s < 2 ? "now" : `${s} s ago`;
}

/** Everything that frames one picture. */
export interface StageFrame {
  snapshot: StageSnapshot;
  /** Right edge of the chart, Pi clock. */
  now: number;
  mode: "live" | "replay";
  metricsOk: boolean;
  decisionsOk: boolean;
}

/** The stage picture as markup (pure). */
export function renderStage(frame: StageFrame): SafeHtml {
  const windowMs = STAGE_WINDOW_S * 1000;
  const from = frame.now - windowMs;
  const s = sliceSnapshot(frame.snapshot, from, frame.now);
  const x = (t: number): number => L + ((t - from) / windowMs) * PLOT_W;

  let top = 3;
  for (const p of s.players) for (const q of p.points) top = Math.max(top, q.v * 1.05);
  for (const q of s.death) top = Math.max(top, q.v * 1.1);
  top = Math.min(Math.ceil(top * 2) / 2, 8);
  const y = (v: number): number => T + (1 - Math.min(v, top) / top) * PLOT_H;
  const poly = (pts: Pt[]): string => pts.map((p) => `${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join(" ");

  const grid: SafeHtml[] = [];
  for (let g = 1; g <= top; g++) {
    grid.push(html`<line class="st-grid" x1="${L}" x2="${W - R}" y1="${y(g)}" y2="${y(g)}" /><text class="st-tick" x="${L - 10}" y="${y(g) + 6}" text-anchor="end">${g} g</text>`);
  }

  const outBySerial = new Map(s.outs.map((o) => [o.serial, o.t]));
  const lines: SafeHtml[] = [];
  const labelY: number[] = [];
  const labelFor: { p: PlayerSeries; color: string; out: boolean }[] = [];
  s.players.forEach((p, i) => {
    if (p.points.length === 0) return;
    const color = PLAYER_COLORS[i % PLAYER_COLORS.length];
    const outT = outBySerial.get(p.serial);
    const before = outT === undefined ? p.points : p.points.filter((q) => q.t <= outT);
    const after = outT === undefined ? [] : p.points.filter((q) => q.t >= outT);
    lines.push(html`<polyline class="st-line" points="${poly(before)}" stroke="${color}" />`);
    if (after.length > 1) lines.push(html`<polyline class="st-line st-dead" points="${poly(after)}" stroke="${color}" />`);
    labelY.push(y(p.points[p.points.length - 1].v));
    labelFor.push({ p, color, out: outT !== undefined });
  });
  const spread = spreadLabels(labelY, 26, T + 12, H - B);
  const labels = labelFor.map(
    (l, i) =>
      html`<text class="st-name${l.out ? " st-name-out" : ""}" x="${W - R + 14}" y="${spread[i] + 7}" fill="${l.color}">${l.out ? "✕ " : ""}${clip(l.p.name, 14)}</text>`,
  );

  const thresholds = html`${s.warning.length > 1 ? html`<polyline class="st-warn" points="${poly(s.warning)}" />` : ""}${
    s.death.length > 1 ? html`<polyline class="st-death" points="${poly(s.death)}" />` : ""
  }`;

  const bySerial = new Map(s.players.map((p) => [p.serial, p]));
  const outs = s.outs.map((o) => {
    const p = bySerial.get(o.serial);
    const v = p ? valueAt(p.points, o.t) : null;
    if (v === null) return html``;
    return html`<g class="st-out"><circle cx="${x(o.t)}" cy="${y(v)}" r="11" /><text x="${x(o.t)}" y="${y(v) - 18}" text-anchor="middle">out</text></g>`;
  });

  const acts = s.decisions.map((d) => {
    const p = d.targetSerial ? bySerial.get(d.targetSerial) : undefined;
    const v = p ? valueAt(p.points, d.t) : null;
    if (p && v !== null) {
      return html`<g class="st-act"><circle cx="${x(d.t)}" cy="${y(v)}" r="13" /><text x="${x(d.t)}" y="${y(v) - 22}" text-anchor="middle">${actionIcon(d.action)}</text></g>`;
    }
    return html`<g class="st-act st-act-wide"><line x1="${x(d.t)}" x2="${x(d.t)}" y1="${T}" y2="${H - B}" /><text x="${x(d.t)}" y="${T - 8}" text-anchor="middle">${actionIcon(d.action)}</text></g>`;
  });

  const empty =
    s.players.length === 0
      ? html`<text class="st-empty" x="${L + PLOT_W / 2}" y="${T + PLOT_H / 2}" text-anchor="middle">${frame.metricsOk ? "Waiting for controllers…" : "No metrics from the stack"}</text>`
      : "";

  const cards = [...s.decisions].reverse().slice(0, STAGE_CARDS).map((d) => decisionCard(d, frame.now, bySerial));

  const chips = html`<span class="st-chip ${frame.mode === "live" ? "st-chip-live" : "st-chip-replay"}">${
    frame.mode === "live" ? "● live" : "▶ replay"
  }</span>${frame.metricsOk ? "" : html`<span class="st-chip st-chip-warn">no metrics</span>`}${
    frame.decisionsOk ? "" : html`<span class="st-chip st-chip-warn">no agent traces</span>`
  }`;

  return html`<div class="st-top">${chips}<span class="st-legend"><span class="st-key st-key-death"></span>death <span class="st-key st-key-warn"></span>warning <span class="st-key st-key-act"></span>the agent acts</span></div>
<svg class="st-chart" viewBox="0 0 ${W} ${H}" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Acceleration per player">
${grid}${thresholds}${lines}${outs}${acts}${labels}${empty}
</svg>
<div class="st-cards">${cards.length > 0 ? cards : html`<div class="st-card st-card-empty">No agent action yet in this minute.</div>`}</div>`;
}

function decisionCard(d: StageDecision, now: number, bySerial: Map<string, PlayerSeries>): SafeHtml {
  const who = d.targetName || bySerial.get(d.targetSerial)?.name || (d.targetSerial ? d.targetSerial : "everyone");
  const conf = d.confidence === null ? "" : ` · ${d.confidence.toFixed(2)}`;
  return html`<div class="st-card"><div class="st-card-head">${actionIcon(d.action)} ${actionLabel(d.action)} <span class="st-arrow">→</span> ${who}</div>${
    d.rationale ? html`<div class="st-card-why">“${clip(d.rationale, 150)}”</div>` : ""
  }<div class="st-card-meta">${d.backend}${conf} · ${agoLabel(now, d.t)}</div></div>`;
}

// --- controller --------------------------------------------------------------

function themeFrom(search: string): "light" | "dark" | null {
  const t = new URLSearchParams(search).get("theme");
  return t === "light" || t === "dark" ? t : null;
}

function download(name: string, data: unknown, doc: Document): void {
  const blob = new Blob([JSON.stringify(data)], { type: "application/json" });
  const a = doc.createElement("a");
  a.href = URL.createObjectURL(blob);
  a.download = name;
  a.click();
  URL.revokeObjectURL(a.href);
}

/** Replace the page with the stage view and keep it updated. */
export async function startStageView(doc: Document = document, win: Window = window): Promise<void> {
  const search = win.location.search;
  const params = new URLSearchParams(search);
  const forced = themeFrom(search);
  if (forced) applyTheme(doc, forced, forced === "dark");
  doc.title = "JoustMania · stage";
  doc.body.className = "stage-body";
  setHtml(
    doc.body,
    html`<div id="stage" class="stage"></div>${
      params.has("record") ? html`<button id="stage-record" class="stage-record">⬇ save the last 3 min</button>` : ""
    }`,
  );
  const root = doc.getElementById("stage") as HTMLElement;
  const paint = (frame: StageFrame): void => setHtml(root, renderStage(frame));

  const replay = params.get("replay");
  if (replay) {
    let recording: StageSnapshot | null = null;
    try {
      const res = await fetch(replay, { headers: { Accept: "application/json" } });
      const body: unknown = res.ok ? await res.json() : null;
      recording = isSnapshot(body) ? body : null;
    } catch {
      recording = null;
    }
    const range = recording ? snapshotRange(recording) : null;
    if (!recording || !range) {
      paint({ snapshot: emptySnapshot(), now: 0, mode: "replay", metricsOk: false, decisionsOk: false });
      return;
    }
    const started = Date.now();
    const frame = (): void =>
      paint({
        snapshot: recording as StageSnapshot,
        now: replayNow(range, Date.now() - started, STAGE_WINDOW_S * 1000),
        mode: "replay",
        metricsOk: true,
        decisionsOk: true,
      });
    frame();
    win.setInterval(frame, STAGE_REPLAY_FRAME_MS);
    return;
  }

  let snapshot = emptySnapshot();
  let metricsOk = true;
  let decisionsOk = true;
  let metricsBusy = false;
  let decideBusy = false;
  const draw = (): void => paint({ snapshot, now: newestTime(snapshot) ?? 0, mode: "live", metricsOk, decisionsOk });

  const pollMetrics = async (): Promise<void> => {
    if (metricsBusy) return;
    metricsBusy = true;
    try {
      const m = await readMetrics(STAGE_WINDOW_S);
      metricsOk = m.ok;
      // A failed read keeps the last picture (A8 step 5).
      if (m.ok) snapshot = { ...snapshot, players: m.players, warning: m.warning, death: m.death, outs: m.outs };
      draw();
    } finally {
      metricsBusy = false;
    }
  };
  const pollDecisions = async (): Promise<void> => {
    if (decideBusy) return;
    decideBusy = true;
    try {
      const d = await readDecisions();
      decisionsOk = d !== null;
      if (d !== null) snapshot = { ...snapshot, decisions: d };
      draw();
    } finally {
      decideBusy = false;
    }
  };

  doc.getElementById("stage-record")?.addEventListener("click", () => {
    void (async () => {
      const [m, d] = await Promise.all([readMetrics(RECORD_WINDOW_S), readDecisions()]);
      const rec: StageSnapshot = {
        version: 1,
        players: m.players,
        warning: m.warning,
        death: m.death,
        outs: m.outs,
        decisions: d ?? [],
      };
      download(`stage-${new Date().toISOString().replace(/[:.]/g, "-")}.json`, rec, doc);
    })();
  });

  draw();
  void pollMetrics();
  void pollDecisions();
  win.setInterval(() => void pollMetrics(), STAGE_METRICS_POLL_MS);
  win.setInterval(() => void pollDecisions(), STAGE_DECIDE_POLL_MS);
}
