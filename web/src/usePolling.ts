import { useEffect, useRef } from "react";

// usePolling calls load now and every interval while the page is visible,
// and once more when it becomes visible again. The signal ends a load that
// is overtaken or unmounted.
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
      try {
        await latest.current(mine.signal);
      } catch {
        // load reports its own errors
      } finally {
        // Only the current tick schedules the next: an overtaken one would
        // start a second chain.
        if (!stopped && controller === mine) timer = setTimeout(tick, intervalMs);
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
