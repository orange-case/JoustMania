/**
 * UC-035 A8, BR-017 (#2721): the stage view's folds, queries and picture.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  NAMES_QUERY,
  REPLAY_PAUSE_MS,
  accelQuery,
  aliveQuery,
  decisionsFrom,
  emptySnapshot,
  isSnapshot,
  lineFrom,
  namesFrom,
  newestTime,
  outsFrom,
  playersFrom,
  promQuery,
  replayNow,
  sliceSnapshot,
  snapshotRange,
} from "../stage.js";
import type { PromAnswer, StageSnapshot } from "../stage.js";
import { actionIcon, isStageView, renderStage, spreadLabels, valueAt } from "../stageView.js";
import type { JaegerTracesResponse } from "../jaeger.js";

const matrix = (series: { metric: Record<string, string>; values: [number, string][] }[]): PromAnswer => ({
  status: "success",
  data: { resultType: "matrix", result: series },
});

const NAMES: PromAnswer = {
  status: "success",
  data: {
    resultType: "vector",
    result: [
      { metric: { serial: "S1", name: "Jev" }, value: [1, "1"] },
      { metric: { serial: "S2", name: "Ana" }, value: [1, "1"] },
    ],
  },
};

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("UC-035/A8 step 2: the stage view reads the OTel stores (BR-017)", () => {
  it("UC-035/A8 BR-017 queries carry no time: range selectors and subqueries relative to the server's now", async () => {
    const urls: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        urls.push(String(input));
        return new Response(JSON.stringify(NAMES), { status: 200 });
      }),
    );
    await promQuery(accelQuery(60));
    const q = new URL(urls[0], "http://pi").searchParams;
    expect([...q.keys()].sort()).toEqual(["nocache", "query"]);
    expect(q.get("query")).toBe("controller_accel_magnitude[60s]");
    expect(aliveQuery(60)).toContain("[60s:500ms]");
    expect(NAMES_QUERY).toContain('backend="mock"');
  });

  it("UC-035/A8 step 5 an unreadable store is null, never a throw", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("offline");
      }),
    );
    expect(await promQuery("up")).toBeNull();
  });

  it("UC-035/A8 step 2 one series per named controller; mocks (no name) are left out; restarts merge", () => {
    const accel = matrix([
      { metric: { serial: "S1", instance: "a" }, values: [[10, "1.0"], [10.1, "1.2"]] },
      { metric: { serial: "S1", instance: "b" }, values: [[10.2, "1.4"]] },
      { metric: { serial: "S2" }, values: [[10, "0.9"]] },
      { metric: { serial: "MOCK1" }, values: [[10, "2.0"]] },
    ]);
    const players = playersFrom(accel, namesFrom(NAMES));
    expect(players.map((p) => p.name)).toEqual(["Ana", "Jev"]);
    expect(players[1].points.map((p) => p.t)).toEqual([10000, 10100, 10200]);
  });

  it("UC-035/A8 step 3 a player is out where their alive state falls from 1 to 0", () => {
    const alive = matrix([
      { metric: { serial: "S1" }, values: [[10, "1"], [10.5, "1"], [11, "0"], [11.5, "0"]] },
      { metric: { serial: "S2" }, values: [[10, "1"], [10.5, "1"]] },
    ]);
    expect(outsFrom(alive)).toEqual([{ serial: "S1", t: 11000 }]);
    expect(lineFrom(matrix([{ metric: {}, values: [[10, "2.5"]] }]))).toEqual([{ t: 10000, v: 2.5 }]);
  });

  it("UC-035/A8 step 3 only applied decisions of real games, with the decide's rationale", () => {
    const decide = (id: string, kind: string, start: number) => ({
      traceID: "t1",
      spanID: id,
      operationName: "agent.decide",
      startTime: start,
      duration: 100,
      tags: [
        { key: "game.kind", value: kind },
        { key: "decide.backend", value: "jev" },
        { key: "decide.confidence", value: 0.72 },
        { key: "decide.rationale", value: "Jev is closest to the death line" },
      ],
      references: [],
    });
    const decision = (id: string, parent: string, action: string, blocked: boolean, start: number) => ({
      traceID: "t1",
      spanID: id,
      operationName: "agent.decision",
      startTime: start,
      duration: 10,
      tags: [
        { key: "decision.action", value: action },
        { key: "decision.blocked", value: blocked },
        { key: "player.serial", value: "S1" },
        { key: "player.name", value: "Jev" },
      ],
      references: [{ refType: "CHILD_OF", traceID: "t1", spanID: parent }],
    });
    const body: JaegerTracesResponse = {
      data: [
        {
          traceID: "t1",
          spans: [
            decide("d1", "real", 1_000_000),
            decision("x1", "d1", "grant_shield", false, 1_000_100),
            decide("d2", "real", 2_000_000),
            decision("x2", "d2", "eliminate_player", true, 2_000_100),
            decide("d3", "shadow", 3_000_000),
            decision("x3", "d3", "grant_shield", false, 3_000_100),
          ],
        },
      ],
    };
    const out = decisionsFrom(body);
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({
      action: "grant_shield",
      targetSerial: "S1",
      targetName: "Jev",
      rationale: "Jev is closest to the death line",
      backend: "jev",
      confidence: 0.72,
    });
  });
});

const SNAP: StageSnapshot = {
  version: 1,
  players: [
    { serial: "S1", name: "Jev", points: [{ t: 0, v: 1 }, { t: 30_000, v: 2.9 }, { t: 60_000, v: 1.1 }] },
    { serial: "S2", name: "<b>Ana</b>", points: [{ t: 0, v: 1.2 }, { t: 60_000, v: 1.3 }] },
  ],
  warning: [{ t: 0, v: 1.6 }, { t: 60_000, v: 1.9 }],
  death: [{ t: 0, v: 1.8 }, { t: 60_000, v: 2.8 }],
  outs: [{ serial: "S1", t: 30_000 }],
  decisions: [
    { t: 20_000, action: "grant_shield", targetSerial: "S1", targetName: "Jev", rationale: 'keep <script>alert("x")</script> alive', backend: "jev", confidence: 0.72 },
    { t: 40_000, action: "adjust_music_tempo", targetSerial: "", targetName: "", rationale: "", backend: "claude", confidence: null },
  ],
};

describe("UC-035/A8 step 3: the picture", () => {
  it("UC-035/A8 draws a line and a name per player, an out marker and the agent's actions", () => {
    const markup = renderStage({ snapshot: SNAP, now: 60_000, mode: "live", metricsOk: true, decisionsOk: true }).value;
    expect(markup.match(/class="st-line"/g)?.length).toBe(2);
    expect(markup).toContain("✕ Jev");
    expect(markup).toContain('class="st-out"');
    expect(markup).toContain('class="st-act"');
    expect(markup).toContain("st-act-wide");
    expect(markup).toContain("st-death");
    expect(markup).toContain("● live");
  });

  it("UC-035/A8 cards: newest first, at most three, model text and names escaped", () => {
    const markup = renderStage({ snapshot: SNAP, now: 60_000, mode: "replay", metricsOk: true, decisionsOk: false }).value;
    expect(markup.indexOf("adjust music tempo")).toBeLessThan(markup.indexOf("grant shield"));
    expect(markup).not.toContain("<script>");
    expect(markup).not.toContain("<b>Ana</b>");
    expect(markup).toContain("&lt;b&gt;Ana&lt;/b&gt;");
    expect(markup).toContain("everyone");
    expect(markup).toContain("no agent traces");
    expect(markup).toContain("▶ replay");
  });

  it("UC-035/A8 step 5 without players it says why instead of drawing nothing", () => {
    expect(renderStage({ snapshot: emptySnapshot(), now: 0, mode: "live", metricsOk: false, decisionsOk: true }).value).toContain(
      "No metrics from the stack",
    );
  });

  it("helpers: value at a time, label spreading, icons, the view switch", () => {
    expect(valueAt(SNAP.players[0].points, 45_000)).toBe(2.9);
    expect(spreadLabels([100, 105, 300], 26, 0, 1000)).toEqual([100, 126, 300]);
    expect(actionIcon("grant_shield")).toBe("🛡️");
    expect(actionIcon("something_new")).toBe("🤖");
    expect(isStageView("?view=stage&theme=dark")).toBe(true);
    expect(isStageView("?view=agent")).toBe(false);
  });
});

describe("UC-035/A8 step 4: recording and replay", () => {
  it("slices a window and finds the newest sample and the range", () => {
    const s = sliceSnapshot(SNAP, 25_000, 60_000);
    expect(s.players[0].points.map((p) => p.t)).toEqual([30_000, 60_000]);
    expect(s.decisions.map((d) => d.action)).toEqual(["adjust_music_tempo"]);
    expect(newestTime(SNAP)).toBe(60_000);
    expect(snapshotRange(SNAP)).toEqual([0, 60_000]);
  });

  it("replays at real speed from one window in, holds the end, then loops", () => {
    const range: [number, number] = [0, 100_000];
    expect(replayNow(range, 0, 60_000)).toBe(60_000);
    expect(replayNow(range, 10_000, 60_000)).toBe(70_000);
    expect(replayNow(range, 40_000 + REPLAY_PAUSE_MS / 2, 60_000)).toBe(100_000);
    expect(replayNow(range, 40_000 + REPLAY_PAUSE_MS + 5_000, 60_000)).toBe(65_000);
    expect(replayNow([0, 30_000], 5_000, 60_000)).toBe(30_000);
  });

  it("accepts only a version-1 recording", () => {
    expect(isSnapshot(SNAP)).toBe(true);
    expect(isSnapshot({ version: 2 })).toBe(false);
    expect(isSnapshot(null)).toBe(false);
  });
});
