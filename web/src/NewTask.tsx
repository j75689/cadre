import { useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { createTask, type TaskSpec } from "./api";
import { RoleLine, useRole } from "./Identity";
import { can } from "./roles";
import { useTeamName } from "./teamList";
import { AssigneePicker, type AssigneeKind } from "./ui/AssigneePicker";
import { Button } from "./ui/Button";
import { Icon } from "./ui/Icon";
import { Heading, useTitle } from "./ui/Page";
import { Segmented } from "./ui/Segmented";

const priorities = ["low", "medium", "high", "urgent"];

export interface Draft {
  title: string;
  description: string;
  kind: AssigneeKind;
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

const blank: Draft = { title: "", description: "", kind: "", assignee: "", priority: "medium", labels: "" };

// NewTask creates a task in the team; the server names it. Its form is
// data-dirty once it differs from a blank draft, so a shortcut keeps it.
export function NewTask() {
  const { ns = "" } = useParams();
  const role = useRole(ns);
  const team = useTeamName(ns);
  const navigate = useNavigate();
  const [draft, setDraft] = useState(blank);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useTitle("New task", team);
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
      <form className="card form" onSubmit={submit} noValidate data-dirty={JSON.stringify(draft) !== JSON.stringify(blank) || undefined}>
        <label className="field">
          Title <input value={draft.title} onChange={(e) => set({ title: e.target.value })} required />
        </label>
        <label className="field">
          Description <textarea value={draft.description} onChange={(e) => set({ description: e.target.value })} rows={6} />
        </label>
        <AssigneePicker ns={ns} kind={draft.kind} assignee={draft.assignee} onChange={set} />
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
