import { useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router";
import { SettingsNav } from "./Access";
import { APIError, getTeam, type Team } from "./api";
import { RoleLine, useIdentity, useRole } from "./Identity";
import { taskLink, teamPath } from "./links";
import { can } from "./roles";
import { TeamForm } from "./TeamForm";
import { useTeamName } from "./teamList";
import { RoleBadge } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { Button } from "./ui/Button";
import { Heading, useTitle } from "./ui/Page";
import { Loading, Skeleton } from "./ui/Skeleton";
import { Time } from "./ui/Time";
import { usePolling } from "./usePolling";

// The server runs one task at a time when a Team sets no cap
// (max(MaxConcurrentRuns, 1) wherever it is checked).
const defaultMaxRuns = 1;

// Settings shows the namespace's Team. The server sends its roles and its
// status to its admins only (project()). The limits are sent to every role,
// but the page shows them to admins only too; the others see its name.
export function Settings() {
  const { ns = "" } = useParams();
  const role = useRole(ns);
  const name = useTeamName(ns);
  // team is null until read, and after a 404 (missing).
  const [team, setTeam] = useState<Team | null>(null);
  const [missing, setMissing] = useState(false);
  const [error, setError] = useState("");
  // forbidden is a 403: reading again cannot help, so there is no Retry.
  const [forbidden, setForbidden] = useState(false);
  // retry reads the Team again after a failed read.
  const [retry, setRetry] = useState(0);
  // editing is the Team card's form, for an admin. left is whether the
  // form was closed, so the Edit button that comes back takes the focus.
  const [editing, setEditing] = useState(false);
  const [left, setLeft] = useState(false);
  const [saved, setSaved] = useState(false);
  // demoted is a save that took the caller's own admin role: the server
  // answered with the Team as a non-admin sees it, without its roles and
  // status, so the page stops showing what only admins see.
  const [demoted, setDemoted] = useState(false);
  const demotedRef = useRef<HTMLParagraphElement>(null);
  // lost is a 403 no_role after the Team was read, on a save or a read:
  // the Team was deleted, or the caller's role on it taken away, so the
  // role /me gave is out of date. It is read again, and the page edits no
  // more.
  const [lost, setLost] = useState(false);
  const lostRef = useRef<HTMLDivElement>(null);
  const missingRef = useRef<HTMLParagraphElement>(null);
  // closed is whether a read closed the form, so the message that took its
  // place takes the focus.
  const closed = useRef(false);
  const { refresh } = useIdentity();
  useEffect(() => {
    if (demoted) demotedRef.current?.focus();
  }, [demoted]);
  useEffect(() => {
    if (!closed.current) return;
    closed.current = false;
    (lost ? lostRef : missingRef).current?.focus();
  }, [lost, missing]);
  const leave = () => {
    setEditing(false);
    setLeft(true);
  };
  const lose = () => {
    if (!lost) refresh();
    closed.current ||= editing;
    setEditing(false);
    setLost(true);
    setTeam(null);
    setError("");
  };
  useTitle("Settings", name);
  usePolling(
    async (signal) => {
      try {
        setTeam(await getTeam(ns, signal));
        setMissing(false);
        setError("");
        setForbidden(false);
      } catch (e) {
        if (signal.aborted) return;
        if (e instanceof APIError && e.code === "no_role" && (team || lost)) {
          lose();
          return;
        }
        if (e instanceof APIError && e.status === 404) {
          closed.current ||= editing;
          setTeam(null);
          setMissing(true);
          setEditing(false);
          setError("");
          return;
        }
        setError(String((e as Error).message));
        setForbidden(e instanceof APIError && e.status === 403);
      }
    },
    15000,
    [ns, retry],
  );
  const admin = can(role, "manage") && !demoted && !lost;
  return (
    <main id="main" className="page settings-page">
      <nav className="breadcrumb" aria-label="Breadcrumb">
        <ol>
          <li>
            <Link to={teamPath(ns)}>{name}</Link>
          </li>
          <li aria-current="page">Settings</li>
        </ol>
      </nav>
      <div className="page-head">
        <Heading>Settings</Heading>
        <RoleLine ns={ns} />
      </div>
      <SettingsNav />
      {error && (
        <Banner onRetry={forbidden ? undefined : () => setRetry((n) => n + 1)}>
          Could not load the team: {error}
        </Banner>
      )}
      {lost && (
        <div ref={lostRef} tabIndex={-1} role="alert">
          <Banner className="banner-warning">Your role on {ns} is gone or has changed: its Team was deleted, or someone changed its roles.</Banner>
        </div>
      )}
      {missing && (
        <div className="card empty">
          <p ref={missingRef} tabIndex={-1}>
            This namespace has no Team. A namespace becomes a team when it has a Team resource:
          </p>
          <pre>cadre team init -n {ns}</pre>
        </div>
      )}
      {!team && !missing && !error && !lost && (
        <Loading what="the settings" className="card card-pad stack">
          <Skeleton width="40%" />
          <Skeleton width="60%" />
          <Skeleton width="30%" />
        </Loading>
      )}
      {team && (
        <div className="stack">
          <section className="card" aria-labelledby="team">
            <div className="card-head">
              <h2 id="team">Team</h2>
              <span role="status" className="hint team-saved">
                {saved && "Saved"}
              </span>
              {admin && !editing && (
                <Button
                  small
                  icon="settings"
                  autoFocus={left}
                  onClick={() => {
                    setSaved(false);
                    setEditing(true);
                  }}
                >
                  Edit
                </Button>
              )}
            </div>
            {editing ? (
              <TeamForm
                team={team}
                onSaved={(t, sent) => {
                  setTeam(t);
                  setSaved(true);
                  if ((sent.roles ?? []).length > 0 && !t.spec?.roles?.length) {
                    setDemoted(true);
                    refresh();
                  }
                  leave();
                }}
                onLost={() => {
                  lose();
                  setRetry((n) => n + 1);
                }}
                onCancel={leave}
              />
            ) : (
              <dl className="details card-body">
                <dt>Display name</dt>
                <dd>{team.spec?.displayName || <span className="muted">{ns}</span>}</dd>
                <dt>Namespace</dt>
                <dd className="mono">{team.metadata.namespace}</dd>
                {admin && (
                  <>
                    <dt>Max concurrent runs</dt>
                    <dd>{team.spec?.maxConcurrentRuns || <>{defaultMaxRuns} <span className="muted">(default)</span></>}</dd>
                    <dt>Archive after</dt>
                    <dd>{team.spec?.archiveAfter ? <span className="mono">{team.spec.archiveAfter}</span> : <span className="muted">never</span>}</dd>
                  </>
                )}
              </dl>
            )}
            {demoted ? (
              <p ref={demotedRef} className="hint card-body" tabIndex={-1}>
                You no longer have the admin role on {ns}, so its limits, roles and run slots are hidden.
              </p>
            ) : (
              !admin && role !== undefined && <p className="hint card-body">This page shows a team's limits, roles and run slots to its admins only.</p>
            )}
          </section>
          {admin && <Roles team={team} />}
          {admin && <RunSlots team={team} />}
        </div>
      )}
    </main>
  );
}

function Roles({ team }: { team: Team }) {
  const roles = team.spec?.roles ?? [];
  return (
    <section className="card" aria-labelledby="roles">
      <div className="card-head">
        <h2 id="roles">Roles</h2>
      </div>
      {roles.length === 0 ? (
        <p className="hint card-body">No roles are given on this team.</p>
      ) : (
        <div className="table-wrap" role="region" aria-label="Roles table" tabIndex={0}>
          <table className="table" aria-labelledby="roles">
            <thead>
              <tr>
                <th scope="col">Role</th>
                <th scope="col">Subjects</th>
              </tr>
            </thead>
            <tbody>
              {roles.map((r, i) => (
                <tr key={`${r.role}-${i}`}>
                  <td>
                    <RoleBadge role={r.role} />
                  </td>
                  <td className="table-text">
                    <ul className="subjects">
                      {r.subjects.map((s) => (
                        <li key={`${s.kind}:${s.name}`} className="mono">
                          {s.kind}:{s.name}
                        </li>
                      ))}
                    </ul>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

function RunSlots({ team }: { team: Team }) {
  const ns = team.metadata.namespace;
  const slots = team.status?.slots ?? [];
  const running = team.status?.running ?? 0;
  const max = team.spec?.maxConcurrentRuns || defaultMaxRuns;
  return (
    <section className="card" aria-labelledby="slots">
      <div className="card-head">
        <h2 id="slots">Run slots</h2>
        <span className="hint">{running} of {max} in use</span>
      </div>
      {slots.length === 0 ? (
        <p className="hint card-body">No run holds a slot.</p>
      ) : (
        <div className="table-wrap" role="region" aria-label="Run slots table" tabIndex={0}>
          <table className="table" aria-labelledby="slots">
            <thead>
              <tr>
                <th scope="col">Task</th>
                <th scope="col">Run id</th>
                <th scope="col">Acquired</th>
              </tr>
            </thead>
            <tbody>
              {slots.map((s) => (
                <tr key={s.runId}>
                  <td>
                    <Link to={taskLink(ns, s.task, s.runId)}>{s.task}</Link>
                  </td>
                  <td className="mono">{s.runId}</td>
                  <td>
                    <Time at={s.acquiredAt} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
