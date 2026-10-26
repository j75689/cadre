import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { createTask, listAgents, type AgentSummary, type TaskSpec } from "./api";
import { RoleLine, useRole } from "./Identity";
import { can } from "./roles";
import { useTeamName } from "./teamList";
import { Banner } from "./ui/Banner";
import { Button } from "./ui/Button";
import { Icon } from "./ui/Icon";
import { Heading, useTitle } from "./ui/Page";
import { Segmented } from "./ui/Segmented";

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
  const role = useRole(ns);
  const team = useTeamName(ns);
  const navigate = useNavigate();
  const [draft, setDraft] = useState<Draft>({ title: "", description: "", kind: "", assignee: "", priority: "medium", labels: "" });
  const [agents, setAgents] = useState<AgentSummary[] | null>(null);
  const [agentsError, setAgentsError] = useState("");
  // retry reads the agents again after a failed read.
  const [retry, setRetry] = useState(0);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useTitle("New task", team);
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
  const board = `/t/${encodeURIComponent(ns)}`;
  const chosen = agents?.find((a) => a.name === draft.assignee);
  const back = (
    <nav className="breadcrumb" aria-label="Breadcrumb">
      <ol>
        <li>
          <Link to={board}>{team}</Link>
        </li>
        <li aria-current="page">New task</li>
      </ol>
    </nav>
  );
  if (!can(role, "work")) {
    return (
      <main id="main" className="form-page">
        {back}
        <div className="card card-pad stack">
          <Heading>New task</Heading>
          <RoleLine ns={ns} />
          {role !== undefined && <p>Creating tasks needs the member role.</p>}
        </div>
      </main>
    );
  }
  return (
    <main id="main" className="form-page">
      {back}
      <div className="page-head">
        <Heading>New task</Heading>
      </div>
      <form className="card form" onSubmit={submit} noValidate>
        <label className="field">
          Title <input value={draft.title} onChange={(e) => set({ title: e.target.value })} required />
        </label>
        <label className="field">
          Description <textarea value={draft.description} onChange={(e) => set({ description: e.target.value })} rows={6} />
        </label>
        <Segmented
          legend="Assignee"
          name="kind"
          value={draft.kind}
          options={[
            { value: "", label: "Unassigned" },
            { value: "Agent", label: "Agent" },
            { value: "User", label: "User" },
          ]}
          onChange={(kind) => set({ kind, assignee: "" })}
        >
          {draft.kind === "Agent" && (
            <label className="field">
              <span className="sr-only">Agent to assign</span>
              <select value={draft.assignee} onChange={(e) => set({ assignee: e.target.value })}>
                <option value="">Choose an agent…</option>
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
            <p className="help">
              {chosen.name} is not ready{chosen.message ? `: ${chosen.message}` : ""}. Its tasks wait until it is.
            </p>
          )}
          {draft.kind === "Agent" && agentsError && <Banner onRetry={() => setRetry((n) => n + 1)}>Could not load the agents: {agentsError}</Banner>}
          {draft.kind === "Agent" && !agentsError && agents?.length === 0 && <p className="help">This team has no agents.</p>}
          {draft.kind === "User" && (
            <label className="field">
              <span className="sr-only">User to assign</span>
              <input value={draft.assignee} onChange={(e) => set({ assignee: e.target.value })} placeholder="Their user name" />
            </label>
          )}
        </Segmented>
        <Segmented legend="Priority" name="priority" value={draft.priority} options={priorities.map((p) => ({ value: p, label: p }))} onChange={(priority) => set({ priority })} />
        <label className="field">
          Labels
          <input value={draft.labels} onChange={(e) => set({ labels: e.target.value })} aria-describedby="labels-help" />
          <span id="labels-help" className="help">
            Comma-separated
          </span>
        </label>
        <div className="buttons">
          <Button type="submit" variant="primary" icon="plus" disabled={busy}>
            Create task
          </Button>
          <Link to={board}>
            <Icon name="arrow-left" size={14} /> Cancel
          </Link>
        </div>
        {error && (
          <p className="inline-error" role="alert">
            {error}
          </p>
        )}
      </form>
    </main>
  );
}
