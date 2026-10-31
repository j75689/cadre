import { Link } from "react-router";
import { listAgents } from "./api";
import { ConfigList } from "./ConfigList";
import { agentLink, boardLink } from "./links";

// Agents is the team's agents: whether each can take work, and why not,
// each with a link to its tasks.
export function Agents() {
  return (
    <ConfigList
      title="Agents"
      noun="agent"
      link={agentLink}
      load={listAgents}
      empty="This team has no agents. Agents are Agent resources in the namespace, applied with kubectl or GitOps."
      extra={{
        header: "Tasks",
        cell: (ns, name) => (
          <Link to={boardLink(ns, { assignee: name })} aria-label={`Tasks of ${name}`}>
            Tasks
          </Link>
        ),
      }}
    />
  );
}
