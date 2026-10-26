import { Link } from "react-router";
import { phases, type Task } from "./api";
import { params, taskLink, useParam } from "./links";
import { PhaseBadge, PriorityChip } from "./ui/Badge";
import { Button } from "./ui/Button";
import { Icon } from "./ui/Icon";
import { Time } from "./ui/Time";

const priorities = ["urgent", "high", "medium", "low"];

// columns are the table's sortable columns: the key kept in ?sort= and how
// two tasks compare by it, ascending.
const columns: { key: string; label: string; compare: (a: Task, b: Task) => number }[] = [
  { key: "number", label: "#", compare: (a, b) => number(a) - number(b) },
  { key: "title", label: "Title", compare: (a, b) => a.spec.title.localeCompare(b.spec.title) },
  { key: "phase", label: "Phase", compare: (a, b) => phaseRank(a) - phaseRank(b) },
  { key: "assignee", label: "Assignee", compare: (a, b) => (a.spec.assignee?.name ?? "").localeCompare(b.spec.assignee?.name ?? "") },
  { key: "priority", label: "Priority", compare: (a, b) => priorities.indexOf(a.spec.priority ?? "medium") - priorities.indexOf(b.spec.priority ?? "medium") },
  { key: "created", label: "Created", compare: (a, b) => (a.metadata.creationTimestamp ?? "").localeCompare(b.metadata.creationTimestamp ?? "") },
];

// A task without a number yet is a new one: it sorts after the numbered.
const number = (t: Task) => t.status?.number ?? Number.MAX_SAFE_INTEGER;
const phaseRank = (t: Task) => phases.findIndex((p) => p === t.status?.phase);

// parseSort reads a sort key, "-" first for descending. No key, or an
// unknown one, is the API's order: priority ascending.
function parseSort(sort: string) {
  const desc = sort.startsWith("-");
  const col = columns.find((c) => c.key === (desc ? sort.slice(1) : sort));
  return col ? { col, desc } : { col: columns.find((c) => c.key === "priority")!, desc: false };
}

// sorted orders the tasks by the sort key, the number breaking ties.
export function sorted(tasks: Task[], sort: string): Task[] {
  const { col, desc } = parseSort(sort);
  return [...tasks].sort((a, b) => (desc ? -1 : 1) * col.compare(a, b) || number(a) - number(b));
}

// TaskTable is the board as a list: the tasks of the loaded columns in one
// table, sorted by a header's button. "Load more" reads the next page of
// every column that has more, as the board's buttons do one at a time.
export function TaskTable(props: { ns: string; tasks: Task[]; more: boolean; onMore: () => void }) {
  const [sortParam, setSort] = useParam(params.sort);
  const { col, desc } = parseSort(sortParam);
  const key = col.key;
  const dir = desc ? "descending" : "ascending";
  // A header sorts ascending, then descending; the default needs no key.
  const toggle = (k: string) => {
    const next = k === key && dir === "ascending" ? `-${k}` : k;
    setSort(next === "priority" ? "" : next);
  };
  return (
    <div className="card table-wrap task-list" tabIndex={0} role="region" aria-label="Tasks">
      <table className="table">
        <thead>
          <tr>
            {columns.map((c) => (
              <th scope="col" key={c.key} aria-sort={c.key === key ? dir : undefined}>
                <button type="button" className="sort" onClick={() => toggle(c.key)}>
                  {c.label}
                  {c.key === key && <Icon name="chevron-down" size={12} className={dir === "ascending" ? "sort-asc" : undefined} />}
                </button>
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {props.tasks.length === 0 && (
            <tr>
              <td colSpan={columns.length} className="muted">
                Nothing here
              </td>
            </tr>
          )}
          {sorted(props.tasks, sortParam).map((t) => (
            <tr key={t.metadata.name}>
              <td className="mono">{t.status?.number !== undefined ? `#${t.status.number}` : "new"}</td>
              <td className="table-text">
                <Link to={taskLink(props.ns, t.metadata.name)}>{t.spec.title}</Link>
              </td>
              <td>{t.status?.phase && <PhaseBadge phase={t.status.phase} />}</td>
              <td>{t.spec.assignee?.name}</td>
              <td>
                <PriorityChip priority={t.spec.priority} always />
              </td>
              <td>{t.metadata.creationTimestamp && <Time at={t.metadata.creationTimestamp} />}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {props.more && (
        <Button variant="ghost" small className="btn-block" onClick={props.onMore}>
          Load more
        </Button>
      )}
    </div>
  );
}
