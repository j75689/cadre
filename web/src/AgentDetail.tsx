import { Link, useParams } from "react-router";
import { getAgent, listSchedules } from "./api";
import { ConfigDetail } from "./ConfigDetail";
import { modelProviderLink, scheduleLink, toolServerLink } from "./links";
import { UsedByCard } from "./ui/UsedBy";

// Ref is a used resource's name, a link to its page.
export function Ref({ kind, name }: { kind: "ModelProvider" | "ToolServer"; name: string }) {
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

// AgentDetail is one Agent: whether it can take work and why, its
// conditions, what it uses, the Schedules that use it, and its YAML, as the
// caller's role is shown it.
export function AgentDetail() {
  const { name = "" } = useParams();
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
          {/* The Schedules that create tasks for the Agent: those whose list
              item names it as assignee. */}
          <UsedByCard
            label="Schedules"
            what="the schedules"
            kind="Schedule"
            load={listSchedules}
            pick={(items) => items.filter((s) => s.assignee?.kind === "Agent" && s.assignee.name === name).map((s) => s.name)}
            link={scheduleLink}
          />
        </>
      )}
    />
  );
}
