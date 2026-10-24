import { useRef, useState } from "react";
import { Link, useParams } from "react-router";
import { APIError, getTask, readThread, type Task, type ThreadEntry } from "./api";
import { Actions } from "./Actions";
import { RoleLine } from "./Identity";
import { Runs } from "./Runs";
import { Thread } from "./Thread";
import { usePolling } from "./usePolling";

// threadPage is how many thread entries one read asks for.
export const threadPage = 200;

// TaskPage shows one task: spec, status, thread, actions and runs. It polls
// the task, and the thread from the last entry it has, never from the
// start again.
export function TaskPage() {
  const { ns = "", name = "" } = useParams();
  const [task, setTask] = useState<Task | null>(null);
  const [entries, setEntries] = useState<ThreadEntry[]>([]);
  const lastSeq = useRef(0);
  const [error, setError] = useState("");
  // refresh restarts polling at once, after a conflict.
  const [refresh, setRefresh] = useState(0);

  // append adds what a read past the last entry held returned.
  const append = (more: ThreadEntry[]) => {
    const fresh = more.filter((e) => e.seq > lastSeq.current).sort((a, b) => a.seq - b.seq);
    if (fresh.length === 0) return;
    lastSeq.current = fresh[fresh.length - 1].seq;
    setEntries((old) => [...old, ...fresh]);
  };

  usePolling(
    async (signal) => {
      try {
        setTask(await getTask(ns, name, signal));
        for (;;) {
          const page = await readThread(ns, name, lastSeq.current, threadPage, signal);
          append(page.items);
          if (page.items.length < threadPage) break;
        }
        setError("");
      } catch (e) {
        if (signal.aborted) return;
        // A task that is gone shows no stale copy.
        if (e instanceof APIError && e.status === 404) setTask(null);
        setError(String((e as Error).message));
      }
    },
    5000,
    [ns, name, refresh],
  );

  const board = `/t/${encodeURIComponent(ns)}`;
  return (
    <main className="task">
      <p>
        <Link to={board}>← {ns}</Link>
      </p>
      {error && <p className="error">Could not load the task: {error}</p>}
      {task && (
        <>
          <h1>
            {task.status?.number !== undefined && <span className="number">#{task.status.number} </span>}
            {task.spec.title}
          </h1>
          <RoleLine ns={ns} />
          {task.spec.parentRef && (
            <p className="parent">
              subtask of <Link to={`${board}/tasks/${encodeURIComponent(task.spec.parentRef.name)}`}>{task.spec.parentRef.name}</Link>
            </p>
          )}
          <div className="task-layout">
            <section className="task-main">
              {/* Plain text: markdown needs a renderer and sanitising. */}
              {task.spec.description && <pre className="description">{task.spec.description}</pre>}
              <h2>Thread</h2>
              <Thread entries={entries} waitingOn={task.status?.waiting?.threadSeq} />
              <Actions
                task={task}
                onTask={setTask}
                onRefresh={() => setRefresh((n) => n + 1)}
              />
            </section>
            <aside className="task-side">
              <Details task={task} />
              {/* A run ends when the current run goes away or is replaced; the
                  count catches one that started and ended between reads. */}
              <Runs ns={ns} name={name} current={`${task.status?.run?.id ?? ""}/${task.status?.phase ?? ""}/${task.status?.runs?.count ?? 0}`} />
            </aside>
          </div>
        </>
      )}
    </main>
  );
}

function Details({ task }: { task: Task }) {
  const { spec, status = {} } = task;
  const run = status.phase === "Running" ? status.run : undefined;
  return (
    <dl className="details">
      <dt>Phase</dt>
      <dd className="phase">{status.phase ?? "Todo"}</dd>
      {status.waiting && (
        <>
          <dt>Waiting</dt>
          <dd>
            {status.waiting.reason}
            {status.waiting.since && <> since {new Date(status.waiting.since).toLocaleString()}</>}
            {status.waiting.threadSeq ? <> (entry {status.waiting.threadSeq})</> : null}
          </dd>
        </>
      )}
      <dt>Assignee</dt>
      <dd>{spec.assignee ? `${spec.assignee.kind === "Agent" ? "agent" : "user"} ${spec.assignee.name}` : "unassigned"}</dd>
      <dt>Priority</dt>
      <dd>{spec.priority ?? "medium"}</dd>
      {spec.labels && spec.labels.length > 0 && (
        <>
          <dt>Labels</dt>
          <dd>{spec.labels.join(", ")}</dd>
        </>
      )}
      {run && (
        <>
          <dt>Run</dt>
          <dd>
            {run.mode ?? "runtime"} on {run.runnerName ?? "a runner"}
            {run.step ? `, step ${run.step}` : ""}
            {run.attempt ? `, attempt ${run.attempt}` : ""}
            {run.leaseUntil && <>, lease until {new Date(run.leaseUntil).toLocaleTimeString()}</>}
          </dd>
        </>
      )}
      <dt>Runs</dt>
      <dd>{status.runs?.count ?? 0}</dd>
      {status.conditions?.map((c) => (
        <div key={c.type}>
          <dt>{c.type}</dt>
          <dd>
            {c.status}
            {c.reason && ` (${c.reason})`}
            {c.message && `: ${c.message}`}
          </dd>
        </div>
      ))}
    </dl>
  );
}
