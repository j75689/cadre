import { getToolServer, listToolServers } from "./api";
import { ConfigDetail } from "./ConfigDetail";
import { ConfigList } from "./ConfigList";
import { toolServerLink } from "./links";

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
// in the YAML. The Agents that use it (spec.toolRefs) are not shown, as on
// a ModelProvider's page. TODO(used-by): a Used by card here.
export function ToolServerDetail() {
  return <ConfigDetail kind="ToolServer" noun="tool server" title="Tool servers" section="toolservers" load={getToolServer} />;
}
