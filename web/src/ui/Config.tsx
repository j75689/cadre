import { useRef, useState } from "react";
import type { Condition, ConfigResource } from "../api";
import { toYAML } from "../yaml";
import { Button } from "./Button";
import { Time } from "./Time";

// The parts of a configuration resource's page, any kind's: its
// conditions, its metadata and its YAML, each a card, as the server
// answers them.

// masked is the value a member is shown in place of one a spec may hold a
// credential in (owner decisions 1 and 1b in internal/api/config.go).
export const masked = "(set)";

// ConditionsCard is the resource's conditions, each with the generation
// it is about: one older than the resource's is about a spec since changed.
export function ConditionsCard({ resource }: { resource: ConfigResource }) {
  const { conditions, generation } = resource;
  return (
    <section className="card" aria-labelledby="conditions">
      <div className="card-head">
        <h2 id="conditions">Conditions</h2>
      </div>
      {conditions.length === 0 ? (
        <p className="card-body muted">No conditions yet: the controller has not reported on it.</p>
      ) : (
        <div className="table-wrap" tabIndex={0} role="region" aria-label="Conditions table">
          <table className="table">
            <thead>
              <tr>
                <th scope="col">Type</th>
                <th scope="col">Status</th>
                <th scope="col">Reason</th>
                <th scope="col">Message</th>
                <th scope="col">Last transition</th>
                <th scope="col">Generation</th>
              </tr>
            </thead>
            <tbody>
              {conditions.map((c: Condition) => (
                <tr key={c.type}>
                  <td>{c.type}</td>
                  <td>{c.status}</td>
                  <td className="muted">{c.reason}</td>
                  <td className="table-text">{c.message}</td>
                  <td>{c.lastTransitionTime && <Time at={c.lastTransitionTime} />}</td>
                  <td className="num">
                    {c.observedGeneration ?? "–"} of {generation}
                    {c.observedGeneration !== undefined && c.observedGeneration < generation && <span className="muted"> (older spec)</span>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

// MetadataCard is the resource's metadata: annotations only when the
// caller is shown them.
export function MetadataCard({ resource }: { resource: ConfigResource }) {
  const { metadata: m, generation, observedGeneration } = resource;
  const pairs = (map: Record<string, string>) =>
    Object.entries(map).map(([k, v]) => (
      <code key={k} className="sub">
        {k}={v}
      </code>
    ));
  return (
    <section className="card" aria-labelledby="metadata">
      <div className="card-head">
        <h2 id="metadata">Metadata</h2>
      </div>
      <dl className="details card-body">
        <dt>Created</dt>
        <dd>
          <Time at={m.creationTimestamp} />
        </dd>
        <dt>Generation</dt>
        <dd className="num">
          {generation}, observed {observedGeneration}
        </dd>
        <dt>Labels</dt>
        <dd>{m.labels ? pairs(m.labels) : <span className="muted">None</span>}</dd>
        {m.annotations && (
          <>
            <dt>Annotations</dt>
            <dd>{pairs(m.annotations)}</dd>
          </>
        )}
        <dt>UID</dt>
        <dd className="mono">{m.uid}</dd>
        <dt>Resource version</dt>
        <dd className="mono">{m.resourceVersion}</dd>
      </dl>
    </section>
  );
}

// YAMLCard is the resource's metadata, spec and status as the server
// answered them, printed as YAML, to read and copy; never to edit, as Git
// owns them.
export function YAMLCard({ resource }: { resource: ConfigResource }) {
  const { metadata, spec, status } = resource;
  const text = toYAML({ metadata, ...(spec && { spec }), ...(status && { status }) });
  const pre = useRef<HTMLPreElement>(null);
  const [copied, setCopied] = useState("");
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied("Copied");
    } catch {
      // A plain-HTTP origin has no clipboard: the text is selected, to copy by hand.
      if (pre.current) getSelection()?.selectAllChildren(pre.current);
      setCopied("Press Ctrl+C to copy it.");
    }
  };
  return (
    <section className="card" aria-labelledby="yaml">
      <div className="card-head">
        <h2 id="yaml">YAML</h2>
        <span role="status" className="hint">
          {copied}
        </span>
        <Button small onClick={copy}>
          Copy
        </Button>
      </div>
      {/^(.*: |\s*- )\(set\)$/m.test(text) && (
        <p className="hint card-body">
          Values shown as <code>{masked}</code> are set but hidden from members; admins see them.
        </p>
      )}
      <pre ref={pre} className="yaml card-body" tabIndex={0} role="region" aria-label="YAML text">
        {text}
      </pre>
    </section>
  );
}
