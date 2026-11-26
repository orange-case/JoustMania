// @vitest-environment happy-dom
/**
 * UC-035 (#2291): Controls and GameStatus on a real DOM; the flag panel's A9 rows.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Controls } from "../components/Controls.js";
import { GameStatus } from "../components/GameStatus.js";
import { renderFlagPanel } from "../components/AgentView.js";
import { flagPanelFromState } from "../agent.js";

function mountControls() {
  document.body.innerHTML =
    '<select id="game-mode"></select><button id="start-btn"></button><button id="stop-btn" disabled></button>';
}

describe("UC-035: Controls", () => {
  beforeEach(mountControls);
  afterEach(() => vi.unstubAllGlobals());

  it("UC-035/main step 1 start and stop buttons call their handlers; a rejected handler does not throw", async () => {
    const onStartGame = vi.fn().mockRejectedValue(new Error("down"));
    const onStopGame = vi.fn();
    vi.spyOn(console, "error").mockImplementation(() => {});
    new Controls({ onStartGame, onStopGame });
    (document.getElementById("stop-btn") as HTMLButtonElement).disabled = false;
    document.getElementById("start-btn")!.click();
    document.getElementById("stop-btn")!.click();
    expect(onStartGame).toHaveBeenCalledTimes(1);
    expect(onStopGame).toHaveBeenCalledTimes(1);
    await Promise.resolve();
  });

  it("UC-035/main step 1 setModeChoices fills the select and preselects; the change callback is optional", () => {
    vi.stubGlobal(
      "Option",
      function (text: string, value: string, _d: boolean, selected: boolean) {
        const o = document.createElement("option");
        o.textContent = text;
        o.value = value;
        o.selected = selected;
        return o;
      },
    );
    const controls = new Controls({ onStartGame: vi.fn(), onStopGame: vi.fn() });
    controls.setModeChoices({
      options: [
        { value: "JoustFFA", label: "FFA" },
        { value: "Zombies", label: "Zombies" },
      ],
      selected: "Zombies",
    });
    expect(controls.getSelectedMode()).toBe("Zombies");
    const select = document.getElementById("game-mode") as HTMLSelectElement;
    select.value = "JoustFFA";
    expect(() => select.dispatchEvent(new Event("change"))).not.toThrow();
  });

  it("UC-035/main step 1 setGameRunning disables start and the mode while a game runs", () => {
    const controls = new Controls({ onStartGame: vi.fn(), onStopGame: vi.fn() });
    controls.setGameRunning(true);
    expect((document.getElementById("start-btn") as HTMLButtonElement).disabled).toBe(true);
    expect((document.getElementById("stop-btn") as HTMLButtonElement).disabled).toBe(false);
    expect((document.getElementById("game-mode") as HTMLSelectElement).disabled).toBe(true);
    controls.setGameRunning(false);
    expect((document.getElementById("start-btn") as HTMLButtonElement).disabled).toBe(false);
  });
});

describe("UC-035: GameStatus", () => {
  it("UC-035/main step 1 shows state and counts, and writes event text as text, not HTML", () => {
    document.body.innerHTML =
      '<div id="game-state"></div><div id="player-count"></div><div id="alive-count"></div><div id="event-log"></div>';
    const status = new GameStatus();
    status.updateState("RUNNING");
    status.updatePlayerCount(4, 0);
    status.updateEvents(["<img src=x>", "Game stopped"]);
    expect(document.getElementById("game-state")!.textContent).toBe("Game: RUNNING");
    expect(document.getElementById("alive-count")!.textContent).toBe("Alive: -");
    const items = [...document.querySelectorAll(".event-item")];
    expect(items.map((i) => i.textContent)).toEqual(["<img src=x>", "Game stopped"]);
    expect(document.querySelector("#event-log img")).toBeNull();
  });
});

describe("UC-035: BR-013 flag panel shows the A9 fallback state", () => {
  it("UC-035/BR-013 experiments.enabled and llm.eligible_game_kinds are rows of the panel", () => {
    const on = renderFlagPanel(flagPanelFromState({ game_id: "g1", experiments_enabled: true, llm_eligible_game_kinds: ["real", "shadow"] }));
    expect(on).toContain("experiments enabled");
    expect(on).toContain("real,shadow");
    expect(flagPanelFromState({ game_id: "g1", experiments_enabled: false }).experimentsEnabled).toBe(false);
  });

  it("UC-035/BR-013 a missing field shows dashes, not off", () => {
    const panel = flagPanelFromState({ game_id: "g1" });
    expect(panel.experimentsEnabled).toBeNull();
    expect(panel.eligibleGameKinds).toBe("");
  });
});