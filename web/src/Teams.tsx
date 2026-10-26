import { Link } from "react-router";
import { roleOf, useIdentityDetails } from "./Identity";
import { teamName, useTeams } from "./teamList";
import { RoleBadge } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { Heading, useTitle } from "./ui/Page";
import { Loading, Skeleton } from "./ui/Skeleton";

// Teams is the team picker: a card per team the caller may see.
export function Teams() {
  const { mode, me } = useIdentityDetails();
  const { teams, error, retry } = useTeams();
  useTitle("Teams");
  return (
    <main id="main" className="page">
      <div className="page-head">
        <Heading>Teams</Heading>
      </div>
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
      {teams?.length === 0 && (
        <div className="card empty">
          <p>No team yet. A namespace becomes a team when it has a Team resource:</p>
          <pre>cadre team init -n &lt;namespace&gt;</pre>
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
