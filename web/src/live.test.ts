import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { maxStreams, restart, retryMs, subscribe, tab, type Subscriber, type Tab, type Topic } from "./live";
import { FakeEventSource, fakeTabs } from "./testing";

const hello = { tasks: true, config: true, runs: false, lifetimeSeconds: 300 };

let offs: (() => void)[] = [];
let tabs: Tab[] = [];

beforeEach(() => {
  vi.useFakeTimers();
  fakeTabs();
});
afterEach(async () => {
  offs.forEach((off) => off());
  offs = [];
  tabs.forEach((t) => t.leave());
  tabs = [];
  await vi.runAllTimersAsync();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

// sub subscribes to ns's stream and records what it is told: hints as a
// count, and whether it is live.
// With t, it is one of another tab.
function sub(ns: string, topic: Topic, extra: Partial<Subscriber> = {}, t?: Tab) {
  const got = { hints: 0, live: false };
  const off = (t?.subscribe ?? subscribe)(ns, { topic, onHint: () => got.hints++, onLive: (on) => (got.live = on), ...extra });
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
    // The hello is one each.
    s.emit("task", { name: "y", number: 2, phase: "Todo" });
    expect([board.got.hints, page.got.hints, agents.got.hints, run.got.hints]).toEqual([2, 1, 1, 1]);
    s.emit("task", { name: "x", number: 1, phase: "Running" });
    expect([board.got.hints, page.got.hints, agents.got.hints, run.got.hints]).toEqual([3, 2, 1, 1]);
    s.emit("config", { kind: "ModelProvider", name: "claude" });
    s.emit("config", { kind: "Agent", name: "writer" });
    expect([board.got.hints, page.got.hints, agents.got.hints, run.got.hints]).toEqual([3, 2, 2, 1]);
    s.emit("run", { task: "x", run: "r1", seq: 4 });
    expect([board.got.hints, page.got.hints, agents.got.hints, run.got.hints]).toEqual([3, 2, 2, 2]);
  });

  it("gives no hint to a topic hello did not serve", async () => {
    const agents = sub("team-a", "config");
    await settle();
    only().emit("hello", { ...hello, config: false });
    only().emit("config", { kind: "Agent", name: "writer" });
    only().emit("resync");
    expect(agents.got.hints).toBe(0);
  });

  it("tells every subscriber on resync, and at every hello, as hints before it were not seen", async () => {
    const board = sub("team-a", "tasks");
    const page = sub("team-a", "tasks", { match: () => false });
    const agents = sub("team-a", "config", { match: () => false });
    await settle();
    const s = only();
    s.emit("hello", hello);
    expect([board.got.hints, page.got.hints, agents.got.hints]).toEqual([1, 1, 1]);
    s.emit("resync");
    expect([board.got.hints, page.got.hints, agents.got.hints]).toEqual([2, 2, 2]);
    s.fail();
    s.emit("hello", hello);
    expect([board.got.hints, page.got.hints, agents.got.hints]).toEqual([3, 3, 3]);
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
    expect(board.got.hints).toBe(2);
  });

  it.each(["no_role", "no_team"])("closes the stream for good after end %s", async (reason) => {
    const board = sub("team-a", "tasks");
    await settle();
    only().emit("hello", hello);
    only().emit("end", { reason });
    expect(FakeEventSource.open()).toHaveLength(0);
    expect(board.got.live).toBe(false);
    // Neither another page of the team nor time opens it again.
    sub("team-a", "config");
    await vi.advanceTimersByTimeAsync(retryMs);
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it("polls after a refused open (401, 403, 404 or 503), and opens the stream again a minute later", async () => {
    const board = sub("team-a", "tasks");
    await settle();
    only().fail(true);
    expect(board.got.live).toBe(false);
    sub("team-a", "config");
    await vi.advanceTimersByTimeAsync(retryMs - 1);
    expect(FakeEventSource.open()).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(1);
    only().emit("hello", hello);
    expect(board.got.live).toBe(true);
  });

  it("opens nothing where there is no EventSource", async () => {
    vi.stubGlobal("EventSource", undefined);
    const board = sub("team-a", "tasks");
    await settle();
    expect(FakeEventSource.instances).toHaveLength(0);
    expect(board.got.live).toBe(false);
  });
});

describe("subscribe in many tabs", () => {
  // other opens another tab of the profile: it waits for the lock this
  // tab holds.
  const other = () => {
    const t = tab();
    tabs.push(t);
    return t;
  };

  it("holds one stream for every tab of the profile, and gives each tab its hints", async () => {
    const pages = [sub("team-a", "tasks")];
    for (let i = 1; i < 8; i++) pages.push(sub("team-a", "tasks", {}, other()));
    await settle();
    only().emit("hello", hello);
    await settle();
    expect(pages.map((p) => p.got.live)).toEqual(Array(8).fill(true));
    only().emit("task", { name: "x", phase: "Todo" });
    await settle();
    // One read at the hello, one at the hint.
    expect(pages.map((p) => p.got.hints)).toEqual(Array(8).fill(2));
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it("holds maxStreams streams at most, for the teams of the tabs that asked last", async () => {
    const teams = ["team-a", "team-b", "team-c"];
    const pages = [];
    for (const ns of teams) {
      pages.push(sub(ns, "tasks", {}, other()));
      await vi.advanceTimersByTimeAsync(1);
    }
    expect(maxStreams).toBe(2);
    expect(FakeEventSource.open().map((s) => s.url)).toEqual(["/api/v1/namespaces/team-b/watch", "/api/v1/namespaces/team-c/watch"]);
    FakeEventSource.open().forEach((s) => s.emit("hello", hello));
    await settle();
    expect(pages.map((p) => p.got.live)).toEqual([false, true, true]);
  });

  it("opens the stream with ?task= of the tab that asked last; the other's run polls", async () => {
    const x = sub("team-a", "runs", { task: "x" });
    await vi.advanceTimersByTimeAsync(1);
    const y = sub("team-a", "runs", { task: "y" }, other());
    await settle();
    expect(only().url).toBe("/api/v1/namespaces/team-a/watch?task=y");
    only().emit("hello", { ...hello, runs: true });
    await settle();
    expect([x.got.live, y.got.live]).toEqual([false, true]);
  });

  it("hands the stream over when the leader's tab goes, polling till the new one's hello", async () => {
    sub("team-a", "tasks");
    const page = sub("team-a", "tasks", {}, other());
    await settle();
    const first = only();
    first.emit("hello", hello);
    await settle();
    expect(page.got).toEqual({ live: true, hints: 1 });
    // The leader's tab closes, or reloads.
    offs = [];
    restart();
    await settle();
    expect(first.closed).toBe(true);
    expect(page.got.live).toBe(false);
    const next = only();
    expect(next).not.toBe(first);
    next.emit("hello", hello);
    expect(page.got).toEqual({ live: true, hints: 2 });
    next.emit("task", { name: "x", phase: "Todo" });
    expect(page.got.hints).toBe(3);
  });

  // away and back are a page that is left and kept in the back/forward
  // cache, and shown from it again, on page (window for this tab).
  const away = (page: EventTarget = window) => page.dispatchEvent(new PageTransitionEvent("pagehide", { persisted: true }));
  const back = (page: EventTarget = window) => page.dispatchEvent(new PageTransitionEvent("pageshow", { persisted: true }));

  it("hands the stream over when the leader's page is left for the back/forward cache, and takes it back there when shown", async () => {
    const leader = sub("team-a", "tasks");
    const t = tab(new EventTarget());
    tabs.push(t);
    const page = sub("team-a", "tasks", {}, t);
    await settle();
    const first = only();
    first.emit("hello", hello);
    await settle();
    expect(page.got).toEqual({ live: true, hints: 1 });
    // The leader's tab goes to another URL; the browser keeps the page.
    away();
    await settle();
    expect(first.closed).toBe(true);
    expect(leader.got.live).toBe(false);
    // The other tab polls at once, not on a hello of a stream gone.
    expect(page.got.live).toBe(false);
    const next = only();
    expect(next).not.toBe(first);
    next.emit("hello", hello);
    await settle();
    expect(page.got).toEqual({ live: true, hints: 2 });
    // Back: the page asks the new leader again, which keeps its one stream.
    back();
    await settle();
    expect(only()).toBe(next);
    expect(leader.got.live).toBe(true);
    next.emit("task", { name: "x", phase: "Todo" });
    await settle();
    expect([leader.got.hints, page.got.hints]).toEqual([3, 3]);
  });

  it("polls when the leader's page is left and no tab takes the lock yet", async () => {
    sub("team-a", "tasks");
    // The tab next in line for the lock is frozen: it gets it late.
    const locks = navigator.locks;
    Object.defineProperty(navigator, "locks", { value: { request: () => new Promise(() => {}) }, configurable: true });
    const t = tab(new EventTarget());
    tabs.push(t);
    Object.defineProperty(navigator, "locks", { value: locks, configurable: true });
    const page = sub("team-a", "tasks", {}, t);
    await settle();
    only().emit("hello", hello);
    await settle();
    expect(page.got.live).toBe(true);
    away();
    await settle();
    expect(FakeEventSource.open()).toHaveLength(0);
    expect(page.got.live).toBe(false);
  });

  it("takes the lock again in a page shown from the back/forward cache with no other tab", async () => {
    const board = sub("team-a", "tasks");
    await settle();
    const first = only();
    away();
    await settle();
    expect(FakeEventSource.open()).toHaveLength(0);
    back();
    await settle();
    const next = only();
    expect(next).not.toBe(first);
    next.emit("hello", hello);
    expect(board.got.live).toBe(true);
  });

  it("gives the lock to no page in the back/forward cache, and serves it again when shown", async () => {
    sub("team-a", "tasks");
    const cached = new EventTarget();
    const t = tab(cached);
    tabs.push(t);
    const page = sub("team-b", "tasks", {}, t);
    await settle();
    expect(FakeEventSource.open()).toHaveLength(2);
    // The waiting tab is left: its stream closes, and it stops waiting.
    away(cached);
    await settle();
    expect(only().url).toBe("/api/v1/namespaces/team-a/watch");
    // The leader goes, and a third tab takes the lock, not the cached one.
    const third = tab(new EventTarget());
    tabs.push(third);
    sub("team-a", "tasks", {}, third);
    offs = offs.slice(1);
    restart();
    await settle();
    only().emit("hello", hello);
    await settle();
    // Shown again, it asks the third tab: 2 streams, its own live.
    back(cached);
    await settle();
    expect(FakeEventSource.open().map((s) => s.url)).toEqual(["/api/v1/namespaces/team-a/watch", "/api/v1/namespaces/team-b/watch"]);
    FakeEventSource.open()[1].emit("hello", hello);
    await settle();
    expect(page.got.live).toBe(true);
  });

  it("polls in a tab the new leader holds no stream for", async () => {
    let visibility: DocumentVisibilityState = "visible";
    vi.spyOn(document, "visibilityState", "get").mockImplementation(() => visibility);
    sub("team-a", "tasks");
    const page = sub("team-a", "tasks", {}, other());
    await settle();
    only().emit("hello", hello);
    await settle();
    expect(page.got.live).toBe(true);
    // Both tabs are hidden; the leader's goes, and the other asks for none.
    visibility = "hidden";
    offs = [];
    restart();
    await settle();
    expect(FakeEventSource.open()).toHaveLength(0);
    expect(page.got.live).toBe(false);
    vi.restoreAllMocks();
  });

  it("asks for no stream while the tab is hidden", async () => {
    let visibility: DocumentVisibilityState = "visible";
    vi.spyOn(document, "visibilityState", "get").mockImplementation(() => visibility);
    sub("team-a", "tasks");
    await settle();
    const first = only();
    visibility = "hidden";
    // At once: a tab being closed runs no more timers.
    document.dispatchEvent(new Event("visibilitychange"));
    expect(first.closed).toBe(true);
    expect(FakeEventSource.open()).toHaveLength(0);
    visibility = "visible";
    document.dispatchEvent(new Event("visibilitychange"));
    await settle();
    expect(only()).not.toBe(first);
    vi.restoreAllMocks();
  });

  it.each([
    ["Web Locks (not a secure context)", { locks: false }],
    ["BroadcastChannel", { channel: false }],
  ])("opens no stream in any tab without %s", async (_, missing) => {
    fakeTabs(missing);
    const pages = [sub("team-a", "tasks"), sub("team-a", "tasks", {}, other()), sub("team-a", "tasks", {}, other())];
    await vi.advanceTimersByTimeAsync(1000);
    expect(FakeEventSource.instances).toHaveLength(0);
    expect(pages.map((p) => p.got.live)).toEqual([false, false, false]);
  });
});
