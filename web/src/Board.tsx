import { useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router";
import { APIError, listTasks, phases, terminalPhases, type Phase, type Task, type TaskFilter } from "./api";
import { RoleLine, useIdentityDetails, useRole } from "./Identity";
import { params, useParam, useParamList, useTextParam } from "./links";
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

// searchDelayMs is how long the typing pauses before the search is read.
export const searchDelayMs = 300;

// maxLabels is the most labels the server filters by (api.MaxLabels).
export const maxLabels = 10;

export interface Column {
  phase: Phase;
  tasks: Task[];
  // more says the last page came back full: there may be more.
  more: boolean;
  // total is how many tasks of the phase the filters select, as the last
  // page read counted them. A server without the filters sends none.
  total?: number;
}

// loadColumn reads the first pages of one phase, in the order the API
// returns: priority, then number.
async function loadColumn(ns: string, phase: Phase, pages: number, filter: TaskFilter, signal: AbortSignal): Promise<Column> {
  const tasks: Task[] = [];
  let more = false;
  let total: number | undefined;
  for (let p = 0; p < pages; p++) {
    const res = await listTasks(ns, phase, p * pageSize, pageSize, signal, filter);
    tasks.push(...res.items);
    total = res.total;
    more = res.items.length >= res.limit;
    if (!more) break;
  }
  return { phase, tasks, more, total };
}

// useDebounced is value once it has stayed the same for ms, and "" at once.
function useDebounced(value: string, ms: number): string {
  const [settled, setSettled] = useState(value);
  if (value === "" && settled !== "") setSettled("");
  useEffect(() => {
    const t = setTimeout(() => setSettled(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return value === "" ? "" : settled;
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
  // last is the last read's columns, and the filters it was read with.
  const [last, setLast] = useState<{ filter: string; columns: Column[] } | null>(null);
  const [pages, setPages] = useState<Record<string, number>>({});
  // failure is why the last read failed, and the filters it was read with.
  const [failure, setFailure] = useState({ filter: "", message: "" });
  // refusal is why the server refused the filters, such as a search too
  // long, and the filters it refused.
  const [refusal, setRefusal] = useState({ filter: "", message: "" });
  const [search, setSearch, searchBox] = useTextParam(params.q);
  const [phaseParam, setPhase] = useParam(params.phase);
  const [assignee, setAssignee] = useParam(params.assignee);
  const [labelParam, setLabels] = useParamList(params.label);
  // The reads send each label once, none empty, and at most maxLabels.
  const labels = [...new Set(labelParam.filter(Boolean))].slice(0, maxLabels);
  // moved is the index of the chip a label change focuses, as the control
  // that had the focus goes away or is disabled: the chip now there, else
  // the one before, else the search box.
  const moved = useRef<number | undefined>(undefined);
  const chipsRef = useRef<HTMLDivElement>(null);
  const changeLabels = (next: string[], at: number) => {
    moved.current = at;
    setLabels(next);
  };
  useEffect(() => {
    if (moved.current === undefined) return;
    const chips = chipsRef.current?.querySelectorAll<HTMLButtonElement>(".label-chip") ?? [];
    (chips[Math.min(moved.current, chips.length - 1)] ?? document.getElementById("board-search"))?.focus();
    moved.current = undefined;
  });
  // The reads send the search trimmed, once the typing pauses.
  const q = useDebounced(search.trim(), searchDelayMs);
  // A filter change reads the first pages again.
  const filter = JSON.stringify([q, assignee, labels]);
  const [paged, setPaged] = useState(filter);
  if (paged !== filter) {
    setPaged(filter);
    setPages({});
  }
  // Until a new filter's read is back, the board shows neither the columns
  // nor the refusal nor the error of the filter before it.
  const columns = last?.filter === filter ? last.columns : null;
  const refused = refusal.filter === filter ? refusal.message : "";
  const error = failure.filter === filter ? failure.message : "";
  // seen are the assignees read on this team's board, as the select's
  // options: a read filtered to one assignee holds no other.
  const [seen, setSeen] = useState({ ns, names: [] as string[] });
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
          read.map((ph) => loadColumn(ns, ph, pages[ph] ?? 1, { q, assignee, labels }, signal).then((c) => (arrived.push(ph), c))),
        );
        setLast({ filter, columns: latest(next, arrived) });
        const names = next.flatMap((c) => c.tasks.map((t) => t.spec.assignee?.name ?? ""));
        setSeen((s) => ({ ns, names: [...new Set([...(s.ns === ns ? s.names : []), ...names])] }));
        setUpdated(new Date());
        setFailure({ filter, message: "" });
        setRefusal({ filter, message: "" });
      } catch (e) {
        if (signal.aborted) return;
        if (e instanceof APIError && e.status === 400 && e.code === "invalid") setRefusal({ filter, message: e.message });
        else setFailure({ filter, message: String((e as Error).message) });
      }
    },
    pollMs,
    [ns, pages, retry, phase, filter],
    { ns, topic: "tasks" },
  );
  const board = `/t/${encodeURIComponent(ns)}`;
  // Until the read for a new phase filter is back, the columns read before
  // it show only as far as the filter keeps them. Refused filters show none.
  const shownColumns = refused ? null : (columns?.filter((c) => read.includes(c.phase)) ?? null);
  const loaded = shownColumns?.reduce((n, c) => n + c.tasks.length, 0) ?? 0;
  const shown = shownColumns?.reduce((n, c) => n + filtered(c, search, assignee, labels).length, 0) ?? 0;
  // The server counts the matches on the whole board; one without the
  // filters leaves them to the loaded pages.
  const counted = shownColumns?.every((c) => c.total !== undefined) ?? false;
  const matching = shownColumns?.reduce((n, c) => n + (c.total ?? 0), 0) ?? 0;
  // The assignees seen, and the one asked for even if none of them has it.
  const assignees = [...new Set([...(seen.ns === ns ? seen.names : []), assignee].filter(Boolean))].sort();
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
              Search the board
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
          {labels.length > 0 && (
            <div className="label-filter" role="group" aria-label="Labels" ref={chipsRef}>
              {labels.map((l, i) => (
                <button type="button" className="pill label-chip" key={l} aria-label={`Remove label ${l}`} onClick={() => changeLabels(labels.filter((x) => x !== l), i)}>
                  {l}
                  <Icon name="x" size={12} />
                </button>
              ))}
              {labels.length >= maxLabels && <span className="label-note">At most {maxLabels} labels</span>}
              <Button variant="ghost" small onClick={() => changeLabels([], 0)}>
                Clear labels
              </Button>
            </div>
          )}
          <div className="view-switch" role="group" aria-label="View">
            <Button variant="ghost" small icon="kanban" aria-pressed={!list} onClick={() => setView("")}>
              Board
            </Button>
            <Button variant="ghost" small icon="list" aria-pressed={list} onClick={() => setView("list")}>
              List
            </Button>
          </div>
          {(search.trim() !== "" || assignee !== "" || labels.length > 0) && shownColumns && (
            <span className="search-count">
              {counted ? `${matching} ${matching === 1 ? "task matches" : "tasks match"}` : `${shown} of ${loaded} loaded tasks shown`}
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
      {refused && <Banner>Could not filter the board: {refused}</Banner>}
      {columns === null && !error && !refused && <BoardSkeleton />}
      {shownColumns && list && (
        <TaskTable
          ns={ns}
          tasks={shownColumns.flatMap((c) => filtered(c, search, assignee, labels))}
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
          labels={labels}
          onLabel={(l) => changeLabels([...labels, l], labels.length)}
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

// matches is the search, assignee and labels filter over what is loaded,
// for a server that does not filter. The assignee, when set, is matched
// exactly, by name only: a User and an Agent of the same name are one
// assignee, as in the Agents page's link, which carries only the name. A
// task matches the labels when it has every one of them.
export function matches(t: Task, query: string, assignee = "", labels: string[] = []): boolean {
  if (assignee !== "" && t.spec.assignee?.name !== assignee) return false;
  if (!labels.every((l) => t.spec.labels?.includes(l))) return false;
  const q = query.trim().toLowerCase();
  if (q === "") return true;
  const fields = [t.spec.title, t.metadata.name, t.spec.assignee?.name ?? "", ...(t.spec.labels ?? [])];
  if (t.status?.number !== undefined) fields.push("#" + t.status.number);
  return fields.some((f) => f.toLowerCase().includes(q));
}

// filtered is what a column shows: what the server read, which it filtered
// when it counted it, else the loaded tasks that match.
function filtered(c: Column, search: string, assignee: string, labels: string[]): Task[] {
  return c.total === undefined ? c.tasks.filter((t) => matches(t, search, assignee, labels)) : c.tasks;
}

export function Columns(props: {
  ns: string;
  columns: Column[];
  search: string;
  assignee?: string;
  labels?: string[];
  onLabel?: (label: string) => void;
  onMore: (phase: Phase) => void;
}) {
  const labels = props.labels ?? [];
  return (
    <div className={props.columns.length === 1 ? "columns one-phase" : "columns"}>
      {props.columns.map((col) => {
        const shown = filtered(col, props.search, props.assignee ?? "", labels);
        const terminal = terminalPhases.includes(col.phase);
        return (
          <section className={`column ${phaseTone(col.phase)}${terminal ? " terminal" : ""}`} key={col.phase} aria-label={col.phase}>
            <div className="column-head">
              <span className="dot" aria-hidden="true" />
              <h2>{col.phase}</h2>
              <span className="count-pill">{col.total ?? `${col.tasks.length}${col.more ? "+" : ""}`}</span>
            </div>
            <div className="column-body">
              {shown.length === 0 && <p className="column-empty">Nothing here</p>}
              {shown.map((t) => (
                <Card key={t.metadata.name} ns={props.ns} task={t} labels={labels} onLabel={props.onLabel} />
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
// link and the label buttons sit above it, since links cannot nest.
function Card({ ns, task, labels, onLabel }: { ns: string; task: Task; labels: string[]; onLabel?: (label: string) => void }) {
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
      {s.labels && s.labels.length > 0 && (onLabel ? <LabelButtons labels={s.labels} active={labels} onLabel={onLabel} /> : <Pills labels={s.labels} />)}
    </article>
  );
}

// LabelButtons are a card's labels, each a button that adds it to the
// filter: not one already in it, nor one past maxLabels.
function LabelButtons({ labels, active, onLabel }: { labels: string[]; active: string[]; onLabel: (label: string) => void }) {
  const full = active.length >= maxLabels;
  return (
    <span className="pills">
      {labels.map((l) => (
        <button
          type="button"
          className="pill label-button"
          key={l}
          aria-label={`Filter by label ${l}`}
          aria-pressed={active.includes(l)}
          disabled={active.includes(l) || full}
          onClick={() => onLabel(l)}
        >
          {l}
        </button>
      ))}
    </span>
  );
}
