import { cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FakeEventSource } from "./testing";
import { hintDelayMs, livePollMs, usePolling, type Live } from "./usePolling";

beforeEach(() => vi.useFakeTimers());
afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

// A load that takes ms and, like fetch, rejects when aborted.
function slowLoad(starts: number[], ms = 100) {
  return (signal: AbortSignal) =>
    new Promise<void>((resolve, reject) => {
      starts.push(Date.now());
      const t = setTimeout(resolve, ms);
      signal.addEventListener("abort", () => {
        clearTimeout(t);
        reject(new DOMException("aborted", "AbortError"));
      });
    });
}

describe("usePolling", () => {
  it("keeps one chain when a tick is overtaken", async () => {
    const t0 = Date.now();
    const starts: number[] = [];
    renderHook(() => usePolling(slowLoad(starts), 1000, []));
    await vi.advanceTimersByTimeAsync(50);
    // Back to the tab mid-load: the load in flight is overtaken.
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(2300);
    // Loads at 0 and 50, then one interval after the start of each that
    // completed: 1050 and 2050. An overtaken tick scheduling too would add a
    // load at 1000.
    expect(starts.map((s) => s - t0)).toEqual([0, 50, 1050, 2050]);
  });

  it("schedules nothing from a tick aborted by hiding the tab", async () => {
    const t0 = Date.now();
    const starts: number[] = [];
    let visibility: DocumentVisibilityState = "visible";
    vi.spyOn(document, "visibilityState", "get").mockImplementation(() => visibility);
    renderHook(() => usePolling(slowLoad(starts), 1000, []));
    await vi.advanceTimersByTimeAsync(50);
    visibility = "hidden";
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(3000);
    expect(starts).toHaveLength(1);
    // Shown again: polling resumes from then.
    visibility = "visible";
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(1050);
    expect(starts.map((s) => s - t0)).toEqual([0, 3050, 4050]);
    vi.restoreAllMocks();
  });

  it("stops when unmounted", async () => {
    const starts: number[] = [];
    const { unmount } = renderHook(() => usePolling(slowLoad(starts), 1000, []));
    await vi.advanceTimersByTimeAsync(200);
    unmount();
    await vi.advanceTimersByTimeAsync(5000);
    expect(starts).toHaveLength(1);
  });

  it("starts a load every interval, counted from the start of the one before", async () => {
    const t0 = Date.now();
    const starts: number[] = [];
    renderHook(() => usePolling(slowLoad(starts), 1000, []));
    await vi.advanceTimersByTimeAsync(3050);
    expect(starts.map((s) => s - t0)).toEqual([0, 1000, 2000, 3000]);
  });

  it("waits a fifth of the interval after a load longer than the interval, never overlapping", async () => {
    const t0 = Date.now();
    const starts: number[] = [];
    let open = 0;
    let most = 0;
    const load = () =>
      new Promise<void>((resolve) => {
        starts.push(Date.now());
        most = Math.max(most, ++open);
        setTimeout(() => {
          open--;
          resolve();
        }, 1500);
      });
    renderHook(() => usePolling(load, 1000, []));
    await vi.advanceTimersByTimeAsync(3500);
    // Each load ends 1500ms after it starts; the next starts 200ms later.
    expect(starts.map((s) => s - t0)).toEqual([0, 1700, 3400]);
    expect(most).toBe(1);
  });

  it("waits a fifth of the interval after a load that ends less than that before the next is due", async () => {
    const t0 = Date.now();
    const starts: number[] = [];
    const load = () =>
      new Promise<void>((resolve) => {
        starts.push(Date.now());
        setTimeout(resolve, 3500);
      });
    renderHook(() => usePolling(load, 4000, []));
    await vi.advanceTimersByTimeAsync(9000);
    // Each ends at start + 3500; start + 4000 is less than 800ms after.
    expect(starts.map((s) => s - t0)).toEqual([0, 4300, 8600]);
  });

  it("keeps the interval after a load that fails", async () => {
    const t0 = Date.now();
    const starts: number[] = [];
    const load = async () => {
      starts.push(Date.now());
      throw new Error("boom");
    };
    renderHook(() => usePolling(load, 4000, []));
    await vi.advanceTimersByTimeAsync(9000);
    expect(starts.map((s) => s - t0)).toEqual([0, 4000, 8000]);
  });
});

describe("usePolling with live", () => {
  const hello = { tasks: true, config: true, runs: false, lifetimeSeconds: 300 };
  const board: Live = { ns: "team-a", topic: "tasks" };
  let t0 = 0;

  beforeEach(() => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    t0 = Date.now();
  });
  afterEach(async () => {
    cleanup();
    // The stream closes once the hook is gone.
    await vi.advanceTimersByTimeAsync(0);
    vi.unstubAllGlobals();
  });

  // live renders the hook with a load of loadMs, polling every 5s, opens
  // its stream and returns the loads' start times since t0 and the stream.
  async function live(opts: Live = board, helloData: object | null = hello, loadMs = 100) {
    const starts: number[] = [];
    const { unmount } = renderHook(() => usePolling(slowLoad(starts, loadMs), 5000, [], opts));
    await vi.advanceTimersByTimeAsync(0);
    const stream = FakeEventSource.open()[0];
    if (helloData) stream.emit("hello", helloData);
    await vi.advanceTimersByTimeAsync(100);
    return { at: () => starts.map((s) => s - t0), stream, unmount };
  }

  it("reads within a second of a matching hint", async () => {
    const { at, stream } = await live({ ...board, match: (h) => h.event === "task" && h.name === "x" });
    expect(at()).toEqual([0]);
    stream.emit("task", { name: "y", number: 2, phase: "Todo" });
    await vi.advanceTimersByTimeAsync(1000);
    expect(at()).toEqual([0]);
    stream.emit("task", { name: "x", number: 1, phase: "Done" });
    await vi.advanceTimersByTimeAsync(hintDelayMs);
    // The hint came at 1100: the load starts the debounce after, and its
    // 100ms make what it read show by 1300.
    expect(at()).toEqual([0, 1100 + hintDelayMs]);
    expect(hintDelayMs).toBeLessThanOrEqual(250);
  });

  it("makes hints that come together one read", async () => {
    const { at, stream } = await live();
    for (let i = 0; i < 5; i++) {
      stream.emit("task", { name: `t${i}`, phase: "Todo" });
      await vi.advanceTimersByTimeAsync(20);
    }
    await vi.advanceTimersByTimeAsync(500);
    expect(at()).toHaveLength(2);
  });

  it("reads once more after a load that a hint came during, without ending it", async () => {
    const { at, stream } = await live(board, hello, 500);
    // The first load runs till 500; hints now wait for it.
    stream.emit("task", { name: "x", phase: "Todo" });
    await vi.advanceTimersByTimeAsync(hintDelayMs + 50);
    stream.emit("task", { name: "x", phase: "Running" });
    await vi.advanceTimersByTimeAsync(1000);
    // One read after it, not one at 200 that ended it.
    expect(at()).toEqual([0, 500]);
  });

  it("polls every livePollMs while live, and at its interval again when the stream drops", async () => {
    const { at, stream } = await live();
    await vi.advanceTimersByTimeAsync(livePollMs);
    expect(at()).toEqual([0, livePollMs]);
    stream.fail();
    // Down, the next read is due an interval after the last one began.
    await vi.advanceTimersByTimeAsync(5000);
    expect(at()).toEqual([0, livePollMs, livePollMs + 5000]);
    // Reconnected: its hello reads at once, as hints may have been missed.
    stream.emit("hello", hello);
    await vi.advanceTimersByTimeAsync(hintDelayMs);
    expect(at()).toEqual([0, livePollMs, livePollMs + 5000, livePollMs + 5100 + hintDelayMs]);
  });

  it.each([
    ["a refused open", (s: FakeEventSource) => s.fail(true)],
    ["end no_role", (s: FakeEventSource) => s.emit("end", { reason: "no_role" })],
    ["end no_team", (s: FakeEventSource) => s.emit("end", { reason: "no_team" })],
    ["a dropped stream", (s: FakeEventSource) => s.fail()],
  ])("polls at its interval after %s", async (_, end) => {
    const { at, stream } = await live();
    end(stream);
    await vi.advanceTimersByTimeAsync(10_000);
    expect(at()).toEqual([0, 5000, 10_000]);
  });

  it("polls at its interval when hello does not serve the topic", async () => {
    const { at } = await live(board, { ...hello, tasks: false });
    await vi.advanceTimersByTimeAsync(10_000);
    expect(at()).toEqual([0, 5000, 10_000]);
  });

  it("polls at its interval, opening no stream, without live", async () => {
    const starts: number[] = [];
    renderHook(() => usePolling(slowLoad(starts), 5000, []));
    await vi.advanceTimersByTimeAsync(10_000);
    expect(starts.map((s) => s - t0)).toEqual([0, 5000, 10_000]);
    expect(FakeEventSource.instances).toHaveLength(0);
  });

  it("reads on no hint while the tab is hidden", async () => {
    let visibility: DocumentVisibilityState = "visible";
    vi.spyOn(document, "visibilityState", "get").mockImplementation(() => visibility);
    const { at, stream } = await live();
    visibility = "hidden";
    document.dispatchEvent(new Event("visibilitychange"));
    stream.emit("task", { name: "x", phase: "Todo" });
    await vi.advanceTimersByTimeAsync(livePollMs * 2);
    expect(at()).toEqual([0]);
    vi.restoreAllMocks();
  });

  it("closes the stream when unmounted", async () => {
    const { stream, unmount } = await live();
    unmount();
    await vi.advanceTimersByTimeAsync(0);
    expect(stream.closed).toBe(true);
  });
});
