/**
 * The agent's read-only state endpoint, GET /experiments/state (UC-035 steps 3 and
 * 4, docs/specs/services.md §5), through envoy like the /experiments dossier.
 *
 * One small JSON answer replaces the Jaeger searches the Agent and Experiments tabs
 * used to rebuild their state from: the newest decide spans the agent recorded (in
 * OTLP/JSON, folded by the same code as a Jaeger v3 answer), each game's effective
 * flags and each experiment's progress. Jaeger stays the drill-down: every row
 * carries the trace id its link opens. Times are the agent's clock and are only
 * shown or compared with each other, never with the browser's.
 */

import { BASE_URL } from "./baseUrl.js";
import { fromOtlp } from "./jaeger.js";
import type { JaegerTracesResponse, OtlpResourceSpans } from "./jaeger.js";
import { flagPanelFromState } from "./agentFlags.js";
import type { FlagPanel, FlagStateJson } from "./agentFlags.js";
import { experimentFromState } from "./experiments.js";
import type { ExperimentState, ExperimentView } from "./experiments.js";

export const AGENT_STATE_PATH = "/experiments/state";

/** The answer as the agent sends it. */
export interface AgentStateBody {
  decides?: { resourceSpans?: OtlpResourceSpans[] | null } | null;
  flags?: FlagStateJson[] | null;
  experiments?: ExperimentState[] | null;
}

/** The answer folded for the views. */
export interface AgentState {
  decides: JaegerTracesResponse;
  flags: FlagPanel[];
  experiments: ExperimentView[];
}

/** The agent could not answer: transport error, timeout, non-2xx (envoy's 503 with the agent down) or no JSON. */
export class AgentUnavailableError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "AgentUnavailableError";
  }
}

/** Fold an answer. Pure. */
export function parseAgentState(body: AgentStateBody | null | undefined): AgentState {
  return {
    decides: fromOtlp({ result: { resourceSpans: body?.decides?.resourceSpans ?? [] } }),
    flags: (body?.flags ?? []).map(flagPanelFromState).filter((f) => f.gameId !== ""),
    experiments: (body?.experiments ?? []).map(experimentFromState).filter((e) => e.experimentId !== ""),
  };
}

export async function fetchAgentState(signal?: AbortSignal): Promise<AgentState> {
  let response: Response;
  try {
    response = await fetch(`${BASE_URL}${AGENT_STATE_PATH}`, { headers: { Accept: "application/json" }, signal });
  } catch (error) {
    throw new AgentUnavailableError(`agent unreachable: ${String(error)}`);
  }
  if (!response.ok) throw new AgentUnavailableError(`agent state failed: ${response.status} ${response.statusText}`);
  try {
    return parseAgentState((await response.json()) as AgentStateBody);
  } catch (error) {
    throw new AgentUnavailableError(`agent answered no JSON: ${String(error)}`);
  }
}
