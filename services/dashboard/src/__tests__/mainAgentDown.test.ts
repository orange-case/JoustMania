// @vitest-environment happy-dom
/**
 * UC-035 A3 step 2 through main.ts: with the agent's state endpoint down and
 * VictoriaMetrics up, the Agent tab's feed and flag panel name the agent, never the
 * no-data hints, while the overlay still renders.
 */
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it, vi } from "vitest";

vi.mock("../client.js", () => ({
  controllerClient: { watchGameplayData: vi.fn(), identifyController: vi.fn() },
  gameClient: {
    streamGameEvents: vi.fn(),
    getGameState: vi.fn().mockRejectedValue(new Error("down")),
    listGames: vi.fn().mockRejectedValue(new Error("down")),
    forceEndGame: vi.fn(),
  },
  menuClient: {
    listGameModes: vi.fn().mockResolvedValue({ modes: [] }),
    getMenuStatus: vi.fn().mockRejectedValue(new Error("down")),
    processInput: vi.fn(),
  },
}));
vi.mock("../controllerStream.js", async (orig) => ({
  ...(await orig<typeof import("../controllerStream.js")>()),
  keepControllerStreamOpen: vi.fn().mockResolvedValue(undefined),
}));
vi.mock("../stream.js", () => ({ keepGameEventStreamOpen: vi.fn().mockResolvedValue(undefined) }));

const html = readFileSync(join(__dirname, "..", "index.html"), "utf8");

describe("UC-035: A3 the agent's state down on the Agent tab (main.ts)", () => {
  it("UC-035/A3 the feed and flag panel name the agent while VictoriaMetrics answers", async () => {
    document.body.innerHTML = html.slice(html.indexOf("<body"), html.indexOf("</body>")).replace(/<(script|iframe)[\s\S]*?<\/\1>/g, "");
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url.endsWith("/experiments/state")) return new Response("no healthy upstream", { status: 503 });
        if (url.includes("/victoria-metrics/")) {
          return new Response(JSON.stringify({ status: "success", data: { resultType: "vector", result: [] } }), { status: 200 });
        }
        return new Response("", { status: 503 });
      }),
    );
    vi.spyOn(console, "log").mockImplementation(() => {});
    vi.spyOn(console, "warn").mockImplementation(() => {});
    vi.spyOn(console, "error").mockImplementation(() => {});

    await import("../main.js");
    (document.querySelector('.tab-btn[data-tab="agent"]') as HTMLElement).click();

    await vi.waitFor(() => {
      expect(document.getElementById("agent-overlay")!.innerHTML).toContain("agent-stat-applied");
      expect(document.getElementById("agent-flags")!.innerHTML).toContain("Could not reach the agent");
    });
    // A later counts poll must not paint the no-data hints over the agent-down message.
    await new Promise((r) => setTimeout(r, 50));
    expect(document.getElementById("agent-flags")!.innerHTML).toContain("Could not reach the agent");
    expect(document.getElementById("agent-flags")!.innerHTML).not.toContain("No flag state yet");
    vi.unstubAllGlobals();
  });
});
