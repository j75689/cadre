import { useState } from "react";
import { Link, useParams } from "react-router";
import { listTasks, phases, type Phase, type Task } from "./api";
import { RoleLine, useRole } from "./Identity";
import { can } from "./roles";
import { usePolling } from "./usePolling";

// pageSize is one "load more" of a column.
export const pageSize = 50;

export interface Column {
  phase: Phase;
  tasks: Task[];
  // more says the last page came back full: there may be more.
  more: boolean;
}

// loadColumn reads the first pages of one phase, in the order the API
// returns: priority, then number.
async function loadColumn(ns: string, phase: Phase, pages: number, signal: AbortSignal): Promise<Column> {
  const tasks: Task[] = [];
  let more = false;
  for (let p = 0; p < pages; p++) {
    const res = await listTasks(ns, phase, p * pageSize, pageSize, signal);
    tasks.push(...res.items);
    more = res.items.length >= res.limit;
    if (!more) break;
  }
  return { phase, tasks, more };
}

export function Board() {
  const { ns = "" } = useParams();
  const role = useRole(ns);
  const [columns, setColumns] = useState<Column[] | null>(null);
  const [pages, setPages] = useState<Record<string, number>>({});
  const [error, setError] = useState("");
  const [search, setSearch] = useState("");
  usePolling(
    async (signal) => {
      try {
        const next = await Promise.all(phases.map((ph) => loadColumn(ns, ph, pages[ph] ?? 1, signal)));
        setColumns(next);
        setError("");
      } catch (e) {
        if (!signal.aborted) setError(String((e as Error).message));
      }
    },
    5000,
    [ns, pages],
  );
  return (
    <main className="board">
      <header className="board-header">
        <h1>{ns}</h1>
        {can(role, "work") && (
          <Link className="new" to={`/t/${encodeURIComponent(ns)}/new`}>
            New task
          </Link>
        )}
        <label className="search">
          Search the loaded page{" "}
          <input type="search" value={search} onChange={(e) => setSearch(e.target.value)} placeholder="title, label, assignee, #number" />
        </label>
      </header>
      <RoleLine ns={ns} />
      {error && <p className="error">Could not load the board: {error}</p>}
      {columns === null && !error && <p>Loading…</p>}
      {columns && (
        <Columns
          ns={ns}
          columns={columns}
          search={search}
          onMore={(phase) => setPages((p) => ({ ...p, [phase]: (p[phase] ?? 1) + 1 }))}
        />
      )}
    </main>
  );
}

// matches is the client-side search over what is loaded.
export function matches(t: Task, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (q === "") return true;
  const fields = [t.spec.title, t.metadata.name, t.spec.assignee?.name ?? "", ...(t.spec.labels ?? [])];
  if (t.status?.number !== undefined) fields.push("#" + t.status.number);
  return fields.some((f) => f.toLowerCase().includes(q));
}

export function Columns(props: { ns: string; columns: Column[]; search: string; onMore: (phase: Phase) => void }) {
  return (
    <div className="columns">
      {props.columns.map((col) => {
        const shown = col.tasks.filter((t) => matches(t, props.search));
        return (
          <section className="column" key={col.phase} aria-label={col.phase}>
            <h2>
              {col.phase} <span className="count">{col.tasks.length}{col.more ? "+" : ""}</span>
            </h2>
            {shown.map((t) => (
              <Card key={t.metadata.name} ns={props.ns} task={t} />
            ))}
            {col.more && (
              <button type="button" className="more" onClick={() => props.onMore(col.phase)}>
                Load more
              </button>
            )}
          </section>
        );
      })}
    </div>
  );
}

// Card is a task on the board. The title link covers the card; the parent
// link sits above it, since links cannot nest.
function Card({ ns, task }: { ns: string; task: Task }) {
  const s = task.spec;
  const tasks = `/t/${encodeURIComponent(ns)}/tasks/`;
  return (
    <article className={`card priority-${s.priority ?? "medium"}`}>
      <div className="card-top">
        <span className="number">{task.status?.number !== undefined ? `#${task.status.number}` : "new"}</span>
        <span className="priority">{s.priority ?? "medium"}</span>
      </div>
      <Link className="title" to={tasks + encodeURIComponent(task.metadata.name)}>
        {s.title}
      </Link>
      {s.parentRef && (
        <div className="parent">
          subtask of <Link to={tasks + encodeURIComponent(s.parentRef.name)}>{s.parentRef.name}</Link>
        </div>
      )}
      {s.assignee && (
        <div className="assignee">
          {s.assignee.kind === "Agent" ? "agent" : "user"} {s.assignee.name}
        </div>
      )}
      {task.status?.phase === "Waiting" && task.status.waiting && <div className="waiting">waiting: {task.status.waiting.reason}</div>}
      {s.labels && s.labels.length > 0 && (
        <div className="labels">
          {s.labels.map((l) => (
            <span className="label" key={l}>
              {l}
            </span>
          ))}
        </div>
      )}
    </article>
  );
}
