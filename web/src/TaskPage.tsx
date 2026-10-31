import { useRef, useState } from "react";
import { Link, useParams } from "react-router";
import { APIError, getTask, readThread, type Task, type ThreadEntry } from "./api";
import { CancelTask, Composer } from "./Actions";
import { RoleLine } from "./Identity";
import { params, useParam } from "./links";
import { LiveRun, RunEnded } from "./LiveRun";
import { Runs } from "./Runs";
import { Subtasks } from "./Subtasks";
import { useTeamName } from "./teamList";
import { Thread } from "./Thread";
import { assigneeIcon, PhaseBadge, Pills, PriorityChip } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { Icon } from "./ui/Icon";
import { Heading, useTitle } from "./ui/Page";
import { Loading, Skeleton } from "./ui/Skeleton";
import { Time } from "./ui/Time";
import { usePolling } from "./usePolling";

// threadPage is how many thread entries one read asks for.
export const threadPage = 200;

// TaskPage shows one task: spec, status, thread, actions and runs. It polls
// the task, and the thread from the last entry it has, never from the
// start again.
export function TaskPage() {
  const { ns = "", name = "" } = useParams();
  const [linkedRun] = useParam(params.run);
  const team = useTeamName(ns);
  const [task, setTask] = useState<Task | null>(null);
  const [entries, setEntries] = useState<ThreadEntry[]>([]);
  // A linked run is scrolled to once the thread, the subtasks and the runs
  // have first been read (or failed to be): under 800 px the thread and the
  // subtasks are above it, and the runs make the page long enough to bring
  // it to the top.
  const [threadRead, setThreadRead] = useState(false);
  const [subtasksRead, setSubtasksRead] = useState(false);
  const [runsRead, setRunsRead] = useState(false);
  const ready = threadRead && subtasksRead && runsRead;
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
      setThreadRead(true);
    },
    5000,
    [ns, name, refresh],
    { ns, topic: "tasks", match: (h) => h.event === "task" && h.name === name },
  );

  const number = task?.status?.number;
  useTitle(task ? `${number !== undefined ? `#${number} ` : ""}${task.spec.title}` : undefined, team);
  const board = `/t/${encodeURIComponent(ns)}`;
  const onRefresh = () => setRefresh((n) => n + 1);
  const waitingOn = task?.status?.waiting?.threadSeq;
  const waitedCall = entries.find((e) => e.seq === waitingOn)?.call;
  const liveRun = task?.status?.phase === "Running" && task.status.run?.id ? task.status.run : undefined;
  return (
    <main id="main" className="task-page">
      <nav className="breadcrumb" aria-label="Breadcrumb">
        <ol>
          <li>
            <Link to={board}>{team}</Link>
          </li>
          <li aria-current="page" className="mono">
            {number !== undefined ? `#${number}` : name}
          </li>
        </ol>
      </nav>
      {error && (
        <Banner onRetry={onRefresh} className="page-banner">
          Could not load the task: {error}
        </Banner>
      )}
      {!task && !error && <TaskSkeleton />}
      {task && (
        <>
          <div className="task-head">
            <div className="title-block">
              <Heading>
                {number !== undefined && <span className="mono num">#{number}</span>}{" "}
                <span>{task.spec.title}</span>
              </Heading>
              <div className="badges">
                <PhaseBadge phase={task.status?.phase ?? "Todo"} />
                <PriorityChip priority={task.spec.priority} always />
                {task.spec.parentRef && (
                  <span className="meta">
                    <Icon name="git-branch" size={14} />
                    <span>
                      subtask of <Link to={`${board}/tasks/${encodeURIComponent(task.spec.parentRef.name)}`}>{task.spec.parentRef.name}</Link>
                    </span>
                  </span>
                )}
              </div>
              <RoleLine ns={ns} />
            </div>
            <CancelTask task={task} onTask={setTask} onRefresh={onRefresh} />
          </div>
          <div className="task-layout">
            <div className="stack">
              {task.spec.description && (
                <section className="card" aria-labelledby="description">
                  <div className="card-head">
                    <h2 id="description">Description</h2>
                  </div>
                  {/* Plain text: markdown needs a renderer and sanitising. */}
                  <pre className="card-body prose">{task.spec.description}</pre>
                </section>
              )}
              <section className="card" aria-labelledby="thread">
                <div className="card-head">
                  <h2 id="thread">Thread</h2>
                  <span className="count-pill">{entries.length}</span>
                </div>
                <div className="card-body">
                  <Thread entries={entries} waitingOn={waitingOn} />
                </div>
              </section>
              <Composer task={task} onTask={setTask} onRefresh={onRefresh} waitingTool={waitedCall?.name ?? waitedCall?.tool} />
              <Subtasks ns={ns} name={name} onRead={() => setSubtasksRead(true)} />
            </div>
            <aside className="stack" aria-label="About the task">
              <Details task={task} />
              <RunEnded run={liveRun?.id} />
              {liveRun && (
                <LiveRun key={liveRun.id} ns={ns} name={name} run={liveRun} linked={liveRun.id.toLowerCase() === linkedRun.toLowerCase()} ready={ready} />
              )}
              {/* A run ends when the current run goes away or is replaced; the
                  count catches one that started and ended between reads. */}
              <Runs ns={ns} name={name} current={`${task.status?.run?.id ?? ""}/${task.status?.phase ?? ""}/${task.status?.runs?.count ?? 0}`} open={linkedRun} ready={ready} onRead={() => setRunsRead(true)} />
            </aside>
          </div>
        </>
      )}
    </main>
  );
}

function TaskSkeleton() {
  return (
    <Loading what="the task">
      <div className="task-head" aria-hidden="true">
        <div className="title-block">
          <Skeleton width="50%" height={24} />
          <Skeleton width="20%" height={16} />
        </div>
      </div>
      <div className="task-layout" aria-hidden="true">
        <div className="stack">
          <div className="card card-pad stack">
            <Skeleton width="90%" />
            <Skeleton width="75%" />
            <Skeleton width="60%" />
          </div>
        </div>
        <div className="card card-pad stack">
          <Skeleton width="70%" />
          <Skeleton width="50%" />
        </div>
      </div>
    </Loading>
  );
}

function Details({ task }: { task: Task }) {
  const { spec, status = {} } = task;
  const run = status.phase === "Running" ? status.run : undefined;
  return (
    <section className="card" aria-labelledby="details">
      <div className="card-head">
        <h2 id="details">Details</h2>
      </div>
      <dl className="details card-body">
        <dt>Phase</dt>
        <dd>
          <PhaseBadge phase={status.phase ?? "Todo"} />
        </dd>
        {status.waiting && (
          <>
            <dt>Waiting</dt>
            <dd>
              <span className="chip tone-waiting">
                <Icon name="hourglass" size={12} />
                {status.waiting.reason}
              </span>
              {status.waiting.since && (
                <span className="sub">
                  since <Time at={status.waiting.since} />
                  {status.waiting.threadSeq ? <> (entry {status.waiting.threadSeq})</> : null}
                </span>
              )}
              {!status.waiting.since && status.waiting.threadSeq ? <span className="sub">entry {status.waiting.threadSeq}</span> : null}
            </dd>
          </>
        )}
        <dt>Assignee</dt>
        <dd>
          {spec.assignee ? (
            <>
              <Icon name={assigneeIcon(spec.assignee.kind)} />
              <span>
                <span className="sr-only">{spec.assignee.kind === "Agent" ? "agent " : "user "}</span>
                {spec.assignee.name}
              </span>
            </>
          ) : (
            <span className="muted">unassigned</span>
          )}
        </dd>
        <dt>Priority</dt>
        <dd>
          <PriorityChip priority={spec.priority} always />
        </dd>
        {spec.labels && spec.labels.length > 0 && (
          <>
            <dt>Labels</dt>
            <dd>
              <Pills labels={spec.labels} />
            </dd>
          </>
        )}
        {run && (
          <>
            <dt>Run</dt>
            <dd>
              <span>
                {run.mode ?? "runtime"} on {run.runnerName ?? "a runner"}
                {run.step ? `, step ${run.step}` : ""}
                {run.attempt ? `, attempt ${run.attempt}` : ""}
              </span>
              {run.leaseUntil && <span className="sub">lease until {new Date(run.leaseUntil).toLocaleTimeString()}</span>}
            </dd>
          </>
        )}
        <dt>Runs</dt>
        <dd className="num">{status.runs?.count ?? 0}</dd>
        {status.conditions?.map((c) => (
          <div key={c.type} className="condition">
            <dt>{c.type}</dt>
            <dd>
              <Icon name={c.status === "True" ? "check" : "alert-triangle"} className={c.status === "True" ? "ok" : "bad"} />
              <span>
                {c.status}
                {c.reason && ` (${c.reason})`}
              </span>
              {c.message && <span className="sub">{c.message}</span>}
            </dd>
          </div>
        ))}
      </dl>
    </section>
  );
}
