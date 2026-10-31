import { useState, type ReactNode } from "react";
import { Link, useParams } from "react-router";
import { APIError, type ConfigResource } from "./api";
import { teamPath } from "./links";
import { useTeamName } from "./teamList";
import { ReadyBadge } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { ConditionsCard, MetadataCard, YAMLCard } from "./ui/Config";
import { Heading, useTitle } from "./ui/Page";
import { Loading, Skeleton } from "./ui/Skeleton";
import { usePolling } from "./usePolling";

// pollMs is how often the resource is read again, as its list does.
const pollMs = 15000;

interface Failure {
  message: string;
  // forbidden is a 403: reading again cannot help, so there is no Retry.
  forbidden: boolean;
}

// ConfigDetail is one configuration resource, any kind's: whether it is
// ready and why, its conditions, its metadata and its YAML, as the
// caller's role is shown it. noun names one in a sentence ("agent"),
// title names the kind's list ("Agents"), at section/<name> under the
// team. spec is the kind's own cards, given a spec: a viewer gets none.
export function ConfigDetail({
  noun,
  title,
  section,
  load,
  spec,
}: {
  noun: string;
  title: string;
  section: string;
  load: (ns: string, name: string, signal: AbortSignal) => Promise<ConfigResource>;
  spec?: (spec: Record<string, unknown>) => ReactNode;
}) {
  const { ns = "", name = "" } = useParams();
  const team = useTeamName(ns);
  const [resource, setResource] = useState<ConfigResource | null>(null);
  const [missing, setMissing] = useState(false);
  const [failed, setFailed] = useState<Failure | null>(null);
  // retry reads the resource again at once, after a failed read.
  const [retry, setRetry] = useState(0);
  const listPath = `${teamPath(ns)}/${section}`;
  useTitle(name, title, team);
  usePolling(
    async (signal) => {
      try {
        setResource(await load(ns, name, signal));
        setMissing(false);
        setFailed(null);
      } catch (e) {
        if (signal.aborted) return;
        const api = e instanceof APIError ? e : undefined;
        // A plain 404 is the mux's: a server that serves no such detail.
        if (api?.status === 404 && api.code === "not_found") {
          setResource(null);
          setMissing(true);
          setFailed(null);
          return;
        }
        setFailed({ message: String((e as Error).message), forbidden: api?.status === 403 });
      }
    },
    pollMs,
    [ns, name, retry],
  );
  return (
    <main id="main" className="page">
      <nav className="breadcrumb" aria-label="Breadcrumb">
        <ol>
          <li>
            <Link to={teamPath(ns)}>{team}</Link>
          </li>
          <li>
            <Link to={listPath}>{title}</Link>
          </li>
          <li aria-current="page" className="mono">
            {name}
          </li>
        </ol>
      </nav>
      <div className="page-head">
        <Heading>{name}</Heading>
        {resource && (
          <>
            <span className="muted">{resource.kind}</span>
            <ReadyBadge ready={resource.ready} reason={resource.reason} />
          </>
        )}
      </div>
      {failed && (
        <Banner onRetry={failed.forbidden ? undefined : () => setRetry((n) => n + 1)} className="page-banner">
          Could not load the {noun}: {failed.message}
        </Banner>
      )}
      {missing && (
        <div className="card empty">
          <p>
            No {noun} named <code>{name}</code> in this team.
          </p>
          <p>
            <Link to={listPath}>All {noun}s</Link>
          </p>
        </div>
      )}
      {!resource && !missing && !failed && (
        <Loading what={`the ${noun}`} className="card card-pad stack">
          <Skeleton width="40%" />
          <Skeleton width="60%" />
          <Skeleton width="30%" />
        </Loading>
      )}
      {resource && (
        <div className="stack">
          {/* As the server says it: Pending is readiness()'s, not worked out here. */}
          {(resource.message || (resource.reason && resource.reason !== "Pending")) && (
            <p>
              {resource.reason && resource.reason !== "Pending" && <strong>{resource.reason}: </strong>}
              {resource.message}
            </p>
          )}
          <ConditionsCard resource={resource} />
          {resource.spec ? (
            spec?.(resource.spec)
          ) : (
            <p className="hint">The spec and status are shown to the team&apos;s members and admins.</p>
          )}
          <MetadataCard resource={resource} />
          <YAMLCard resource={resource} />
        </div>
      )}
    </main>
  );
}
