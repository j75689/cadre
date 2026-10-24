import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { createTask, listAgents, type AgentSummary, type TaskSpec } from "./api";

const priorities = ["low", "medium", "high", "urgent"];

export interface Draft {
  title: string;
  description: string;
  kind: "" | "Agent" | "User";
  assignee: string;
  priority: string;
  labels: string;
}

// validate mirrors the server's task.ValidateSpec: a title that is not
// blank, and a name for an assignee of a chosen kind.
export function validate(d: Draft): string {
  if (d.title.trim() === "") return "A title is required.";
  if (d.kind !== "" && d.assignee.trim() === "") return `Name the ${d.kind === "Agent" ? "agent" : "user"} to assign it to.`;
  return "";
}

export function toSpec(d: Draft): TaskSpec {
  const spec: TaskSpec = { title: d.title.trim(), priority: d.priority };
  if (d.description.trim() !== "") spec.description = d.description;
  if (d.kind !== "") spec.assignee = { kind: d.kind, name: d.assignee.trim() };
  const labels = [...new Set(d.labels.split(",").map((l) => l.trim()).filter((l) => l !== ""))];
  if (labels.length > 0) spec.labels = labels;
  return spec;
}

// NewTask creates a task in the team; the server names it.
export function NewTask() {
  const { ns = "" } = useParams();
  const navigate = useNavigate();
  const [draft, setDraft] = useState<Draft>({ title: "", description: "", kind: "", assignee: "", priority: "medium", labels: "" });
  const [agents, setAgents] = useState<AgentSummary[] | null>(null);
  const [agentsError, setAgentsError] = useState("");
  // retry reads the agents again after a failed read.
  const [retry, setRetry] = useState(0);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  // The agents are read once: picking one is not live data.
  useEffect(() => {
    const controller = new AbortController();
    listAgents(ns, controller.signal).then(
      (r) => {
        setAgents(r.items);
        setAgentsError("");
      },
      (e) => {
        if (!controller.signal.aborted) setAgentsError((e as Error).message);
      },
    );
    return () => controller.abort();
  }, [ns, retry]);
  const set = (patch: Partial<Draft>) => setDraft((d) => ({ ...d, ...patch }));
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const invalid = validate(draft);
    setError(invalid);
    if (invalid) return;
    setBusy(true);
    try {
      const t = await createTask(ns, toSpec(draft));
      navigate(`/t/${encodeURIComponent(ns)}/tasks/${encodeURIComponent(t.metadata.name)}`);
    } catch (err) {
      setError((err as Error).message);
      setBusy(false);
    }
  };
  const chosen = agents?.find((a) => a.name === draft.assignee);
  return (
    <main className="new-task">
      <p>
        <Link to={`/t/${encodeURIComponent(ns)}`}>← {ns}</Link>
      </p>
      <h1>New task</h1>
      <form onSubmit={submit} noValidate>
        <label>
          Title <input value={draft.title} onChange={(e) => set({ title: e.target.value })} required />
        </label>
        <label>
          Description <textarea value={draft.description} onChange={(e) => set({ description: e.target.value })} rows={6} />
        </label>
        <fieldset>
          <legend>Assignee</legend>
          <label>
            Kind{" "}
            <select value={draft.kind} onChange={(e) => set({ kind: e.target.value as Draft["kind"], assignee: "" })}>
              <option value="">unassigned</option>
              <option value="Agent">agent</option>
              <option value="User">user</option>
            </select>
          </label>
          {draft.kind === "Agent" && (
            <label>
              Agent{" "}
              <select value={draft.assignee} onChange={(e) => set({ assignee: e.target.value })}>
                <option value="">choose…</option>
                {agents?.map((a) => (
                  <option key={a.name} value={a.name}>
                    {a.name}
                    {a.ready ? "" : ` (not ready: ${a.reason ?? "unknown"})`}
                  </option>
                ))}
              </select>
            </label>
          )}
          {draft.kind === "Agent" && chosen && !chosen.ready && (
            <p className="hint">
              {chosen.name} is not ready{chosen.message ? `: ${chosen.message}` : ""}. Its tasks wait until it is.
            </p>
          )}
          {draft.kind === "Agent" && agentsError && (
            <p className="error">
              Could not load the agents: {agentsError}{" "}
              <button type="button" onClick={() => setRetry((n) => n + 1)}>
                Retry
              </button>
            </p>
          )}
          {draft.kind === "Agent" && !agentsError && agents?.length === 0 && <p className="hint">This team has no agents.</p>}
          {draft.kind === "User" && (
            <label>
              User <input value={draft.assignee} onChange={(e) => set({ assignee: e.target.value })} />
            </label>
          )}
        </fieldset>
        <label>
          Priority{" "}
          <select value={draft.priority} onChange={(e) => set({ priority: e.target.value })}>
            {priorities.map((p) => (
              <option key={p}>{p}</option>
            ))}
          </select>
        </label>
        <label>
          Labels <input value={draft.labels} onChange={(e) => set({ labels: e.target.value })} placeholder="comma-separated" />
        </label>
        {error && (
          <p className="error" role="alert">
            {error}
          </p>
        )}
        <button type="submit" disabled={busy}>
          Create task
        </button>
      </form>
    </main>
  );
}
