// @vitest-environment happy-dom
/**
 * FR-084: the dashboard's light / dark theme (Orange Case look). Auto follows the
 * OS, the header toggle pins Light or Dark per browser, the Grafana embed follows.
 */
import { describe, expect, it, beforeEach, vi } from "vitest";
import { EmbedGate } from "../visibilityGate";
import {
  THEME_STORAGE_KEY,
  applyTheme,
  effectiveTheme,
  initTheme,
  nextTheme,
  readTheme,
  withGrafanaTheme,
} from "../theme";

describe("theme", () => {
  beforeEach(() => {
    document.documentElement.removeAttribute("data-theme");
    document.body.innerHTML = `
      <button id="theme-toggle"></button>
      <iframe id="g" data-src="/grafana/d/x/x?orgId=1&theme=dark&kiosk"></iframe>
      <iframe id="j" data-src="/jaeger/search?theme=dark"></iframe>`;
    localStorage.clear();
  });

  it("cycles auto -> light -> dark -> auto", () => {
    expect(nextTheme("auto")).toBe("light");
    expect(nextTheme("light")).toBe("dark");
    expect(nextTheme("dark")).toBe("auto");
  });

  it("reads only known stored values and survives a throwing storage", () => {
    expect(readTheme({ getItem: () => "dark" })).toBe("dark");
    expect(readTheme({ getItem: () => "purple" })).toBe("auto");
    expect(readTheme({ getItem: () => { throw new Error("blocked"); } })).toBe("auto");
    expect(readTheme(undefined)).toBe("auto");
  });

  it("auto follows the OS preference", () => {
    expect(effectiveTheme("auto", true)).toBe("dark");
    expect(effectiveTheme("auto", false)).toBe("light");
    expect(effectiveTheme("light", true)).toBe("light");
  });

  it("rewrites only an existing Grafana theme parameter", () => {
    expect(withGrafanaTheme("/grafana/d?orgId=1&theme=dark&kiosk", "light")).toBe("/grafana/d?orgId=1&theme=light&kiosk");
    expect(withGrafanaTheme("/grafana/d?orgId=1", "light")).toBe("/grafana/d?orgId=1");
  });

  it("pins data-theme, themes the Grafana embed only, labels the toggle", () => {
    applyTheme(document, "light", true);
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    expect(document.getElementById("g")!.getAttribute("data-src")).toContain("theme=light");
    expect(document.getElementById("j")!.getAttribute("data-src")).toBe("/jaeger/search?theme=dark");
    expect(document.getElementById("theme-toggle")!.textContent).toContain("Light");

    applyTheme(document, "auto", true);
    expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
    expect(document.getElementById("g")!.getAttribute("data-src")).toContain("theme=dark");
  });

  it("the toggle stores the pinned choice and clears it on auto", () => {
    initTheme(window);
    const btn = document.getElementById("theme-toggle")!;
    btn.click();
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("light");
    btn.click();
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    btn.click();
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBeNull();
    expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
  });

  it("starts from a stored choice", () => {
    localStorage.setItem(THEME_STORAGE_KEY, "dark");
    expect(initTheme(window)).toBe("dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    expect(document.getElementById("theme-toggle")!.textContent).toContain("Dark");
  });

  it("keeps working for this page when storage refuses writes", () => {
    const setItem = vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("quota");
    });
    try {
      initTheme(window);
      document.getElementById("theme-toggle")!.click();
      expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    } finally {
      setItem.mockRestore();
    }
  });

  it("re-themes an already loaded Grafana frame", () => {
    const frame = document.getElementById("g") as HTMLIFrameElement;
    frame.setAttribute("src", "/grafana/d/x/x?orgId=1&theme=dark&kiosk");
    applyTheme(document, "light", true);
    expect(frame.getAttribute("src")).toContain("theme=light");
  });

  it("the embed gate loads Grafana in the theme shown at load time, also after an unload", () => {
    const frame = document.getElementById("g") as HTMLIFrameElement;
    const gate = new EmbedGate([
      {
        frame,
        tab: "metrics",
        get src() {
          return frame.dataset.src ?? "";
        },
        unloadWhenAway: true,
      },
    ]);
    applyTheme(document, "light", true);
    gate.sync("metrics", false);
    expect(frame.getAttribute("src")).toContain("theme=light");

    gate.sync("metrics", true); // hidden page: unloaded at once
    expect(frame.getAttribute("src")).toBe("about:blank");
    applyTheme(document, "dark", false);
    gate.sync("metrics", false);
    expect(frame.getAttribute("src")).toContain("theme=dark");
  });
});
