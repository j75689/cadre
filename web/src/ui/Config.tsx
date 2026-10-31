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

// hasMasked says whether a value holds one shown as masked.
const hasMasked = (v: unknown): boolean => v === masked || (typeof v === "object" && v !== null && Object.values(v).some(hasMasked));

// maskedURL matches a URL with parts shown as masked: its user info, a
// query value or a valueless query part, its fragment (maskURL in
// internal/api/config.go).
const maskedURL = /:\/\/\(set\)@|[?&]([^&#=]*=)?\(set\)(?=[&#]|$)|#\(set\)$/;

// hasMaskedURL says whether the resource holds a URL shown with masked
// parts: only a ModelProvider's spec.baseURL, the one the server masks so.
// Elsewhere such a text is an admin's, as written.
const hasMaskedURL = ({ kind, spec }: ConfigResource): boolean =>
  kind === "ModelProvider" && typeof spec?.baseURL === "string" && maskedURL.test(spec.baseURL);

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

// apiVersion is every configuration kind's group and version
// (GroupVersion in api/v1alpha1/groupversion_info.go).
export const apiVersion = "cadre.dev/v1alpha1";

// manifest is the resource as one to kubectl apply: what Git holds, so not
// the fields the server sets (uid, resourceVersion, creationTimestamp,
// status), with the name and namespace in its metadata.
function manifest({ kind, name, metadata: { labels, annotations }, spec }: ConfigResource, namespace: string) {
  return { apiVersion, kind, metadata: { name, namespace, ...(labels && { labels }), ...(annotations && { annotations }) }, ...(spec && { spec }) };
}

// YAMLCard is the resource as a manifest, printed as YAML, to read and
// copy; never to edit, as Git owns it. Its status is shown under it, not
// copied. A copy applies as is only with the whole spec: a viewer's has
// none, a member's may hold masked values, and the note beside Copy says
// so.
export function YAMLCard({ resource, namespace }: { resource: ConfigResource; namespace: string }) {
  const { spec, status } = resource;
  const text = toYAML(manifest(resource, namespace));
  const hidden = hasMasked(spec) || hasMaskedURL(resource);
  const note = !spec ? "No spec; not a manifest to apply." : hidden ? "Contains hidden values; not applyable as is." : "";
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
        {note && (
          <span id="yaml-note" className="hint">
            {note}
          </span>
        )}
        <Button small onClick={copy} aria-describedby={note ? "yaml-note" : undefined}>
          {note ? "Copy" : "Copy manifest"}
        </Button>
      </div>
      {hidden && (
        <p className="hint card-body">
          Values shown as <code>{masked}</code> are set but hidden from members; admins see them.
        </p>
      )}
      <pre ref={pre} className="yaml card-body" tabIndex={0} role="region" aria-label="YAML text">
        {text}
      </pre>
      {status && (
        <>
          <div className="card-head">
            <h3 id="yaml-status">Status</h3>
            <span className="hint">Not copied</span>
          </div>
          <pre className="yaml card-body" tabIndex={0} role="region" aria-labelledby="yaml-status">
            {toYAML(status)}
          </pre>
        </>
      )}
    </section>
  );
}
