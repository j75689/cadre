import { useEffect, useState } from "react";
import { listRuns, phases, runEvents, ulidTime, type Phase, type RunEvent, type RunRecord } from "./api";
import { Badge, phaseTone } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { Icon } from "./ui/Icon";
import { Loading, Skeleton } from "./ui/Skeleton";

// Runs lists a task's ended runs, read again when current (the run in
// progress and the phase) changes: a run is in the list once it is not the
// current one any more. Expanding one reads its events once.
export function Runs({ ns, name, current }: { ns: string; name: string; current: string }) {
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
            <Run key={r.id} ns={ns} name={name} run={r} />
          ))}
        </ol>
      )}
    </section>
  );
}

// outcomeTone is the hue of how a run ended: an outcome is the phase it
// sent the task to (Done, Review, Waiting or Failed).
const outcomeTone = (outcome: string) => (phases as readonly string[]).includes(outcome) ? phaseTone(outcome as Phase) : undefined;

function Run({ ns, name, run }: { ns: string; name: string; run: RunRecord }) {
  const [events, setEvents] = useState<RunEvent[] | null>(null);
  const [open, setOpen] = useState(false);
  const [error, setError] = useState("");
  const started = ulidTime(run.id);
  const toggle = async () => {
    setOpen(!open);
    if (events !== null || open) return;
    try {
      setEvents((await runEvents(ns, name, run.id)).items);
      setError("");
    } catch (e) {
      setError((e as Error).message);
    }
  };
  return (
    <li className="run">
      <button type="button" className="run-head" aria-expanded={open} onClick={toggle}>
        <Icon name={open ? "chevron-down" : "chevron-right"} />
        <span className="line">
          <code title={run.id}>{run.id}</code>
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
          {events && events.length > 0 && (
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
                </li>
              ))}
            </ol>
          )}
        </div>
      )}
    </li>
  );
}
