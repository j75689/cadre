import { useState } from "react";
import { Link, useParams } from "react-router";
import { APIError, getAgent, type ConfigResource } from "./api";
import { teamPath } from "./links";
import { useTeamName } from "./teamList";
import { ReadyBadge } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { ConditionsCard, MetadataCard, YAMLCard } from "./ui/Config";
import { Heading, useTitle } from "./ui/Page";
import { Loading, Skeleton } from "./ui/Skeleton";
import { usePolling } from "./usePolling";

// pollMs is how often the agent is read again, as the Agents page does.
const pollMs = 15000;

const agentsPath = (ns: string) => `${teamPath(ns)}/agents`;

// Ref is a used resource's name. The ModelProvider and ToolServer pages
// are not built yet: once they are, it links to them here.
function Ref({ name }: { kind: "ModelProvider" | "ToolServer"; name: string }) {
  return <code>{name}</code>;
}

// UsesCard is what the Agent's spec names: its ModelProvider and its
// ToolServers.
function UsesCard({ spec }: { spec: Record<string, unknown> }) {
  const model = typeof spec.modelRef === "string" ? spec.modelRef : "";
  const tools = Array.isArray(spec.toolRefs) ? spec.toolRefs.filter((t): t is string => typeof t === "string") : [];
  return (
    <section className="card" aria-labelledby="uses">
      <div className="card-head">
        <h2 id="uses">Uses</h2>
      </div>
      <dl className="details card-body">
        <dt>ModelProvider</dt>
        <dd>{model ? <Ref kind="ModelProvider" name={model} /> : <span className="muted">None</span>}</dd>
        <dt>ToolServers</dt>
        <dd>{tools.length > 0 ? tools.map((t) => <Ref key={t} kind="ToolServer" name={t} />) : <span className="muted">None</span>}</dd>
      </dl>
    </section>
  );
}

interface Failure {
  message: string;
  // forbidden is a 403: reading again cannot help, so there is no Retry.
  forbidden: boolean;
}

// AgentDetail is one Agent: whether it can take work and why, its
// conditions, what it uses, and its YAML, as the caller's role is shown it.
export function AgentDetail() {
  const { ns = "", name = "" } = useParams();
  const team = useTeamName(ns);
  const [agent, setAgent] = useState<ConfigResource | null>(null);
  const [missing, setMissing] = useState(false);
  const [failed, setFailed] = useState<Failure | null>(null);
  // retry reads the agent again at once, after a failed read.
  const [retry, setRetry] = useState(0);
  useTitle(name, "Agents", team);
  usePolling(
    async (signal) => {
      try {
        setAgent(await getAgent(ns, name, signal));
        setMissing(false);
        setFailed(null);
      } catch (e) {
        if (signal.aborted) return;
        const api = e instanceof APIError ? e : undefined;
        // A plain 404 is the mux's: a server that serves no Agent detail.
        if (api?.status === 404 && api.code === "not_found") {
          setAgent(null);
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
            <Link to={agentsPath(ns)}>Agents</Link>
          </li>
          <li aria-current="page" className="mono">
            {name}
          </li>
        </ol>
      </nav>
      <div className="page-head">
        <Heading>{name}</Heading>
        {agent && (
          <>
            <span className="muted">{agent.kind}</span>
            <ReadyBadge ready={agent.ready} reason={agent.reason} />
          </>
        )}
      </div>
      {failed && (
        <Banner onRetry={failed.forbidden ? undefined : () => setRetry((n) => n + 1)} className="page-banner">
          Could not load the agent: {failed.message}
        </Banner>
      )}
      {missing && (
        <div className="card empty">
          <p>
            No agent named <code>{name}</code> in this team.
          </p>
          <p>
            <Link to={agentsPath(ns)}>All agents</Link>
          </p>
        </div>
      )}
      {!agent && !missing && !failed && (
        <Loading what="the agent" className="card card-pad stack">
          <Skeleton width="40%" />
          <Skeleton width="60%" />
          <Skeleton width="30%" />
        </Loading>
      )}
      {agent && (
        <div className="stack">
          {/* As the server says it: Pending is readiness()'s, not worked out here. */}
          {(agent.message || (agent.reason && agent.reason !== "Pending")) && (
            <p>
              {agent.reason && agent.reason !== "Pending" && <strong>{agent.reason}: </strong>}
              {agent.message}
            </p>
          )}
          <ConditionsCard resource={agent} />
          {agent.spec ? (
            <UsesCard spec={agent.spec} />
          ) : (
            <p className="hint">The spec and status are shown to the team&apos;s members and admins.</p>
          )}
          <MetadataCard resource={agent} />
          <YAMLCard resource={agent} />
        </div>
      )}
    </main>
  );
}
