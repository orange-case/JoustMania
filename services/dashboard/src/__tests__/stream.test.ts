/**
 * Dashboard stream loss and telemetry-backend loss (UC-035 A4, A3; #1706, #1725).
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { keepGameEventStreamOpen, keepStreamOpen } from "../stream.js";
import { CONTROLLER_STREAM_LOST, keepControllerStreamOpen } from "../controllerStream.js";
import { ShieldStore, type GameEventLike } from "../shield.js";
import { AgentView } from "../components/AgentView.js";
import type { InterventionOverlay } from "../agent.js";
import {
  agentPanelsDown,
  onDecidePollFailed,
  onExperimentsPollFailed,
  paintAgentPanels,
} from "../degrade.js";
import { ExperimentsView } from "../components/ExperimentsView.js";

/** A stream that yields the events, then ends cleanly or throws. */
async function* stream<T = string>(events: T[], failWith?: Error): AsyncGenerator<T> {
  for (const e of events) yield e;
  if (failWith) throw failWith;
}

describe("UC-035: A4 the dashboard loses a live stream", () => {
  it("UC-035/A4 reopens a game event stream that ended cleanly, so the next game shows", async () => {
    const seen: string[] = [];
    const delays: number[] = [];
    const queue = [["game1-start", "game1-end"], ["game2-start"]];
    const open = vi.fn(() => stream(queue.shift() ?? []));
    let reopen: (() => void) | undefined;

    await keepStreamOpen({
      open,
      onEvent: (e) => seen.push(e),
      schedule: (fn, ms) => {
        reopen = fn;
        delays.push(ms);
      },
    });
    expect(open).toHaveBeenCalledTimes(1);
    expect(delays).toEqual([2000]);

    reopen?.();
    await vi.waitFor(() => expect(open).toHaveBeenCalledTimes(2));
    await vi.waitFor(() => expect(seen).toContain("game2-start"));
    expect(seen).toEqual(["game1-start", "game1-end", "game2-start"]);
  });

  it("UC-035/A4 reopens a failed stream every 2 s and reports the loss", async () => {
    const lost: unknown[] = [];
    const delays: number[] = [];
    await keepStreamOpen({
      open: () => stream(["a"], new Error("proxy restarted")),
      onEvent: () => {},
      onLost: (e) => lost.push(e),
      schedule: (_fn, ms) => void delays.push(ms),
    });
    expect(delays).toEqual([2000]);
    expect((lost[0] as Error).message).toBe("proxy restarted");
  });

  it("UC-035/A4 the controller grid shows 'Connection lost. Retrying...' and reopens the gameplay stream after 2 s", async () => {
    const errors: string[] = [];
    const streaming: boolean[] = [];
    const delays: number[] = [];
    const updates: string[] = [];
    const queue: Array<() => AsyncGenerator<string>> = [
      () => stream(["frame1"], new Error("controller manager restarted")),
      () => stream(["frame2"]),
    ];
    const open = vi.fn(() => queue.shift()!());
    let reopen: (() => void) | undefined;
    vi.spyOn(console, "error").mockImplementation(() => {});

    await keepControllerStreamOpen({
      open,
      onUpdate: (u) => updates.push(u),
      grid: { setLoading: () => {}, setError: (m) => errors.push(m) },
      setStreaming: (s) => streaming.push(s),
      schedule: (fn, ms) => {
        reopen = fn;
        delays.push(ms);
      },
    });
    expect(errors).toEqual([CONTROLLER_STREAM_LOST]);
    expect(CONTROLLER_STREAM_LOST).toBe("Connection lost. Retrying...");
    expect(delays).toEqual([2000]);
    expect(streaming).toEqual([true, false]);

    // The retry reopens the stream; its clean end is a loss as well.
    reopen?.();
    await vi.waitFor(() => expect(open).toHaveBeenCalledTimes(2));
    await vi.waitFor(() => expect(delays).toEqual([2000, 2000]));
    expect(updates).toEqual(["frame1", "frame2"]);
    expect(errors).toEqual([CONTROLLER_STREAM_LOST, CONTROLLER_STREAM_LOST]);
    expect(streaming).toEqual([true, false, true, false]);
    vi.restoreAllMocks();
  });

  it("UC-035/A4 the game event stream's loss clears the shield badges (UC-040 A32 step 3)", async () => {
    const shields = new ShieldStore();
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    await keepGameEventStreamOpen(
      () => stream<GameEventLike>([{ eventType: "player_shield_charged", data: { serial: "MOCK0001", expires_ms: "0" } }]),
      (e) => shields.apply(e),
      () => {},
      () => shields.clear(),
    );
    warn.mockRestore();
    expect(shields.get("MOCK0001", Date.now())).toBeNull(); // folded, then cleared by the loss hook
  });

  it("UC-035/A4 a clean end is reported as a loss without an error", async () => {
    const lost: unknown[] = [];
    await keepStreamOpen({
      open: () => stream([]),
      onEvent: () => {},
      onLost: (e) => lost.push(e),
      schedule: () => {},
    });
    expect(lost).toEqual([null]);
  });
});

describe("UC-035: A3 a telemetry backend is down (dashboard side)", () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  function fakeEl() {
    const classes = new Set<string>();
    return {
      innerHTML: "",
      textContent: "",
      hidden: true,
      classList: {
        toggle: (c: string, on: boolean) => void (on ? classes.add(c) : classes.delete(c)),
        has: (c: string) => classes.has(c),
      },
    };
  }

  it("UC-035/A3 a rendered panel keeps its data and is marked stale", () => {
    vi.useFakeTimers();
    const grid = fakeEl();
    const fresh = fakeEl();
    vi.stubGlobal("document", {
      getElementById: (id: string) => (id === "grid" ? grid : fresh),
    });
    const view = new ExperimentsView("grid", "fresh");
    view.render([]);
    const painted = grid.innerHTML;

    vi.advanceTimersByTime(20_000);
    view.markStale(); // a poll failed; keep what is on screen

    expect(grid.innerHTML).toBe(painted);
    expect(fresh.textContent).toBe("stale — last updated 20s ago");
    expect(fresh.classList.has("stale")).toBe(true);
  });

  it("UC-035/A3 a panel that never rendered shows no freshness hint", () => {
    const fresh = fakeEl();
    vi.stubGlobal("document", { getElementById: () => fresh });
    const view = new ExperimentsView("grid", "fresh");
    view.markStale();
    expect(view.rendered).toBe(false);
    expect(fresh.hidden).toBe(true);
  });

  /** Every element id resolves to its own fake element. */
  function stubDom() {
    const els = new Map<string, ReturnType<typeof fakeEl>>();
    vi.stubGlobal("document", {
      getElementById: (id: string) => {
        if (!els.has(id)) els.set(id, fakeEl());
        return els.get(id);
      },
    });
    return (id: string) => els.get(id)!;
  }

  const agentView = () => new AgentView("feed", "flags", "overlay", "fresh", { strip: "strip", feed: "dfeed" });
  const emptyView = {
    feed: [],
    flags: null,
    overlay: { blockedByAction: [], blockedByReason: [], totalBlocked: null, shadowBlocked: null, appliedOk: null, appliedError: null, shadowAppliedOk: null, shadowAppliedError: null },
  };

  it("UC-035/A3 an experiments panel that never rendered says the agent is unreachable", () => {
    const el = stubDom();
    onExperimentsPollFailed(new ExperimentsView("grid", "fresh"));
    expect(el("grid").innerHTML).toContain("Could not reach the agent for its experiments (/experiments/state).");
  });

  it("UC-035/A3 an agent with nothing recorded yet shows the no-data hints, not an error", () => {
    const el = stubDom();
    agentView().render(emptyView as never);
    new ExperimentsView("grid", "fresh").render([]);
    expect(el("flags").innerHTML).toContain("No flag state yet.");
    expect(el("grid").innerHTML).toContain("No experiments yet.");
    expect(el("flags").innerHTML + el("grid").innerHTML).not.toContain("Could not reach");
  });

  it("UC-035/A3 a decide strip that never rendered says the agent is unreachable", () => {
    const el = stubDom();
    const view = agentView();
    onDecidePollFailed(view);
    expect(el("dfeed").innerHTML).toContain("Could not reach the agent for its decisions (/experiments/state).");
    expect(view.decideHasRendered).toBe(true);
  });

  it("UC-035/A3 a decide strip that rendered keeps its rows when the agent's state fails", () => {
    const el = stubDom();
    const view = agentView();
    view.renderDecide({ current: null, rows: [] } as never);
    const painted = el("dfeed").innerHTML;
    onDecidePollFailed(view);
    expect(el("dfeed").innerHTML).toBe(painted);
  });

  it("UC-035/A3 VictoriaMetrics down shows the Applied and Blocked counts as n/a", () => {
    const el = stubDom();
    agentView().render(emptyView as never);
    const html = el("overlay").innerHTML;
    expect(html.match(/title="VictoriaMetrics unreachable">n\/a<\/span>/g)?.length).toBe(4);
  });

  const counts: InterventionOverlay = {
    blockedByAction: [{ action: "grant_shield", blocked: 3 }],
    blockedByReason: [],
    totalBlocked: 3,
    shadowBlocked: 0,
    appliedOk: 7,
    appliedError: 0,
    shadowAppliedOk: 0,
    shadowAppliedError: 0,
  };

  it("UC-035/A3 real counts lead and shadow counts sit on their own labelled line (#2294)", () => {
    const el = stubDom();
    agentView().renderOverlay({ ...counts, shadowBlocked: 12, shadowAppliedOk: 40, shadowAppliedError: 2 });
    const html = el("overlay").innerHTML;
    const real = html.indexOf('agent-stat-applied">7<');
    const shadow = html.indexOf("shadow applied");
    expect(real).toBeGreaterThan(-1);
    expect(shadow).toBeGreaterThan(real);
    expect(html).toContain('agent-stat-shadow">40<');
    expect(html).toContain("(2 failed)");
    expect(html).toContain('agent-stat-shadow">12<');
    expect(html).toContain('agent-stat-blocked">3<');
  });

  it("UC-035/A3 the agent's state poll fails after a good render: feed and flags go stale, the overlay updates", async () => {
    vi.useFakeTimers();
    const el = stubDom();
    const view = agentView();
    paintAgentPanels(view, emptyView.overlay, agentPanelsDown(true), () => ({ feed: [], flags: null }));
    const flags = el("flags").innerHTML;
    vi.advanceTimersByTime(20_000);
    // main.ts: the state poll's onError sets agentStateOk=false and repaints.
    paintAgentPanels(view, counts, agentPanelsDown(false), () => {
      throw new Error("a failed agent poll renders no panel data");
    });
    expect(el("flags").innerHTML).toBe(flags);
    expect(el("overlay").innerHTML).toContain("agent-stat-applied");
    expect(el("fresh").textContent).toBe("stale — last updated 20s ago");
  });

  it("UC-035/A3 nothing rendered and no counts yet: every agent panel names the agent", () => {
    const el = stubDom();
    paintAgentPanels(agentView(), null, agentPanelsDown(false), () => {
      throw new Error("a failed agent poll renders no panel data");
    });
    for (const panel of ["feed", "flags", "overlay"]) {
      expect(el(panel).innerHTML).toContain("Could not reach the agent for its decisions");
    }
  });
  it("UC-035/A3 the agent's state never delivered: the panels name the agent, not Jaeger", () => {
    const el = stubDom();
    paintAgentPanels(agentView(), counts, agentPanelsDown(false), () => ({ feed: [], flags: null }));
    expect(el("flags").innerHTML).toContain("Could not reach the agent for its decisions");
    expect(el("flags").innerHTML).not.toContain("Jaeger");
    expect(agentPanelsDown(true)).toBeNull();
  });
});
