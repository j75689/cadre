import { useState } from "react";
import { Link, useParams } from "react-router";
import { accessLink, settingsPath, SettingsNav } from "./Access";
import { APIError, listActivity, type Activity as Item } from "./api";
import { RoleLine } from "./Identity";
import { agentLink, modelProviderLink, notifierLink, runnerLink, scheduleLink, taskLink, teamPath, toolServerLink } from "./links";
import { useTeamName } from "./teamList";
import { Badge } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { Button } from "./ui/Button";
import { Icon } from "./ui/Icon";
import { Heading, useTitle } from "./ui/Page";
import { Loading, Skeleton } from "./ui/Skeleton";
import { Time } from "./ui/Time";
import { usePolling } from "./usePolling";

// activityPage is one "load more" of the activity.
export const activityPage = 50;

// pollMs is how often the activity is read again.
const pollMs = 15000;

// pages are where the kinds an Event may be about have one: a Team's is
// the settings.
const pages: Record<string, (ns: string, name: string) => string> = {
  Agent: agentLink,
  ModelProvider: modelProviderLink,
  ToolServer: toolServerLink,
  Schedule: scheduleLink,
  Notifier: notifierLink,
  Runner: runnerLink,
  Task: (ns, name) => taskLink(ns, name),
  Team: (ns) => settingsPath(ns),
  AgentAccess: accessLink,
};

// Activity is the team's recent activity, newest first, a page at a time:
// the Events about cadre's objects, which the server serves its admins
// only. They are read again on any configuration change, as no hint names
// an Event, and every pollMs even while live, as an AgentAccess change
// sends no hint.
export function Activity() {
  const { ns = "" } = useParams();
  const name = useTeamName(ns);
  const [items, setItems] = useState<Item[] | null>(null);
  const [more, setMore] = useState(false);
  const [shown, setShown] = useState(1);
  const [error, setError] = useState<APIError | Error | null>(null);
  // retry reads the activity again at once, after a failed read.
  const [retry, setRetry] = useState(0);
  useTitle("Activity", name);
  usePolling(
    async (signal) => {
      try {
        const all: Item[] = [];
        // seen drops an item a later page repeats: an Event written
        // between two page reads moves the rest down by one.
        const seen = new Set<string>();
        let full = false;
        for (let p = 0; p < shown; p++) {
          const res = await listActivity(ns, { offset: p * activityPage, limit: activityPage }, signal);
          for (const a of res.items) {
            const key = JSON.stringify(a);
            if (!seen.has(key)) {
              seen.add(key);
              all.push(a);
            }
          }
          full = res.items.length >= res.limit;
          if (!full) break;
        }
        setItems(all);
        setMore(full);
        setError(null);
      } catch (e) {
        if (!signal.aborted) setError(e as Error);
      }
    },
    pollMs,
    [ns, shown, retry],
    { ns, topic: "config", match: (h) => h.event === "config", steady: true },
  );
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
          <li aria-current="page">Activity</li>
        </ol>
      </nav>
      <div className="page-head">
        <Heading>Activity</Heading>
        <RoleLine ns={ns} />
      </div>
      <SettingsNav />
      <p className="hint">Recent activity (about the last hour); changes made with kubectl or GitOps are not listed.</p>
      {error && <Problem error={error} onRetry={() => setRetry((n) => n + 1)} />}
      {items === null && !error && (
        <Loading what="the activity" className="card table-loading">
          {[0, 1, 2].map((i) => (
            <Skeleton key={i} width="60%" height={16} />
          ))}
        </Loading>
      )}
      {items?.length === 0 && (
        <div className="card empty">
          <p>No activity in the last hour.</p>
        </div>
      )}
      {items && items.length > 0 && (
        <div className="card table-wrap" tabIndex={0} role="region" aria-label="Activity">
          <table className="table activity-table">
            <thead>
              <tr>
                <th scope="col">Time</th>
                <th scope="col">Type</th>
                <th scope="col">Reason</th>
                <th scope="col">Note</th>
                <th scope="col">Regarding</th>
                <th scope="col">Reported by</th>
                <th scope="col">Count</th>
              </tr>
            </thead>
            <tbody>
              {items.map((a, i) => (
                <tr key={i}>
                  <td className="muted">
                    <Time at={a.time} />
                  </td>
                  <td>
                    <TypeBadge type={a.type} />
                  </td>
                  <td>{a.reason}</td>
                  <td className="table-text">{a.note}</td>
                  <td className="activity-name">
                    <Regarding ns={ns} {...a.regarding} />
                  </td>
                  <td className="activity-name mono muted">{a.reportedBy}</td>
                  <td className="num">{a.count > 1 ? `×${a.count}` : ""}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {more && !error && (
            <div className="card-foot">
              <Button variant="ghost" small className="btn-block" onClick={() => setShown((p) => p + 1)}>
                Load more
              </Button>
            </div>
          )}
        </div>
      )}
    </main>
  );
}

// Problem is why the activity could not be read. A 403 is the caller's
// role, and a server_forbidden the server's own: reading again cannot help
// either, so neither has Retry.
function Problem({ error, onRetry }: { error: Error; onRetry: () => void }) {
  if (error instanceof APIError && error.code === "server_forbidden") {
    return (
      <Banner>
        cadre-server may not read Events: its ClusterRole predates the activity. Upgrade the chart (<code>helm upgrade</code>) or apply{" "}
        <code>config/rbac/role.yaml</code>.
      </Banner>
    );
  }
  const forbidden = error instanceof APIError && error.status === 403;
  return <Banner onRetry={forbidden ? undefined : onRetry}>Could not load the activity: {error.message}</Banner>;
}

// TypeBadge is an Event's type: Warning with an icon in the high hue,
// Normal plain.
function TypeBadge({ type }: { type: string }) {
  if (type !== "Warning") return <Badge>{type}</Badge>;
  return (
    <Badge tone="tone-high">
      <Icon name="alert-triangle" size={12} />
      {type}
    </Badge>
  );
}

// Regarding is the object an Event is about, a link to its page where its
// kind has one. A narrow column breaks it after the kind.
function Regarding({ ns, kind, name }: { ns: string; kind: string; name: string }) {
  const page = pages[kind];
  return (
    <span className="mono">
      <span className="muted">{kind}/</span>
      <wbr />
      {page ? <Link to={page(ns, name)}>{name}</Link> : name}
    </span>
  );
}
