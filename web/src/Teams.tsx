import { useRef, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate } from "react-router";
import { APIError, putTeam } from "./api";
import { roleOf, useIdentityDetails } from "./Identity";
import { teamPath } from "./links";
import { teamName, useTeams } from "./teamList";
import { RoleBadge } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { Button } from "./ui/Button";
import { Heading, useTitle } from "./ui/Page";
import { Loading, Skeleton } from "./ui/Skeleton";

// validNamespace mirrors the server's validNamespace: a DNS-1123 label,
// as Kubernetes names a namespace.
export function validNamespace(ns: string): string {
  if (ns === "") return "A namespace is required.";
  if (ns.length > 63) return "A namespace is at most 63 characters.";
  if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(ns)) return "A namespace is lower-case letters, digits and '-', and starts and ends with a letter or a digit.";
  return "";
}

// CreateTeam makes a namespace a team, for a bootstrap admin: a PUT of
// its Team without a version creates it, then the board opens. autoFocus
// puts the caret in the Namespace field, for a form the caller just opened.
function CreateTeam({ autoFocus = false }: { autoFocus?: boolean }) {
  const { retry } = useTeams();
  const navigate = useNavigate();
  const [ns, setNs] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [error, setError] = useState<ReactNode>("");
  // nsInvalid is whether error is about the namespace, so the field is
  // marked invalid and described by it.
  const [nsInvalid, setNsInvalid] = useState(false);
  const [busy, setBusy] = useState(false);
  const nsField = useRef<HTMLInputElement>(null);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const name = ns.trim();
    const invalid = validNamespace(name);
    setError(invalid);
    setNsInvalid(!!invalid);
    if (invalid) {
      nsField.current?.focus();
      return;
    }
    setBusy(true);
    try {
      await putTeam(name, { displayName: displayName.trim() || undefined });
      retry();
      navigate(teamPath(name));
    } catch (err) {
      if (err instanceof APIError && err.code === "already_exists") {
        setError(
          <>
            {name} already has a Team. <Link to={teamPath(name)}>Open it</Link>
          </>,
        );
      } else if (err instanceof APIError && err.status === 404) {
        // The server's message is a bare "not found": the namespace
        // isn't in the cluster.
        setError(
          <>
            There is no namespace {name} in the cluster. Create it first (<code>kubectl create namespace {name}</code>), then create the team.
          </>,
        );
        setNsInvalid(true);
        nsField.current?.focus();
      } else {
        setError((err as Error).message);
      }
      setBusy(false);
    }
  };
  return (
    <form id="create-team-form" className="card form" onSubmit={submit} noValidate aria-labelledby="create-team">
      <h2 id="create-team">Create a team</h2>
      <label className="field">
        Namespace
        <input
          ref={nsField}
          value={ns}
          onChange={(e) => setNs(e.target.value)}
          className="mono"
          required
          autoFocus={autoFocus}
          aria-invalid={nsInvalid}
          aria-describedby={nsInvalid ? "namespace-help create-team-error" : "namespace-help"}
        />
        <span id="namespace-help" className="help">
          The Kubernetes namespace the team's tasks and agents live in.
        </span>
      </label>
      <label className="field">
        Display name <input value={displayName} onChange={(e) => setDisplayName(e.target.value)} />
      </label>
      <div className="buttons">
        <Button type="submit" variant="primary" icon="plus" disabled={busy}>
          Create team
        </Button>
      </div>
      {error && (
        <p id="create-team-error" className="inline-error" role="alert">
          {error}
        </p>
      )}
    </form>
  );
}

// Teams is the team picker: a card per team the caller may see. A
// bootstrap admin can also create a team here.
export function Teams() {
  const { mode, me } = useIdentityDetails();
  const { teams, error, retry } = useTeams();
  const [creating, setCreating] = useState(false);
  useTitle("Teams");
  return (
    <main id="main" className="page">
      <div className="page-head">
        <Heading>Teams</Heading>
        {me?.admin && teams && teams.length > 0 && (
          <Button icon="plus" aria-expanded={creating} aria-controls="create-team-form" onClick={() => setCreating((c) => !c)}>
            New team
          </Button>
        )}
      </div>
      {creating && teams && teams.length > 0 && (
        <div className="form-page team-create">
          <CreateTeam autoFocus />
        </div>
      )}
      {error && (
        <Banner onRetry={retry} className="page-banner">
          Could not load the teams: {error}
        </Banner>
      )}
      {teams === null && !error && (
        <Loading what="the teams" className="team-grid">
          {[0, 1, 2].map((i) => (
            <div className="card team-card" key={i} aria-hidden="true">
              <Skeleton width="60%" height={16} />
              <Skeleton width="35%" height={12} />
            </div>
          ))}
        </Loading>
      )}
      {teams?.length === 0 && !me?.admin && (
        <div className="card empty">
          <p>No team yet. A namespace becomes a team when it has a Team resource:</p>
          <pre>cadre team init -n &lt;namespace&gt;</pre>
        </div>
      )}
      {teams?.length === 0 && me?.admin && (
        <div className="form-page stack">
          <p className="empty">
            No team yet. A namespace becomes a team when it has a Team resource: create it here, or with <code>cadre team init -n &lt;namespace&gt;</code>.
          </p>
          <CreateTeam />
        </div>
      )}
      {teams && teams.length > 0 && (
        <ul className="team-grid">
          {teams.map((t) => {
            const ns = t.metadata.namespace;
            const role = roleOf(me, ns);
            return (
              <li key={ns} className="card team-card">
                <Link to={`/t/${encodeURIComponent(ns)}`}>{teamName(t)}</Link>
                <div className="row">
                  <span className="mono">{ns}</span>
                  {mode === "oidc" && role && <RoleBadge role={role} />}
                </div>
              </li>
            );
          })}
        </ul>
      )}
    </main>
  );
}
