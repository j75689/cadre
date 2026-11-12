import { useState } from "react";
import { Link, useParams } from "react-router";
import type { AgentSummary } from "../api";
import { usePolling } from "../usePolling";
import { Banner } from "./Banner";
import { Loading, Skeleton } from "./Skeleton";

// pollMs is how often the list is read again, as the resource is.
const pollMs = 15000;

// UsedByCard is the resources of one kind that use the page's resource:
// those pick finds in one read of their list, each a link. It is shown with
// the spec, to members and admins: a viewer's list items name nothing they
// use. label is the kind's plural, what the list in a message, and kind
// the kind whose changes read the list again.
export function UsedByCard({
  label,
  what,
  kind,
  load,
  pick,
  link,
}: {
  label: string;
  what: string;
  kind: string;
  load: (ns: string, signal: AbortSignal) => Promise<{ items: AgentSummary[] }>;
  pick: (items: AgentSummary[]) => string[];
  link: (ns: string, name: string) => string;
}) {
  const { ns = "", name = "" } = useParams();
  const [users, setUsers] = useState<string[] | null>(null);
  const [error, setError] = useState("");
  // retry reads the list again at once, after a failed read.
  const [retry, setRetry] = useState(0);
  usePolling(
    async (signal) => {
      try {
        const { items } = await load(ns, signal);
        setUsers(pick(items));
        setError("");
      } catch (e) {
        if (!signal.aborted) setError(String((e as Error).message));
      }
    },
    pollMs,
    [ns, name, retry],
    { ns, topic: "config", match: (h) => h.event === "config" && h.kind === kind },
  );
  return (
    <section className="card" aria-labelledby="used-by">
      <div className="card-head">
        <h2 id="used-by">Used by</h2>
      </div>
      {error && (
        <Banner onRetry={() => setRetry((n) => n + 1)}>
          Could not load {what}: {error}
        </Banner>
      )}
      <dl className="details card-body">
        <dt>{label}</dt>
        <dd>
          {users === null ? (
            <Loading what={what}>
              <Skeleton width="30%" />
            </Loading>
          ) : users.length > 0 ? (
            users.map((u) => (
              <Link key={u} to={link(ns, u)}>
                <code>{u}</code>
              </Link>
            ))
          ) : (
            <span className="muted">None</span>
          )}
        </dd>
      </dl>
    </section>
  );
}
