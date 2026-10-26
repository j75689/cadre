import { useEffect, useRef, useState } from "react";
import { Link } from "react-router";
import { listRuns, phases, runEvents, ulidTime, type Phase, type RunEvent, type RunRecord } from "./api";
import { Badge, phaseTone } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { taskLink } from "./links";
import { Icon } from "./ui/Icon";
import { Loading, Skeleton } from "./ui/Skeleton";

// Runs lists a task's ended runs, read again when current (the run in
// progress and the phase) changes: a run is in the list once it is not the
// current one any more. Expanding one reads its events once; the run named
// by open (any case) is expanded.
export function Runs({ ns, name, current, open = "" }: { ns: string; name: string; current: string; open?: string }) {
  const [runs, setRuns] = useState<RunRecord[] | null>(null);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  useEffect(() => {
    const controller = new AbortController();
    listRuns(ns, name, controller.signal).then(
      (r) => {
        setRuns(r.items);
        setError("");
      },
      (e) => {
        if (!controller.signal.aborted) setError((e as Error).message);
      },
    );
    return () => controller.abort();
  }, [ns, name, current, retry]);
  return (
    <section className="card runs" aria-label="Runs">
      <div className="card-head">
        <h2>Runs</h2>
        {runs && <span className="count-pill">{runs.length}</span>}
      </div>
      {error && <Banner onRetry={() => setRetry((n) => n + 1)}>Could not load the runs: {error}</Banner>}
      {runs === null && !error && (
        <Loading what="the runs" className="card-body">
          <Skeleton width="80%" />
        </Loading>
      )}
      {runs?.length === 0 && <p className="card-body hint">No run has ended yet.</p>}
      {runs && runs.length > 0 && (
        <ol className="rows">
          {runs.map((r) => (
            <Run key={r.id} ns={ns} name={name} run={r} linked={open !== "" && r.id.toLowerCase() === open.toLowerCase()} />
          ))}
        </ol>
      )}
    </section>
  );
}

// outcomeTone is the hue of how a run ended: an outcome is the phase it
// sent the task to (Done, Review, Waiting or Failed).
const outcomeTone = (outcome: string) => (phases as readonly string[]).includes(outcome) ? phaseTone(outcome as Phase) : undefined;

// Run is one ended run. While it is linked (?run=<id>) it is open, its
// events read, and it is scrolled into view once each time it is linked:
// to the top when the page loads at it, and only as far as it takes later,
// so clicking the id of a run in view does not move the list.
function Run({ ns, name, run, linked }: { ns: string; name: string; run: RunRecord; linked: boolean }) {
  const [events, setEvents] = useState<RunEvent[] | null>(null);
  const [open, setOpen] = useState(linked);
  const [wasLinked, setWasLinked] = useState(linked);
  if (linked !== wasLinked) {
    setWasLinked(linked);
    if (linked) setOpen(true);
  }
  const [error, setError] = useState("");
  const row = useRef<HTMLLIElement>(null);
  const scrolled = useRef(false);
  const loaded = useRef(false);
  // asked keeps the events to one read: a row closed and opened again, or
  // linked again, while or after they are read reads none. A failed read
  // can be tried again.
  const asked = useRef(false);
  const started = ulidTime(run.id);
  const load = async () => {
    if (asked.current) return;
    asked.current = true;
    try {
      setEvents((await runEvents(ns, name, run.id)).items);
      setError("");
    } catch (e) {
      asked.current = false;
      setError((e as Error).message);
    }
  };
  useEffect(() => {
    const block = loaded.current ? "nearest" : "start";
    loaded.current = true;
    if (!linked) {
      scrolled.current = false;
      return;
    }
    if (scrolled.current) return;
    scrolled.current = true;
    row.current?.scrollIntoView?.({ block });
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [linked]);
  const toggle = () => {
    setOpen(!open);
    if (!open) void load();
  };
  return (
    <li className="run" ref={row}>
      {/* The id links to the run, so it sits outside the row's button. */}
      <Link className="run-link" to={taskLink(ns, name, run.id)} aria-label={`Run ${run.id}`}>
        <code title={run.id}>{run.id}</code>
      </Link>
      <button type="button" className="run-head" aria-expanded={open} onClick={toggle}>
        <Icon name={open ? "chevron-down" : "chevron-right"} />
        <span className="line">
          <span>
            {run.mode} on {run.runnerName || run.runner || "a runner"}: {run.reason}
          </span>
          {run.outcome && <Badge tone={outcomeTone(run.outcome)}>{run.outcome}</Badge>}
        </span>
        <span className="run-times">
          {started && <>started {started.toLocaleString()}, </>}ended {new Date(run.endedAt).toLocaleString()}
        </span>
      </button>
      {open && (
        <div className="events">
          {error && <p className="inline-error">{error}</p>}
          {events === null && !error && (
            <Loading what="the run's events">
              <Skeleton width="70%" />
            </Loading>
          )}
          {events?.length === 0 && <p className="hint">No events.</p>}
          {events && events.length > 0 && <EventList events={events} />}
        </div>
      )}
    </li>
  );
}

// EventList is a run's events: progress lines and usage, with the token
// counts an event has.
export function EventList({ events }: { events: RunEvent[] }) {
  return (
    <ol>
      {events.map((e) => (
        <li key={e.seq}>
          #{e.seq} {e.progress ? <>progress: {e.progress}</> : "usage"}
          {Boolean(e.inputTokens || e.outputTokens) && (
            <span className="muted">
              {" "}
              {e.inputTokens ?? 0} in, {e.outputTokens ?? 0} out
            </span>
          )}
          {Boolean(e.cacheReadInputTokens || e.cacheCreationInputTokens) && (
            <span className="muted">
              {" "}
              (cache {e.cacheReadInputTokens ?? 0} read, {e.cacheCreationInputTokens ?? 0} written)
            </span>
          )}
        </li>
      ))}
    </ol>
  );
}

// totals adds up a run's tokens. contextTokens is the size of the
// conversation after a reply, so it is the latest one, not a sum.
export function totals(events: RunEvent[]) {
  const t = { inputTokens: 0, outputTokens: 0, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, contextTokens: 0 };
  for (const e of events) {
    t.inputTokens += e.inputTokens ?? 0;
    t.outputTokens += e.outputTokens ?? 0;
    t.cacheReadInputTokens += e.cacheReadInputTokens ?? 0;
    t.cacheCreationInputTokens += e.cacheCreationInputTokens ?? 0;
    if (e.contextTokens) t.contextTokens = e.contextTokens;
  }
  return t;
}
