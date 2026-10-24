import { useEffect, useState } from "react";
import { listRuns, runEvents, ulidTime, type RunEvent, type RunRecord } from "./api";

// Runs lists a task's ended runs, read again when their count changes.
// Expanding one reads its events once.
export function Runs({ ns, name, count }: { ns: string; name: string; count: number }) {
  const [runs, setRuns] = useState<RunRecord[] | null>(null);
  const [error, setError] = useState("");
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
  }, [ns, name, count]);
  return (
    <section className="runs" aria-label="Runs">
      <h2>Runs</h2>
      {error && <p className="error">Could not load the runs: {error}</p>}
      {runs?.length === 0 && <p className="hint">No run has ended yet.</p>}
      <ol>
        {runs?.map((r) => (
          <Run key={r.id} ns={ns} name={name} run={r} />
        ))}
      </ol>
    </section>
  );
}

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
    } catch (e) {
      setError((e as Error).message);
    }
  };
  return (
    <li className="run">
      <button type="button" className="run-head" aria-expanded={open} onClick={toggle}>
        <code>{run.id}</code> {run.mode} on {run.runnerName || run.runner || "a runner"}: {run.reason}
        {run.outcome ? ` (${run.outcome})` : ""}
      </button>
      <div className="hint">
        {started && <>started {started.toLocaleString()}, </>}ended {new Date(run.endedAt).toLocaleString()}
      </div>
      {open && (
        <div className="events">
          {error && <p className="error">{error}</p>}
          {events?.length === 0 && <p className="hint">No events.</p>}
          {events && events.length > 0 && (
            <ol>
              {events.map((e) => (
                <li key={e.seq}>
                  #{e.seq} {e.progress ? <>progress: {e.progress}</> : "usage"}
                  {(e.inputTokens || e.outputTokens) && (
                    <span className="hint">
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
