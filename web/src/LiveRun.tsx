import { useEffect, useRef, useState } from "react";
import { Link } from "react-router";
import { runEvents, type RunEvent, type TaskStatus } from "./api";
import { taskLink } from "./links";
import { EventList, totals } from "./Runs";
import { Loading, Skeleton } from "./ui/Skeleton";
import { usePolling } from "./usePolling";

// LiveRun is the run in progress: its events, read again every four seconds
// while the page is visible (so an event shows within five while a read
// takes up to one), and its token totals. The list is no live region, so a
// screen reader is not read each poll; RunEnded says when the run ends. A
// linked run (?run=<id>) is scrolled into view once.
export function LiveRun({ ns, name, run, linked }: { ns: string; name: string; run: NonNullable<TaskStatus["run"]>; linked: boolean }) {
  const [events, setEvents] = useState<RunEvent[] | null>(null);
  const [error, setError] = useState("");
  const card = useRef<HTMLElement>(null);
  const scrolled = useRef(false);
  usePolling(
    async (signal) => {
      try {
        setEvents((await runEvents(ns, name, run.id, signal)).items);
        setError("");
      } catch (e) {
        if (!signal.aborted) setError((e as Error).message);
      }
    },
    4000,
    [ns, name, run.id],
  );
  useEffect(() => {
    if (!linked || scrolled.current) return;
    scrolled.current = true;
    card.current?.scrollIntoView?.({ block: "start" });
  }, [linked]);
  const t = totals(events ?? []);
  const n = (v: number) => v.toLocaleString();
  return (
    <section className="card live-run" aria-labelledby="live-run" ref={card}>
      <div className="card-head">
        <h2 id="live-run">Run in progress</h2>
      </div>
      <div className="card-body live-run-about">
        <Link to={taskLink(ns, name, run.id)} aria-label={`Run ${run.id}`}>
          <code title={run.id}>{run.id}</code>
        </Link>
        <span>
          {run.mode ?? "runtime"} on {run.runnerName ?? "a runner"}
          {run.step ? `, step ${run.step}` : ""}
        </span>
        {events && (
          <span className="live-run-totals">
            {n(t.inputTokens)} in · {n(t.outputTokens)} out · context {n(t.contextTokens)}
            {Boolean(t.cacheReadInputTokens || t.cacheCreationInputTokens) && (
              <>
                {" "}
                · cache {n(t.cacheReadInputTokens)} read, {n(t.cacheCreationInputTokens)} written
              </>
            )}
          </span>
        )}
      </div>
      <div className="events">
        {error && <p className="inline-error">Could not load the events: {error}</p>}
        {events === null && !error && (
          <Loading what="the run's events">
            <Skeleton width="70%" />
          </Loading>
        )}
        {events?.length === 0 && <p className="hint">No events yet.</p>}
        {events && events.length > 0 && <EventList events={events} />}
      </div>
    </section>
  );
}

// RunEnded is a visually hidden status that says "Run ended" when the run in
// progress goes away, and is cleared, silently, when another starts.
export function RunEnded({ run }: { run?: string }) {
  const [seen, setSeen] = useState(run);
  const [ended, setEnded] = useState(false);
  if (run !== seen) {
    setSeen(run);
    setEnded(Boolean(seen) && !run);
  }
  return (
    <p role="status" aria-atomic="true" className="sr-only">
      {ended ? "Run ended" : ""}
    </p>
  );
}
