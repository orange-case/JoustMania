/**
 * Experiments data source (issue #981): the agent's progress snapshot per
 * experiment from GET /experiments/state (agentState.ts, UC-035 step 4).
 *
 * The agent records a snapshot after every state change (started, game assigned /
 * concluded / released, concluded, promoting, terminal, rehydrate) and holds every live experiment plus the newest finished
 * ones: status, flag, verdict once there is one, the end reason, and per arm the
 * games in flight, the samples and mean fitness of the journal summary and the
 * report-only prediction calibration (UC-043 A21).
 */

/** Finished experiments kept on screen (the newest ones; maintainer decision D6). */
export const MAX_TERMINAL_EXPERIMENTS = 10;

/** Terminal registry statuses (services/agent/experiment/registry.go IsTerminal). */
const TERMINAL_STATUSES: ReadonlySet<string> = new Set(["done", "discarded", "aborted"]);

/** Statuses that still accrue games: the verdict is not final yet. */
const LIVE_STATUSES: ReadonlySet<string> = new Set(["proposed", "running", ""]);

/**
 * Statuses whose verdict outcome is the final verdict. An aborted experiment's
 * snapshot still carries the journal's INTERIM verdict (journal.go
 * KindInterimVerdict), which is not its outcome.
 */
const VERDICT_STATUSES: ReadonlySet<string> = new Set(["concluded", "promoting", "done", "discarded"]);

/** Lifecycle rank of a status: the tiebreak between two snapshots of the same time. */
function statusRank(status: string): number {
  if (TERMINAL_STATUSES.has(status)) return 3;
  if (status === "promoting") return 2;
  if (status === "concluded") return 1;
  return 0;
}

/** One arm as GET /experiments/state sends it (experiment.ProgressArm). */
export interface ArmState {
  arm?: string;
  in_flight?: number;
  samples?: number;
  mean_fitness?: number;
}

/** One experiment as GET /experiments/state sends it (experiment.ProgressSnapshot). */
export interface ExperimentState {
  id?: string;
  status?: string;
  flag_key?: string;
  objective?: string;
  thesis?: string;
  experimental_value?: unknown;
  verdict?: { outcome?: string; delta?: number; significant?: boolean } | null;
  end_reason?: string;
  recommendation?: { flag?: string; value?: string; owner?: string } | null;
  arms?: ArmState[] | null;
  updated_at_ms?: number;
  trace_id?: string;
}

/** Per-arm values of the snapshot. */
export interface ArmAggregate {
  arm: string;
  /** Games the journal folded for this arm. */
  sampleCount: number;
  /** Mean fitness across the folded games (0 when none). */
  meanFitness: number;
  /** Games bound to the arm and still in flight (point in time). */
  inFlight: number;
}

/**
 * The winning value of an experiment whose promotion was refused because another
 * writer owns the real overlay (UC-043 A19, #2276).
 */
export interface Recommendation {
  flag: string;
  /** JSON encoding of the value (as the agent wrote it). */
  value: string;
  owner: string;
}

/** One experiment, from its newest progress snapshot. */
export interface ExperimentView {
  experimentId: string;
  status: string;
  /** The game flag under test. */
  flagKey: string;
  /** The fitness objective being optimized. */
  objective: string;
  /** Why the change should help (#2291). */
  thesis: string;
  /** The experimental arm's flag value as JSON (#2291); "" when unset. */
  experimentalValue: string;
  arms: ArmAggregate[];
  /** experimental mean minus control mean when both arms have samples, else null. */
  delta: number | null;
  /** The verdict label source: see resolveVerdict. */
  verdict: string;
  /** The teardown reason, once the experiment ended. */
  reason: string;
  /** Set when a promotion was refused for a foreign real-overlay owner, else null. */
  recommendation: Recommendation | null;
  /** Snapshot time (agent clock). */
  lastUpdatedMs: number;
  /** True while the experiment is proposed or running. */
  live: boolean;
  /** The agent.experiment trace (the Jaeger drill-down), "" without one. */
  traceId: string;
}

/** The two canonical arm names (mirror journal.ArmExperimental / ArmControl). */
const ARM_EXPERIMENTAL = "experimental";
const ARM_CONTROL = "control";

function armRank(arm: string): number {
  if (arm === ARM_EXPERIMENTAL) return 0;
  if (arm === ARM_CONTROL) return 1;
  return 2;
}

/**
 * The rendered verdict: "running" while the experiment is proposed or running;
 * the statistical verdict once it concluded (concluded, promoting, done,
 * discarded), else its status. Any other status (aborted) shows the status: its
 * outcome is only the interim verdict, shown as "(interim: <outcome>)".
 */
export function resolveVerdict(outcome: string, status: string): string {
  if (LIVE_STATUSES.has(status)) return "running";
  if (VERDICT_STATUSES.has(status)) return outcome || status;
  return outcome ? `${status} (interim: ${outcome})` : status;
}

function num(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

/** Fold one experiment of the answer. */
export function experimentFromState(s: ExperimentState): ExperimentView {
  const status = s.status ?? "";
  const arms: ArmAggregate[] = (s.arms ?? [])
    .map((a) => ({
      arm: a.arm ?? "",
      sampleCount: num(a.samples),
      meanFitness: num(a.mean_fitness),
      inFlight: num(a.in_flight),
    }))
    .filter((a) => a.arm !== "")
    .sort((x, y) => armRank(x.arm) - armRank(y.arm) || x.arm.localeCompare(y.arm));

  // Δ from the arm means, not the verdict's delta (the paired delta).
  const experimental = arms.find((a) => a.arm === ARM_EXPERIMENTAL);
  const control = arms.find((a) => a.arm === ARM_CONTROL);
  const delta =
    experimental && control && experimental.sampleCount > 0 && control.sampleCount > 0
      ? experimental.meanFitness - control.meanFitness
      : null;

  const rec = s.recommendation;
  const recommendation: Recommendation | null =
    rec && rec.owner ? { flag: rec.flag || s.flag_key || "", value: rec.value ?? "", owner: rec.owner } : null;

  return {
    experimentId: s.id ?? "",
    status,
    flagKey: s.flag_key ?? "",
    objective: s.objective ?? "",
    thesis: s.thesis ?? "",
    experimentalValue:
      s.experimental_value === undefined || s.experimental_value === null ? "" : JSON.stringify(s.experimental_value),
    arms,
    delta,
    verdict: resolveVerdict(s.verdict?.outcome ?? "", status),
    reason: s.end_reason ?? "",
    recommendation,
    lastUpdatedMs: num(s.updated_at_ms),
    live: LIVE_STATUSES.has(status),
    traceId: s.trace_id ?? "",
  };
}

/** Newest first: snapshot time, then the later lifecycle status, then id. */
function newestFirst(a: ExperimentView, b: ExperimentView): number {
  return (
    b.lastUpdatedMs - a.lastUpdatedMs ||
    statusRank(b.status) - statusRank(a.status) ||
    b.experimentId.localeCompare(a.experimentId)
  );
}

/**
 * The experiments of the last answer: every live one, finished ones only the
 * newest `maxTerminal` (the agent bounds them too).
 */
export class ExperimentStore {
  private list_: ExperimentView[] = [];

  constructor(private readonly maxTerminal = MAX_TERMINAL_EXPERIMENTS) {}

  replace(experiments: ExperimentView[]): void {
    const sorted = [...experiments].sort(newestFirst);
    let terminal = 0;
    this.list_ = sorted.filter((e) => !TERMINAL_STATUSES.has(e.status) || terminal++ < this.maxTerminal);
  }

  get size(): number {
    return this.list_.length;
  }

  list(): ExperimentView[] {
    return [...this.list_];
  }
}
