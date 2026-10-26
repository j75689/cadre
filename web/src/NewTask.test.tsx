import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useEffect } from "react";
import { MemoryRouter, useNavigate } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { toSpec, validate, type Draft } from "./NewTask";
import { json } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const draft = (d: Partial<Draft>): Draft => ({ title: "t", description: "", kind: "", assignee: "", priority: "medium", labels: "", ...d });

describe("validate", () => {
  it("mirrors the server's checks", () => {
    expect(validate(draft({ title: "  " }))).toMatch(/title/);
    expect(validate(draft({ kind: "Agent" }))).toMatch(/agent/);
    expect(validate(draft({ kind: "User", assignee: " " }))).toMatch(/user/);
    expect(validate(draft({ kind: "User", assignee: "dana" }))).toBe("");
    expect(validate(draft({}))).toBe("");
  });

  it("builds the spec", () => {
    expect(toSpec(draft({ title: " Fix it ", kind: "Agent", assignee: "writer", labels: "a, b,,a", priority: "high" }))).toEqual({
      title: "Fix it",
      priority: "high",
      assignee: { kind: "Agent", name: "writer" },
      labels: ["a", "b"],
    });
  });
});

describe("NewTask", () => {
  it("shows why an agent is not ready, and creates the task", async () => {
    const posts: unknown[] = [];
    vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      if (init?.method === "POST") {
        posts.push(JSON.parse(String(init.body)));
        return json({ metadata: { name: "01jnew", namespace: "a" }, spec: { title: "Write it" }, status: {} }, 201);
      }
      if (url.endsWith("/tasks/01jnew")) return json({ metadata: { name: "01jnew", namespace: "a" }, spec: { title: "Write it" }, status: { phase: "Todo" } });
      if (url.endsWith("/agents")) {
        return json({ items: [{ name: "coder", ready: false, reason: "ModelProviderNotFound", message: "ModelProvider claude does not exist" }, { name: "writer", ready: true }] });
      }
      return json({ items: [] });
    });
    render(
      <MemoryRouter initialEntries={["/t/a/new"]}>
        <App />
      </MemoryRouter>,
    );
    fireEvent.click(await screen.findByRole("button", { name: "Create task" }));
    expect(screen.getByRole("alert").textContent).toMatch(/title/i);
    expect(posts).toHaveLength(0);

    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Write it" } });
    fireEvent.click(screen.getByRole("radio", { name: "Agent" }));
    expect(await screen.findByRole("option", { name: "coder (not ready: ModelProviderNotFound)" })).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Agent to assign"), { target: { value: "coder" } });
    expect(screen.getByText(/ModelProvider claude does not exist/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Create task" }));
    expect(await screen.findByText("Write it")).toBeTruthy();
    expect(posts).toEqual([{ spec: { title: "Write it", priority: "medium", assignee: { kind: "Agent", name: "coder" } } }]);
  });

  it("says when the agents cannot be read, and reads them once", async () => {
    let reads = 0;
    vi.stubGlobal("fetch", (url: string) => {
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      if (url.endsWith("/agents")) {
        reads++;
        return reads === 1 ? json({ error: { code: "Unavailable", message: "the cache is not synced" } }, 503) : json({ items: [{ name: "writer", ready: true }] });
      }
      return json({ items: [] });
    });
    render(
      <MemoryRouter initialEntries={["/t/a/new"]}>
        <App />
      </MemoryRouter>,
    );
    fireEvent.click(await screen.findByRole("radio", { name: "Agent" }));
    expect(await screen.findByText(/Could not load the agents: the cache is not synced/)).toBeTruthy();
    expect(screen.queryByText(/no agents/)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("option", { name: "writer" })).toBeTruthy();
    expect(reads).toBe(2);
  });

  it("starts fresh for another team", async () => {
    vi.stubGlobal("fetch", (url: string) => (url.endsWith("/me") ? json({ mode: "none", admin: true }) : json({ items: [] })));
    const nav: { go: (to: string) => void } = { go: () => {} };
    function Navigator() {
      const navigate = useNavigate();
      useEffect(() => {
        nav.go = (to) => void navigate(to);
      }, [navigate]);
      return null;
    }
    render(
      <MemoryRouter initialEntries={["/t/a/new"]}>
        <Navigator />
        <App />
      </MemoryRouter>,
    );
    fireEvent.change(await screen.findByLabelText("Title"), { target: { value: "For team a" } });
    act(() => nav.go("/t/b/new"));
    expect(((await screen.findByLabelText("Title")) as HTMLInputElement).value).toBe("");
  });
});
