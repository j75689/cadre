import { useState } from "react";
import { Link, useParams } from "react-router";
import { listAgents, type AgentSummary } from "./api";
import { agentLink, boardLink } from "./links";
import { useTeamName } from "./teamList";
import { ReadyBadge } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { Heading, useTitle } from "./ui/Page";
import { Loading, Skeleton } from "./ui/Skeleton";
import { usePolling } from "./usePolling";

// pollMs is how often the agents are read again.
const pollMs = 15000;

// Agents is the team's agents: whether each can take work, and why not.
export function Agents() {
  const { ns = "" } = useParams();
  const name = useTeamName(ns);
  const [agents, setAgents] = useState<AgentSummary[] | null>(null);
  const [error, setError] = useState("");
  // retry reads the agents again at once, after a failed read.
  const [retry, setRetry] = useState(0);
  useTitle("Agents", name);
  usePolling(
    async (signal) => {
      try {
        const res = await listAgents(ns, signal);
        setAgents(res.items);
        setError("");
      } catch (e) {
        if (!signal.aborted) setError(String((e as Error).message));
      }
    },
    pollMs,
    [ns, retry],
  );
  return (
    <main id="main" className="page">
      <div className="page-head">
        <Heading>Agents</Heading>
      </div>
      {error && (
        <Banner onRetry={() => setRetry((n) => n + 1)} className="page-banner">
          Could not load the agents: {error}
        </Banner>
      )}
      {agents === null && !error && (
        <Loading what="the agents" className="card table-loading">
          {[0, 1, 2].map((i) => (
            <Skeleton key={i} width="60%" height={16} />
          ))}
        </Loading>
      )}
      {agents?.length === 0 && (
        <div className="card empty">
          <p>This team has no agents. Agents are Agent resources in the namespace, applied with kubectl or GitOps.</p>
        </div>
      )}
      {agents && agents.length > 0 && (
        <div className="card table-wrap" tabIndex={0} role="region" aria-label="Agents">
          <table className="table">
            <thead>
              <tr>
                <th scope="col">Agent</th>
                <th scope="col">Status</th>
                <th scope="col">Reason</th>
                <th scope="col">Message</th>
                <th scope="col">Tasks</th>
              </tr>
            </thead>
            <tbody>
              {agents.map((a) => (
                <tr key={a.name}>
                  <td className="mono">
                    <Link to={agentLink(ns, a.name)}>{a.name}</Link>
                  </td>
                  <td>
                    <ReadyBadge ready={a.ready} reason={a.reason} />
                  </td>
                  {/* The badge already says Pending (readiness() in internal/api/server.go). */}
                  <td className="muted">{a.reason === "Pending" ? "" : a.reason}</td>
                  <td className="table-text">{a.message}</td>
                  <td>
                    <Link to={boardLink(ns, { assignee: a.name })} aria-label={`Tasks of ${a.name}`}>
                      Tasks
                    </Link>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </main>
  );
}
