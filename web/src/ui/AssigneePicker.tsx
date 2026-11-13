import { useEffect, useState } from "react";
import { listAgents, type AgentSummary } from "../api";
import { Banner } from "./Banner";
import { Segmented } from "./Segmented";

// AssigneeKind is whom a task goes to: nobody, an Agent or a user.
export type AssigneeKind = "" | "Agent" | "User";

// AssigneePicker is whom a task goes to: nobody, one of the team's Agents,
// read once (picking one is not live data), or a user by name. Changing
// the kind clears the name. group names its radio group.
export function AssigneePicker({
  ns,
  kind,
  assignee,
  onChange,
  group = "kind",
}: {
  ns: string;
  kind: AssigneeKind;
  assignee: string;
  onChange: (patch: { kind?: AssigneeKind; assignee: string }) => void;
  group?: string;
}) {
  const [agents, setAgents] = useState<AgentSummary[] | null>(null);
  const [agentsError, setAgentsError] = useState("");
  // retry reads the agents again after a failed read.
  const [retry, setRetry] = useState(0);
  useEffect(() => {
    const controller = new AbortController();
    listAgents(ns, controller.signal).then(
      (r) => {
        setAgents(r.items);
        setAgentsError("");
      },
      (e) => {
        if (!controller.signal.aborted) setAgentsError((e as Error).message);
      },
    );
    return () => controller.abort();
  }, [ns, retry]);
  const chosen = agents?.find((a) => a.name === assignee);
  return (
    <Segmented
      legend="Assignee"
      name={group}
      value={kind}
      options={[
        { value: "", label: "Unassigned" },
        { value: "Agent", label: "Agent" },
        { value: "User", label: "User" },
      ]}
      onChange={(next) => onChange({ kind: next, assignee: "" })}
    >
      {kind === "Agent" && (
        <label className="field">
          <span className="sr-only">Agent to assign</span>
          <select value={assignee} onChange={(e) => onChange({ assignee: e.target.value })}>
            <option value="">Choose an agent…</option>
            {agents?.map((a) => (
              <option key={a.name} value={a.name}>
                {a.name}
                {a.ready ? "" : ` (not ready: ${a.reason ?? "unknown"})`}
              </option>
            ))}
          </select>
        </label>
      )}
      {kind === "Agent" && chosen && !chosen.ready && (
        <p className="help">
          {chosen.name} is not ready{chosen.message ? `: ${chosen.message}` : ""}. Its tasks wait until it is.
        </p>
      )}
      {kind === "Agent" && agentsError && <Banner onRetry={() => setRetry((n) => n + 1)}>Could not load the agents: {agentsError}</Banner>}
      {kind === "Agent" && !agentsError && agents?.length === 0 && <p className="help">This team has no agents.</p>}
      {kind === "User" && (
        <label className="field">
          <span className="sr-only">User to assign</span>
          <input value={assignee} onChange={(e) => onChange({ assignee: e.target.value })} placeholder="Their user name" />
        </label>
      )}
    </Segmented>
  );
}
