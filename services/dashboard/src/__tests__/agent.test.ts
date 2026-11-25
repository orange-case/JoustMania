/**
 * Agent tab data (UC-035 step 3, UC-040 A21, UC-070 BR-005): the decide feed and the
 * flag panel from the agent's GET /experiments/state, rollout rows from Jaeger.
 *
 * Fixtures are in the folds' input shape (the legacy /api/traces JSON); fetch stubs
 * serve them as OTLP answers through ./otlp.ts:
 *   jaeger-flag-state-games   two game traces (a real game, then a newer shadow
 *                             game): agent.signal_received -> invoke_agent +
 *                             agent.decision (one blocked, one allowed)
 *   jaeger-rollout-*          one-span agent.infrastructure.decision traces
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  AgentUnavailableError,
  BLOCKED_UNAVAILABLE,
  DecideStore,
  FlagStateStore,
  ROLLOUT_ACTIONS,
  ROLLOUT_OVERLAP_MS,
  RolloutStore,
  activityFeed,
  blockedRowsFrom,
  fetchAgentState,
  fetchInterventionCounts,
  fetchRollouts,
  flagPanelFromState,
  parseAgentState,
  parseAppliedCounts,
  APPLIED_UNAVAILABLE,
  parseBlockedCounts,
  rolloutRowFrom,
} from "../agent.js";
import type { FlagStateJson, JaegerTracesResponse, VmQueryResponse } from "../agent.js";
import { WRITE_GATE_OFF_HINT, renderFlagPanel } from "../components/AgentView.js";
import { FLAG_STATE_NO_DATA } from "../degrade.js";
import { COLD_START_TIME_MIN, SEARCH_TIME_MAX } from "../jaeger.js";
import { agentStateBody, toOtlp } from "./otlp.js";
import gamesFixture from "./fixtures/jaeger-flag-state-games.json";
import expandFixture from "./fixtures/jaeger-rollout-expand.json";
import rollbackFixture from "./fixtures/jaeger-rollout-rollback.json";

const games = gamesFixture as unknown as JaegerTracesResponse;
const expand = expandFixture as unknown as JaegerTracesResponse;
const rollback = rollbackFixture as unknown as JaegerTracesResponse;

const T0_MS = 1_760_000_000_000;

/** A real game's flag state as the agent sends it, and a newer shadow game's. */
const FLAG_REAL: FlagStateJson = {
  game_id: "game_real000001",
  game_kind: "real",
  game_mode: "JoustFFA",
  trace_id: "0af7651916cd43dd8448eb211c80319c",
  updated_at_ms: T0_MS,
  enabled: true,
  objectives: "engagement=1.00",
  interventions_allowed: "grant_shield,end_game",
  interventions_enabled: true,
  battery_threshold: 20,
  max_interventions_per_minute: 6,
  decision_backend: "claude",
  act_confidence_threshold: 0.7,
  experiments_enabled: true,
  llm_eligible_game_kinds: ["real", "shadow"],
};
const FLAG_SHADOW: FlagStateJson = {
  ...FLAG_REAL,
  game_id: "game_shadow00001",
  game_kind: "shadow",
  updated_at_ms: T0_MS + 60_000,
  enabled: false,
};
const flagStates = () => [FLAG_REAL, FLAG_SHADOW].map(flagPanelFromState);

function vm(result: { metric: Record<string, string>; value: [number, string] }[]): VmQueryResponse {
  return { status: "success", data: { resultType: "vector", result } };
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

/** Stub fetch; returns every requested URL. */
function stubFetch(answer: (url: string) => Response | Promise<Response>): string[] {
  const urls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      urls.push(url);
      return answer(url);
    }),
  );
  return urls;
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("UC-035: main step 3, flag panel from the agent's state", () => {
  it("UC-035/main step 3 folds a game's flag state, booleans and lists included", () => {
    const [real, shadow] = flagStates();
    expect(real).toMatchObject({
      tsMs: T0_MS,
      traceId: FLAG_REAL.trace_id,
      gameKind: "real",
      gameMode: "JoustFFA",
      decisionBackend: "claude",
      actThreshold: 0.7,
      batteryThreshold: "20",
      maxPerMinute: "6",
      enabled: true,
      objectives: "engagement=1.00",
      interventionsAllowed: "grant_shield,end_game",
      interventionsEnabled: true,
      experimentsEnabled: true,
      eligibleGameKinds: "real,shadow",
    });
    expect(shadow.enabled).toBe(false);
  });

  it("UC-035/main step 3 a missing field reads — (null), never off", () => {
    const panel = flagPanelFromState({ game_id: "g" });
    expect(panel).toMatchObject({ enabled: null, interventionsEnabled: null, experimentsEnabled: null, actThreshold: null, eligibleGameKinds: "" });
  });

  it("UC-035/main step 3 the panel shows the kill switch, objectives and allow-list, and links the game trace", () => {
    const html = renderFlagPanel(flagStates()[0]);
    expect(html).toContain("agent enabled (kill switch)");
    expect(html).toContain("interventions allowed");
    expect(html).toContain("objectives");
    expect(html).toContain(`/jaeger/trace/${FLAG_REAL.trace_id}`);
    expect(html).not.toContain("variance window");
    expect(renderFlagPanel({ ...flagStates()[0], traceId: "" })).not.toContain("/jaeger/trace/");
  });

  it("UC-035/main step 3 enabled=on with interventions_enabled=off shows the write-gate hint (#2093)", () => {
    const base = flagStates()[0];
    const off = renderFlagPanel({ ...base, enabled: true, interventionsEnabled: false });
    expect(off).toContain("interventions enabled (write gate)");
    expect(off).toContain(WRITE_GATE_OFF_HINT);
    for (const quiet of [
      { enabled: true, interventionsEnabled: true },
      { enabled: true, interventionsEnabled: null },
      { enabled: false, interventionsEnabled: false },
    ]) {
      expect(renderFlagPanel({ ...base, ...quiet })).not.toContain(WRITE_GATE_OFF_HINT);
    }
  });

  it("UC-035/A3 no flag state yet is a hint, not an error", () => {
    const html = renderFlagPanel(null);
    expect(html).toContain(FLAG_STATE_NO_DATA);
    expect(html).not.toMatch(/Could not reach/);
  });
});

describe("UC-035: main step 3, the agent's state endpoint", () => {
  it("UC-035/main step 3 one GET /experiments/state carries decides, flags and experiments", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2031-01-01T00:00:00Z")); // a browser clock far off the agent's
    const urls = stubFetch(() =>
      json(agentStateBody(games, { flags: [FLAG_REAL], experiments: [{ id: "exp_1", status: "running" }] })),
    );
    const state = await fetchAgentState();
    expect(urls).toEqual(["/experiments/state"]);
    expect(state.flags.map((f) => f.gameId)).toEqual(["game_real000001"]);
    expect(state.experiments.map((e) => e.experimentId)).toEqual(["exp_1"]);
    const store = new DecideStore();
    store.load(state.decides);
    expect(store.size).toBeGreaterThan(0);
  });

  it("UC-035/A3 the agent down (envoy's 503, a transport error, no JSON) is AgentUnavailableError", async () => {
    stubFetch(() => new Response("no healthy upstream", { status: 503 }));
    await expect(fetchAgentState()).rejects.toBeInstanceOf(AgentUnavailableError);
    vi.stubGlobal("fetch", vi.fn(async () => Promise.reject(new TypeError("network"))));
    await expect(fetchAgentState()).rejects.toBeInstanceOf(AgentUnavailableError);
    stubFetch(() => new Response("<html>", { status: 200 }));
    await expect(fetchAgentState()).rejects.toBeInstanceOf(AgentUnavailableError);
  });

  it("UC-035/A3 an empty answer is data: no rows, no flags, no experiments", () => {
    const state = parseAgentState({ decides: { resourceSpans: [] }, flags: [], experiments: [] });
    expect(state).toEqual({ decides: { data: [] }, flags: [], experiments: [] });
    expect(parseAgentState(null)).toEqual({ decides: { data: [] }, flags: [], experiments: [] });
  });
});

describe("UC-070: BR-005 the flag panel follows the stage game", () => {
  it("UC-070/BR-005 a newer shadow game's flag state never replaces the real game's", () => {
    const store = new FlagStateStore();
    store.replace(flagStates());
    expect(store.current()?.gameId).toBe("game_real000001");
    expect(store.current({ realGameLive: true, followGameId: "" })?.gameId).toBe("game_real000001");
  });

  it("UC-070/BR-005 with no real game live the followed shadow game's flags show", () => {
    const store = new FlagStateStore();
    store.replace(flagStates());
    expect(store.current({ realGameLive: false, followGameId: "game_shadow00001" })?.gameId).toBe(
      "game_shadow00001",
    );
  });

  it("UC-070/BR-005 a new real game the agent has not evaluated yet: the hint, never the old game's flags", () => {
    const store = new FlagStateStore();
    store.replace(flagStates());
    const follow = { realGameLive: true, followGameId: "", primaryGameId: "game_real000002" };
    expect(store.current(follow)).toBeNull();
    // Even with shadow_policy=allow, a live real game never shows a shadow game's flags.
    const shadowOnly = new FlagStateStore();
    shadowOnly.replace(flagStates().filter((s) => s.gameKind === "shadow"));
    expect(shadowOnly.current({ realGameLive: true, followGameId: "" })).toBeNull();
    // The next answer carries game 2's flag state.
    store.replace([...flagStates(), flagPanelFromState({ ...FLAG_REAL, game_id: "game_real000002" })]);
    expect(store.current(follow)?.gameId).toBe("game_real000002");
  });
});

describe("UC-035: main step 3, activity feed (blocked decisions + rollout)", () => {
  it("UC-035/main step 3 blocked rows come from the agent.decision spans of the agent's answer", () => {
    const store = new DecideStore();
    store.load(games);
    const rows = blockedRowsFrom(store.feed().rows);
    expect(rows.map((r) => [r.sessionId, r.action, r.reason, r.target])).toEqual([
      ["game_shadow00001", "grant_shield", "not_allowed", "Ana"],
      ["game_real000001", "grant_shield", "rate_limit", "Ana"],
    ]);
    // Allowed decisions stay in the decide feed only.
    expect(rows.every((r) => r.blocked && r.kind === "blocked")).toBe(true);
  });

  it("UC-035/main step 3 rollout rows come from agent.infrastructure.decision by remediation.action", () => {
    const row = rolloutRowFrom(rollback.data![0].spans[0]);
    expect(row).toMatchObject({ kind: "rollout", signal: "rollout rollback (dry run)", action: "bluez", reason: "event_gap_ms>40" });
    const none = { ...expand.data![0].spans[0], tags: [{ key: "remediation.action", value: "none" }] };
    expect(rolloutRowFrom(none)).toBeNull();
  });

  it("UC-035/main step 3 one tag search per action; later searches start at the newest span minus the overlap", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2031-01-01T00:00:00Z"));
    const store = new RolloutStore();
    const urls = stubFetch((url) => {
      const tags = new URL(url, "http://x").searchParams.get("query.attributes") ?? "";
      if (tags.includes('"expand"')) return json(toOtlp(expand));
      if (tags.includes('"rollback"')) return json(toOtlp(rollback));
      return json({ data: [] });
    });

    await fetchRollouts(store);
    const first = urls.map((u) => new URL(u, "http://x").searchParams);
    expect(first.map((p) => JSON.parse(p.get("query.attributes")!)["remediation.action"])).toEqual([...ROLLOUT_ACTIONS]);
    expect(
      first.every(
        (p) =>
          p.get("query.operationName") === "agent.infrastructure.decision" &&
          p.get("query.startTimeMin") === COLD_START_TIME_MIN &&
          p.get("query.startTimeMax") === SEARCH_TIME_MAX,
      ),
    ).toBe(true);

    urls.length = 0;
    await fetchRollouts(store);
    const second = new Map(urls.map((u) => {
      const p = new URL(u, "http://x").searchParams;
      return [JSON.parse(p.get("query.attributes")!)["remediation.action"], p.get("query.startTimeMin")];
    }));
    expect(second.get("expand")).toBe(new Date(T0_MS + 70_000 - ROLLOUT_OVERLAP_MS).toISOString());
    expect(second.get("rollback")).toBe(new Date(T0_MS + 80_000 - ROLLOUT_OVERLAP_MS).toISOString());
    expect(second.get("recommended_only")).toBe(COLD_START_TIME_MIN);
    expect(store.list().map((r) => r.signal)).toEqual(["rollout rollback (dry run)", "rollout expand"]);
  });

  it("UC-035/main step 3 the feed merges both, newest first", () => {
    const decide = new DecideStore();
    decide.load(games);
    const rollouts = new RolloutStore();
    rollouts.merge("expand", expand);
    const feed = activityFeed(blockedRowsFrom(decide.feed().rows), rollouts.list());
    expect(feed.map((r) => r.kind)).toEqual(["rollout", "blocked", "blocked"]);
  });
});
describe("UC-035: main step 3, parseAppliedCounts", () => {
  it("sums result series across namespaced jobs", () => {
    const out = parseAppliedCounts(
      vm([
        { metric: { result: "ok", job: "a/agent" }, value: [1, "3"] },
        { metric: { result: "ok", job: "b/agent" }, value: [1, "2"] },
        { metric: { result: "error", job: "a/agent" }, value: [1, "1"] },
      ]),
    );
    expect(out).toEqual({ appliedOk: 5, appliedError: 1, shadowAppliedOk: 0, shadowAppliedError: 0 });
  });

  it("UC-035/A3 game_kind splits real from shadow; an unlabelled series counts as real", () => {
    const out = parseAppliedCounts(
      vm([
        { metric: { result: "ok", game_kind: "real" }, value: [1, "4"] },
        { metric: { result: "ok" }, value: [1, "1"] },
        { metric: { result: "ok", game_kind: "shadow" }, value: [1, "9"] },
        { metric: { result: "error", game_kind: "shadow" }, value: [1, "2"] },
      ]),
    );
    expect(out).toEqual({ appliedOk: 5, appliedError: 0, shadowAppliedOk: 9, shadowAppliedError: 2 });
  });

  it("UC-035/A3 the applied query groups by game_kind", async () => {
    const urls = stubFetch(() => json(games));
    await fetchInterventionCounts();
    const q = new URL(urls.find((u) => u.includes("agent_intervention_writes_total"))!, "http://x").searchParams.get("query") ?? "";
    expect(q).toContain("game_kind");
  });

  it("UC-035/A3 an empty answer is zero applied, not n/a; a malformed body is n/a", () => {
    expect(parseAppliedCounts(vm([]))).toEqual({ appliedOk: 0, appliedError: 0, shadowAppliedOk: 0, shadowAppliedError: 0 });
    expect(parseAppliedCounts({ status: "error" })).toEqual(APPLIED_UNAVAILABLE);
  });
});

describe("UC-035: A3/#2288 interventions panel windows and fetch results", () => {
  it("UC-035/A3 VM up but empty for both queries: zero applied, zero blocked", async () => {
    stubFetch(() => json(vm([])));
    const overlay = await fetchInterventionCounts();
    expect(overlay.appliedOk).toBe(0);
    expect(overlay.totalBlocked).toBe(0);
  });

  it("UC-035/A3 an HTTP error from VM is unreachable (n/a)", async () => {
    stubFetch(() => json({}, 502));
    const overlay = await fetchInterventionCounts();
    expect(overlay.appliedOk).toBeNull();
    expect(overlay.totalBlocked).toBeNull();
  });

  it("UC-035/A3 applied and blocked use the same 600 s window; blocked leaves out already_applied", async () => {
    const urls = stubFetch(() => json(vm([])));
    await fetchInterventionCounts();
    const queries = urls.map((u) => new URL(u, "http://x").searchParams.get("query") ?? "");
    const applied = queries.find((q) => q.includes("agent_intervention_writes_total")) ?? "";
    const blocked = queries.find((q) => q.includes("agent_decisions_total")) ?? "";
    expect(applied).toContain("increase(");
    expect(applied).toContain("[600s]");
    expect(blocked).toContain("[600s]");
    expect(blocked).toContain('block_reason!="already_applied"');
  });
});

describe("UC-040: A21 blocked tallies from agent_decisions_total", () => {
  it("UC-040/A21 counts repeated rate_limit refusals that emit no agent.decision span", () => {
    const out = parseBlockedCounts(
      vm([
        { metric: { action: "grant_shield", block_reason: "rate_limit" }, value: [1, "6.6"] },
        { metric: { action: "end_game", block_reason: "not_allowed" }, value: [1, "1"] },
        { metric: { action: "x", block_reason: "y" }, value: [1, "NaN"] },
      ]),
    );
    expect(out.totalBlocked).toBe(8);
    expect(out.blockedByAction[0]).toEqual({ action: "grant_shield", blocked: 7 });
  });

  it("UC-040/A21 no samples in the window is zero blocked, not n/a", () => {
    expect(parseBlockedCounts(vm([]))).toEqual({
      blockedByAction: [],
      blockedByReason: [],
      totalBlocked: 0,
      shadowBlocked: 0,
    });
  });

  it("UC-035/A3 shadow-game refusals are tallied apart and stay out of the real breakdowns", () => {
    const out = parseBlockedCounts(
      vm([
        { metric: { action: "grant_shield", block_reason: "rate_limit", game_kind: "real" }, value: [1, "2"] },
        { metric: { action: "grant_shield", block_reason: "rate_limit", game_kind: "shadow" }, value: [1, "30"] },
      ]),
    );
    expect(out.totalBlocked).toBe(2);
    expect(out.shadowBlocked).toBe(30);
    expect(out.blockedByAction).toEqual([{ action: "grant_shield", blocked: 2 }]);
    expect(out.blockedByReason).toEqual([{ reason: "rate_limit", count: 2 }]);
  });

  it("UC-040/A21 asks VictoriaMetrics over 600 s; unreachable shows Blocked as n/a, never a span fold", async () => {
    const urls = stubFetch((url) => {
      if (url.includes("/victoria-metrics/")) throw new TypeError("Failed to fetch");
      return json(games);
    });
    const overlay = await fetchInterventionCounts();
    const query = new URL(urls.find((u) => u.includes("agent_decisions_total"))!, "http://x").searchParams.get("query") ?? "";
    expect(query).toContain('agent_decisions_total{blocked="true",block_reason!="already_applied"}');
    expect(query).toContain("[600s]");
    expect(overlay).toEqual({ ...BLOCKED_UNAVAILABLE, ...APPLIED_UNAVAILABLE });
    expect(overlay.totalBlocked).toBeNull();
    expect(urls.some((u) => u.includes("/jaeger/"))).toBe(false);
  });
});
