import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import type { Me } from "./api";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

// The pages under the nav never get an answer: only the nav is tested.
function at(path: string) {
  vi.stubGlobal("fetch", () => new Promise(() => {}));
  render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
  return within(screen.getByRole("navigation", { name: "Team" }));
}

describe("TeamNav", () => {
  it("marks Board current on the team's board", () => {
    const nav = at("/t/a");
    const board = nav.getByRole("link", { name: "Board" });
    expect(board.getAttribute("aria-current")).toBe("page");
    expect(board.getAttribute("href")).toBe("/t/a");
  });

  it("marks Board current on the team's path with a trailing slash", () => {
    const nav = at("/t/a/");
    expect(nav.getByRole("link", { name: "Board" }).getAttribute("aria-current")).toBe("page");
  });

  it("does not mark Board current on a task's page", () => {
    const nav = at("/t/a/tasks/x");
    expect(nav.getByRole("link", { name: "Board" }).getAttribute("aria-current")).toBeNull();
  });

  it("marks Archive current on the archive and on an archived task's page", () => {
    for (const path of ["/t/a/archive", "/t/a/archive/u1"]) {
      const nav = at(path);
      const archive = nav.getByRole("link", { name: "Archive" });
      expect(archive.getAttribute("aria-current"), path).toBe("page");
      expect(archive.getAttribute("href")).toBe("/t/a/archive");
      expect(nav.getByRole("link", { name: "Board" }).getAttribute("aria-current"), path).toBeNull();
      cleanup();
    }
  });

  it("links to the team by its encoded name", () => {
    const nav = at("/t/a%20b/new");
    expect(nav.getByRole("link", { name: "Board" }).getAttribute("href")).toBe("/t/a%20b");
  });

  it("is not on the teams page", () => {
    vi.stubGlobal("fetch", () => new Promise(() => {}));
    render(
      <MemoryRouter initialEntries={["/"]}>
        <App />
      </MemoryRouter>,
    );
    expect(screen.queryByRole("navigation", { name: "Team" })).toBeNull();
  });

  // Every tab's show is true today: no role or mode hides one.
  const tabs = ["Board", "Archive", "Agents", "Model providers", "Tool servers", "Schedules", "Notifiers", "Runners", "Settings"];
  it.each<[string, Me | null]>([
    ["a viewer", { mode: "oidc", name: "lee", roles: { a: "viewer" } }],
    ["a member", { mode: "oidc", name: "lee", roles: { a: "member" } }],
    ["an admin", { mode: "oidc", name: "dana", roles: { a: "admin" } }],
    ["an oidc user with no role in the team", { mode: "oidc", name: "lee", roles: {} }],
    ["--auth=none", { mode: "none", admin: true }],
    ["no /me yet (bootstrap)", null],
  ])("shows exactly the 9 tabs to %s", async (_, me) => {
    vi.stubGlobal("fetch", (url: string) => {
      if (url.endsWith("/me")) return Promise.resolve(me ? new Response(JSON.stringify(me), { status: 200, headers: { "Content-Type": "application/json" } }) : new Response("{}", { status: 503 }));
      return new Promise(() => {});
    });
    render(
      <MemoryRouter initialEntries={["/t/a"]}>
        <App />
      </MemoryRouter>,
    );
    const nav = within(screen.getByRole("navigation", { name: "Team" }));
    await waitFor(() => expect(nav.getAllByRole("link").map((l) => l.textContent)).toEqual(tabs));
    await new Promise((r) => setTimeout(r, 20));
    expect(nav.getAllByRole("link").map((l) => l.textContent)).toEqual(tabs);
  });
});
