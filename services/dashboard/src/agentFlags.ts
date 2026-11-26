/** Flag panel: each game's effective flag set from the agent's state endpoint, and its store. */

import { isShadowGame, pickAnchor } from "./agentAnchor.js";
import type { DecideFollow } from "./agentAnchor.js";

/** One game's flag state as GET /experiments/state sends it (decision.FlagState). */
export interface FlagStateJson {
  game_id?: string;
  game_kind?: string;
  game_mode?: string;
  trace_id?: string;
  updated_at_ms?: number;
  enabled?: string;
  objectives?: string;
  interventions_allowed?: string;
  battery_threshold?: number;
  max_interventions_per_minute?: number;
  decision_backend?: string;
  act_confidence_threshold?: number;
  experiments_enabled?: boolean;
  llm_eligible_game_kinds?: string[] | null;
}

/** The effective flag set of one game, as its newest evaluated cycle resolved it. */
export interface FlagPanel {
  /** The cycle's time (agent clock). */
  tsMs: number;
  /** The game's trace, "" before its trace ids arrived. */
  traceId: string;
  gameId: string;
  gameKind: string;
  gameMode: string;
  /** decision.backend flag value. */
  decisionBackend: string;
  actThreshold: number | null;
  batteryThreshold: string;
  maxPerMinute: string;
  /** agent/enabled: off (stopped), watch (decides, writes nothing) or act (applies decisions); null when absent or unknown. */
  enabled: AgentEnabled | null;
  /** Objective weights summary ("k=v,..."). */
  objectives: string;
  /** interventions.allowed summary. */
  interventionsAllowed: string;
  /** experiments_enabled (#2291): the stage fallback (UC-070 A9) turns it on; null when absent. */
  experimentsEnabled: boolean | null;
  /** llm.eligible_game_kinds (#2291), comma joined. */
  eligibleGameKinds: string;
}

/** The agent/enabled states. */
export type AgentEnabled = "off" | "watch" | "act";

function agentEnabledOrNull(v: unknown): AgentEnabled | null {
  return v === "off" || v === "watch" || v === "act" ? v : null;
}

function boolOrNull(v: unknown): boolean | null {
  return typeof v === "boolean" ? v : null;
}

function numberText(v: unknown): string {
  return typeof v === "number" && Number.isFinite(v) ? String(v) : "";
}

/** Fold one flag state of the answer. */
export function flagPanelFromState(s: FlagStateJson): FlagPanel {
  return {
    tsMs: typeof s.updated_at_ms === "number" ? s.updated_at_ms : 0,
    traceId: s.trace_id ?? "",
    gameId: s.game_id ?? "",
    gameKind: s.game_kind ?? "",
    gameMode: s.game_mode ?? "",
    decisionBackend: s.decision_backend ?? "",
    actThreshold: typeof s.act_confidence_threshold === "number" ? s.act_confidence_threshold : null,
    batteryThreshold: numberText(s.battery_threshold),
    maxPerMinute: numberText(s.max_interventions_per_minute),
    enabled: agentEnabledOrNull(s.enabled),
    objectives: s.objectives ?? "",
    interventionsAllowed: s.interventions_allowed ?? "",
    experimentsEnabled: boolOrNull(s.experiments_enabled),
    eligibleGameKinds: (s.llm_eligible_game_kinds ?? []).join(","),
  };
}

/** Newest first by cycle time, then game id. */
function flagNewestFirst(a: FlagPanel, b: FlagPanel): number {
  return b.tsMs - a.tsMs || b.gameId.localeCompare(a.gameId);
}

/**
 * The flag states of the last answer. The panel shows the followed game
 * (UC-070 BR-005) chosen by the same anchor rule as the current-backend strip
 * (`pickAnchor`), so a shadow game's flag state never replaces the stage game's.
 */
export class FlagStateStore {
  private byGame = new Map<string, FlagPanel>();

  /** Replace the held states with an answer's (the agent sends every game it holds). */
  replace(states: FlagPanel[]): void {
    this.byGame = new Map(states.map((s) => [s.gameId, s]));
  }

  get size(): number {
    return this.byGame.size;
  }

  /**
   * The followed game's flag state (null before any). While a real game is live
   * it is that game's (`primaryGameId`) or null (the hint until the agent has
   * evaluated it), never an older game's or a shadow game's.
   */
  current(follow?: DecideFollow): FlagPanel | null {
    if (follow?.realGameLive === true) {
      if (follow.primaryGameId) return this.byGame.get(follow.primaryGameId) ?? null;
      return this.sorted().find((s) => !isShadowGame(s.gameKind)) ?? null;
    }
    return pickAnchor(this.sorted(), follow) ?? null;
  }

  private sorted(): FlagPanel[] {
    return Array.from(this.byGame.values()).sort(flagNewestFirst);
  }
}
