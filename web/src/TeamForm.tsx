import { useEffect, useRef, useState, type FormEvent } from "react";
import { APIError, getTeam, putTeam, type RoleSubject, type Team, type TeamRole, type TeamRoleName, type TeamSpec } from "./api";
import { managedByGitOps } from "./gitops";
import { Banner } from "./ui/Banner";
import { Button } from "./ui/Button";
import { Segmented } from "./ui/Segmented";

// A Go duration, as time.ParseDuration takes it, without a sign.
const duration = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;

// validate checks what the form can before the server does.
export function validate(d: TeamSpec): string {
  const max = d.maxConcurrentRuns;
  if (max !== undefined && (!Number.isInteger(max) || max < 0)) return "Max concurrent runs is a whole number, 0 or more.";
  if (d.archiveAfter && !duration.test(d.archiveAfter.trim())) return "Archive after is a duration such as 168h or 30m.";
  return "";
}

// toSpec is the whole spec, as the PUT replaces it: an unset cap goes as
// 0, which the server saves as 1, and no duration as none (never).
export function toSpec(d: TeamSpec): TeamSpec {
  return {
    displayName: d.displayName?.trim() ?? "",
    maxConcurrentRuns: d.maxConcurrentRuns ?? 0,
    archiveAfter: d.archiveAfter?.trim() || undefined,
    roles: (d.roles ?? []).map((r) => ({ role: r.role, subjects: r.subjects.map((s) => ({ kind: s.kind, name: s.name.trim() })) })),
  };
}

// draftOf copies a Team's spec, for the form to change.
function draftOf(t: Team): TeamSpec {
  const s = t.spec ?? {};
  return { ...s, roles: (s.roles ?? []).map((r) => ({ role: r.role, subjects: r.subjects.map((x) => ({ ...x })) })) };
}

// TeamForm edits a Team's spec. The draft is made once, from the Team the
// form opened with; the page's reads do not touch it. A save carries the
// version of base, the Team the draft was last checked against: after a
// 409 that is the Team read again, shown as "Current" above the form.
export function TeamForm({ team, onSaved, onCancel }: { team: Team; onSaved: (t: Team) => void; onCancel: () => void }) {
  const ns = team.metadata.namespace;
  const [base, setBase] = useState(team);
  const [draft, setDraft] = useState(() => draftOf(team));
  const [conflict, setConflict] = useState("");
  // stale is whether base was read again after a 409, so it is shown.
  const [stale, setStale] = useState(false);
  // deleted is a 409 after which the Team could not be found: there is
  // nothing left to save to.
  const [deleted, setDeleted] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [confirmed, setConfirmed] = useState(false);
  const gitops = managedByGitOps(base.metadata);
  const errorRef = useRef<HTMLParagraphElement>(null);
  const conflictRef = useRef<HTMLDivElement>(null);
  const saveRef = useRef<HTMLButtonElement>(null);
  // An error takes the focus, as the button that had it is disabled
  // during the request. A save clears both first, so each failure sets
  // them anew.
  useEffect(() => {
    if (error) errorRef.current?.focus();
  }, [error]);
  useEffect(() => {
    if (conflict) conflictRef.current?.focus();
  }, [conflict]);
  const roles = draft.roles ?? [];
  const setRoles = (f: (rs: TeamRole[]) => TeamRole[]) => setDraft((d) => ({ ...d, roles: f(d.roles ?? []) }));
  const setRole = (i: number, f: (r: TeamRole) => TeamRole) => setRoles((rs) => rs.map((r, k) => (k === i ? f(r) : r)));
  const setSubject = (i: number, j: number, patch: Partial<RoleSubject>) =>
    setRole(i, (r) => ({ ...r, subjects: r.subjects.map((s, k) => (k === j ? { ...s, ...patch } : s)) }));
  // save asks first, once, on a Team that looks managed by GitOps; force
  // is the confirmation's own save.
  const save = async (force: boolean) => {
    const invalid = validate(draft);
    setError(invalid);
    if (invalid) return;
    if (gitops && !confirmed && !force) {
      setConfirming(true);
      return;
    }
    setConfirming(false);
    setConflict("");
    setBusy(true);
    try {
      onSaved(await putTeam(ns, toSpec(draft), base.metadata.resourceVersion));
      return;
    } catch (err) {
      if (err instanceof APIError && err.status === 409) {
        setConflict(err.message);
        try {
          setBase(await getTeam(ns));
          setStale(true);
        } catch (read) {
          if (read instanceof APIError && read.status === 404) setDeleted(true);
          else setError(`Could not read the team again: ${(read as Error).message}`);
        }
      } else {
        setError((err as Error).message);
      }
    }
    setBusy(false);
  };
  return (
    <form
      className="form team-form"
      onSubmit={(e: FormEvent) => {
        e.preventDefault();
        void save(false);
      }}
      noValidate aria-label="Edit the team">
      {conflict && (
        <div ref={conflictRef} tabIndex={-1} role="alert">
          <Banner className="banner-warning">
            {conflict}.{" "}
            {deleted
              ? "The Team is gone, so there is nothing to save to."
              : "Someone else changed the team. Their current values are shown below; your edits are kept. Check them and save again."}
          </Banner>
        </div>
      )}
      {gitops && (
        <Banner className="banner-warning">
          This Team looks managed by GitOps ({gitops}). A change here drifts from Git and may be reverted by the next sync.
        </Banner>
      )}
      {stale && !deleted && <Current team={base} />}
      <label className="field">
        Display name
        <input value={draft.displayName ?? ""} onChange={(e) => setDraft((d) => ({ ...d, displayName: e.target.value }))} autoFocus />
      </label>
      <label className="field">
        Max concurrent runs
        <input
          type="number"
          min={0}
          step={1}
          value={draft.maxConcurrentRuns ?? ""}
          onChange={(e) => setDraft((d) => ({ ...d, maxConcurrentRuns: e.target.value === "" ? undefined : Number(e.target.value) }))}
          aria-describedby="max-runs-help"
        />
        <span id="max-runs-help" className="help">
          How many of the team's tasks run at once. Empty or 0 is saved as 1.
        </span>
      </label>
      <label className="field">
        Archive after
        <input
          className="mono"
          value={draft.archiveAfter ?? ""}
          onChange={(e) => setDraft((d) => ({ ...d, archiveAfter: e.target.value }))}
          aria-describedby="archive-after-help"
        />
        <span id="archive-after-help" className="help">
          A duration such as 168h or 30m. Empty: never.
        </span>
      </label>
      <fieldset>
        <legend>Roles</legend>
        {roles.length === 0 && <p className="help">No roles are given on this team.</p>}
        {roles.map((r, i) => (
          <fieldset key={i} className="role-edit">
            <legend>Role {i + 1}</legend>
            <label className="field">
              Role
              <select value={r.role} onChange={(e) => setRole(i, (x) => ({ ...x, role: e.target.value as TeamRoleName }))}>
                <option value="admin">admin</option>
                <option value="member">member</option>
                <option value="viewer">viewer</option>
              </select>
            </label>
            <ul className="subject-rows">
              {r.subjects.map((s, j) => (
                <li key={j} className="subject-row">
                  <Segmented
                    legend={`Kind of subject ${j + 1}`}
                    hideLegend
                    name={`subject-${i}-${j}`}
                    value={s.kind}
                    options={[
                      { value: "user", label: "user" },
                      { value: "group", label: "group" },
                    ]}
                    onChange={(kind) => setSubject(i, j, { kind })}
                  />
                  <label className="field">
                    <span className="sr-only">Name of subject {j + 1}</span>
                    <input className="mono" value={s.name} onChange={(e) => setSubject(i, j, { name: e.target.value })} placeholder={`A ${s.kind} name`} />
                  </label>
                  <Button
                    variant="ghost"
                    small
                    icon="x"
                    aria-label={`Remove subject ${j + 1}`}
                    onClick={() => setRole(i, (x) => ({ ...x, subjects: x.subjects.filter((_, k) => k !== j) }))}
                  />
                </li>
              ))}
            </ul>
            <div className="buttons">
              <Button small icon="plus" onClick={() => setRole(i, (x) => ({ ...x, subjects: [...x.subjects, { kind: "user", name: "" }] }))}>
                Add subject
              </Button>
              <Button small variant="danger-ghost" icon="x" onClick={() => setRoles((rs) => rs.filter((_, k) => k !== i))}>
                Remove role
              </Button>
            </div>
          </fieldset>
        ))}
        <div className="buttons">
          <Button small icon="plus" onClick={() => setRoles((rs) => [...rs, { role: "viewer", subjects: [{ kind: "user", name: "" }] }])}>
            Add role
          </Button>
        </div>
      </fieldset>
      {error && (
        <p ref={errorRef} className="inline-error" role="alert" tabIndex={-1}>
          {error}
        </p>
      )}
      <div className="buttons">
        <Button ref={saveRef} type="submit" variant="primary" icon="check" disabled={busy || deleted}>
          Save
        </Button>
        <Button onClick={onCancel}>Cancel</Button>
      </div>
      {confirming && (
        <div className="confirm" role="group" aria-label="Save anyway?">
          <p>Save anyway? Git still holds the old values.</p>
          <div className="buttons">
            <Button
              variant="primary"
              small
              autoFocus
              onClick={() => {
                setConfirmed(true);
                void save(true);
              }}
            >
              Save anyway
            </Button>
            <Button
              small
              onClick={() => {
                setConfirming(false);
                saveRef.current?.focus();
              }}
            >
              Keep editing
            </Button>
          </div>
        </div>
      )}
    </form>
  );
}

// Current is the Team as read again after a 409, beside the user's edits.
function Current({ team }: { team: Team }) {
  const s = team.spec ?? {};
  return (
    <section className="team-current" aria-labelledby="team-current">
      <h3 id="team-current">Current</h3>
      <dl className="details">
        <dt>Display name</dt>
        <dd>{s.displayName || <span className="muted">none</span>}</dd>
        <dt>Max concurrent runs</dt>
        <dd>{s.maxConcurrentRuns || 1}</dd>
        <dt>Archive after</dt>
        <dd>{s.archiveAfter ? <span className="mono">{s.archiveAfter}</span> : <span className="muted">never</span>}</dd>
        <dt>Roles</dt>
        <dd>
          {(s.roles ?? []).length === 0 ? (
            <span className="muted">none</span>
          ) : (
            <ul className="subjects">
              {s.roles!.map((r, i) => (
                <li key={i} className="mono">
                  {r.role}: {r.subjects.map((x) => `${x.kind}:${x.name}`).join(", ")}
                </li>
              ))}
            </ul>
          )}
        </dd>
      </dl>
    </section>
  );
}
