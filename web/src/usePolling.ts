import { useEffect, useRef } from "react";
import { subscribe, type Hint, type Topic } from "./live";

// livePollMs is how often a live page still reads, as hints may be lost.
export const livePollMs = 60000;
// hintDelayMs gathers the hints that come together into one read.
export const hintDelayMs = 100;

// Live names the watch stream's hints a page reads again on: those of topic
// in namespace ns that match (all of them without match), and for "runs"
// the task whose run it shows.
export interface Live {
  ns: string;
  topic: Topic;
  task?: string;
  match?: (hint: Hint) => boolean;
}

// usePolling calls load now and every interval while the page is visible,
// and once more when it becomes visible again. The interval counts from the
// start of a load, and loads never overlap: one that takes longer is
// followed a fifth of the interval after it ends, so a slow server is not
// read back to back. The signal ends a load that is overtaken or unmounted.
// With live, a matching hint calls load at once (after any load in flight),
// and while the topic is live the interval is livePollMs at least.
export function usePolling(load: (signal: AbortSignal) => Promise<void>, intervalMs: number, deps: unknown[], live?: Live) {
  const latest = useRef(load);
  const match = useRef(live?.match);
  useEffect(() => {
    latest.current = load;
    match.current = live?.match;
  });
  useEffect(() => {
    let controller: AbortController | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let hinted: ReturnType<typeof setTimeout> | undefined;
    let stopped = false;
    let streamed = false;
    // again is a hint that came during a load: read once more after it.
    let again = false;
    let started = 0;
    const schedule = () => {
      const period = streamed ? Math.max(intervalMs, livePollMs) : intervalMs;
      const now = Date.now();
      timer = setTimeout(tick, Math.max(started + period, now + intervalMs / 5) - now);
    };
    const tick = async () => {
      clearTimeout(timer);
      timer = undefined;
      controller?.abort();
      if (document.visibilityState === "hidden") {
        // The aborted tick is no longer current: it must not schedule.
        controller = undefined;
        return;
      }
      const mine = new AbortController();
      controller = mine;
      started = Date.now();
      try {
        await latest.current(mine.signal);
      } catch {
        // load reports its own errors
      } finally {
        // Only the current tick schedules the next: an overtaken one would
        // start a second chain.
        if (!stopped && controller === mine) {
          controller = undefined;
          if (again) {
            again = false;
            void tick();
          } else schedule();
        }
      }
    };
    const onVisible = () => {
      if (document.visibilityState === "visible") void tick();
    };
    const off =
      live &&
      subscribe(live.ns, {
        topic: live.topic,
        task: live.task,
        match: (hint) => match.current?.(hint) ?? true,
        onHint: () => {
          clearTimeout(hinted);
          hinted = setTimeout(() => {
            if (controller) again = true;
            else if (document.visibilityState === "visible") void tick();
          }, hintDelayMs);
        },
        onLive: (on) => {
          if (on === streamed) return;
          streamed = on;
          // The next read moves to the new interval.
          if (timer !== undefined) {
            clearTimeout(timer);
            schedule();
          }
        },
      });
    document.addEventListener("visibilitychange", onVisible);
    void tick();
    return () => {
      stopped = true;
      clearTimeout(timer);
      clearTimeout(hinted);
      controller?.abort();
      off?.();
      document.removeEventListener("visibilitychange", onVisible);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [intervalMs, live?.ns, live?.topic, live?.task, ...deps]);
}
