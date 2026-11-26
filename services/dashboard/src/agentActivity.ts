/** Activity feed: blocked decisions. Split out of agent.ts (#2388). */

import { decisionTargetLabel } from "./agentDecide.js";
import type { DecideRow } from "./agentDecide.js";

/** Max activity-feed rows (newest first). */
const MAX_FEED_ROWS = 80;

/** One row in the activity feed. */
export interface DecisionFeedRow {
  /** Epoch ms of the span (agent clock). */
  tsMs: number;
  /** Drives the row style. */
  kind: "blocked";
  /** Span name the row came from. */
  msg: string;
  /** Short human label. */
  signal: string;
  /** The intervention, "" when unknown. */
  action: string;
  /** Whether this decision was blocked. */
  blocked: boolean;
  /** Block reason. */
  reason: string;
  /** Game id the row belongs to. */
  sessionId: string;
  /** Targeted player (name, else serial), "" when session-scoped. */
  target: string;
  /** trace/span for the deep link. */
  traceId: string;
  spanId: string;
}

/* ======================================================================== */
/* Activity feed: blocked decisions (#2059, D5)                             */
/* ======================================================================== */

/** The blocked agent.decision spans the decide poll already holds, as feed rows. */
export function blockedRowsFrom(rows: DecideRow[]): DecisionFeedRow[] {
  const out: DecisionFeedRow[] = [];
  const seen = new Set<string>();
  for (const r of rows) {
    for (const d of r.decisions) {
      if (!d.blocked) continue;
      const key = `${r.traceId}/${d.spanId}`;
      if (seen.has(key)) continue;
      seen.add(key);
      out.push({
        tsMs: d.tsMs || r.tsMs,
        kind: "blocked",
        msg: "agent.decision",
        signal: "decision blocked",
        action: d.action,
        blocked: true,
        reason: d.blockReason || d.reason,
        sessionId: r.gameId,
        target: decisionTargetLabel(d),
        traceId: r.traceId,
        spanId: d.spanId,
      });
    }
  }
  return out;
}

function feedNewestFirst(a: DecisionFeedRow, b: DecisionFeedRow): number {
  return b.tsMs - a.tsMs || b.spanId.localeCompare(a.spanId);
}

/** The activity feed: blocked decisions, newest first, capped. */
export function activityFeed(blocked: DecisionFeedRow[]): DecisionFeedRow[] {
  return [...blocked].sort(feedNewestFirst).slice(0, MAX_FEED_ROWS);
}
