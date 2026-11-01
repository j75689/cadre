import { act, cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import type { ConfigResource } from "./api";
import { FakeEventSource, fakeTabs, json, team } from "./testing";
import { hintDelayMs } from "./usePolling";

const runId = "01JRUN0000000000000000000A";
const resource = (kind: string, name: string): ConfigResource => ({
  name,
  kind,
  ready: true,
  generation: 1,
  observedGeneration: 1,
  conditions: [],
  metadata: { uid: "u-1", resourceVersion: "1", creationTimestamp: "2026-10-25T08:00:00Z" },
});

// reads counts the reads of each route the pages under test poll, by
// what tells them apart.
let reads: Record<string, number> = {};
const routes: [string, (url: string) => boolean, () => unknown][] = [
  ["subtasks", (u) => u.includes("parent="), () => ({ items: [], limit: 50 })],
  ["board", (u) => u.includes("/namespaces/a/tasks?"), () => ({ items: [], limit: 50 })],
  ["events", (u) => u.endsWith("/events"), () => ({ items: [] })],
  ["thread", (u) => u.includes("/thread"), () => ({ items: [] })],
  ["runs", (u) => u.endsWith("/runs"), () => ({ items: [] })],
  ["task", (u) => u.endsWith("/namespaces/a/tasks/x"), () => ({ metadata: { name: "x", namespace: "a" }, spec: { title: "Task x" }, status: { phase: "Running", run: { id: runId, mode: "runtime" } } })],
  ["team", (u) => u.endsWith("/namespaces/a/team"), () => team()],
  ["agents", (u) => u.endsWith("/namespaces/a/agents"), () => ({ items: [] })],
  ["agent", (u) => u.endsWith("/namespaces/a/agents/writer"), () => resource("Agent", "writer")],
  ["agentspec", (u) => u.endsWith("/namespaces/a/agents/planner"), () => ({ ...resource("Agent", "planner"), spec: { modelRef: "claude" } })],
  ["modelproviders", (u) => u.endsWith("/namespaces/a/modelproviders"), () => ({ items: [] })],
  ["modelprovider", (u) => u.endsWith("/namespaces/a/modelproviders/claude"), () => resource("ModelProvider", "claude")],
  ["toolservers", (u) => u.endsWith("/namespaces/a/toolservers"), () => ({ items: [] })],
  ["toolserver", (u) => u.endsWith("/namespaces/a/toolservers/github"), () => resource("ToolServer", "github")],
  ["schedules", (u) => u.endsWith("/namespaces/a/schedules"), () => ({ items: [] })],
  ["schedule", (u) => u.endsWith("/namespaces/a/schedules/nightly"), () => resource("Schedule", "nightly")],
  ["notifiers", (u) => u.endsWith("/namespaces/a/notifiers"), () => ({ items: [] })],
  ["notifier", (u) => u.endsWith("/namespaces/a/notifiers/slack"), () => resource("Notifier", "slack")],
];

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  fakeTabs();
  reads = {};
  vi.stubGlobal("fetch", (url: string) => {
    if (url.endsWith("/me")) return json({ mode: "none", admin: true });
    if (url.endsWith("/teams")) return json({ items: [team({ metadata: { name: "team", namespace: "a" } })] });
    const route = routes.find(([, is]) => is(url));
    if (!route) return new Promise(() => {});
    reads[route[0]] = (reads[route[0]] ?? 0) + 1;
    return json(route[2]());
  });
});
afterEach(async () => {
  cleanup();
  await vi.advanceTimersByTimeAsync(0);
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

// open renders path in team a and gives its stream a hello serving
// everything; it returns the stream once the page has been read.
async function open(path: string, read: string) {
  render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
  // A task page reopens its stream with ?task= once its run in progress
  // shows, which reads the run's events.
  await vi.waitFor(() => expect(reads[path.includes("/tasks/") ? "events" : read]).toBeGreaterThan(0));
  await act(() => vi.advanceTimersByTimeAsync(0));
  const streams = FakeEventSource.open();
  expect(streams).toHaveLength(1);
  act(() => streams[0].emit("hello", { tasks: true, config: true, runs: streams[0].url.includes("?task="), lifetimeSeconds: 300 }));
  await act(() => vi.advanceTimersByTimeAsync(500));
  return streams[0];
}

// after counts read's reads that the event brings within a second.
async function after(stream: FakeEventSource, read: string, event: string, data: object) {
  const before = reads[read] ?? 0;
  act(() => stream.emit(event, data));
  await act(() => vi.advanceTimersByTimeAsync(hintDelayMs + 50));
  return (reads[read] ?? 0) - before;
}

describe("Live pages", () => {
  it.each([
    // page, the route it reads, a hint for it, a hint not for it
    ["/t/a", "board", ["task", { name: "y", number: 2, phase: "Todo" }], ["config", { kind: "Agent", name: "writer" }]],
    ["/t/a?view=list", "board", ["task", { name: "y", number: 2, phase: "Todo" }], ["config", { kind: "Team", name: "team" }]],
    ["/t/a/tasks/x", "task", ["task", { name: "x", number: 1, phase: "Review" }], ["task", { name: "y", number: 2, phase: "Todo" }]],
    ["/t/a/tasks/x", "subtasks", ["task", { name: "y", number: 2, phase: "Done", parent: "x" }], ["task", { name: "z", number: 3, phase: "Done", parent: "w" }]],
    ["/t/a/tasks/x", "events", ["run", { task: "x", run: runId, seq: 4 }], ["run", { task: "x", run: "01JOTHER", seq: 1 }]],
    ["/t/a/agents", "agents", ["config", { kind: "Agent", name: "writer" }], ["config", { kind: "ModelProvider", name: "writer" }]],
    ["/t/a/agents/writer", "agent", ["config", { kind: "Agent", name: "writer" }], ["config", { kind: "Agent", name: "reader" }]],
    ["/t/a/modelproviders", "modelproviders", ["config", { kind: "ModelProvider", name: "claude" }], ["config", { kind: "ToolServer", name: "claude" }]],
    ["/t/a/modelproviders/claude", "modelprovider", ["config", { kind: "ModelProvider", name: "claude" }], ["config", { kind: "Agent", name: "claude" }]],
    ["/t/a/toolservers", "toolservers", ["config", { kind: "ToolServer", name: "github" }], ["config", { kind: "ModelProvider", name: "github" }]],
    ["/t/a/toolservers/github", "toolserver", ["config", { kind: "ToolServer", name: "github" }], ["config", { kind: "ToolServer", name: "jira" }]],
    ["/t/a/schedules", "schedules", ["config", { kind: "Schedule", name: "nightly" }], ["config", { kind: "ToolServer", name: "nightly" }]],
    ["/t/a/schedules/nightly", "schedule", ["config", { kind: "Schedule", name: "nightly" }], ["config", { kind: "Schedule", name: "weekly" }]],
    ["/t/a/agents/planner", "schedules", ["config", { kind: "Schedule", name: "weekly" }], ["config", { kind: "Agent", name: "writer" }]],
    ["/t/a/notifiers", "notifiers", ["config", { kind: "Notifier", name: "slack" }], ["config", { kind: "ToolServer", name: "slack" }]],
    ["/t/a/notifiers/slack", "notifier", ["config", { kind: "Notifier", name: "slack" }], ["config", { kind: "Notifier", name: "pager" }]],
    ["/t/a/settings", "team", ["config", { kind: "Team", name: "team" }], ["config", { kind: "Agent", name: "team" }]],
  ] as const)("%s reads %s again on its hint, within a second, and not on another's", async (path, read, [event, data], [otherEvent, otherData]) => {
    const stream = await open(path, read);
    expect(await after(stream, read, otherEvent, otherData)).toBe(0);
    expect(await after(stream, read, event, data)).toBeGreaterThan(0);
  });

  it.each([
    ["/t/a", "board"],
    ["/t/a/tasks/x", "task"],
    ["/t/a/agents", "agents"],
    ["/t/a/settings", "team"],
  ])("%s reads %s again on resync", async (path, read) => {
    const stream = await open(path, read);
    expect(await after(stream, read, "resync", {})).toBeGreaterThan(0);
  });

  it("opens one stream for the task page, with ?task= for its run in progress", async () => {
    const stream = await open("/t/a/tasks/x", "events");
    expect(stream.url).toBe("/api/v1/namespaces/a/watch?task=x");
  });

  it("polls the board every five seconds as before when the stream is refused", async () => {
    render(
      <MemoryRouter initialEntries={["/t/a"]}>
        <App />
      </MemoryRouter>,
    );
    await vi.waitFor(() => expect(reads.board).toBeGreaterThan(0));
    await act(() => vi.advanceTimersByTimeAsync(0));
    // An older server has no watch route: a 404.
    act(() => FakeEventSource.open()[0].fail(true));
    const before = reads.board;
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(reads.board - before).toBe(7);
    expect(screen.getByRole("region", { name: "Todo" })).toBeTruthy();
  });

  it.each([
    ["/t/a/notifiers", "notifiers"],
    ["/t/a/notifiers/slack", "notifier"],
  ])("%s polls %s every 15 seconds when the stream is refused", async (path, read) => {
    render(
      <MemoryRouter initialEntries={[path]}>
        <App />
      </MemoryRouter>,
    );
    await vi.waitFor(() => expect(reads[read]).toBeGreaterThan(0));
    await act(() => vi.advanceTimersByTimeAsync(0));
    act(() => FakeEventSource.open()[0].fail(true));
    const before = reads[read];
    await act(() => vi.advanceTimersByTimeAsync(15000));
    expect(reads[read] - before).toBe(1);
  });
});
