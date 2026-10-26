import { act, cleanup, render, screen } from "@testing-library/react";
import { useEffect } from "react";
import { MemoryRouter, useNavigate } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const nav: { go: (to: string) => void } = { go: () => {} };
function Navigator() {
  const navigate = useNavigate();
  useEffect(() => {
    nav.go = (to) => void navigate(to);
  }, [navigate]);
  return null;
}

describe("App", () => {
  it("shows another team's board fresh, not the last one's cards", async () => {
    localStorage.setItem("cadre.identity", "dana");
    // Team a has one Todo task; team b's board never answers.
    vi.stubGlobal("fetch", (url: string) => {
      if (url.includes("/namespaces/b/")) return new Promise(() => {});
      const todo = url.includes("phase=Todo");
      const items = todo ? [{ metadata: { name: "a-task", namespace: "a" }, spec: { title: "A's task" }, status: { phase: "Todo" } }] : [];
      return Promise.resolve(new Response(JSON.stringify({ items, offset: 0, limit: 50 })));
    });
    render(
      <MemoryRouter initialEntries={["/t/a"]}>
        <Navigator />
        <App />
      </MemoryRouter>,
    );
    expect(await screen.findByText("A's task")).toBeTruthy();
    act(() => nav.go("/t/b"));
    expect(screen.getByRole("heading", { name: "b" })).toBeTruthy();
    expect(screen.queryByText("A's task")).toBeNull();
    expect(screen.getByText("Loading the board")).toBeTruthy();
  });

  it("shows another task's page fresh, not the last one's", async () => {
    localStorage.setItem("cadre.identity", "dana");
    // Task x answers; task y never does.
    vi.stubGlobal("fetch", (url: string) => {
      if (url.endsWith("/tasks/y")) return new Promise(() => {});
      return Promise.resolve(new Response(JSON.stringify({ metadata: { name: "x", namespace: "a" }, spec: { title: "Task x" }, status: { phase: "Todo" } })));
    });
    render(
      <MemoryRouter initialEntries={["/t/a/tasks/x"]}>
        <Navigator />
        <App />
      </MemoryRouter>,
    );
    expect(await screen.findByText("Task x")).toBeTruthy();
    act(() => nav.go("/t/a/tasks/y"));
    expect(screen.queryByText("Task x")).toBeNull();
  });
});
