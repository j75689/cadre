import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { subscribe, type Subscriber, type Topic } from "./live";
import { FakeEventSource } from "./testing";

const hello = { tasks: true, config: true, runs: false, lifetimeSeconds: 300 };

let offs: (() => void)[] = [];

beforeEach(() => {
  vi.useFakeTimers();
  FakeEventSource.instances = [];
  vi.stubGlobal("EventSource", FakeEventSource);
});
afterEach(async () => {
  offs.forEach((off) => off());
  offs = [];
  await vi.runAllTimersAsync();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

// sub subscribes to ns's stream and records what it is told: hints as a
// count, and whether it is live.
function sub(ns: string, topic: Topic, extra: Partial<Subscriber> = {}) {
  const got = { hints: 0, live: false };
  const off = subscribe(ns, { topic, onHint: () => got.hints++, onLive: (on) => (got.live = on), ...extra });
  offs.push(off);
  return { got, off };
}

// settle lets the stream be opened, reopened or closed.
const settle = () => vi.advanceTimersByTimeAsync(0);

const only = () => {
  expect(FakeEventSource.open()).toHaveLength(1);
  return FakeEventSource.open()[0];
};

describe("subscribe", () => {
  it("shares one stream per namespace, and closes it when the last subscriber goes", async () => {
    const a = sub("team-a", "tasks");
    const b = sub("team-a", "config");
    await settle();
    expect(only().url).toBe("/api/v1/namespaces/team-a/watch");
    sub("team-b", "tasks");
    await settle();
    expect(FakeEventSource.open().map((s) => s.url)).toEqual(["/api/v1/namespaces/team-a/watch", "/api/v1/namespaces/team-b/watch"]);
    a.off();
    await settle();
    expect(FakeEventSource.open()).toHaveLength(2);
    b.off();
    await settle();
    expect(only().url).toBe("/api/v1/namespaces/team-b/watch");
  });

  it("keeps the stream for a subscriber that leaves and comes back at once", async () => {
    const a = sub("team-a", "tasks");
    await settle();
    a.off();
    sub("team-a", "tasks");
    await settle();
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(only()).toBe(FakeEventSource.instances[0]);
  });

  it("opens the stream with ?task= for a run, and again when the task changes", async () => {
    sub("team-a", "tasks");
    const run = sub("team-a", "runs", { task: "release notes" });
    await settle();
    expect(only().url).toBe("/api/v1/namespaces/team-a/watch?task=release%20notes");
    run.off();
    sub("team-a", "runs", { task: "y" });
    await settle();
    expect(only().url).toBe("/api/v1/namespaces/team-a/watch?task=y");
    expect(FakeEventSource.instances[0].closed).toBe(true);
  });

  it("makes a topic live when hello serves it", async () => {
    const tasks = sub("team-a", "tasks");
    const config = sub("team-a", "config");
    const runs = sub("team-a", "runs", { task: "x" });
    await settle();
    expect([tasks.got.live, config.got.live, runs.got.live]).toEqual([false, false, false]);
    only().emit("hello", { tasks: true, config: false, runs: true, lifetimeSeconds: 300 });
    expect([tasks.got.live, config.got.live, runs.got.live]).toEqual([true, false, true]);
    // One that subscribes later is told at once.
    expect(sub("team-a", "tasks").got.live).toBe(true);
  });

  it("gives each hint to the subscribers of its topic that match it", async () => {
    const board = sub("team-a", "tasks");
    const page = sub("team-a", "tasks", { match: (h) => h.event === "task" && h.name === "x" });
    const agents = sub("team-a", "config", { match: (h) => h.event === "config" && h.kind === "Agent" });
    const run = sub("team-a", "runs", { task: "x", match: (h) => h.event === "run" && h.run === "r1" });
    await settle();
    const s = only();
    s.emit("hello", { ...hello, runs: true });
    s.emit("task", { name: "y", number: 2, phase: "Todo" });
    expect([board.got.hints, page.got.hints, agents.got.hints, run.got.hints]).toEqual([1, 0, 0, 0]);
    s.emit("task", { name: "x", number: 1, phase: "Running" });
    expect([board.got.hints, page.got.hints, agents.got.hints, run.got.hints]).toEqual([2, 1, 0, 0]);
    s.emit("config", { kind: "ModelProvider", name: "claude" });
    s.emit("config", { kind: "Agent", name: "writer" });
    expect([board.got.hints, page.got.hints, agents.got.hints, run.got.hints]).toEqual([2, 1, 1, 0]);
    s.emit("run", { task: "x", run: "r1", seq: 4 });
    expect([board.got.hints, page.got.hints, agents.got.hints, run.got.hints]).toEqual([2, 1, 1, 1]);
  });

  it("gives no hint to a topic hello did not serve", async () => {
    const agents = sub("team-a", "config");
    await settle();
    only().emit("hello", { ...hello, config: false });
    only().emit("config", { kind: "Agent", name: "writer" });
    only().emit("resync");
    expect(agents.got.hints).toBe(0);
  });

  it("tells every subscriber on resync, and on a hello after a reconnect", async () => {
    const board = sub("team-a", "tasks");
    const page = sub("team-a", "tasks", { match: () => false });
    const agents = sub("team-a", "config", { match: () => false });
    await settle();
    const s = only();
    s.emit("hello", hello);
    expect([board.got.hints, page.got.hints, agents.got.hints]).toEqual([0, 0, 0]);
    s.emit("resync");
    expect([board.got.hints, page.got.hints, agents.got.hints]).toEqual([1, 1, 1]);
    s.fail();
    s.emit("hello", hello);
    expect([board.got.hints, page.got.hints, agents.got.hints]).toEqual([2, 2, 2]);
  });

  it("polls while a dropped stream reconnects, and is live again at its hello", async () => {
    const board = sub("team-a", "tasks");
    await settle();
    only().emit("hello", hello);
    only().fail();
    expect(board.got.live).toBe(false);
    only().emit("hello", hello);
    expect(board.got.live).toBe(true);
  });

  it.each(["lifetime", "shutdown"])("lets the stream reconnect after end %s", async (reason) => {
    const board = sub("team-a", "tasks");
    await settle();
    const s = only();
    s.emit("hello", hello);
    s.emit("end", { reason });
    // The server closes the stream; EventSource reconnects by itself.
    s.fail();
    expect(s.closed).toBe(false);
    expect(board.got.live).toBe(false);
    s.emit("hello", hello);
    expect(board.got.live).toBe(true);
    expect(board.got.hints).toBe(1);
  });

  it.each(["no_role", "no_team"])("closes the stream for good after end %s", async (reason) => {
    const board = sub("team-a", "tasks");
    await settle();
    only().emit("hello", hello);
    only().emit("end", { reason });
    expect(FakeEventSource.open()).toHaveLength(0);
    expect(board.got.live).toBe(false);
    // Another page of the team does not open it again.
    sub("team-a", "config");
    await settle();
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it("polls for good after a refused open (401, 403, 404 or 503)", async () => {
    const board = sub("team-a", "tasks");
    await settle();
    only().fail(true);
    expect(board.got.live).toBe(false);
    sub("team-a", "config");
    await vi.advanceTimersByTimeAsync(60_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(FakeEventSource.open()).toHaveLength(0);
  });

  it("opens nothing where there is no EventSource", async () => {
    vi.stubGlobal("EventSource", undefined);
    const board = sub("team-a", "tasks");
    await settle();
    expect(FakeEventSource.instances).toHaveLength(0);
    expect(board.got.live).toBe(false);
  });
});
