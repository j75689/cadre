import { act, cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import type { RunEvent } from "./api";
import { LiveRun, RunEnded } from "./LiveRun";
import { totals } from "./Runs";
import { json } from "./testing";

beforeEach(() => vi.useFakeTimers({ shouldAdvanceTime: true }));
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

const runId = "01JRUN0000000000000000000A";
const eventsPath = `/api/v1/namespaces/a/tasks/x/runs/${runId}/events`;

const event = (seq: number, extra: Partial<RunEvent> = {}): RunEvent => ({ runId, seq, ...extra });

// serveEvents answers the events read with what events holds then, and
// records the times it was read.
function serveEvents(events: RunEvent[]) {
  const t0 = Date.now();
  const reads: number[] = [];
  vi.stubGlobal("fetch", (url: string) => {
    if (url === eventsPath) reads.push(Date.now() - t0);
    return json({ items: events });
  });
  return reads;
}

function renderLive(linked = false) {
  return render(
    <MemoryRouter>
      <LiveRun ns="a" name="x" run={{ id: runId, mode: "runtime", runnerName: "r1", step: 3 }} linked={linked} />
    </MemoryRouter>,
  );
}

describe("LiveRun", () => {
  it("reads the events every four seconds while the page is visible", async () => {
    let visibility: DocumentVisibilityState = "visible";
    vi.spyOn(document, "visibilityState", "get").mockImplementation(() => visibility);
    const reads = serveEvents([]);
    renderLive();
    await act(() => vi.advanceTimersByTimeAsync(8000));
    expect(reads.map((t) => Math.round(t / 1000))).toEqual([0, 4, 8]);
    visibility = "hidden";
    document.dispatchEvent(new Event("visibilitychange"));
    await act(() => vi.advanceTimersByTimeAsync(20_000));
    expect(reads).toHaveLength(3);
  });

  it("shows the run, its events and the totals: tokens summed, the latest context", async () => {
    serveEvents([
      event(1, { inputTokens: 100, outputTokens: 10, contextTokens: 300 }),
      event(2, { progress: "reading the code" }),
      event(3, { inputTokens: 200, outputTokens: 20, contextTokens: 500, cacheReadInputTokens: 50 }),
    ]);
    renderLive();
    const card = screen.getByRole("region", { name: "Run in progress" });
    expect(await screen.findByText(/^300 in · 30 out · context 500/)).toBeTruthy();
    expect(card.textContent).toContain("runtime on r1, step 3");
    expect(card.textContent).toContain("cache 50 read, 0 written");
    expect(card.textContent).toContain("progress: reading the code");
    expect(screen.getByRole("link", { name: `Run ${runId}` }).getAttribute("href")).toBe(`/t/a/tasks/x?run=${runId}`);
    // The list is not a live region: polls are not read out.
    expect(card.querySelector("[aria-live], [role=status]")).toBeNull();
  });

  it("says when there is no event yet", async () => {
    serveEvents([]);
    renderLive();
    expect(await screen.findByText("No events yet.")).toBeTruthy();
  });

  it("scrolls into view once when linked", async () => {
    const scroll = vi.fn();
    Element.prototype.scrollIntoView = scroll;
    serveEvents([event(1)]);
    renderLive(true);
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    expect(scroll).toHaveBeenCalledTimes(1);
  });
});

describe("totals", () => {
  it("adds the tokens up and keeps the latest context size", () => {
    expect(
      totals([
        event(1, { inputTokens: 100, outputTokens: 10, contextTokens: 300, cacheCreationInputTokens: 7 }),
        event(2, { progress: "p" }),
        event(3, { inputTokens: 200, outputTokens: 20, contextTokens: 500, cacheReadInputTokens: 5 }),
      ]),
    ).toEqual({ inputTokens: 300, outputTokens: 30, cacheReadInputTokens: 5, cacheCreationInputTokens: 7, contextTokens: 500 });
  });
});

describe("RunEnded", () => {
  it("speaks only when the run in progress goes away", () => {
    const { rerender } = render(<RunEnded run={runId} />);
    const status = screen.getByRole("status");
    expect(status.textContent).toBe("");
    rerender(<RunEnded run={runId} />);
    expect(status.textContent).toBe("");
    rerender(<RunEnded />);
    expect(status.textContent).toBe("Run ended");
    rerender(<RunEnded run="01JRUN0000000000000000000B" />);
    expect(status.textContent).toBe("");
  });
});

describe("The task page", () => {
  it("drops the live card when the run ends", async () => {
    let status: Record<string, unknown> = { phase: "Running", run: { id: runId, mode: "runtime" }, runs: { count: 1 } };
    vi.stubGlobal("fetch", (url: string) => {
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      if (url.includes("/thread") || url.endsWith("/runs") || url.includes("/events") || url.includes("parent=")) return json({ items: [] });
      return json({ metadata: { name: "x", namespace: "a" }, spec: { title: "Task x" }, status });
    });
    localStorage.setItem("cadre.identity", "dana");
    render(
      <MemoryRouter initialEntries={["/t/a/tasks/x"]}>
        <App />
      </MemoryRouter>,
    );
    expect(await screen.findByRole("region", { name: "Run in progress" })).toBeTruthy();
    status = { phase: "Review", runs: { count: 1 } };
    await act(() => vi.advanceTimersByTimeAsync(5000));
    await vi.waitFor(() => expect(screen.queryByRole("region", { name: "Run in progress" })).toBeNull());
    expect(screen.getByText("Run ended")).toBeTruthy();
  });
});
