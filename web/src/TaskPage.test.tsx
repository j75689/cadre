import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
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

// server answers the task page's reads from a growing thread and records
// what it was asked.
function server(thread: ReturnType<typeof entry>[], onPost?: (url: string) => Promise<Response>) {
  const calls: string[] = [];
  vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
    calls.push(`${init?.method ?? "GET"} ${url}`);
    if (init?.method === "POST") return onPost ? onPost(url) : json({});
    const u = new URL(url, "http://cadre.test");
    if (u.pathname.endsWith("/thread")) {
      const after = Number(u.searchParams.get("after"));
      return json({ items: thread.filter((e) => e.seq > after) });
    }
    if (u.pathname.endsWith("/runs")) return json({ items: [] });
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
});
