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
    // Loads at 0 and 50, then one interval after each that completed: 1150
    // and 2250. An overtaken tick scheduling too would add loads at 1050,
    // 2150 and so on.
    expect(starts.map((s) => s - t0)).toEqual([0, 50, 1150, 2250]);
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
    await vi.advanceTimersByTimeAsync(1150);
    expect(starts.map((s) => s - t0)).toEqual([0, 3050, 4150]);
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
});
