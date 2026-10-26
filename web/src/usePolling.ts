import { useEffect, useRef } from "react";

// usePolling calls load now and every interval while the page is visible,
// and once more when it becomes visible again. The interval counts from the
// start of a load, and loads never overlap: one that takes longer is
// followed a fifth of the interval after it ends, so a slow server is not
// read back to back. The signal ends a load that is overtaken or unmounted.
export function usePolling(load: (signal: AbortSignal) => Promise<void>, intervalMs: number, deps: unknown[]) {
  const latest = useRef(load);
  useEffect(() => {
    latest.current = load;
  });
  useEffect(() => {
    let controller: AbortController | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let stopped = false;
    const tick = async () => {
      clearTimeout(timer);
      controller?.abort();
      if (document.visibilityState === "hidden") {
        // The aborted tick is no longer current: it must not schedule.
        controller = undefined;
        return;
      }
      const mine = new AbortController();
      controller = mine;
      const started = Date.now();
      try {
        await latest.current(mine.signal);
      } catch {
        // load reports its own errors
      } finally {
        // Only the current tick schedules the next: an overtaken one would
        // start a second chain.
        if (!stopped && controller === mine) {
          const now = Date.now();
          timer = setTimeout(tick, Math.max(started + intervalMs, now + intervalMs / 5) - now);
        }
      }
    };
    const onVisible = () => {
      if (document.visibilityState === "visible") void tick();
    };
    document.addEventListener("visibilitychange", onVisible);
    void tick();
    return () => {
      stopped = true;
      clearTimeout(timer);
      controller?.abort();
      document.removeEventListener("visibilitychange", onVisible);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [intervalMs, ...deps]);
}
