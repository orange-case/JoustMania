/**
 * Light / dark theme, matching the Orange Case website.
 *
 * "auto" follows the OS (`prefers-color-scheme`); "light" and "dark" pin the
 * theme through `data-theme` on <html>, which style.css reads. The choice is a
 * per-browser convenience kept in localStorage; when storage is unavailable
 * the dashboard simply stays on "auto".
 */

export type ThemeChoice = "auto" | "light" | "dark";

export const THEME_STORAGE_KEY = "joustmania.theme";
const ORDER: ThemeChoice[] = ["auto", "light", "dark"];
const LABELS: Record<ThemeChoice, string> = {
  auto: "◐ Auto",
  light: "☀ Light",
  dark: "☾ Dark",
};

export function nextTheme(current: ThemeChoice): ThemeChoice {
  return ORDER[(ORDER.indexOf(current) + 1) % ORDER.length];
}

export function readTheme(storage: Pick<Storage, "getItem"> | undefined): ThemeChoice {
  try {
    const v = storage?.getItem(THEME_STORAGE_KEY);
    return v === "light" || v === "dark" ? v : "auto";
  } catch {
    return "auto";
  }
}

function writeTheme(storage: Pick<Storage, "setItem" | "removeItem"> | undefined, choice: ThemeChoice) {
  try {
    if (choice === "auto") storage?.removeItem(THEME_STORAGE_KEY);
    else storage?.setItem(THEME_STORAGE_KEY, choice);
  } catch {
    /* storage blocked: the choice lasts for this page only */
  }
}

/** The theme actually shown: the choice, or the OS preference for "auto". */
export function effectiveTheme(choice: ThemeChoice, prefersDark: boolean): "light" | "dark" {
  if (choice !== "auto") return choice;
  return prefersDark ? "dark" : "light";
}

/** Point a Grafana embed URL at the matching Grafana theme. */
export function withGrafanaTheme(url: string, theme: "light" | "dark"): string {
  return /([?&])theme=(light|dark)/.test(url)
    ? url.replace(/([?&])theme=(light|dark)/, `$1theme=${theme}`)
    : url;
}

export function applyTheme(doc: Document, choice: ThemeChoice, prefersDark: boolean): void {
  const root = doc.documentElement;
  if (choice === "auto") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", choice);

  const shown = effectiveTheme(choice, prefersDark);
  doc.querySelectorAll<HTMLIFrameElement>("iframe[data-src*='/grafana/']").forEach((frame) => {
    const dataSrc = frame.getAttribute("data-src");
    if (dataSrc) frame.setAttribute("data-src", withGrafanaTheme(dataSrc, shown));
    const src = frame.getAttribute("src");
    if (src) {
      const themed = withGrafanaTheme(src, shown);
      if (themed !== src) frame.setAttribute("src", themed);
    }
  });

  const button = doc.getElementById("theme-toggle");
  if (button) {
    button.textContent = LABELS[choice];
    button.setAttribute("aria-label", `Theme: ${choice}. Switch to ${nextTheme(choice)}.`);
    button.title = `Theme: ${choice} (click to switch)`;
  }
}

/** Wire the header toggle; returns the starting choice. */
export function initTheme(win: Window = window): ThemeChoice {
  let storage: Storage | undefined;
  try {
    storage = win.localStorage;
  } catch {
    storage = undefined;
  }
  const media = win.matchMedia?.("(prefers-color-scheme: dark)");
  let choice = readTheme(storage);
  const render = () => applyTheme(win.document, choice, media?.matches ?? false);
  render();

  media?.addEventListener?.("change", render);
  win.document.getElementById("theme-toggle")?.addEventListener("click", () => {
    choice = nextTheme(choice);
    writeTheme(storage, choice);
    render();
  });
  return choice;
}
