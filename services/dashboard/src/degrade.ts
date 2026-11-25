/**
 * What each panel does when its source does not deliver (UC-035 A3 step 2). Three
 * states, kept apart:
 *
 *   source down     the agent's state endpoint (decide feed, flag panel,
 *                   Experiments tab: AgentUnavailableError) or Jaeger (rollout
 *                   rows: JaegerUnavailableError) did not answer. A panel that has
 *                   rendered keeps its data and goes stale; one that never
 *                   rendered names the source.
 *   no data         the agent answered without a flag state or experiment yet: not
 *                   an error. The panel says what it is waiting for.
 *   collector down  the agent receives no telemetry, so its state stops moving:
 *                   rows stop growing, the freshness hint stays "updated".
 *
 * VictoriaMetrics down never fails a poll: Applied and Blocked show `n/a`.
 */
import type { AgentView as AgentViewData, InterventionOverlay } from "./agent.js";

export const AGENT_JAEGER_DOWN = "Could not reach Jaeger for agent telemetry.";
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

export function onAgentPollFailed(view: StaleCapable): void {
  if (view.rendered) view.markStale();
  else view.setError(AGENT_JAEGER_DOWN);
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
  setJaegerError(message: string): void;
}

/**
 * The source the feed and flag panel miss, null when both deliver: the agent's state
 * (blocked rows, flags) first, else Jaeger (rollout rows).
 */
export function agentPanelsDown(agentStateOk: boolean, jaegerOk: boolean): string | null {
  if (!agentStateOk) return DECIDE_AGENT_DOWN;
  return jaegerOk ? null : AGENT_JAEGER_DOWN;
}

/**
 * Paint the Agent tab's feed, flag panel and overlay. `down` (agentPanelsDown) names
 * the source that failed on its last poll: the overlay still renders, the feed and
 * flag panel keep their last data and go stale, or name the source when they never
 * rendered (UC-035 A3 step 2).
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
  else if (overlay !== null) view.setJaegerError(down);
  else view.setError(down);
}

export type AgentPollResults = [PromiseSettledResult<InterventionOverlay>, PromiseSettledResult<unknown>];

/**
 * Fold one settled Agent poll (VictoriaMetrics counts, rollout searches) into the
 * state. Returns the overlay to keep, whether the Jaeger leg succeeded and the
 * first failure (the poller backs off on it). `rolloutsRan` is false on a tick
 * that skipped the rollout searches: `jaegerOk` is then null, so the caller keeps
 * its previous verdict (a Jaeger outage must not clear on a tick that never asked).
 */
export function settleAgentPoll(
  [counts, rollouts]: AgentPollResults,
  overlay: InterventionOverlay | null,
  rolloutsRan: boolean,
): { overlay: InterventionOverlay | null; jaegerOk: boolean | null; failed: PromiseRejectedResult | undefined } {
  const failed = [counts, rollouts].find((r): r is PromiseRejectedResult => r.status === "rejected");
  return {
    overlay: counts.status === "fulfilled" ? counts.value : overlay,
    jaegerOk: rolloutsRan ? rollouts.status === "fulfilled" : null,
    failed,
  };
}