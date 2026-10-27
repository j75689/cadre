import { useEffect, useState } from "react";
import { Link, useParams } from "react-router";
import { listTasks, phases, terminalPhases, type Phase, type Task } from "./api";
import { RoleLine, useIdentityDetails, useRole } from "./Identity";
import { params, useParam, useTextParam } from "./links";
import { can } from "./roles";
import { TaskTable } from "./TaskTable";
import { useTeamName } from "./teamList";
import { assigneeIcon, Meta, phaseTone, Pills, PriorityChip, RoleBadge } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { Button, ButtonLink } from "./ui/Button";
import { Icon } from "./ui/Icon";
import { Heading, useTitle } from "./ui/Page";
import { Loading, Skeleton, SkeletonCard } from "./ui/Skeleton";
import { ago } from "./ui/Time";
import { usePolling } from "./usePolling";

// pageSize is one "load more" of a column.
export const pageSize = 50;

// pollMs is how often the board is read again.
const pollMs = 5000;

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

// latest keeps each task in one column only. The phases are read in
// parallel, so a task that changes phase between two reads comes back in
// both; it stays in the column whose read came back last, which saw it
// after the move. Its resourceVersion cannot tell the two apart: it is
// opaque, never compared (storage.Objects).
export function latest(columns: Column[], arrived: Phase[]): Column[] {
  const owner = new Map<string, Phase>();
  for (const ph of arrived) {
    for (const t of columns.find((c) => c.phase === ph)?.tasks ?? []) owner.set(t.metadata.name, ph);
  }
  return columns.map((c) => ({ ...c, tasks: c.tasks.filter((t) => owner.get(t.metadata.name) === c.phase) }));
}

export function Board() {
  const { ns = "" } = useParams();
  const role = useRole(ns);
  const { mode } = useIdentityDetails();
  const name = useTeamName(ns);
  const [columns, setColumns] = useState<Column[] | null>(null);
  const [pages, setPages] = useState<Record<string, number>>({});
  const [error, setError] = useState("");
  const [search, setSearch, searchBox] = useTextParam(params.q);
  const [phaseParam, setPhase] = useParam(params.phase);
  const [assignee, setAssignee] = useParam(params.assignee);
  const [view, setView] = useParam(params.view);
  // Any view but the list is the board.
  const list = view === "list";
  // The phase matches in any case, so "?phase=todo" reads Todo and the
  // select can clear it. An unknown phase reads as all of them; it is left
  // in the URL, not set.
  const phase = phases.find((ph) => ph.toLowerCase() === phaseParam.toLowerCase());
  const read = phase ? [phase] : phases;
  const [updated, setUpdated] = useState<Date | null>(null);
  // retry reads the board again at once, after a failed first read.
  const [retry, setRetry] = useState(0);
  useTitle(name);
  usePolling(
    async (signal) => {
      try {
        const arrived: Phase[] = [];
        const next = await Promise.all(
          read.map((ph) => loadColumn(ns, ph, pages[ph] ?? 1, signal).then((c) => (arrived.push(ph), c))),
        );
        setColumns(latest(next, arrived));
        setUpdated(new Date());
        setError("");
      } catch (e) {
        if (!signal.aborted) setError(String((e as Error).message));
      }
    },
    pollMs,
    [ns, pages, retry, phase],
  );
  const board = `/t/${encodeURIComponent(ns)}`;
  // Until the read for a new phase filter is back, the columns read before
  // it show only as far as the filter keeps them.
  const shownColumns = columns?.filter((c) => read.includes(c.phase)) ?? null;
  const total = shownColumns?.reduce((n, c) => n + c.tasks.length, 0) ?? 0;
  const shown = shownColumns?.reduce((n, c) => n + c.tasks.filter((t) => matches(t, search, assignee)).length, 0) ?? 0;
  // The assignees on the loaded tasks, and the one asked for even if none
  // of them has it.
  const assignees = [
    ...new Set([...(columns ?? []).flatMap((c) => c.tasks.map((t) => t.spec.assignee?.name ?? "")), assignee].filter(Boolean)),
  ].sort();
  return (
    <main id="main" className="board">
      <div className="toolbar">
        <div className="toolbar-title">
          <Heading>{name}</Heading>
          {mode === "oidc" && role && <RoleBadge role={role} />}
        </div>
        {columns && <LiveStatus updated={updated} error={error} />}
        <div className="toolbar-end">
          <div className="search" role="search">
            <Icon name="search" />
            <label htmlFor="board-search" className="sr-only">
              Search the loaded page
            </label>
            <input
              id="board-search"
              type="search"
              {...searchBox}
              placeholder="Title, label, assignee, #number"
            />
            {searchBox.value && <Button variant="ghost" small icon="x" aria-label="Clear the search" onClick={() => setSearch("")} />}
          </div>
          <div className="filters">
            <label htmlFor="board-phase" className="sr-only">
              Phase
            </label>
            <select id="board-phase" value={phase ?? ""} onChange={(e) => setPhase(e.target.value)}>
              <option value="">All phases</option>
              {phases.map((ph) => (
                <option key={ph}>{ph}</option>
              ))}
            </select>
            <label htmlFor="board-assignee" className="sr-only">
              Assignee
            </label>
            <select id="board-assignee" value={assignee} onChange={(e) => setAssignee(e.target.value)}>
              <option value="">Anyone</option>
              {assignees.map((a) => (
                <option key={a}>{a}</option>
              ))}
            </select>
          </div>
          <div className="view-switch" role="group" aria-label="View">
            <Button variant="ghost" small icon="kanban" aria-pressed={!list} onClick={() => setView("")}>
              Board
            </Button>
            <Button variant="ghost" small icon="list" aria-pressed={list} onClick={() => setView("list")}>
              List
            </Button>
          </div>
          {(search.trim() !== "" || assignee !== "") && shownColumns && (
            <span className="search-count">
              {shown} of {total} loaded tasks shown
            </span>
          )}
          {can(role, "work") && (
            <ButtonLink variant="primary" icon="plus" to={`${board}/new`}>
              New task
            </ButtonLink>
          )}
        </div>
      </div>
      <div className="board-role">
        <RoleLine ns={ns} />
      </div>
      {error && columns === null && (
        <Banner onRetry={() => setRetry((n) => n + 1)}>Could not load the board: {error}</Banner>
      )}
      {columns === null && !error && <BoardSkeleton />}
      {shownColumns && list && (
        <TaskTable
          ns={ns}
          tasks={shownColumns.flatMap((c) => c.tasks.filter((t) => matches(t, search, assignee)))}
          more={shownColumns.some((c) => c.more)}
          onMore={() =>
            setPages((p) => {
              const next = { ...p };
              for (const c of shownColumns) if (c.more) next[c.phase] = (p[c.phase] ?? 1) + 1;
              return next;
            })
          }
        />
      )}
      {shownColumns && !list && (
        <Columns
          ns={ns}
          columns={shownColumns}
          search={search}
          assignee={assignee}
          onMore={(phase) => setPages((p) => ({ ...p, [phase]: (p[phase] ?? 1) + 1 }))}
        />
      )}
    </main>
  );
}

// LiveStatus says how fresh the board is, as the poll keeps it. Only a
// change of state reaches the polite live region, "Up to date" or "Could
// not refresh: ...", so it is not read out again on every poll; how long
// ago the board was read is shown beside it, outside the region.
function LiveStatus({ updated, error }: { updated: Date | null; error: string }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), pollMs);
    return () => clearInterval(t);
  }, []);
  return (
    <p className={error ? "live stale" : "live"}>
      <span role="status" aria-atomic="true" className={error ? undefined : "sr-only"}>
        {error ? `Could not refresh: ${error}` : "Up to date"}
      </span>
      {!error && updated && <span>Updated {ago(updated, Math.max(now, updated.getTime())) ?? updated.toLocaleString()}</span>}
    </p>
  );
}

function BoardSkeleton() {
  return (
    <Loading what="the board" className="columns">
      {phases.map((phase) => (
        <div className="column skeleton-column" key={phase} aria-hidden="true">
          <div className="column-head">
            <Skeleton width="45%" height={12} />
          </div>
          <div className="column-body">
            <SkeletonCard />
            <SkeletonCard />
          </div>
        </div>
      ))}
    </Loading>
  );
}

// matches is the client-side search and assignee filter over what is
// loaded. The assignee, when set, is matched exactly, by name only: a User
// and an Agent of the same name are one assignee, as in the Agents page's
// link, which carries only the name.
export function matches(t: Task, query: string, assignee = ""): boolean {
  if (assignee !== "" && t.spec.assignee?.name !== assignee) return false;
  const q = query.trim().toLowerCase();
  if (q === "") return true;
  const fields = [t.spec.title, t.metadata.name, t.spec.assignee?.name ?? "", ...(t.spec.labels ?? [])];
  if (t.status?.number !== undefined) fields.push("#" + t.status.number);
  return fields.some((f) => f.toLowerCase().includes(q));
}

export function Columns(props: { ns: string; columns: Column[]; search: string; assignee?: string; onMore: (phase: Phase) => void }) {
  return (
    <div className={props.columns.length === 1 ? "columns one-phase" : "columns"}>
      {props.columns.map((col) => {
        const shown = col.tasks.filter((t) => matches(t, props.search, props.assignee));
        const terminal = terminalPhases.includes(col.phase);
        return (
          <section className={`column ${phaseTone(col.phase)}${terminal ? " terminal" : ""}`} key={col.phase} aria-label={col.phase}>
            <div className="column-head">
              <span className="dot" aria-hidden="true" />
              <h2>{col.phase}</h2>
              <span className="count-pill">
                {col.tasks.length}
                {col.more ? "+" : ""}
              </span>
            </div>
            <div className="column-body">
              {shown.length === 0 && <p className="column-empty">Nothing here</p>}
              {shown.map((t) => (
                <Card key={t.metadata.name} ns={props.ns} task={t} />
              ))}
              {col.more && (
                <Button variant="ghost" small className="btn-block" onClick={() => props.onMore(col.phase)}>
                  Load more
                </Button>
              )}
            </div>
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
  const phase = task.status?.phase;
  const waiting = phase === "Waiting" ? task.status?.waiting : undefined;
  return (
    <article className={`card task-card${waiting ? " waiting" : ""}`}>
      <div className="card-top">
        {phase === "Running" && <span className="dot pulse" aria-label="running" role="img" />}
        <span className="mono num">{task.status?.number !== undefined ? `#${task.status.number}` : "new"}</span>
        <PriorityChip priority={s.priority} />
      </div>
      <Link className="title" to={tasks + encodeURIComponent(task.metadata.name)}>
        {s.title}
      </Link>
      {s.assignee && <Meta icon={assigneeIcon(s.assignee.kind)}>{s.assignee.name}</Meta>}
      {s.parentRef && (
        <Meta icon="git-branch" className="parent">
          subtask of <Link to={tasks + encodeURIComponent(s.parentRef.name)}>{s.parentRef.name}</Link>
        </Meta>
      )}
      {waiting && (
        <Meta icon="hourglass" className="waiting-row">
          Waiting: {waiting.reason}
        </Meta>
      )}
      {s.labels && s.labels.length > 0 && <Pills labels={s.labels} />}
    </article>
  );
}
