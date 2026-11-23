/**
 * "Latest span" deep links for the stage (#2717): `/?latest=summary`,
 * `/?latest=retro` or `/?latest=decide` opens the newest such agent span in the
 * Jaeger UI, focused on that span (`uiFind`), instead of the dashboard.
 *
 * A slide served from the speaker's laptop cannot search the Pi's Jaeger itself
 * (another origin), but it can link to the dashboard, which sits behind the same
 * envoy as Jaeger. The search uses the query API v3 like every other Jaeger read
 * here (newest traces first, no browser-clock time). No span found, or Jaeger
 * unreachable: the Jaeger search page for that operation instead.
 */
import { jaegerTraceUrl } from "./agent.js";
import { AGENT_SERVICE, searchTraces, spanStartUs, spansNamed } from "./jaeger.js";
import type { JaegerTracesResponse } from "./jaeger.js";

/** The `latest` values and the agent span each one opens. */
export const LATEST_OPERATIONS: Readonly<Record<string, string>> = {
  summary: "agent.game.summary",
  retro: "agent.llm.retro",
  decide: "invoke_agent joustmania-agent",
};

/** Traces searched: the newest few, enough for one matching span per game. */
export const LATEST_SEARCH_DEPTH = 5;

/** The operation a `?latest=` query names, or null (absent or unknown value). */
export function latestOperation(search: string): string | null {
  const value = new URLSearchParams(search).get("latest");
  if (!value) return null;
  return Object.prototype.hasOwnProperty.call(LATEST_OPERATIONS, value) ? LATEST_OPERATIONS[value] : null;
}

/** Jaeger UI link to the newest `operation` span of a search answer, or null. */
export function newestSpanUrl(body: JaegerTracesResponse, operation: string): string | null {
  let newest = null;
  for (const span of spansNamed(body, operation)) {
    if (newest === null || spanStartUs(span) > spanStartUs(newest)) newest = span;
  }
  return newest === null ? null : jaegerTraceUrl(newest.traceID, newest.spanID);
}

/** Jaeger UI search page for one agent operation (the fallback). */
export function searchPageUrl(operation: string): string {
  const q = new URLSearchParams({ service: AGENT_SERVICE, operation, limit: String(LATEST_SEARCH_DEPTH) });
  return `/jaeger/search?${q.toString()}`;
}

/**
 * Follow a `?latest=` link: replace the location with the newest span (or the
 * search page). Returns false when the page was opened without one, so the
 * dashboard starts as usual.
 */
export async function redirectToLatest(
  location: Pick<Location, "search" | "replace"> = window.location,
): Promise<boolean> {
  const operation = latestOperation(location.search);
  if (operation === null) return false;
  let target = searchPageUrl(operation);
  try {
    const body = await searchTraces({ operation, limit: LATEST_SEARCH_DEPTH, startMs: null });
    target = newestSpanUrl(body, operation) ?? target;
  } catch {
    // Jaeger unreachable: the search page shows why.
  }
  location.replace(target);
  return true;
}
