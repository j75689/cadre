import { useState } from "react";
import { Link } from "react-router";
import { listSubtasks, type Task } from "./api";
import { assigneeIcon, Meta, PhaseBadge } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { Button } from "./ui/Button";
import { usePolling } from "./usePolling";

// subtaskPage is one "load more" of the subtasks.
export const subtaskPage = 50;

// Subtasks lists a task's subtasks, in board order, a page at a time. It
// polls with the task page and shows nothing for a task without any.
// onRead is told after each read, whether it worked or not.
export function Subtasks({ ns, name, onRead }: { ns: string; name: string; onRead?: () => void }) {
  const [tasks, setTasks] = useState<Task[] | null>(null);
  const [more, setMore] = useState(false);
  const [pages, setPages] = useState(1);
  const [error, setError] = useState("");

  usePolling(
    async (signal) => {
      try {
        const all: Task[] = [];
        let full = false;
        for (let p = 0; p < pages; p++) {
          const res = await listSubtasks(ns, name, p * subtaskPage, subtaskPage, signal);
          all.push(...res.items);
          full = res.items.length >= res.limit;
          if (!full) break;
        }
        setTasks(all);
        setMore(full);
        setError("");
      } catch (e) {
        if (signal.aborted) return;
        setError(String((e as Error).message));
      }
      onRead?.();
    },
    5000,
    [ns, name, pages],
  );

  // Until a page has loaded, an error is all there is to show; after, what
  // was loaded stays beside it. A task with none shows nothing.
  if (tasks === null) return error ? <Banner>Could not load the subtasks: {error}</Banner> : null;
  if (!error && tasks.length === 0) return null;
  const base = `/t/${encodeURIComponent(ns)}/tasks/`;
  return (
    <section className="card subtasks" aria-label="Subtasks">
      <div className="card-head">
        <h2>Subtasks</h2>
        <span className="count-pill">
          {tasks.length}
          {more ? "+" : ""}
        </span>
      </div>
      {error && <Banner>Could not load the subtasks: {error}</Banner>}
      <ul className="rows">
        {tasks.map((t) => (
          <li key={t.metadata.name} className="subtask-row">
            <span className="mono num muted">{t.status?.number !== undefined ? `#${t.status.number}` : "new"}</span>
            <Link to={base + encodeURIComponent(t.metadata.name)}>{t.spec.title}</Link>
            <PhaseBadge phase={t.status?.phase ?? "Todo"} />
            <Meta icon={t.spec.assignee ? assigneeIcon(t.spec.assignee.kind) : "user"}>{t.spec.assignee ? t.spec.assignee.name : "unassigned"}</Meta>
          </li>
        ))}
      </ul>
      {more && !error && (
        <div className="card-foot">
          <Button variant="ghost" small className="btn-block" onClick={() => setPages((p) => p + 1)}>
            Load more
          </Button>
        </div>
      )}
    </section>
  );
}
