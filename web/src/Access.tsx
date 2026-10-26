import { useState, type ReactNode } from "react";
import { Link, NavLink, useLocation, useParams } from "react-router";
import { APIError, getAgentAccess, listAgentAccesses, type AgentAccess } from "./api";
import { RoleLine, useIdentityDetails, useRole } from "./Identity";
import { teamPath } from "./links";
import { can } from "./roles";
import { useTeamName } from "./teamList";
import { ReadyBadge, RoleBadge } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { Heading, useTitle } from "./ui/Page";
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
// token, or a token minted into a Secret, and when unless minted is false.
function Credential({ access, minted = true }: { access: AgentAccess; minted?: boolean }) {
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
      {minted && access.mintedAt && (
        <span className="muted">
          {" "}
          minted <Time at={access.mintedAt} />
        </span>
      )}
    </>
  );
}

// failure is what a failed read shows: a plain 404, the mux's for a route
// it does not have, means the server serves no AgentAccesses.
interface Failure {
  notServed: boolean;
  message: string;
  // forbidden is a 403: reading again cannot help, so there is no Retry.
  forbidden: boolean;
}

function failure(e: unknown): Failure {
  const api = e instanceof APIError ? e : undefined;
  return {
    notServed: api?.status === 404 && api.code === "",
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
      <AccessTable ns={ns} />
    </AccessPage>
  );
}

function AccessTable({ ns }: { ns: string }) {
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
        if (!signal.aborted) setFailed(failure(e));
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

function AccessCard({ ns, name }: { ns: string; name: string }) {
  const [access, setAccess] = useState<AgentAccess | null>(null);
  const [missing, setMissing] = useState(false);
  const [failed, setFailed] = useState<Failure | null>(null);
  const [retry, setRetry] = useState(0);
  usePolling(
    async (signal) => {
      try {
        setAccess(await getAgentAccess(ns, name, signal));
        setMissing(false);
        setFailed(null);
      } catch (e) {
        if (signal.aborted) return;
        if (e instanceof APIError && e.status === 404 && e.code === "not_found") {
          setAccess(null);
          setMissing(true);
          setFailed(null);
          return;
        }
        setFailed(failure(e));
      }
    },
    pollMs,
    [ns, name, retry],
  );
  if (failed?.notServed) return <NotServed />;
  if (missing) {
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
          <div className="card-head">
            <h2 id="access">AgentAccess</h2>
          </div>
          <dl className="details card-body">
            <dt>Name</dt>
            <dd className="mono">{access.name}</dd>
            <dt>Role</dt>
            <dd>
              <RoleBadge role={access.spec.role} />
            </dd>
            <dt>Credential</dt>
            <dd>
              <Credential access={access} minted={false} />
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
          {access.tokenSecret && !access.spec.serviceAccount && (
            <p className="hint card-body">
              The token is never shown again after it is minted; read it from Secret <code>{access.tokenSecret}</code> with kubectl.
            </p>
          )}
        </section>
      )}
    </>
  );
}
