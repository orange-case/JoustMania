import { describe, expect, it, beforeEach } from "vitest";
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
});
