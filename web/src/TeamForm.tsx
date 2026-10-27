import { useEffect, useRef, useState, type FormEvent } from "react";
import { APIError, getTeam, putTeam, type RoleSubject, type Team, type TeamRole, type TeamRoleName, type TeamSpec } from "./api";
import { managedByGitOps } from "./gitops";
import { Banner } from "./ui/Banner";
import { Button } from "./ui/Button";
import { Segmented } from "./ui/Segmented";

// A Go duration, as time.ParseDuration takes it, without a minus: the
// server takes positive ones only.
const duration = /^\+?((\d+\.?\d*|\.\d+)(ns|us|µs|μs|ms|s|m|h))+$/;
const nanos: Record<string, number> = { ns: 1, us: 1e3, µs: 1e3, μs: 1e3, ms: 1e6, s: 1e9, m: 60e9, h: 3600e9 };

// The largest duration Go takes, 2^63 - 1 ns.
const maxDuration = 2n ** 63n - 1n;

// nanosOf is a duration in whole nanoseconds, as Go counts it: the whole
// part exactly, the fraction as Go's float64 does. Undefined when it is not
// a duration; it may be over Go's int64.
function nanosOf(v: string): bigint | undefined {
  if (!duration.test(v)) return undefined;
  let n = 0n;
  for (const [, whole = "", fraction = "", unit] of v.matchAll(/(\d*)\.?(\d*)(ns|us|µs|μs|ms|s|m|h)/g))
    n += BigInt(whole || "0") * BigInt(nanos[unit]) + BigInt(Math.floor(Number(fraction || "0") * (nanos[unit] / 10 ** fraction.length)));
  return n;
}

// durationOf is a duration in whole nanoseconds, as Go counts it, or
// undefined when it is not one or overflows Go's int64.
export function durationOf(v: string): number | undefined {
  const n = nanosOf(v);
  return n === undefined || n > maxDuration ? undefined : Number(n);
}

// The largest cap the server's int32 takes.
const maxInt32 = 2147483647;

// A Problem is why a save cannot go through, and the field it is about, if
// any: maxConcurrentRuns, archiveAfter, role-<i> or subject-<i>-<j>,
// numbered from 0.
export interface Problem {
  message: string;
  at?: string;
}

// validate checks what the form can before the server does, numbering
// roles and subjects as the form shows them. A number field the browser
// could not read holds NaN.
export function validate(d: TeamSpec): Problem | undefined {
  const max = d.maxConcurrentRuns;
  if (max !== undefined && (!Number.isInteger(max) || max < 0 || max > maxInt32))
    return { message: `Max concurrent runs is a whole number, from 0 to ${maxInt32}.`, at: "maxConcurrentRuns" };
  const after = d.archiveAfter?.trim();
  if (after) {
    const n = nanosOf(after);
    if (n !== undefined && n > maxDuration) return { message: "Archive after is too long: the longest the server takes is 2562047h47m16.854775807s.", at: "archiveAfter" };
    if (!(n !== undefined && n > 0n)) return { message: "Archive after is a duration longer than 0, such as 168h or 30m.", at: "archiveAfter" };
  }
  for (const [i, r] of (d.roles ?? []).entries()) {
    if (r.subjects.length === 0) return { message: `Role ${i + 1} needs at least one subject.`, at: `role-${i}` };
    const j = r.subjects.findIndex((s) => !s.name.trim());
    if (j >= 0) return { message: `Role ${i + 1}, subject ${j + 1} needs a name.`, at: `subject-${i}-${j}` };
  }
  return undefined;
}

// refusal is a server message as a Problem: its roles and subjects
// numbered as the form does, and the field its prefix names.
export function refusal(msg: string): Problem {
  const m = /^(maxConcurrentRuns|archiveAfter)\b|^roles\[(\d+)\](?:\.subjects\[(\d+)\])?/.exec(msg);
  const at = !m ? undefined : (m[1] ?? (m[3] !== undefined ? `subject-${m[2]}-${m[3]}` : `role-${m[2]}`));
  return { message: formIndexes(msg), at };
}

// formIndexes numbers the roles and subjects of a server message as the
// form does: roles[1].subjects[0] is Role 2, subject 1.
export function formIndexes(msg: string): string {
  return msg.replace(/roles\[(\d+)\](?:\.subjects\[(\d+)\])?/g, (_, i: string, j?: string) => `Role ${Number(i) + 1}${j ? `, subject ${Number(j) + 1}` : ""}`);
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

type Field = "displayName" | "maxConcurrentRuns" | "archiveAfter" | "roles";

// same says, per field, what a spec would save, so that 168h and 168h0m0s
// or an empty cap and 1 compare equal.
const same: Record<Field, (s: TeamSpec) => string> = {
  displayName: (s) => s.displayName?.trim() ?? "",
  maxConcurrentRuns: (s) => String(s.maxConcurrentRuns || 1),
  archiveAfter: (s) => {
    const a = s.archiveAfter?.trim() ?? "";
    return String(durationOf(a) ?? a);
  },
  roles: (s) => JSON.stringify(toSpec(s).roles),
};

// shown is a field of a Team's spec as "Current" shows it.
function shown(s: TeamSpec, f: Field): string {
  switch (f) {
    case "displayName":
      return s.displayName || "none";
    case "maxConcurrentRuns":
      return String(s.maxConcurrentRuns || 1);
    case "archiveAfter":
      return s.archiveAfter || "never";
    case "roles":
      return (s.roles ?? []).map((r) => `${r.role}: ${r.subjects.map((x) => `${x.kind}:${x.name}`).join(", ")}`).join("; ") || "none";
  }
}

// ids joins the ids of an aria-describedby, leaving out the unset ones.
const ids = (...xs: (string | false)[]) => xs.filter(Boolean).join(" ") || undefined;

// TeamForm edits a Team's spec. The draft is made once, from the Team the
// form opened with; the page's reads do not touch it. A save carries the
// version of base, the Team the draft was last checked against: after a
// 409 that is the Team read again, shown as "Current" above the form, and
// each field someone else changed since the form opened is marked, unless
// the form already holds their value. onSaved gets the Team saved and the
// spec sent; onLost is a save refused for want of a role: the Team was
// deleted, or the caller's admin role taken away. The form is data-dirty
// while the draft would save something other than the Team it opened with.
export function TeamForm({
  team,
  onSaved,
  onLost,
  onCancel,
}: {
  team: Team;
  onSaved: (t: Team, sent: TeamSpec) => void;
  onLost: () => void;
  onCancel: () => void;
}) {
  const ns = team.metadata.namespace;
  const [opened] = useState(team);
  const [base, setBase] = useState(team);
  const [draft, setDraft] = useState(() => draftOf(team));
  const [conflict, setConflict] = useState(false);
  // stale is whether base was read again after a 409, so it is shown.
  const [stale, setStale] = useState(false);
  // deleted is a 409 after which the Team could not be found: there is
  // nothing left to save to.
  const [deleted, setDeleted] = useState(false);
  const [error, setError] = useState<Problem | undefined>(undefined);
  // failures counts the failed saves. It keys the alert, so that a failure
  // with the same message is focused and announced again.
  const [failures, setFailures] = useState(0);
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [confirmed, setConfirmed] = useState(false);
  const gitops = managedByGitOps(base.metadata);
  // changed is whether someone else changed field f since the form opened,
  // to a value the form does not hold.
  const changed = (f: Field) =>
    stale && !deleted && same[f](base.spec ?? {}) !== same[f](opened.spec ?? {}) && same[f](base.spec ?? {}) !== same[f](draft);
  const note = (f: Field) => changed(f) && `changed-${f}`;
  // invalid is whether the error is about the field at, which it then
  // describes.
  const invalid = (at: string) => error?.at === at;
  const fail = (p: Problem) => {
    setError(p);
    setFailures((n) => n + 1);
  };
  const errorRef = useRef<HTMLParagraphElement>(null);
  const conflictRef = useRef<HTMLDivElement>(null);
  const saveRef = useRef<HTMLButtonElement>(null);
  const rolesRef = useRef<HTMLFieldSetElement>(null);
  const addRoleRef = useRef<HTMLButtonElement>(null);
  // removed is the index of the role just removed: the focus goes to the
  // role now there, else the one before, else "Add role".
  const removed = useRef<number | undefined>(undefined);
  // An error takes the focus, as the button that had it is disabled
  // during the request, and so does each conflict: a save clears it
  // first.
  useEffect(() => {
    if (failures) errorRef.current?.focus();
  }, [failures]);
  useEffect(() => {
    if (conflict) conflictRef.current?.focus();
  }, [conflict]);
  useEffect(() => {
    if (removed.current === undefined) return;
    const selects = rolesRef.current?.querySelectorAll("select") ?? [];
    (selects[Math.min(removed.current, selects.length - 1)] ?? addRoleRef.current)?.focus();
    removed.current = undefined;
  });
  const roles = draft.roles ?? [];
  const setRoles = (f: (rs: TeamRole[]) => TeamRole[]) => setDraft((d) => ({ ...d, roles: f(d.roles ?? []) }));
  const setRole = (i: number, f: (r: TeamRole) => TeamRole) => setRoles((rs) => rs.map((r, k) => (k === i ? f(r) : r)));
  const setSubject = (i: number, j: number, patch: Partial<RoleSubject>) =>
    setRole(i, (r) => ({ ...r, subjects: r.subjects.map((s, k) => (k === j ? { ...s, ...patch } : s)) }));
  // save asks first, once, on a Team that looks managed by GitOps; force
  // is the confirmation's own save.
  const save = async (force: boolean) => {
    setError(undefined);
    const problem = validate(draft);
    if (problem) {
      fail(problem);
      return;
    }
    if (gitops && !confirmed && !force) {
      setConfirming(true);
      return;
    }
    setConfirming(false);
    setConflict(false);
    setBusy(true);
    const spec = toSpec(draft);
    try {
      onSaved(await putTeam(ns, spec, base.metadata.resourceVersion), spec);
      return;
    } catch (err) {
      if (err instanceof APIError && err.code === "conflict") {
        setConflict(true);
        try {
          setBase(await getTeam(ns));
          setStale(true);
        } catch (read) {
          if (read instanceof APIError && read.status === 404) setDeleted(true);
          else fail({ message: `Could not read the team again: ${(read as Error).message}` });
        }
      } else if (err instanceof APIError && err.code === "no_role") {
        onLost();
        return;
      } else {
        fail(refusal((err as Error).message));
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
      noValidate
      aria-label="Edit the team"
      aria-describedby={gitops ? "team-gitops" : undefined}
      data-dirty={JSON.stringify(toSpec(draft)) !== JSON.stringify(toSpec(opened.spec ?? {})) || undefined}>
      {conflict && (
        <div ref={conflictRef} tabIndex={-1} role="alert">
          <Banner className="banner-warning">
            {deleted
              ? "The Team was deleted since it was read, so there is nothing to save to."
              : "Someone else changed the team since it was read. Their current values are shown below, and the fields they changed are marked. Saving again replaces their values with the ones in the form."}
          </Banner>
        </div>
      )}
      {gitops && (
        <Banner className="banner-warning" icon="git-branch" id="team-gitops">
          This Team looks managed by GitOps ({gitops}). A change here drifts from Git and may be reverted by the next sync.
        </Banner>
      )}
      {stale && !deleted && <Current team={base} />}
      <label className="field">
        Display name
        <input
          value={draft.displayName ?? ""}
          onChange={(e) => setDraft((d) => ({ ...d, displayName: e.target.value }))}
          aria-describedby={ids(note("displayName"))}
          autoFocus
        />
        {changed("displayName") && <Changed field="displayName" spec={base.spec ?? {}} />}
      </label>
      <label className="field">
        Max concurrent runs
        <input
          type="number"
          min={0}
          step={1}
          value={Number.isNaN(draft.maxConcurrentRuns) ? "" : (draft.maxConcurrentRuns ?? "")}
          onChange={(e) => {
            // A number the browser cannot read, such as 1e, is "" too.
            const v = e.target.validity.badInput ? NaN : e.target.value === "" ? undefined : Number(e.target.value);
            setDraft((d) => ({ ...d, maxConcurrentRuns: v }));
          }}
          aria-invalid={invalid("maxConcurrentRuns") || undefined}
          aria-describedby={ids("max-runs-help", note("maxConcurrentRuns"), invalid("maxConcurrentRuns") && "team-error")}
        />
        <span id="max-runs-help" className="help">
          How many of the team's tasks run at once. Empty or 0 is saved as 1.
        </span>
        {changed("maxConcurrentRuns") && <Changed field="maxConcurrentRuns" spec={base.spec ?? {}} />}
      </label>
      <label className="field">
        Archive after
        <input
          className="mono"
          value={draft.archiveAfter ?? ""}
          onChange={(e) => setDraft((d) => ({ ...d, archiveAfter: e.target.value }))}
          aria-invalid={invalid("archiveAfter") || undefined}
          aria-describedby={ids("archive-after-help", note("archiveAfter"), invalid("archiveAfter") && "team-error")}
        />
        <span id="archive-after-help" className="help">
          A duration such as 168h or 30m. Empty: never.
        </span>
        {changed("archiveAfter") && <Changed field="archiveAfter" spec={base.spec ?? {}} />}
      </label>
      <fieldset ref={rolesRef} aria-describedby={ids(note("roles"))}>
        <legend>Roles</legend>
        {changed("roles") && <Changed field="roles" spec={base.spec ?? {}} />}
        {roles.length === 0 && <p className="help">No roles are given on this team.</p>}
        {roles.map((r, i) => (
          <fieldset key={i} className="role-edit" aria-describedby={ids(invalid(`role-${i}`) && "team-error")}>
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
                    <input
                      className="mono"
                      value={s.name}
                      onChange={(e) => setSubject(i, j, { name: e.target.value })}
                      placeholder={`A ${s.kind} name`}
                      aria-invalid={invalid(`subject-${i}-${j}`) || undefined}
                      aria-describedby={ids(invalid(`subject-${i}-${j}`) && "team-error")}
                    />
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
              <Button
                small
                variant="danger-ghost"
                icon="x"
                onClick={() => {
                  removed.current = i;
                  setRoles((rs) => rs.filter((_, k) => k !== i));
                }}
              >
                Remove role
              </Button>
            </div>
          </fieldset>
        ))}
        <div className="buttons">
          <Button ref={addRoleRef} small icon="plus" onClick={() => setRoles((rs) => [...rs, { role: "viewer", subjects: [{ kind: "user", name: "" }] }])}>
            Add role
          </Button>
        </div>
      </fieldset>
      {error && (
        <p key={failures} ref={errorRef} id="team-error" className="inline-error" role="alert" tabIndex={-1}>
          {error.message}
        </p>
      )}
      <div className="buttons">
        <Button ref={saveRef} type="submit" variant="primary" icon="check" disabled={busy || deleted}>
          Save
        </Button>
        <Button onClick={onCancel}>Cancel</Button>
      </div>
      {confirming && (
        <div className="confirm confirm-warning" role="group" aria-label="Save anyway?">
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

// Changed marks a field someone else changed, with their value.
function Changed({ field, spec }: { field: Field; spec: TeamSpec }) {
  return (
    <span id={`changed-${field}`} className="help changed">
      Changed by someone else: <span className="mono">{shown(spec, field)}</span>
    </span>
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
