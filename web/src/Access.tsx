import { useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";
import { flushSync } from "react-dom";
import { Link, NavLink, useLocation, useNavigate, useParams } from "react-router";
import {
  APIError,
  deleteAgentAccess,
  getAgentAccess,
  listAgentAccesses,
  putAgentAccess,
  rotateAgentAccessToken,
  type AgentAccess,
  type AgentAccessSpec,
  type AgentAccessToken,
  type AgentRole,
} from "./api";
import { RoleLine, useIdentityDetails, useRole } from "./Identity";
import { teamPath } from "./links";
import { can } from "./roles";
import { useTeamName } from "./teamList";
import { ReadyBadge, RoleBadge } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { Button } from "./ui/Button";
import { Heading, useTitle } from "./ui/Page";
import { Segmented } from "./ui/Segmented";
import { Loading, Skeleton } from "./ui/Skeleton";
import { Time } from "./ui/Time";
import { usePolling } from "./usePolling";

// pollMs is how often the AgentAccesses are read again.
const pollMs = 15000;

const settingsPath = (ns: string) => `${teamPath(ns)}/settings`;
const accessPath = (ns: string) => `${settingsPath(ns)}/access`;
const accessLink = (ns: string, name: string) => `${accessPath(ns)}/${encodeURIComponent(name)}`;

// useAccessShown says whether the Agent access pages are for the caller.
// The server serves AgentAccesses with logins only (Register in
// internal/api/server.go), to a team's admins.
function useAccessShown(ns: string): boolean {
  const { mode } = useIdentityDetails();
  const role = useRole(ns);
  return mode === "oidc" && can(role, "manage");
}

// SettingsNav is the settings' tabs: the Team, and Agent access for those
// who may manage it.
export function SettingsNav() {
  const { ns = "" } = useParams();
  const { pathname } = useLocation();
  const shown = useAccessShown(ns);
  const team = settingsPath(ns);
  return (
    <nav className="subnav" aria-label="Settings">
      {/* NavLink's end does not match the path with a trailing slash. */}
      <NavLink className="subnav-tab" to={team} end={pathname !== `${team}/`}>
        Team
      </NavLink>
      {shown && (
        <NavLink className="subnav-tab" to={accessPath(ns)}>
          Agent access
        </NavLink>
      )}
    </nav>
  );
}

// NotServed says why there are no AgentAccesses to show: the server runs
// without logins, and serves none.
function NotServed() {
  return (
    <div className="card empty">
      <p>
        Agent access needs logins. This cadre-server runs with <code>--auth=none</code>, so it serves no AgentAccess routes; use kubectl, or run it
        with <code>--auth=oidc</code>.
      </p>
    </div>
  );
}

// Gate stands in for an Agent access page until it may read: nothing is
// requested without logins, nor for a caller who is not an admin. With /me
// unread it waits; when /me failed the server decides.
function Gate({ ns, children }: { ns: string; children: ReactNode }) {
  const { mode } = useIdentityDetails();
  const role = useRole(ns);
  if (mode === "loading") return null;
  if (mode === "none") return <NotServed />;
  if (mode === "oidc" && !can(role, "manage")) {
    return (
      <div className="card empty">
        <p>Managing agent access needs the admin role.</p>
      </div>
    );
  }
  return children;
}

// AccessPage is the frame of an Agent access page: the breadcrumb, the
// heading and the settings' tabs.
function AccessPage({ ns, crumbs, title, children }: { ns: string; crumbs: ReactNode; title: ReactNode; children: ReactNode }) {
  const name = useTeamName(ns);
  return (
    <main id="main" className="page settings-page">
      <nav className="breadcrumb" aria-label="Breadcrumb">
        <ol>
          <li>
            <Link to={teamPath(ns)}>{name}</Link>
          </li>
          <li>
            <Link to={settingsPath(ns)}>Settings</Link>
          </li>
          {crumbs}
        </ol>
      </nav>
      <div className="page-head">
        <Heading>{title}</Heading>
        <RoleLine ns={ns} />
      </div>
      <SettingsNav />
      <Gate ns={ns}>{children}</Gate>
    </main>
  );
}

// Credential is how an AgentAccess's agent signs in: a ServiceAccount's
// token, or a token minted into a Secret, and when unless showMintedAt is
// false.
function Credential({ access, showMintedAt = true }: { access: AgentAccess; showMintedAt?: boolean }) {
  const sa = access.spec.serviceAccount?.name;
  if (sa) {
    return (
      <>
        ServiceAccount <code>{sa}</code>
      </>
    );
  }
  if (!access.tokenSecret) return <span className="muted">Token not minted yet</span>;
  return (
    <>
      Minted token in Secret <code>{access.tokenSecret}</code>
      {showMintedAt && access.mintedAt && (
        <span className="muted">
          {" "}
          minted <Time at={access.mintedAt} />
        </span>
      )}
    </>
  );
}

// failure is what a failed read shows: a plain 404, the mux's for a route
// it does not have, means the server serves no AgentAccesses. With logins
// the routes are always served, so there it comes from something in front
// of the server, and is shown as it is.
interface Failure {
  notServed: boolean;
  message: string;
  // forbidden is a 403: reading again cannot help, so there is no Retry.
  forbidden: boolean;
}

function failure(e: unknown, oidc: boolean): Failure {
  const api = e instanceof APIError ? e : undefined;
  return {
    notServed: !oidc && api?.status === 404 && api.code === "",
    message: String((e as Error).message),
    forbidden: api?.status === 403,
  };
}

// AccessList is the team's AgentAccesses: the credentials its agents sign
// in with, and whether each is ready.
export function AccessList() {
  const { ns = "" } = useParams();
  const name = useTeamName(ns);
  useTitle("Agent access", name);
  return (
    <AccessPage ns={ns} crumbs={<li aria-current="page">Agent access</li>} title="Agent access">
      <AccessCreate ns={ns} />
      <AccessTable ns={ns} />
    </AccessPage>
  );
}

// AccessCreate is "New agent access" and, once pressed, its form. A
// delete comes back here, and says so.
function AccessCreate({ ns }: { ns: string }) {
  const navigate = useNavigate();
  const deleted = (useLocation().state as { deleted?: string } | null)?.deleted;
  // said is set after the status mounts empty: a live region is
  // announced when it changes, not when it comes with its text.
  const [said, setSaid] = useState("");
  // The second render is the point: it is the change that is announced.
  // eslint-disable-next-line react-hooks/set-state-in-effect
  useEffect(() => setSaid(deleted ? `Deleted ${deleted}.` : ""), [deleted]);
  const [open, setOpen] = useState(false);
  return (
    <>
      <div className="access-toolbar">
        <p role="status" className="hint">
          {said}
        </p>
        {!open && (
          <Button variant="primary" icon="plus" onClick={() => setOpen(true)}>
            New agent access
          </Button>
        )}
      </div>
      {open && (
        <AccessForm
          ns={ns}
          label="New agent access"
          initial={{ name: "", role: "member", credential: "token", sa: "" }}
          onSaved={(a) => navigate(accessLink(ns, a.name))}
          onCancel={() => setOpen(false)}
        />
      )}
    </>
  );
}

function AccessTable({ ns }: { ns: string }) {
  const oidc = useIdentityDetails().mode === "oidc";
  const [items, setItems] = useState<AgentAccess[] | null>(null);
  const [failed, setFailed] = useState<Failure | null>(null);
  // retry reads the AgentAccesses again at once, after a failed read.
  const [retry, setRetry] = useState(0);
  usePolling(
    async (signal) => {
      try {
        const res = await listAgentAccesses(ns, signal);
        setItems(res.items);
        setFailed(null);
      } catch (e) {
        if (!signal.aborted) setFailed(failure(e, oidc));
      }
    },
    pollMs,
    [ns, retry],
  );
  if (failed?.notServed) return <NotServed />;
  return (
    <>
      {failed && (
        <Banner onRetry={failed.forbidden ? undefined : () => setRetry((n) => n + 1)} className="page-banner">
          Could not load the agent accesses: {failed.message}
        </Banner>
      )}
      {items === null && !failed && (
        <Loading what="the agent accesses" className="card table-loading">
          {[0, 1, 2].map((i) => (
            <Skeleton key={i} width="60%" height={16} />
          ))}
        </Loading>
      )}
      {items?.length === 0 && (
        <div className="card empty">
          <p>No AgentAccess yet.</p>
        </div>
      )}
      {items && items.length > 0 && (
        <div className="card table-wrap" tabIndex={0} role="region" aria-label="Agent accesses">
          <table className="table">
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Role</th>
                <th scope="col">Credential</th>
                <th scope="col">Status</th>
                <th scope="col">Reason</th>
                <th scope="col">Message</th>
              </tr>
            </thead>
            <tbody>
              {items.map((a) => (
                <tr key={a.name}>
                  <td className="access-name mono">
                    <Link to={accessLink(ns, a.name)}>{a.name}</Link>
                  </td>
                  <td>
                    <RoleBadge role={a.spec.role} />
                  </td>
                  <td className="table-text">
                    <Credential access={a} />
                  </td>
                  <td>
                    <ReadyBadge ready={a.ready} reason={a.reason} />
                  </td>
                  {/* The badge already says Pending (readiness() in internal/api/server.go). */}
                  <td className="muted">{a.reason === "Pending" ? "" : a.reason}</td>
                  <td className="table-text">{a.message}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

// AccessDetail is one AgentAccess.
export function AccessDetail() {
  const { ns = "", name = "" } = useParams();
  const team = useTeamName(ns);
  useTitle(name, "Agent access", team);
  return (
    <AccessPage
      ns={ns}
      crumbs={
        <>
          <li>
            <Link to={accessPath(ns)}>Agent access</Link>
          </li>
          <li aria-current="page" className="mono">
            {name}
          </li>
        </>
      }
      title={name}
    >
      <AccessCard ns={ns} name={name} />
    </AccessPage>
  );
}

// dnsLabel is an AgentAccess's name: the CRD refuses any other, so a name
// that is not one is not requested.
const dnsLabel = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/;

// A Draft is an AgentAccess as its form holds it. sa is kept while a
// minted token is picked, so going back to ServiceAccount loses nothing.
interface Draft {
  name: string;
  role: AgentRole;
  credential: "token" | "sa";
  sa: string;
}

function draftOf(a: AgentAccess): Draft {
  const sa = a.spec.serviceAccount?.name;
  return { name: a.name, role: a.spec.role, credential: sa ? "sa" : "token", sa: sa ?? "" };
}

// specOf is the spec a draft saves: a minted token is no ServiceAccount.
function specOf(d: Draft): AgentAccessSpec {
  return d.credential === "sa" ? { role: d.role, serviceAccount: { name: d.sa.trim() } } : { role: d.role };
}

interface Problem {
  message: string;
  // at is the field the problem is about, if any.
  at?: "name" | "sa";
}

// check is what the form can refuse before the server does.
function check(d: Draft): Problem | undefined {
  if (!dnsLabel.test(d.name)) return { message: "A name is at most 63 lower-case letters, digits and '-', and starts and ends with a letter or a digit.", at: "name" };
  if (d.credential === "sa" && !d.sa.trim()) return { message: "Name the ServiceAccount the agent signs in as.", at: "sa" };
  return undefined;
}

const sameSpec = (a: AgentAccessSpec, b: AgentAccessSpec) => a.role === b.role && a.serviceAccount?.name === b.serviceAccount?.name;

// AccessForm creates an AgentAccess or, given the version it was read at,
// edits it, its name then read-only. The draft is made once, from initial;
// the page's reads do not touch it. A refusal shows the server's message
// as it is; after a 409 the AgentAccess is read again, so the next save
// carries its new version. A 409 whose AgentAccess still has the spec the
// form last read is only its status moving, which the reconciler writes
// after each save and rotation: the form saves again at once.
function AccessForm({
  ns,
  label,
  initial,
  version,
  className = "",
  onSaved,
  onCancel,
}: {
  ns: string;
  label: string;
  initial: Draft;
  version?: string;
  className?: string;
  onSaved: (a: AgentAccess) => void;
  onCancel: () => void;
}) {
  const editing = version !== undefined;
  const [draft, setDraft] = useState(initial);
  const [base, setBase] = useState(version);
  // seen is the spec of the AgentAccess at base.
  const [seen, setSeen] = useState(() => specOf(initial));
  const [error, setError] = useState<Problem | undefined>(undefined);
  // failures keys the alert, so that the same message is focused and
  // announced again.
  const [failures, setFailures] = useState(0);
  const [busy, setBusy] = useState(false);
  const errorRef = useRef<HTMLParagraphElement>(null);
  // An error takes the focus, as the button that had it is disabled
  // during the request.
  useEffect(() => {
    if (failures) errorRef.current?.focus();
  }, [failures]);
  const fail = (p: Problem) => {
    setError(p);
    setFailures((n) => n + 1);
  };
  const invalid = (at: Problem["at"]) => error?.at === at;
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(undefined);
    const problem = check(draft);
    if (problem) {
      fail(problem);
      return;
    }
    setBusy(true);
    const put = (at?: string) => putAgentAccess(ns, draft.name, specOf(draft), at);
    try {
      onSaved(await put(base));
      return;
    } catch (err) {
      let refusal = err;
      if (editing && err instanceof APIError && err.code === "conflict") {
        let now: AgentAccess | undefined;
        try {
          now = await getAgentAccess(ns, draft.name);
        } catch {
          // The save's message stands; the page's reads say the rest.
        }
        if (now) {
          setBase(now.resourceVersion);
          setSeen(now.spec);
          if (sameSpec(now.spec, seen)) {
            try {
              onSaved(await put(now.resourceVersion));
              return;
            } catch (again) {
              refusal = again;
            }
          }
        }
      }
      fail({ message: (refusal as Error).message });
    }
    setBusy(false);
  };
  return (
    <form className={`form access-form ${className}`.trim()} onSubmit={submit} noValidate aria-label={label}>
      {editing && <p className="help">If AgentAccesses are applied from Git, change them there: an edit here drifts from Git.</p>}
      <label className="field">
        Name
        <input
          className="mono"
          value={draft.name}
          readOnly={editing}
          onChange={(e) => setDraft((d) => ({ ...d, name: e.target.value }))}
          autoFocus={!editing}
          autoComplete="off"
          spellCheck={false}
          aria-invalid={invalid("name") || undefined}
          aria-describedby={invalid("name") ? "access-name-help access-error" : "access-name-help"}
        />
        <span id="access-name-help" className="help">
          {editing ? "A name cannot be changed." : "Lower-case letters, digits and '-', at most 63."}
        </span>
      </label>
      <Segmented
        legend="Role"
        name="access-role"
        value={draft.role}
        options={[
          { value: "viewer", label: "viewer" },
          { value: "member", label: "member" },
        ]}
        onChange={(role) => setDraft((d) => ({ ...d, role }))}
      />
      <Segmented
        legend="Credential"
        name="access-credential"
        value={draft.credential}
        options={[
          { value: "token", label: "Minted token" },
          { value: "sa", label: "ServiceAccount" },
        ]}
        onChange={(credential) => setDraft((d) => ({ ...d, credential }))}
      >
        {draft.credential === "sa" ? (
          <label className="field">
            ServiceAccount name
            <input
              className="mono"
              value={draft.sa}
              onChange={(e) => setDraft((d) => ({ ...d, sa: e.target.value }))}
              autoComplete="off"
              spellCheck={false}
              aria-invalid={invalid("sa") || undefined}
              aria-describedby={invalid("sa") ? "access-error" : undefined}
            />
          </label>
        ) : (
          <span className="help">The reconciler mints the token into a Secret.</span>
        )}
      </Segmented>
      {error && (
        <p key={failures} ref={errorRef} id="access-error" className="inline-error" role="alert" tabIndex={-1}>
          {error.message}
        </p>
      )}
      <div className="buttons">
        <Button type="submit" variant="primary" icon={editing ? "check" : "plus"} disabled={busy}>
          {editing ? "Save" : "Create"}
        </Button>
        <Button onClick={onCancel}>Cancel</Button>
      </div>
    </form>
  );
}

// TokenOnce is a token just minted, the only time it is shown. AccessCard
// holds it in its state alone: it goes with Done, or when the page is
// left, the back/forward cache's way too. No live region holds it; the card's status says only that it is
// shown.
function TokenOnce({ token, onDone }: { token: AgentAccessToken; onDone: () => void }) {
  const input = useRef<HTMLInputElement>(null);
  const [copied, setCopied] = useState("");
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(token.token);
      setCopied("Copied");
    } catch {
      // A plain-HTTP origin has no clipboard: the token is selected, to copy by hand.
      input.current?.select();
      setCopied("Press Ctrl+C to copy it.");
    }
  };
  return (
    <div className="token-once card-body">
      <div className="token-row">
        <input ref={input} className="mono" readOnly value={token.token} aria-label="New token" autoComplete="off" spellCheck={false} autoFocus />
        <Button onClick={copy}>Copy</Button>
      </div>
      <span role="status" className="hint">
        {copied}
      </span>
      <p className="hint">
        This is the only time this token is shown. It is also in Secret <code>{token.tokenSecret}</code>.
      </p>
      <div className="buttons">
        <Button small onClick={onDone}>
          Done
        </Button>
      </div>
    </div>
  );
}

function AccessCard({ ns, name }: { ns: string; name: string }) {
  const oidc = useIdentityDetails().mode === "oidc";
  const navigate = useNavigate();
  const valid = dnsLabel.test(name);
  const [access, setAccess] = useState<AgentAccess | null>(null);
  const [missing, setMissing] = useState(false);
  const [failed, setFailed] = useState<Failure | null>(null);
  const [retry, setRetry] = useState(0);
  const [editing, setEditing] = useState(false);
  // note is the card's status: a save, or that a token is shown, never
  // the token.
  const [note, setNote] = useState("");
  // token is a token just minted: here only, never kept anywhere else.
  const [token, setToken] = useState<AgentAccessToken | null>(null);
  const [confirming, setConfirming] = useState<"rotate" | "delete" | null>(null);
  const [busy, setBusy] = useState(false);
  // refused is a rotation's or a delete's refusal, as the server says it.
  const [refused, setRefused] = useState("");
  const rotateRef = useRef<HTMLButtonElement>(null);
  const deleteRef = useRef<HTMLButtonElement>(null);
  // A page left for the back/forward cache is kept as it is, and Back
  // shows it again: the token goes before that, at once, as the page is
  // frozen right after pagehide. left counts the times it was left, so a
  // rotation answered after that shows no token either.
  const left = useRef(0);
  useEffect(() => {
    const hide = () => {
      left.current++;
      flushSync(() => {
        setToken(null);
        setNote("");
      });
    };
    window.addEventListener("pagehide", hide);
    return () => window.removeEventListener("pagehide", hide);
  }, []);
  usePolling(
    async (signal) => {
      if (!valid) return;
      try {
        setAccess(await getAgentAccess(ns, name, signal));
        setMissing(false);
        setFailed(null);
      } catch (e) {
        if (signal.aborted) return;
        if (e instanceof APIError && e.status === 404 && e.code === "not_found") {
          // Gone, a token or a form for it goes too: an access made again
          // with this name does not bring them back.
          setAccess(null);
          setMissing(true);
          setFailed(null);
          setToken(null);
          setNote("");
          setEditing(false);
          setConfirming(null);
          return;
        }
        setFailed(failure(e, oidc));
      }
    },
    pollMs,
    [ns, name, retry],
  );
  const ask = (what: "rotate" | "delete") => {
    setRefused("");
    setConfirming(what);
  };
  const keep = () => {
    (confirming === "rotate" ? rotateRef : deleteRef).current?.focus();
    setConfirming(null);
  };
  const rotate = async () => {
    setBusy(true);
    setToken(null);
    setNote("");
    const at = left.current;
    try {
      const minted = await rotateAgentAccessToken(ns, name);
      if (left.current === at) {
        setToken(minted);
        setNote("New token shown below");
      } else {
        // The server rotated it, and the old one is gone: say so, and
        // where the new one is, but not the token.
        setNote(`Rotated while the page was left, so the token is not shown: it is in Secret ${minted.tokenSecret}; rotate again to see one here`);
      }
    } catch (e) {
      setRefused((e as Error).message);
    }
    setBusy(false);
    setConfirming(null);
  };
  const remove = async () => {
    setBusy(true);
    try {
      await deleteAgentAccess(ns, name);
    } catch (e) {
      // A 404 is an AgentAccess already gone, as the delete would leave it.
      if (!(e instanceof APIError && e.status === 404)) {
        setRefused((e as Error).message);
        setBusy(false);
        setConfirming(null);
        return;
      }
    }
    navigate(accessPath(ns), { state: { deleted: name } });
  };
  if (failed?.notServed) return <NotServed />;
  if (missing || !valid) {
    return (
      <div className="card empty">
        <p>
          No AgentAccess <code>{name}</code>.
        </p>
        <p>
          <Link to={accessPath(ns)}>All agent accesses</Link>
        </p>
      </div>
    );
  }
  const minted = access && !access.spec.serviceAccount;
  return (
    <>
      {failed && (
        <Banner onRetry={failed.forbidden ? undefined : () => setRetry((n) => n + 1)} className="page-banner">
          Could not load the agent access: {failed.message}
        </Banner>
      )}
      {!access && !failed && (
        <Loading what="the agent access" className="card card-pad stack">
          <Skeleton width="40%" />
          <Skeleton width="60%" />
          <Skeleton width="30%" />
        </Loading>
      )}
      {access && (
        <section className="card" aria-labelledby="access">
          <div className="card-head access-head">
            <h2 id="access">AgentAccess</h2>
            <span role="status" className="hint access-status">
              {note}
            </span>
            {!editing && (
              <div className="buttons">
                <Button
                  small
                  icon="settings"
                  onClick={() => {
                    setNote("");
                    setEditing(true);
                  }}
                >
                  Edit
                </Button>
                {minted && (
                  <Button ref={rotateRef} small icon="refresh" onClick={() => ask("rotate")}>
                    Rotate token
                  </Button>
                )}
                <Button ref={deleteRef} small variant="danger-ghost" icon="x" onClick={() => ask("delete")}>
                  Delete
                </Button>
              </div>
            )}
          </div>
          {editing ? (
            <AccessForm
              ns={ns}
              label={`Edit ${name}`}
              initial={draftOf(access)}
              version={access.resourceVersion}
              onSaved={(a) => {
                setAccess(a);
                setEditing(false);
                setNote("Saved");
              }}
              onCancel={() => setEditing(false)}
            />
          ) : (
            <dl className="details card-body">
              <dt>Name</dt>
              <dd className="mono">{access.name}</dd>
              <dt>Role</dt>
              <dd>
                <RoleBadge role={access.spec.role} />
              </dd>
              <dt>Credential</dt>
              <dd>
                <Credential access={access} showMintedAt={false} />
              </dd>
              <dt>Status</dt>
              <dd>
                <ReadyBadge ready={access.ready} reason={access.reason} />
              </dd>
              {access.reason && access.reason !== "Pending" && (
                <>
                  <dt>Reason</dt>
                  <dd>{access.reason}</dd>
                </>
              )}
              {access.message && (
                <>
                  <dt>Message</dt>
                  <dd>{access.message}</dd>
                </>
              )}
              {access.mintedAt && (
                <>
                  <dt>Minted</dt>
                  <dd>
                    <Time at={access.mintedAt} />
                  </dd>
                </>
              )}
            </dl>
          )}
          {confirming === "rotate" && (
            <div className="confirm card-body" role="group" aria-label="Rotate the token?">
              <p>Rotate the token? The old one stops working at once.</p>
              <div className="buttons">
                <Button variant="danger-solid" small autoFocus disabled={busy} onClick={rotate}>
                  Yes, rotate it
                </Button>
                <Button small onClick={keep}>
                  Keep it
                </Button>
              </div>
            </div>
          )}
          {confirming === "delete" && (
            <div className="confirm card-body" role="group" aria-label={`Delete ${name}?`}>
              <p>Delete {name}? Its agent loses access at once.</p>
              <div className="buttons">
                <Button variant="danger-solid" small autoFocus disabled={busy} onClick={remove}>
                  Yes, delete it
                </Button>
                <Button small onClick={keep}>
                  Keep it
                </Button>
              </div>
            </div>
          )}
          {refused && (
            <p className="inline-error card-body" role="alert">
              {refused}
            </p>
          )}
          {token && (
            <TokenOnce
              token={token}
              onDone={() => {
                setToken(null);
                setNote("");
                rotateRef.current?.focus();
              }}
            />
          )}
          {minted && (
            <p className="hint card-body">
              {access.tokenSecret ? (
                <>
                  The token is never shown again after it is minted; read it from Secret <code>{access.tokenSecret}</code>, key <code>token</code>, with{" "}
                  <code>{`kubectl -n ${ns} get secret ${access.tokenSecret} -o jsonpath='{.data.token}' | base64 -d`}</code>, or rotate it to see a new one
                  here.
                </>
              ) : (
                "The reconciler mints the token into a Secret; rotate it to see one here."
              )}
            </p>
          )}
        </section>
      )}
    </>
  );
}
