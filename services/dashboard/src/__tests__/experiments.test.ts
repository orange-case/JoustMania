/**
 * Experiments tab from the agent's progress snapshots (UC-035 step 4), as
 * GET /experiments/state sends them:
 *   exp_alpha   running, an interim `inconclusive` verdict
 *   exp_beta    discarded (terminal) with verdict discard and an end reason
 *   exp_gamma   aborted (terminal) without a verdict
 *   exp_delta   done (terminal) with verdict promote
 *   exp_epsilon aborted (terminal) whose snapshot still carries the interim
 *               `inconclusive` verdict (journal KindInterimVerdict)
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { ExperimentStore, experimentFromState, resolveVerdict } from "../experiments.js";
import type { ExperimentState } from "../experiments.js";
import { ExperimentsView } from "../components/ExperimentsView.js";

const T0_MS = 1_760_000_000_000;

const arms = (exp: [number, number, number], ctl: [number, number, number]) => [
  { arm: "control", samples: ctl[0], mean_fitness: ctl[1], in_flight: ctl[2] },
  { arm: "experimental", samples: exp[0], mean_fitness: exp[1], in_flight: exp[2] },
];

const STATE: ExperimentState[] = [
  {
    id: "exp_alpha",
    status: "running",
    flag_key: "death_grace_period_seconds",
    verdict: { outcome: "inconclusive", delta: 0.1, significant: false },
    arms: arms([3, 0.62, 1], [2, 0.5, 2]),
    updated_at_ms: T0_MS + 30_000,
    trace_id: "aaaa",
  },
  {
    id: "exp_beta",
    status: "discarded",
    verdict: { outcome: "discard" },
    end_reason: "paired: d_z=-0.81 (pairs=6)",
    arms: arms([6, 0.4, 0], [6, 0.6, 0]),
    updated_at_ms: T0_MS + 40_000,
  },
  { id: "exp_gamma", status: "aborted", end_reason: "kill_switch", arms: arms([0, 0, 0], [1, 0.5, 0]), updated_at_ms: T0_MS + 50_000 },
  { id: "exp_delta", status: "done", verdict: { outcome: "promote" }, arms: [], updated_at_ms: T0_MS + 10_000 },
  {
    id: "exp_epsilon",
    status: "aborted",
    verdict: { outcome: "inconclusive" },
    end_reason: "capacity_reclaim",
    arms: [],
    updated_at_ms: T0_MS + 5_000,
  },
];

function stored(maxTerminal?: number): ExperimentStore {
  const store = new ExperimentStore(maxTerminal);
  store.replace(STATE.map(experimentFromState));
  return store;
}

function byId(store: ExperimentStore, id: string) {
  return store.list().find((e) => e.experimentId === id);
}

afterEach(() => vi.unstubAllGlobals());

describe("UC-035: main step 4, the agent's progress snapshot per experiment", () => {
  it("UC-035/main step 4 folds a snapshot: arms experimental first, time, trace", () => {
    const alpha = byId(stored(), "exp_alpha");
    expect(alpha?.lastUpdatedMs).toBe(T0_MS + 30_000);
    expect(alpha?.traceId).toBe("aaaa");
    expect(alpha?.arms).toEqual([
      { arm: "experimental", sampleCount: 3, meanFitness: 0.62, inFlight: 1 },
      { arm: "control", sampleCount: 2, meanFitness: 0.5, inFlight: 2 },
    ]);
  });

  it("UC-035/main step 4 Δ is the experimental mean minus the control mean, only with both arms sampled", () => {
    const store = stored();
    expect(byId(store, "exp_alpha")?.delta).toBeCloseTo(0.12, 10);
    expect(byId(store, "exp_gamma")?.delta).toBeNull();
  });

  it("UC-035/main step 4 the verdict is the outcome once finished, else the status, else running", () => {
    const store = stored();
    expect(byId(store, "exp_alpha")).toMatchObject({ verdict: "running", live: true });
    expect(byId(store, "exp_beta")).toMatchObject({ verdict: "discard", live: false, reason: "paired: d_z=-0.81 (pairs=6)" });
    expect(byId(store, "exp_gamma")).toMatchObject({ verdict: "aborted", live: false, reason: "kill_switch" });
    expect(byId(store, "exp_delta")).toMatchObject({ verdict: "promote", live: false });
    expect(resolveVerdict("promote", "concluded")).toBe("promote");
    expect(resolveVerdict("discard", "discarded")).toBe("discard");
    expect(resolveVerdict("", "promoting")).toBe("promoting");
    expect(resolveVerdict("", "")).toBe("running");
  });

  it("UC-035/main step 4 an aborted experiment shows aborted, its interim verdict only as a note", () => {
    expect(byId(stored(), "exp_epsilon")).toMatchObject({
      verdict: "aborted (interim: inconclusive)",
      live: false,
      reason: "capacity_reclaim",
    });
    expect(resolveVerdict("inconclusive", "aborted")).toBe("aborted (interim: inconclusive)");
    expect(resolveVerdict("", "aborted")).toBe("aborted");
  });

  it("UC-035/main step 4 the experimental value is shown as JSON; none is empty", () => {
    expect(experimentFromState({ id: "x", experimental_value: 0.5 }).experimentalValue).toBe("0.5");
    expect(experimentFromState({ id: "x", experimental_value: { a: "b" } }).experimentalValue).toBe('{"a":"b"}');
    expect(experimentFromState({ id: "x" }).experimentalValue).toBe("");
  });

  it("UC-035/main step 4 keeps every live experiment and only the newest N finished ones, newest first", () => {
    expect(stored(2).list().map((e) => e.experimentId)).toEqual(["exp_gamma", "exp_beta", "exp_alpha"]);
  });

  it("UC-035/main step 4 each answer replaces the list", () => {
    const store = stored();
    store.replace([]);
    expect(store.size).toBe(0);
  });
});

describe("UC-035: main step 4, recommendation of a refused promotion (#2276)", () => {
  const refused: ExperimentState = {
    id: "exp_rec",
    status: "discarded",
    flag_key: "death_grace_period_seconds",
    recommendation: { flag: "death_grace_period_seconds", value: "0.5", owner: "operator" },
    updated_at_ms: T0_MS,
  };

  it("UC-035/main step 4 the snapshot's recommendation is folded; none without an owner", () => {
    expect(experimentFromState(refused).recommendation).toEqual({ flag: "death_grace_period_seconds", value: "0.5", owner: "operator" });
    expect(experimentFromState({ ...refused, recommendation: null }).recommendation).toBeNull();
  });

  it("UC-035/main step 4 the card says would promote <flag>=<value>; real value owned by <writer>", () => {
    const grid = { innerHTML: "", hidden: false, textContent: "", classList: { toggle: () => {} } };
    vi.stubGlobal("document", { getElementById: () => grid });
    new ExperimentsView("grid").render([experimentFromState(refused)]);
    expect(grid.innerHTML).toContain("would promote death_grace_period_seconds=0.5; real value owned by operator");
    grid.innerHTML = "";
    new ExperimentsView("grid").render([experimentFromState({ ...refused, recommendation: null })]);
    expect(grid.innerHTML).not.toContain("would promote");
  });
});
