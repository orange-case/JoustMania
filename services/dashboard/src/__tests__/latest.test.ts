/**
 * "Latest span" deep links (#2717): `/?latest=retro` opens the newest
 * agent.llm.retro span in the Jaeger UI.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { latestOperation, newestSpanUrl, redirectToLatest, searchPageUrl } from "../latest.js";
import type { JaegerTracesResponse } from "../jaeger.js";
import { jsonResponse, toOtlp } from "./otlp.js";

const span = (traceID: string, spanID: string, operationName: string, startTime: number) => ({
  traceID,
  spanID,
  operationName,
  startTime,
  duration: 10,
  tags: [],
});

const BODY: JaegerTracesResponse = {
  data: [
    { traceID: "t1", spans: [span("t1", "a1", "agent.llm.retro", 1_000), span("t1", "s1", "agent.game.summary", 900)] },
    { traceID: "t2", spans: [span("t2", "a2", "agent.llm.retro", 5_000), span("t2", "d2", "agent.decide", 4_000)] },
  ],
};

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("UC-035: latest-span deep links for the stage (#2717)", () => {
  it("maps the latest values to agent spans and ignores anything else", () => {
    expect(latestOperation("?latest=retro")).toBe("agent.llm.retro");
    expect(latestOperation("?latest=summary")).toBe("agent.game.summary");
    expect(latestOperation("?latest=decide")).toBe("agent.decide");
    expect(latestOperation("?latest=toString")).toBeNull();
    expect(latestOperation("?latest=")).toBeNull();
    expect(latestOperation("")).toBeNull();
  });

  it("links the newest span of the operation, focused with uiFind", () => {
    expect(newestSpanUrl(BODY, "agent.llm.retro")).toBe("/jaeger/trace/t2?uiFind=a2");
    expect(newestSpanUrl(BODY, "agent.game.summary")).toBe("/jaeger/trace/t1?uiFind=s1");
    expect(newestSpanUrl({ data: [] }, "agent.llm.retro")).toBeNull();
  });

  it("redirects to the newest span after one v3 search", async () => {
    const urls: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        urls.push(String(input));
        return jsonResponse(200, toOtlp(BODY));
      }),
    );
    const replace = vi.fn();
    expect(await redirectToLatest({ search: "?latest=retro", replace })).toBe(true);
    expect(replace).toHaveBeenCalledWith("/jaeger/trace/t2?uiFind=a2");
    expect(urls).toHaveLength(1);
    expect(urls[0]).toContain("query.operationName=agent.llm.retro");
  });

  it("falls back to the Jaeger search page when Jaeger is unreachable", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("network down");
      }),
    );
    const replace = vi.fn();
    expect(await redirectToLatest({ search: "?latest=summary", replace })).toBe(true);
    expect(replace).toHaveBeenCalledWith(searchPageUrl("agent.game.summary"));
  });

  it("leaves the dashboard alone without a latest query", async () => {
    const replace = vi.fn();
    expect(await redirectToLatest({ search: "", replace })).toBe(false);
    expect(replace).not.toHaveBeenCalled();
  });
});
