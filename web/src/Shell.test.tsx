import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { json, task } from "./testing";

beforeEach(() => {
  localStorage.clear();
  localStorage.setItem("cadre.identity", "dana");
  document.documentElement.removeAttribute("data-theme");
  vi.stubGlobal("matchMedia", undefined);
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const teams = [
  { metadata: { name: "team", namespace: "a" }, spec: { displayName: "Team A" } },
  { metadata: { name: "team", namespace: "b" } },
];

// server answers as cadre-server: two teams, a board with one task in
// Todo, and a task page.
function server(me: unknown = { mode: "none", admin: true }) {
  vi.stubGlobal("fetch", (url: string) => {
    if (url.endsWith("/me")) return json(me);
    if (url.endsWith("/teams")) return json({ items: teams });
    if (url.includes("/thread")) return json({ items: [] });
    if (url.endsWith("/runs")) return json({ items: [] });
    if (url.includes("/tasks?")) {
      const items = url.includes("phase=Todo") ? [{ ...task("Todo", { number: 12 }, { title: "Fix the build" }), metadata: { name: "fix", namespace: "a" } }] : [];
      return json({ items, offset: 0, limit: 50 });
    }
    return json({ ...task("Todo", { number: 12 }, { title: "Fix the build" }), metadata: { name: "fix", namespace: "a" } });
  });
}

function at(path: string) {
  render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
}

// darkSystem stubs matchMedia: a system whose dark preference the test
// flips, telling the listeners as a browser does.
function darkSystem(dark: boolean) {
  const listeners = new Set<() => void>();
  const query = {
    get matches() {
      return dark;
    },
    addEventListener: (_: string, f: () => void) => listeners.add(f),
    removeEventListener: (_: string, f: () => void) => listeners.delete(f),
  };
  vi.stubGlobal("matchMedia", () => query);
  return {
    set(next: boolean) {
      dark = next;
      listeners.forEach((f) => f());
    },
    listeners,
  };
}

describe("theme control", () => {
  const theme = () => document.documentElement.getAttribute("data-theme");
  const control = () => screen.getByRole("button", { name: /^Theme:/ });

  it("cycles system, light and dark, applies it and keeps it", async () => {
    server();
    darkSystem(true);
    at("/");
    expect(control().getAttribute("aria-label")).toBe("Theme: system. Switch to light");
    // The system's choice, resolved.
    expect(theme()).toBe("dark");
    fireEvent.click(control());
    expect(theme()).toBe("light");
    expect(localStorage.getItem("cadre.theme")).toBe("light");
    fireEvent.click(control());
    expect(theme()).toBe("dark");
    expect(localStorage.getItem("cadre.theme")).toBe("dark");

    // A reload starts from the kept choice.
    cleanup();
    at("/");
    expect(control().getAttribute("aria-label")).toBe("Theme: dark. Switch to system");
    fireEvent.click(control());
    expect(localStorage.getItem("cadre.theme")).toBeNull();
    expect(theme()).toBe("dark");
  });

  it("follows the system's changes in system mode only", () => {
    server();
    const system = darkSystem(false);
    at("/");
    expect(theme()).toBe("light");
    act(() => system.set(true));
    expect(theme()).toBe("dark");
    act(() => system.set(false));
    expect(theme()).toBe("light");
    // Picked: the system no longer leads, nor is listened to.
    fireEvent.click(control());
    fireEvent.click(control());
    expect(theme()).toBe("dark");
    act(() => system.set(false));
    expect(theme()).toBe("dark");
    expect(system.listeners.size).toBe(0);
  });
});

describe("team switcher", () => {
  it("names the current team, lists every team with its role, and navigates", async () => {
    server({ mode: "oidc", username: "dana@example.test", name: "Dana", roles: { a: "admin", b: "viewer" } });
    at("/t/b");
    const button = await screen.findByRole("button", { name: "Team b, switch team" });
    expect(button.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(button);
    const menu = screen.getByRole("menu");
    const items = within(menu).getAllByRole("menuitem");
    expect(items.map((i) => i.textContent)).toEqual(["All teams", "Team Aaadmin", "bviewer"]);
    expect(items[2].getAttribute("aria-current")).toBe("page");
    // Arrow keys move through the items.
    expect(document.activeElement).toBe(items[0]);
    fireEvent.keyDown(menu, { key: "ArrowDown" });
    expect(document.activeElement).toBe(items[1]);
    fireEvent.click(items[1]);
    expect(screen.queryByRole("menu")).toBeNull();
    expect(await screen.findByRole("heading", { level: 1, name: "Team A" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Team Team A, switch team" })).toBeTruthy();
  });

  it("names a failed read of the teams in the menu, where assistive technology reads it", async () => {
    vi.stubGlobal("fetch", (url: string) => {
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      return json({ error: { code: "Unavailable", message: "not synced" } }, 503);
    });
    at("/t/a");
    fireEvent.click(await screen.findByRole("button", { name: "Team a, switch team" }));
    await vi.waitFor(() => expect(screen.getByRole("menuitem", { name: "Could not load the teams: not synced" })).toBeTruthy());
    const note = screen.getByRole("menuitem", { name: "Could not load the teams: not synced" });
    expect(note.getAttribute("aria-disabled")).toBe("true");
    // Choosing it does nothing, and the menu stays.
    fireEvent.click(note);
    expect(screen.getByRole("menu")).toBeTruthy();
  });

  it("survives a malformed team in the path", () => {
    server();
    at("/t/%E0%A4%A");
    expect(screen.getByRole("link", { name: "Skip to content" })).toBeTruthy();
  });

  it("reads Teams on the team picker, and closes on Escape", async () => {
    server();
    at("/");
    const button = await screen.findByRole("button", { name: "Switch team" });
    expect(button.textContent).toBe("Teams");
    fireEvent.click(button);
    fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
    expect(screen.queryByRole("menu")).toBeNull();
    expect(document.activeElement).toBe(button);
  });
});

describe("pages", () => {
  it("set the document title, and move the focus to the h1 after a route change", async () => {
    server();
    at("/t/a");
    expect(await screen.findByText("Fix the build")).toBeTruthy();
    await vi.waitFor(() => expect(document.title).toBe("Team A · Cadre"));
    // The first page keeps the browser's focus.
    expect(document.activeElement).toBe(document.body);
    act(() => screen.getByRole("link", { name: "Fix the build" }).click());
    const h1 = await screen.findByRole("heading", { level: 1, name: "#12 Fix the build" });
    await vi.waitFor(() => expect(document.title).toBe("#12 Fix the build · Team A · Cadre"));
    expect(document.activeElement).toBe(h1);
  });

  it("move the focus to the h1 on going back to a page through one still loading", async () => {
    server();
    const serve = globalThis.fetch;
    vi.stubGlobal("fetch", (url: string) => (url.endsWith("/tasks/fix") ? new Promise(() => {}) : serve(url)));
    at("/t/a");
    const card = await screen.findByRole("link", { name: "Fix the build" });
    act(() => card.click());
    // The task is still loading: no h1 yet. Its breadcrumb goes back.
    act(() => within(screen.getByRole("navigation", { name: "Breadcrumb" })).getByRole("link", { name: "Team A" }).click());
    const h1 = await screen.findByRole("heading", { level: 1, name: "Team A" });
    await vi.waitFor(() => expect(document.activeElement).toBe(h1));
  });

  it("offer a skip link to the page's main", async () => {
    server();
    at("/");
    const skip = screen.getByRole("link", { name: "Skip to content" });
    expect(skip.getAttribute("href")).toBe("#main");
    expect(document.getElementById("main")?.tagName).toBe("MAIN");
  });

  it("show a not-found card with a way back", () => {
    server();
    at("/nowhere");
    expect(screen.getByRole("heading", { level: 1, name: "Not found" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Teams" }).getAttribute("href")).toBe("/");
  });
});

describe("Teams", () => {
  it("shows skeletons, then a card per team", async () => {
    let answer: (r: Response) => void = () => {};
    vi.stubGlobal("fetch", (url: string) => {
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      return new Promise<Response>((r) => (answer = r));
    });
    at("/");
    expect(screen.getByText("Loading the teams")).toBeTruthy();
    await act(async () => answer(new Response(JSON.stringify({ items: teams }))));
    expect(screen.queryByText("Loading the teams")).toBeNull();
    expect(screen.getByRole("link", { name: "Team A" }).getAttribute("href")).toBe("/t/a");
    expect(screen.getByRole("link", { name: "b" })).toBeTruthy();
  });

  it("says how a namespace becomes a team when there is none", async () => {
    vi.stubGlobal("fetch", (url: string) => (url.endsWith("/me") ? json({ mode: "none", admin: true }) : json({ items: [] })));
    at("/");
    expect(await screen.findByText("cadre team init -n <namespace>")).toBeTruthy();
  });

  it("shows an error with Retry", async () => {
    let fail = true;
    vi.stubGlobal("fetch", (url: string) => {
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      return fail ? json({ error: { code: "Unavailable", message: "not synced" } }, 503) : json({ items: teams });
    });
    at("/");
    expect(await screen.findByText("Could not load the teams: not synced")).toBeTruthy();
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("link", { name: "Team A" })).toBeTruthy();
  });
});
