import { useState } from "react";
import { Link, useParams } from "react-router";
import { APIError, getTask, type Task } from "./api";
import { usePolling } from "./usePolling";

// TaskPage shows one task's spec and status, read-only for now.
export function TaskPage() {
  const { ns = "", name = "" } = useParams();
  const [task, setTask] = useState<Task | null>(null);
  const [error, setError] = useState("");
  usePolling(
    async (signal) => {
      try {
        setTask(await getTask(ns, name, signal));
        setError("");
      } catch (e) {
        if (signal.aborted) return;
        // A task that is gone shows no stale copy.
        if (e instanceof APIError && e.status === 404) setTask(null);
        setError(String((e as Error).message));
      }
    },
    5000,
    [ns, name],
  );
  return (
    <main className="task">
      <p>
        <Link to={`/t/${encodeURIComponent(ns)}`}>← {ns}</Link>
      </p>
      {error && <p className="error">Could not load the task: {error}</p>}
      {task && (
        <>
          <h1>
            {task.status?.number !== undefined && <span className="number">#{task.status.number} </span>}
            {task.spec.title}
          </h1>
          <p className="phase">{task.status?.phase ?? "Todo"}</p>
          {task.spec.description && <p className="description">{task.spec.description}</p>}
          <h2>Spec</h2>
          <pre>{JSON.stringify(task.spec, null, 2)}</pre>
          <h2>Status</h2>
          <pre>{JSON.stringify(task.status ?? {}, null, 2)}</pre>
        </>
      )}
    </main>
  );
}
