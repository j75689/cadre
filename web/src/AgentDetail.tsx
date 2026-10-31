import { useState } from "react";
import { Link, useParams } from "react-router";
import { APIError, getAgent, getSchedule, listSchedules, type ConfigResource } from "./api";
import { ConfigDetail } from "./ConfigDetail";
import { modelProviderLink, scheduleLink, toolServerLink } from "./links";
import { Banner } from "./ui/Banner";
import { Loading, Skeleton } from "./ui/Skeleton";
import { usePolling } from "./usePolling";

// pollMs is how often the Schedules are read again, as the Agent is.
const pollMs = 15000;

// Ref is a used resource's name, a link to its page.
function Ref({ kind, name }: { kind: "ModelProvider" | "ToolServer"; name: string }) {
  const { ns = "" } = useParams();
  return (
    <Link to={(kind === "ToolServer" ? toolServerLink : modelProviderLink)(ns, name)}>
      <code>{name}</code>
    </Link>
  );
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

// assigns says whether a Schedule's task template assigns its tasks to the
// Agent agent.
function assigns(schedule: ConfigResource, agent: string): boolean {
  const task = schedule.spec?.task as { assignee?: { kind?: string; name?: string } } | undefined;
  return task?.assignee?.kind === "Agent" && task.assignee.name === agent;
}

// UsedByCard is the Schedules that create tasks for the Agent. The list is
// summary-only, so each Schedule is read for its task template; one deleted
// since the list is left out. It is shown with the spec, to members and
// admins: a viewer is shown no Schedule's spec either.
function UsedByCard() {
  const { ns = "", name = "" } = useParams();
  const [schedules, setSchedules] = useState<string[] | null>(null);
  const [error, setError] = useState("");
  // retry reads the Schedules again at once, after a failed read.
  const [retry, setRetry] = useState(0);
  usePolling(
    async (signal) => {
      try {
        const { items } = await listSchedules(ns, signal);
        const read = await Promise.all(
          items.map((s) =>
            getSchedule(ns, s.name, signal).catch((e: unknown) => {
              if (e instanceof APIError && e.status === 404 && e.code === "not_found") return null;
              throw e;
            }),
          ),
        );
        setSchedules(read.filter((s): s is ConfigResource => s !== null && assigns(s, name)).map((s) => s.name));
        setError("");
      } catch (e) {
        if (!signal.aborted) setError(String((e as Error).message));
      }
    },
    pollMs,
    [ns, name, retry],
    { ns, topic: "config", match: (h) => h.event === "config" && h.kind === "Schedule" },
  );
  return (
    <section className="card" aria-labelledby="used-by">
      <div className="card-head">
        <h2 id="used-by">Used by</h2>
      </div>
      {error && <Banner onRetry={() => setRetry((n) => n + 1)}>Could not load the schedules: {error}</Banner>}
      <dl className="details card-body">
        <dt>Schedules</dt>
        <dd>
          {schedules === null ? (
            <Loading what="the schedules">
              <Skeleton width="30%" />
            </Loading>
          ) : schedules.length > 0 ? (
            schedules.map((s) => (
              <Link key={s} to={scheduleLink(ns, s)}>
                <code>{s}</code>
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

// AgentDetail is one Agent: whether it can take work and why, its
// conditions, what it uses, the Schedules that use it, and its YAML, as the
// caller's role is shown it.
export function AgentDetail() {
  return (
    <ConfigDetail
      kind="Agent"
      noun="agent"
      title="Agents"
      section="agents"
      load={getAgent}
      spec={(spec) => (
        <>
          <UsesCard spec={spec} />
          <UsedByCard />
        </>
      )}
    />
  );
}
