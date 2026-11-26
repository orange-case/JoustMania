/**
 * What each panel does when its source does not deliver (UC-035 A3 step 2). Three
 * states, kept apart:
 *
 *   agent down      the agent's state endpoint (decide feed, activity feed, flag
 *                   panel, Experiments tab: AgentUnavailableError) did not answer.
 *                   A panel that has rendered keeps its data and goes stale; one
 *                   that never rendered names the agent.
 *   no data         the agent answered without a flag state or experiment yet: not
 *                   an error. The panel says what it is waiting for.
 *   collector down  the agent receives no telemetry, so its state stops moving:
 *                   rows stop growing, the freshness hint stays "updated".
 *
 * VictoriaMetrics down never fails a poll: Applied and Blocked show `n/a`.
 */
import type { AgentView as AgentViewData, InterventionOverlay } from "./agent.js";


export const EXPERIMENTS_AGENT_DOWN = "Could not reach the agent for its experiments (/experiments/state).";
export const DECIDE_AGENT_DOWN = "Could not reach the agent for its decisions (/experiments/state).";

/** No-data hints (the agent answered, nothing held yet). */
export const FLAG_STATE_NO_DATA =
  "No flag state yet. The effective flags appear once the agent evaluates a game.";
export const EXPERIMENT_PROGRESS_NO_DATA =
  "No experiments yet. They appear once the cohort framework starts one.";

interface StaleCapable {
  rendered: boolean;
  markStale(): void;
  setError(message: string): void;
}

interface DecideCapable {
  decideHasRendered: boolean;
  setDecideError(message: string): void;
}


export function onExperimentsPollFailed(view: StaleCapable): void {
  if (view.rendered) view.markStale();
  else view.setError(EXPERIMENTS_AGENT_DOWN);
}

export function onDecidePollFailed(view: DecideCapable): void {
  if (!view.decideHasRendered) view.setDecideError(DECIDE_AGENT_DOWN);
}

interface AgentPanelsCapable extends StaleCapable {
  render(view: AgentViewData): void;
  renderOverlay(overlay: InterventionOverlay): void;
  setPanelsError(message: string): void;
}

/** What the feed and flag panel miss, null while the agent's state delivers. */
export function agentPanelsDown(agentStateOk: boolean): string | null {
  return agentStateOk ? null : DECIDE_AGENT_DOWN;
}

/**
 * Paint the Agent tab's feed, flag panel and overlay. `down` (agentPanelsDown) is set
 * while the last poll of the agent's state failed: the overlay still renders, the
 * feed and flag panel keep their last data and go stale, or name the agent when they
 * never rendered (UC-035 A3 step 2).
 */
export function paintAgentPanels(
  view: AgentPanelsCapable,
  overlay: InterventionOverlay | null,
  down: string | null,
  panels: () => Pick<AgentViewData, "feed" | "flags">,
): void {
  if (down === null) {
    if (overlay !== null) view.render({ ...panels(), overlay });
    return;
  }
  if (overlay !== null) view.renderOverlay(overlay);
  if (view.rendered) view.markStale();
  else if (overlay !== null) view.setPanelsError(down);
  else view.setError(down);
}
