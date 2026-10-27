import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { json, task } from "./testing";

// jsdom has <dialog> but not showModal() and close(): open and close it
// as a browser would, without the top layer.
beforeAll(() => {
  const proto: Partial<HTMLDialogElement> = HTMLDialogElement.prototype;
  if (proto.showModal) return;
  proto.showModal = function (this: HTMLDialogElement) {
    this.setAttribute("open", "");
  };
  proto.close = function (this: HTMLDialogElement) {
    this.removeAttribute("open");
    this.dispatchEvent(new Event("close"));
  };
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

// server answers as cadre-server with logins, the caller having role on
// team a; a task is task x.
function server(role: string) {
  vi.stubGlobal("fetch", (url: string) => {
    if (url.endsWith("/me")) return json({ mode: "oidc", username: "dana@example.test", name: "Dana", roles: { a: role }, admin: false });
    if (url.endsWith("/teams")) return json({ items: [{ metadata: { name: "team", namespace: "a" } }] });
    if (url.includes("/thread") || url.endsWith("/runs") || url.includes("/subtasks")) return json({ items: [] });
    if (url.includes("/tasks?")) return json({ items: [], offset: 0, limit: 50 });
    return json(task("Todo"));
  });
}

function Where() {
  return <output data-testid="path">{useLocation().pathname}</output>;
}

async function at(path: string, role = "member") {
  server(role);
  render(
    <MemoryRouter initialEntries={[path]}>
      <App />
      <Where />
    </MemoryRouter>,
  );
  expect(await screen.findByText(`You are a ${role} of a.`)).toBeTruthy();
}

const path = () => screen.getByTestId("path").textContent;
const press = (key: string, init: KeyboardEventInit & { keyCode?: number } = {}, target: Element = document.activeElement ?? document.body) =>
  fireEvent.keyDown(target, { key, ...init });

describe("shortcuts", () => {
  it("open the help on ?, which lists them", async () => {
    await at("/t/a");
    expect(screen.queryByRole("dialog")).toBeNull();
    press("?");
    const help = screen.getByRole("dialog", { name: "Keyboard shortcuts" });
    for (const what of ["Show the keyboard shortcuts", "Search the board", "New task", "Go to the board"]) {
      expect(help.textContent).toContain(what);
    }
  });

  it("leave New task out of a viewer's help", async () => {
    await at("/t/a", "viewer");
    press("?");
    expect(screen.getByRole("dialog", { name: "Keyboard shortcuts" }).textContent).not.toContain("New task");
  });

  it("focus the search on /", async () => {
    await at("/t/a");
    expect(press("/")).toBe(false);
    expect(document.activeElement).toBe(screen.getByRole("searchbox", { name: "Search the loaded page" }));
  });

  it("open New task on c for a member, with the focus on its heading", async () => {
    await at("/t/a/tasks/x");
    press("c");
    expect(path()).toBe("/t/a/new");
    expect(document.activeElement).toBe(await screen.findByRole("heading", { level: 1, name: "New task" }));
  });

  it("do nothing on c for a viewer", async () => {
    await at("/t/a", "viewer");
    press("c");
    expect(path()).toBe("/t/a");
  });

  it("go to the board on g then b within a second", async () => {
    const now = vi.spyOn(Date, "now").mockReturnValue(10_000);
    await at("/t/a/tasks/x");
    press("g");
    now.mockReturnValue(10_900);
    press("b");
    expect(path()).toBe("/t/a");
    expect(document.activeElement).toBe(await screen.findByRole("heading", { level: 1, name: "a" }));
  });

  it("do not go to the board on b 1.2 s after g, or on b alone", async () => {
    const now = vi.spyOn(Date, "now").mockReturnValue(10_000);
    await at("/t/a/tasks/x");
    press("g");
    now.mockReturnValue(11_200);
    press("b");
    expect(path()).toBe("/t/a/tasks/x");
    press("b");
    expect(path()).toBe("/t/a/tasks/x");
  });

  it("do not fire while typing in a field", async () => {
    await at("/t/a");
    const search = screen.getByRole("searchbox", { name: "Search the loaded page" });
    search.focus();
    // Not prevented: the browser types the c.
    expect(press("c", {}, search)).toBe(true);
    expect(press("?", {}, search)).toBe(true);
    expect(path()).toBe("/t/a");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("do not fire with Ctrl, Cmd or Alt", async () => {
    await at("/t/a");
    press("c", { ctrlKey: true });
    press("c", { metaKey: true });
    press("c", { altKey: true });
    expect(path()).toBe("/t/a");
  });

  it("do not fire during an input method's composition", async () => {
    await at("/t/a");
    press("c", { isComposing: true });
    press("c", { keyCode: 229 });
    press("?", { isComposing: true });
    expect(path()).toBe("/t/a");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("do not fire in the open help, but for ?", async () => {
    await at("/t/a");
    press("?");
    press("c", {}, screen.getByRole("button", { name: "Close" }));
    expect(path()).toBe("/t/a");
    expect(screen.getByRole("dialog", { name: "Keyboard shortcuts" })).toBeTruthy();
  });

  it("give the focus back to the opener when the help closes", async () => {
    await at("/t/a");
    const opener = screen.getByRole("button", { name: "Keyboard shortcuts" });
    opener.focus();
    fireEvent.click(opener);
    expect(screen.getByRole("dialog", { name: "Keyboard shortcuts" })).toBeTruthy();
    act(() => screen.getByRole("button", { name: "Close" }).focus());
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(opener);
  });
});
