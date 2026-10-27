import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useEffect } from "react";
import { MemoryRouter, useLocation, useNavigate, type NavigateFunction } from "react-router";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import type { Task } from "./api";
import { json, task, team } from "./testing";

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
// team a; a task is t, and creating one is refused.
function server(role: string, t = task("Todo")) {
  vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
    if (url.endsWith("/me")) return json({ mode: "oidc", username: "dana@example.test", name: "Dana", roles: { a: role }, admin: false });
    if (url.endsWith("/team")) return json(team());
    if (init?.method === "POST") return json({ error: { code: "invalid", message: "title is too long" } }, 400);
    if (url.endsWith("/teams")) return json({ items: [{ metadata: { name: "team", namespace: "a" } }] });
    if (url.includes("/thread") || url.endsWith("/runs") || url.includes("/subtasks")) return json({ items: [] });
    if (url.includes("/tasks?")) return json({ items: [], offset: 0, limit: 50 });
    return json(t);
  });
}

// go is the router's navigate, as the browser's Back would use it.
let go: NavigateFunction;

function Where() {
  const { pathname, search } = useLocation();
  const navigate = useNavigate();
  useEffect(() => {
    go = navigate;
  }, [navigate]);
  return (
    <>
      <output data-testid="path">{pathname}</output>
      <output data-testid="search">{search}</output>
    </>
  );
}

// show renders path for a caller with role on team a.
function show(path: string, role = "member", t?: Task) {
  server(role, t);
  render(
    <MemoryRouter initialEntries={[path]}>
      <App />
      <Where />
    </MemoryRouter>,
  );
}

// at shows path once the role on team a is known.
async function at(path: string, role = "member", t?: Task) {
  show(path, role, t);
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

  it("keep the board's search on g then b from the board", async () => {
    await at("/t/a?q=foo");
    press("g");
    press("b");
    expect(path()).toBe("/t/a");
    expect(screen.getByTestId("search").textContent).toBe("?q=foo");
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

  it("fire / and ? typed with AltGr, which comes with Ctrl and Alt", async () => {
    await at("/t/a");
    const altGr = { ctrlKey: true, altKey: true, modifierAltGraph: true };
    press("/", altGr);
    expect(document.activeElement).toBe(screen.getByRole("searchbox", { name: "Search the loaded page" }));
    press("?", altGr, document.body);
    expect(screen.getByRole("dialog", { name: "Keyboard shortcuts" })).toBeTruthy();
  });

  it("do not leave the page from a form or a button, where a draft may be", async () => {
    await at("/t/a");
    press("c");
    const create = await screen.findByRole("button", { name: "Create task" });
    press("g", {}, create);
    press("b", {}, create);
    expect(path()).toBe("/t/a/new");
    const top = screen.getByRole("button", { name: "Keyboard shortcuts" });
    press("g", {}, top);
    press("b", {}, top);
    expect(path()).toBe("/t/a/new");
  });

  it("do not leave a Settings draft on c with the focus on the page", async () => {
    show("/t/a/settings", "admin");
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("Display name"), { target: { value: "Draft name" } });
    act(() => (document.activeElement as HTMLElement).blur());
    press("c", {}, document.body);
    expect(path()).toBe("/t/a/settings");
  });

  it("do not leave a New task draft on g then b after a refused Create", async () => {
    show("/t/a/new");
    fireEvent.change(await screen.findByLabelText("Title"), { target: { value: "draft" } });
    fireEvent.click(screen.getByRole("button", { name: "Create task" }));
    expect(await screen.findByText("title is too long")).toBeTruthy();
    act(() => (document.activeElement as HTMLElement).blur());
    press("g", {}, document.body);
    press("b", {}, document.body);
    expect(path()).toBe("/t/a/new");
  });

  it("do not leave a comment draft on c with the focus on the page", async () => {
    await at("/t/a/tasks/x");
    fireEvent.change(await screen.findByLabelText("Comment"), { target: { value: "draft" } });
    act(() => (document.activeElement as HTMLElement).blur());
    press("c", {}, document.body);
    expect(path()).toBe("/t/a/tasks/x");
  });

  it("do not leave a tool call note on c with the focus on the page", async () => {
    await at("/t/a/tasks/x", "member", task("Waiting", { waiting: { reason: "ToolApproval" } }));
    fireEvent.change(await screen.findByLabelText("Tool call note"), { target: { value: "draft" } });
    act(() => (document.activeElement as HTMLElement).blur());
    press("c", {}, document.body);
    expect(path()).toBe("/t/a/tasks/x");
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

  it("close the help on Escape, after which ? opens it again", async () => {
    await at("/t/a");
    const opener = screen.getByRole("button", { name: "Keyboard shortcuts" });
    opener.focus();
    fireEvent.click(opener);
    const help = screen.getByRole("dialog", { name: "Keyboard shortcuts" }) as HTMLDialogElement;
    // Escape is native: the browser fires cancel, then closes the dialog.
    act(() => {
      help.dispatchEvent(new Event("cancel", { cancelable: true }));
      help.close();
    });
    expect(document.activeElement).toBe(opener);
    press("?", {}, document.body);
    expect(screen.getByRole("dialog", { name: "Keyboard shortcuts" })).toBeTruthy();
  });

  it("give the focus to the page's h1 when the opener has gone with the route", async () => {
    await at("/t/a/tasks/x");
    const opener = screen.getByRole("heading", { level: 1 });
    act(() => opener.focus());
    press("?");
    act(() => void go("/t/a"));
    act(() => screen.getByRole("button", { name: "Close" }).focus());
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(opener.isConnected).toBe(false);
    expect(document.activeElement).toBe(await screen.findByRole("heading", { level: 1, name: "a" }));
  });
});
