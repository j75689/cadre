import { getAgent } from "./api";
import { ConfigDetail } from "./ConfigDetail";

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

// AgentDetail is one Agent: whether it can take work and why, its
// conditions, what it uses, and its YAML, as the caller's role is shown it.
export function AgentDetail() {
  return <ConfigDetail noun="agent" title="Agents" section="agents" load={getAgent} spec={(spec) => <UsesCard spec={spec} />} />;
}
