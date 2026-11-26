// @vitest-environment happy-dom
/**
 * UC-035 BR-013 (#2291): the experiment card says what is tested and how it ended.
 */
import { describe, expect, it } from "vitest";
import { ExperimentsView, endedText, normalizeVerdict, testedText } from "../components/ExperimentsView.js";
import { experimentFromState } from "../experiments.js";
import type { ExperimentState, ExperimentView } from "../experiments.js";

const base: ExperimentState = {
  id: "exp_a",
  status: "done",
  flag_key: "death_grace_period_seconds",
  objective: "balanced",
  thesis: "raising grace reduces frustration deaths",
  experimental_value: 0.5,
  trace_id: "t1",
};
describe("UC-035: BR-013 experiment card detail", () => {
  it("UC-035/BR-013 reads flag, objective, thesis and the experimental value from the agent's snapshot", () => {
    const exp = experimentFromState(base);
    expect(exp).toMatchObject({
      flagKey: "death_grace_period_seconds",
      objective: "balanced",
      thesis: "raising grace reduces frustration deaths",
      experimentalValue: "0.5",
    });
    expect(testedText(exp)).toBe("death_grace_period_seconds = 0.5 · objective: balanced");
  });

  it("UC-035/BR-013 an older agent image (no such attributes) leaves the fields empty", () => {
    const exp = experimentFromState({ id: "exp_a", status: "running" });
    expect(testedText(exp)).toBe("");
    expect(exp.thesis).toBe("");
  });

  it("UC-035/BR-013 a done experiment without a verdict outcome reads Done, never Promote", () => {
    expect(normalizeVerdict("done", false, "done").label).toBe("Done");
    expect(normalizeVerdict("promote", false, "done").label).toBe("Promote");
    expect(normalizeVerdict("discard", false, "done").label).toBe("Discard");
    expect(normalizeVerdict("discarded", false, "discarded").label).toBe("Discard");
    expect(normalizeVerdict("inconclusive", false, "done").label).toBe("Inconclusive");
    expect(normalizeVerdict("running", true, "running").label).toBe("Running");
    expect(normalizeVerdict("", false, "aborted").label).toBe("aborted");
  });

  it("UC-035/BR-013 the end reason is shown with the status", () => {
    const exp = experimentFromState({ ...base, status: "aborted", end_reason: "operator stop" });
    expect(endedText(exp)).toBe("aborted: operator stop");
  });

  it("UC-035/BR-013 the card renders the flag, thesis, how it ended and a trace link, escaped", () => {
    document.body.innerHTML = '<div id="grid"></div>';
    const exp: ExperimentView = experimentFromState({ ...base, thesis: "<b>x</b>", status: "aborted", end_reason: "operator stop" });
    new ExperimentsView("grid").render([exp]);
    const card = document.getElementById("grid")!;
    expect(card.querySelector(".experiment-flag")?.textContent).toContain("death_grace_period_seconds = 0.5");
    expect(card.querySelector(".experiment-thesis")?.textContent).toBe("<b>x</b>");
    expect(card.querySelector(".experiment-thesis b")).toBeNull();
    expect(card.querySelector(".experiment-reason")?.textContent).toBe("aborted: operator stop");
    expect(card.querySelector("a.experiment-trace")?.getAttribute("href")).toBe("/jaeger/trace/t1");
  });
});
