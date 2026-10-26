import { cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { usePolling } from "./usePolling";

beforeEach(() => vi.useFakeTimers());
afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

// A load that takes 100ms and, like fetch, rejects when aborted.
function slowLoad(starts: number[]) {
  return (signal: AbortSignal) =>
    new Promise<void>((resolve, reject) => {
      starts.push(Date.now());
      const t = setTimeout(resolve, 100);
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

  it("starts the next load when a load longer than the interval ends, not before", async () => {
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
    await vi.advanceTimersByTimeAsync(3100);
    // A timer of 0 fires a millisecond on.
    expect(starts.map((s) => Math.round((s - t0) / 10) * 10)).toEqual([0, 1500, 3000]);
    expect(most).toBe(1);
  });
});
