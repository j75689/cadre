import { Ref } from "./AgentDetail";
import { getNotifier, listNotifiers } from "./api";
import { ConfigDetail } from "./ConfigDetail";
import { ConfigList } from "./ConfigList";
import { notifierLink } from "./links";

// Notifiers is the team's notifiers: whether each is ready, and why not.
export function Notifiers() {
  return (
    <ConfigList
      kind="Notifier"
      title="Notifiers"
      noun="notifier"
      link={notifierLink}
      load={listNotifiers}
      empty="This team has no notifiers. Notifiers are Notifier resources in the namespace, applied with kubectl or GitOps."
    />
  );
}

// UsesCard is the ToolServer a Notifier calls a tool on, if it calls one
// rather than a webhook.
function UsesCard({ spec }: { spec: Record<string, unknown> }) {
  const ts = spec.toolServer as { name?: unknown; tool?: unknown } | undefined;
  if (typeof ts?.name !== "string") return null;
  return (
    <section className="card" aria-labelledby="uses">
      <div className="card-head">
        <h2 id="uses">Uses</h2>
      </div>
      <dl className="details card-body">
        <dt>ToolServer</dt>
        <dd>
          <Ref kind="ToolServer" name={ts.name} />
        </dd>
        <dt>Tool</dt>
        <dd>
          <code>{String(ts.tool ?? "")}</code>
        </dd>
      </dl>
    </section>
  );
}

// NotifierDetail is one Notifier: its health, conditions, what it uses,
// metadata and YAML, as the caller's role is shown it; its webhook, events
// and its last delivery (status.lastSent, status.lastError) are in the
// YAML.
export function NotifierDetail() {
  return <ConfigDetail kind="Notifier" noun="notifier" title="Notifiers" section="notifiers" load={getNotifier} spec={(spec) => <UsesCard spec={spec} />} />;
}
