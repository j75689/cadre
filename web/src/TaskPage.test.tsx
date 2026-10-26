import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import type { Phase, Task } from "./api";
import { subtaskPage } from "./Subtasks";
import { entry, json, task } from "./testing";

beforeEach(() => {
  localStorage.setItem("cadre.identity", "dana");
  vi.useFakeTimers({ shouldAdvanceTime: true });
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

// server answers the task page's reads from a growing thread and its
// subtasks, and records what it was asked.
function server(thread: ReturnType<typeof entry>[], onPost?: (url: string) => Promise<Response>, subtasks: Task[] = []) {
  const calls: string[] = [];
  vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
    calls.push(`${init?.method ?? "GET"} ${url}`);
    if (url.endsWith("/me")) return json({ mode: "none", admin: true });
    if (init?.method === "POST") return onPost ? onPost(url) : json({});
    const u = new URL(url, "http://cadre.test");
    if (u.pathname.endsWith("/thread")) {
      const after = Number(u.searchParams.get("after"));
      return json({ items: thread.filter((e) => e.seq > after) });
    }
    if (u.pathname.endsWith("/runs")) return json({ items: [] });
    if (u.pathname.endsWith("/tasks")) {
      // As the API pages: offset and limit, capped at the page size.
      const offset = Number(u.searchParams.get("offset"));
      const limit = Number(u.searchParams.get("limit"));
      const items = u.searchParams.get("parent") === "x" ? subtasks.slice(offset, offset + limit) : [];
      return json({ items, offset, limit });
    }
    return json(task("Running"));
  });
  return calls;
}

const threadReads = (calls: string[]) => calls.filter((c) => c.includes("/thread")).map((c) => new URL(c.split(" ")[1], "http://cadre.test").searchParams.get("after"));

function renderTask() {
  render(
    <MemoryRouter initialEntries={["/t/a/tasks/x"]}>
      <App />
    </MemoryRouter>,
  );
}

describe("TaskPage", () => {
  it("reads the thread once from the start, then only what is new", async () => {
    const thread = [entry(1, "Comment"), entry(2, "Comment")];
    const calls = server(thread);
    renderTask();
    expect(await screen.findByText("Comment 2")).toBeTruthy();
    thread.push(entry(3, "Comment"));
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(await screen.findByText("Comment 3")).toBeTruthy();
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(threadReads(calls)).toEqual(["0", "2", "3"]);
    expect(screen.getAllByText(/^Comment \d$/)).toHaveLength(3);
  });

  it("reads the task again at once when an action meets a conflict", async () => {
    const calls = server([], () => json({ error: { code: "Conflict", message: "the task moved on" } }, 409));
    renderTask();
    await screen.findByText("Task x");
    const gets = () => calls.filter((c) => c === "GET /api/v1/namespaces/a/tasks/x").length;
    const before = gets();
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "hello" } });
    fireEvent.click(screen.getByRole("button", { name: "Comment" }));
    expect(await screen.findByRole("alert")).toHaveProperty("textContent", "the task moved on");
    await vi.waitFor(() => expect(gets()).toBe(before + 1));
  });

  it("fills the gap when an action's entry is not the next one", async () => {
    const thread = [entry(1, "Comment"), entry(2, "Comment")];
    const calls = server(thread, () => {
      // Someone else wrote 3 since the last read; this comment is 4.
      thread.push(entry(3, "Comment", { author: { kind: "Agent", name: "writer" } }), entry(4, "Comment"));
      return json(entry(4, "Comment"), 201);
    });
    renderTask();
    expect(await screen.findByText("Comment 2")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "hello" } });
    fireEvent.click(screen.getByRole("button", { name: "Comment" }));
    // Read again from 2, at once, not from 4 at the next tick.
    expect(await screen.findByText("Comment 3")).toBeTruthy();
    expect(screen.getByText("Comment 4")).toBeTruthy();
    expect(threadReads(calls)).toEqual(["0", "2"]);
  });

  it("reads the runs again when the run in progress ends", async () => {
    let status: Record<string, unknown> = { phase: "Running", run: { id: "01JRUN0000000000000000000A", mode: "runtime" }, runs: { count: 1 } };
    const calls: string[] = [];
    vi.stubGlobal("fetch", (url: string) => {
      calls.push(url);
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      if (url.includes("/thread")) return json({ items: [] });
      if (url.endsWith("/runs")) return json({ items: [] });
      return json({ metadata: { name: "x", namespace: "a" }, spec: { title: "Task x" }, status });
    });
    renderTask();
    await screen.findByText("Task x");
    const runReads = () => calls.filter((c) => c.endsWith("/runs")).length;
    await vi.waitFor(() => expect(runReads()).toBe(1));
    // The run ends; the count, set at claim, does not move.
    status = { phase: "Done", runs: { count: 1 } };
    await act(() => vi.advanceTimersByTimeAsync(5000));
    await vi.waitFor(() => expect(runReads()).toBe(2));
  });

  it("reads the runs again when a run started and ended between reads", async () => {
    let status: Record<string, unknown> = { phase: "Todo", runs: { count: 1 } };
    const calls: string[] = [];
    vi.stubGlobal("fetch", (url: string) => {
      calls.push(url);
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      if (url.includes("/thread")) return json({ items: [] });
      if (url.endsWith("/runs")) return json({ items: [] });
      return json({ metadata: { name: "x", namespace: "a" }, spec: { title: "Task x" }, status });
    });
    renderTask();
    await screen.findByText("Task x");
    const runReads = () => calls.filter((c) => c.endsWith("/runs")).length;
    await vi.waitFor(() => expect(runReads()).toBe(1));
    // Back in Todo with no run, as before, but one more run behind it.
    status = { phase: "Todo", runs: { count: 2 } };
    await act(() => vi.advanceTimersByTimeAsync(5000));
    await vi.waitFor(() => expect(runReads()).toBe(2));
  });
});

// subtask is a subtask of x, numbered n.
function subtask(n: number, phase: Phase = "Todo"): Task {
  return {
    metadata: { name: `x-${n}`, namespace: "a" },
    spec: { title: `Step ${n}`, parentRef: { name: "x" }, assignee: { kind: "Agent", name: "coder" } },
    status: { phase, number: 100 + n },
  };
}

describe("Subtasks", () => {
  it("lists a task's subtasks with their number, phase and assignee", async () => {
    server([], undefined, [subtask(1, "Done"), subtask(2)]);
    renderTask();
    const section = await screen.findByRole("region", { name: "Subtasks" });
    expect(section.textContent).toContain("#101");
    expect(section.textContent).toContain("Step 1");
    expect(section.textContent).toContain("Done");
    expect(section.textContent).toContain("coder");
    expect(screen.getByRole("link", { name: "Step 2" }).getAttribute("href")).toBe("/t/a/tasks/x-2");
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  });

  it("keeps the subtasks it has beside an error", async () => {
    let down = false;
    const subtasks = [subtask(1)];
    server([], undefined, subtasks);
    const inner = globalThis.fetch;
    vi.stubGlobal("fetch", (url: string, init?: RequestInit) =>
      down && url.includes("parent=") ? json({ error: { code: "Internal", message: "index unavailable" } }, 500) : inner(url, init),
    );
    renderTask();
    expect(await screen.findByText("Step 1")).toBeTruthy();
    down = true;
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(await screen.findByText(/Could not load the subtasks/)).toBeTruthy();
    expect(screen.getByText("Step 1")).toBeTruthy();
  });

  it("shows only the error when the first read fails, and no Load more after a failed poll", async () => {
    let down = true;
    server([], undefined, Array.from({ length: subtaskPage + 1 }, (_, i) => subtask(i + 1)));
    const inner = globalThis.fetch;
    vi.stubGlobal("fetch", (url: string, init?: RequestInit) =>
      down && url.includes("parent=") ? json({ error: { code: "Internal", message: "index unavailable" } }, 500) : inner(url, init),
    );
    renderTask();
    expect(await screen.findByText(/Could not load the subtasks/)).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Subtasks" })).toBeNull();
    down = false;
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(await screen.findByRole("button", { name: "Load more" })).toBeTruthy();
    down = true;
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(await screen.findByText(/Could not load the subtasks/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
    expect(screen.getByText("Step 1")).toBeTruthy();
  });

  it("shows no section for a task without subtasks", async () => {
    const calls = server([]);
    renderTask();
    await screen.findByText("Task x");
    await vi.waitFor(() => expect(calls.some((c) => c.includes("parent=x"))).toBe(true));
    expect(screen.queryByRole("region", { name: "Subtasks" })).toBeNull();
  });

  it("loads more past a page, and keeps polling every page loaded", async () => {
    const subtasks = Array.from({ length: subtaskPage + 3 }, (_, i) => subtask(i + 1));
    const calls = server([], undefined, subtasks);
    renderTask();
    await screen.findByText(`Step ${subtaskPage}`);
    expect(screen.queryByText(`Step ${subtaskPage + 1}`)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByText(`Step ${subtaskPage + 3}`)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();

    // A new subtask on the last page shows on the next poll, both pages read.
    subtasks.push(subtask(subtaskPage + 4));
    const reads = () => calls.filter((c) => c.includes("parent=x")).length;
    const before = reads();
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(await screen.findByText(`Step ${subtaskPage + 4}`)).toBeTruthy();
    expect(reads() - before).toBe(2);
  });
});

describe("A link to a run", () => {
  const current = "01JRUN0000000000000000000B";
  const ended = "01JRUN0000000000000000000A";

  // runServer answers for a task running current, with ended in its runs.
  function runServer() {
    const calls: string[] = [];
    vi.stubGlobal("fetch", (url: string) => {
      calls.push(url);
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      if (url.endsWith("/runs"))
        return json({ items: [{ id: ended, mode: "runtime", runner: "r1", reason: "claimed", outcome: "Review", endedAt: "2026-10-25T09:00:00Z" }] });
      if (url.includes("/events")) return json({ items: [{ runId: "", seq: 1, progress: "working" }] });
      if (url.includes("/thread") || url.includes("parent=")) return json({ items: [] });
      return json(task("Running", { run: { id: current, mode: "runtime" }, runs: { count: 2 } }));
    });
    return calls;
  }

  function renderAt(run: string) {
    render(
      <MemoryRouter initialEntries={[`/t/a/tasks/x?run=${run}`]}>
        <App />
      </MemoryRouter>,
    );
  }

  it("opens an ended run and reads its events without a click", async () => {
    const scroll = vi.fn();
    Element.prototype.scrollIntoView = scroll;
    const calls = runServer();
    renderAt(ended);
    const row = await screen.findByRole("button", { name: /runtime on r1/ });
    expect(row.getAttribute("aria-expanded")).toBe("true");
    await vi.waitFor(() => expect(calls).toContain(`/api/v1/namespaces/a/tasks/x/runs/${ended}/events`));
    await vi.waitFor(() => expect(row.closest("li")!.textContent).toContain("progress: working"));
    expect(scroll).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("link", { name: `Link to run ${ended}` }).getAttribute("href")).toBe(`/t/a/tasks/x?run=${ended}`);
  });

  it("scrolls to the run in progress once", async () => {
    const scroll = vi.fn();
    Element.prototype.scrollIntoView = scroll;
    runServer();
    renderAt(current);
    expect(await screen.findByRole("region", { name: "Run in progress" })).toBeTruthy();
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    expect(scroll).toHaveBeenCalledTimes(1);
    expect((await screen.findByRole("button", { name: /runtime on r1/ })).getAttribute("aria-expanded")).toBe("false");
  });

  it("matches the run's id in any case", async () => {
    const calls = runServer();
    renderAt(ended.toLowerCase());
    const row = await screen.findByRole("button", { name: /runtime on r1/ });
    expect(row.getAttribute("aria-expanded")).toBe("true");
    await vi.waitFor(() => expect(calls).toContain(`/api/v1/namespaces/a/tasks/x/runs/${ended}/events`));
  });

  it("matches the run in progress in any case", async () => {
    const scroll = vi.fn();
    Element.prototype.scrollIntoView = scroll;
    runServer();
    renderAt(current.toLowerCase());
    expect(await screen.findByRole("region", { name: "Run in progress" })).toBeTruthy();
    await vi.waitFor(() => expect(scroll).toHaveBeenCalledTimes(1));
  });
});
