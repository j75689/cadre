// The theme: the system's (prefers-color-scheme), or light or dark as the
// top bar's control picks, kept in this browser. html[data-theme] is always
// light or dark, the system's resolved, and the stylesheet's tokens follow
// it alone. index.html sets it before the first paint, as applyTheme does.

export type Theme = "system" | "light" | "dark";

const key = "cadre.theme";

// next is the control's cycle: system, light, dark, system.
export const nextTheme: Record<Theme, Theme> = { system: "light", light: "dark", dark: "system" };

// systemDark is the query the system's choice is read and followed from;
// undefined where there is no matchMedia.
export const systemDark = () => window.matchMedia?.("(prefers-color-scheme: dark)");

export function loadTheme(): Theme {
  try {
    const t = localStorage.getItem(key);
    return t === "light" || t === "dark" ? t : "system";
  } catch {
    return "system";
  }
}

// applyTheme sets html[data-theme] to the theme, "system" resolved.
export function applyTheme(theme: Theme): void {
  const resolved = theme === "system" ? (systemDark()?.matches ? "dark" : "light") : theme;
  document.documentElement.setAttribute("data-theme", resolved);
}

export function saveTheme(theme: Theme): void {
  try {
    if (theme === "system") localStorage.removeItem(key);
    else localStorage.setItem(key, theme);
  } catch {
    // storage unavailable: the choice lasts this page only
  }
}
