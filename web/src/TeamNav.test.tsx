import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";

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
});
