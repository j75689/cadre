import { useParams } from "react-router";
import { getToolServer, listAgents, listToolServers } from "./api";
import { ConfigDetail } from "./ConfigDetail";
import { ConfigList } from "./ConfigList";
import { agentLink, toolServerLink } from "./links";
import { UsedByCard } from "./ui/UsedBy";

// ToolServers is the team's tool servers: whether each is ready, and why
// not.
export function ToolServers() {
  return (
    <ConfigList
      kind="ToolServer"
      title="Tool servers"
      noun="tool server"
      link={toolServerLink}
      load={listToolServers}
      empty="This team has no tool servers. Tool servers are ToolServer resources in the namespace, applied with kubectl or GitOps."
    />
  );
}

// ToolServerDetail is one ToolServer: its health, conditions, metadata and
// YAML, as the caller's role is shown it; its spec, and its status.url, are
// in the YAML. A member and an admin also see the Agents that use it: those
// whose list item names it in toolRefs.
export function ToolServerDetail() {
  const { name = "" } = useParams();
  return (
    <ConfigDetail
      kind="ToolServer"
      noun="tool server"
      title="Tool servers"
      section="toolservers"
      load={getToolServer}
      spec={() => (
        <UsedByCard
          label="Agents"
          what="the agents"
          kind="Agent"
          load={listAgents}
          pick={(items) => items.filter((a) => a.toolRefs?.includes(name)).map((a) => a.name)}
          link={agentLink}
        />
      )}
    />
  );
}
