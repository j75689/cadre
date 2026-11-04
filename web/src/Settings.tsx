import { useEffect, useRef, useState, type Ref } from "react";
import { Link, useParams } from "react-router";
import { SettingsNav } from "./Access";
import { APIError, getTeam, type Team } from "./api";
import { RoleLine, useIdentity, useIdentityDetails, useRole } from "./Identity";
import { taskLink, teamPath } from "./links";
import { can } from "./roles";
import { TeamForm } from "./TeamForm";
import { teamName, useTeamName } from "./teamList";
import { RoleBadge } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { Button } from "./ui/Button";
import { useFocusWhenGone } from "./ui/focus";
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
  const listed = useTeamName(ns);
  // team is null until read, and after a 404 (missing).
  const [team, setTeam] = useState<Team | null>(null);
  // The Team read here, or saved, names the team before /teams is read
  // again.
  const name = team ? teamName(team) : listed;
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
  // more, unless the role is given again (gone). After a save that took
  // the caller's admin role, its alert does not blame someone else.
  const [lost, setLost] = useState(false);
  const lostRef = useRef<HTMLDivElement>(null);
  // gone is whether /me, read again while lost, gave no role on ns: one
  // that gives a role after that is a role given again, and the page shows
  // what the role allows once more.
  const gone = useRef(false);
  const missingRef = useRef<HTMLParagraphElement>(null);
  // closed is whether a read closed the form, or took the Team away while
  // the focus was in it (on Edit), so the message that took its place takes
  // the focus.
  const closed = useRef(false);
  const teamRef = useRef<HTMLDivElement>(null);
  const closing = () => editing || Boolean(teamRef.current?.contains(document.activeElement));
  // Focus elsewhere stays, unless what holds it goes too, such as the
  // admins' settings tabs once /me is read again: then it goes where the
  // Team card's would.
  const press = useFocusWhenGone();
  const losing = () => {
    const at = document.activeElement;
    if (closing()) closed.current = true;
    else if (at instanceof HTMLElement && at !== document.body) press(at, () => lostRef.current ?? missingRef.current);
  };
  const { mode, refresh } = useIdentity();
  const { me, held } = useIdentityDetails();
  useEffect(() => {
    if (demoted) demotedRef.current?.focus();
  }, [demoted]);
  useEffect(() => {
    if (!closed.current) return;
    closed.current = false;
    (lost ? lostRef : missingRef).current?.focus();
  }, [lost, missing]);
  // dropped is whether the /me answer that took the role took what held
  // the focus too (Edit, a tab), before or after the Team's 403: the alert
  // takes it then, from the body or the heading LiveRoles gives it to.
  const dropped = useRef(false);
  useEffect(() => {
    const was = held?.current;
    dropped.current = !role && !!was && !was.isConnected;
  }, [role, held]);
  useEffect(() => {
    if (!lost) return;
    if (!role) {
      gone.current = true;
      const at = document.activeElement;
      if (dropped.current && (at === document.body || at === lostRef.current?.closest("main")?.querySelector("h1"))) lostRef.current?.focus();
      return;
    }
    if (!gone.current) return;
    gone.current = false;
    // The alert goes: its focus goes to the heading, not to the body.
    if (lostRef.current?.contains(document.activeElement)) lostRef.current.closest("main")?.querySelector("h1")?.focus();
    setLost(false);
  }, [lost, role]);
  const leave = () => {
    setEditing(false);
    setLeft(true);
  };
  const lose = () => {
    if (!lost) refresh();
    losing();
    setEditing(false);
    setLost(true);
    setTeam(null);
    setError("");
  };
  useTitle("Settings", name);
  usePolling(
    async (signal) => {
      // The Team is read with the role /me gives, once it is read.
      if (mode === "loading") return;
      try {
        const got = await getTeam(ns, signal);
        // A read for a lower role than /me gave takes Edit away before /me
        // says so: its focus goes to the heading, as LiveRoles' would.
        const at = document.activeElement;
        if (!got.spec?.roles?.length && !me?.admin && at instanceof HTMLElement) press(at, () => teamRef.current?.closest("main")?.querySelector("h1"));
        setTeam(got);
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
          losing();
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
    // A new role reads the Team again at once.
    [ns, retry, mode, role],
    { ns, topic: "config", match: (h) => h.event === "config" && h.kind === "Team" },
  );
  // full is whether the Team read is the one its admins see. The server
  // leaves its roles out for anyone else (project()), and the answer says
  // nothing else of who it was read as, so a Team with roles is an admin's
  // read. One without is too for a bootstrap admin, or without a policy:
  // the server leaves those reads whole. A Team read for a lower role than
  // /me gives now is not one to edit: its form would save no roles.
  const full = !!team?.spec?.roles?.length || !!me?.admin;
  const admin = can(role, "manage") && full && !demoted && !lost;
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
          <Banner className="banner-warning">
            Your role on {ns} is gone or has changed{demoted ? "." : ": its Team was deleted, or someone changed its roles."}
          </Banner>
        </div>
      )}
      {missing && <NoTeam ns={ns} ref={missingRef} />}
      {!team && !missing && !error && !lost && (
        <Loading what="the settings" className="card card-pad stack">
          <Skeleton width="40%" />
          <Skeleton width="60%" />
          <Skeleton width="30%" />
        </Loading>
      )}
      {team && (
        <div className="stack" ref={teamRef}>
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
                onSaved={(t) => {
                  setTeam(t);
                  setSaved(true);
                  // A Team saved without roles answers a bootstrap admin
                  // whole, so it is theirs; anyone else's role went with
                  // them.
                  if (!t.spec?.roles?.length && !me?.admin) {
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
              !can(role, "manage") && role !== undefined && <p className="hint card-body">This page shows a team's limits, roles and run slots to its admins only.</p>
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

// NoTeam says namespace ns has no Team, and how to make it one.
export function NoTeam({ ns, ref }: { ns: string; ref?: Ref<HTMLParagraphElement> }) {
  return (
    <div className="card empty">
      <p ref={ref} tabIndex={-1}>
        This namespace has no Team. A namespace becomes a team when it has a Team resource:
      </p>
      <pre>cadre team init -n {ns}</pre>
    </div>
  );
}
