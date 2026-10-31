import { useState, type ReactNode } from "react";
import { Link, useParams } from "react-router";
import type { AgentSummary } from "./api";
import { useTeamName } from "./teamList";
import { ReadyBadge } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { Heading, useTitle } from "./ui/Page";
import { Loading, Skeleton } from "./ui/Skeleton";
import { usePolling } from "./usePolling";

// pollMs is how often the resources are read again.
const pollMs = 15000;

// ConfigList is the team's configuration resources of one kind: whether
// each is ready, and why not. title names the list ("Agents"), noun one
// in its first column and sentences ("agent"); link is one's page. empty
// says how one is added; extra is a last column, if any. kind is the
// resources' kind ("Agent"), whose watch hints read them again.
export function ConfigList({
  kind,
  title,
  noun,
  link,
  load,
  empty,
  extra,
}: {
  kind: string;
  title: string;
  noun: string;
  link: (ns: string, name: string) => string;
  load: (ns: string, signal: AbortSignal) => Promise<{ items: AgentSummary[] }>;
  empty: string;
  extra?: { header: string; cell: (ns: string, name: string) => ReactNode };
}) {
  const { ns = "" } = useParams();
  const name = useTeamName(ns);
  const [items, setItems] = useState<AgentSummary[] | null>(null);
  const [error, setError] = useState("");
  // retry reads the resources again at once, after a failed read.
  const [retry, setRetry] = useState(0);
  useTitle(title, name);
  usePolling(
    async (signal) => {
      try {
        const res = await load(ns, signal);
        setItems(res.items);
        setError("");
      } catch (e) {
        if (!signal.aborted) setError(String((e as Error).message));
      }
    },
    pollMs,
    [ns, retry],
    { ns, topic: "config", match: (h) => h.event === "config" && h.kind === kind },
  );
  return (
    <main id="main" className="page">
      <div className="page-head">
        <Heading>{title}</Heading>
      </div>
      {error && (
        <Banner onRetry={() => setRetry((n) => n + 1)} className="page-banner">
          Could not load the {noun}s: {error}
        </Banner>
      )}
      {items === null && !error && (
        <Loading what={`the ${noun}s`} className="card table-loading">
          {[0, 1, 2].map((i) => (
            <Skeleton key={i} width="60%" height={16} />
          ))}
        </Loading>
      )}
      {items?.length === 0 && (
        <div className="card empty">
          <p>{empty}</p>
        </div>
      )}
      {items && items.length > 0 && (
        <div className="card table-wrap" tabIndex={0} role="region" aria-label={title}>
          <table className="table">
            <thead>
              <tr>
                <th scope="col">{noun.charAt(0).toUpperCase() + noun.slice(1)}</th>
                <th scope="col">Status</th>
                <th scope="col">Reason</th>
                <th scope="col">Message</th>
                {extra && <th scope="col">{extra.header}</th>}
              </tr>
            </thead>
            <tbody>
              {items.map((a) => (
                <tr key={a.name}>
                  <td className="mono">
                    <Link to={link(ns, a.name)}>{a.name}</Link>
                  </td>
                  <td>
                    <ReadyBadge ready={a.ready} reason={a.reason} />
                  </td>
                  {/* The badge already says Pending (readiness() in internal/api/server.go). */}
                  <td className="muted">{a.reason === "Pending" ? "" : a.reason}</td>
                  <td className="table-text">{a.message}</td>
                  {extra && <td>{extra.cell(ns, a.name)}</td>}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </main>
  );
}
