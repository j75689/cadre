import { useState } from "react";
import { Link } from "react-router";
import { listSubtasks, type Task } from "./api";
import { usePolling } from "./usePolling";

// subtaskPage is one "load more" of the subtasks.
export const subtaskPage = 50;

// Subtasks lists a task's subtasks, in board order, a page at a time. It
// polls with the task page and shows nothing for a task without any.
export function Subtasks({ ns, name }: { ns: string; name: string }) {
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
        if (!signal.aborted) setError(String((e as Error).message));
      }
    },
    5000,
    [ns, name, pages],
  );

  // Until a page has loaded, an error is all there is to show; after, what
  // was loaded stays beside it. A task with none shows nothing.
  if (tasks === null) return error ? <p className="error">Could not load the subtasks: {error}</p> : null;
  if (!error && tasks.length === 0) return null;
  const base = `/t/${encodeURIComponent(ns)}/tasks/`;
  return (
    <section className="subtasks" aria-label="Subtasks">
      <h2>Subtasks</h2>
      {error && <p className="error">Could not load the subtasks: {error}</p>}
      <ul>
        {tasks.map((t) => (
          <li key={t.metadata.name}>
            <span className="number">{t.status?.number !== undefined ? `#${t.status.number}` : "new"}</span>{" "}
            <Link to={base + encodeURIComponent(t.metadata.name)}>{t.spec.title}</Link>{" "}
            <span className="phase">{t.status?.phase ?? "Todo"}</span>{" "}
            <span className="assignee">{t.spec.assignee ? t.spec.assignee.name : "unassigned"}</span>
          </li>
        ))}
      </ul>
      {more && !error && (
        <button type="button" className="more" onClick={() => setPages((p) => p + 1)}>
          Load more
        </button>
      )}
    </section>
  );
}
