import { useParams } from "react-router";
import { getModelProvider, listAgents, listModelProviders } from "./api";
import { ConfigDetail } from "./ConfigDetail";
import { ConfigList } from "./ConfigList";
import { agentLink, modelProviderLink } from "./links";
import { UsedByCard } from "./ui/UsedBy";

// ModelProviders is the team's model providers: whether each is ready, and
// why not.
export function ModelProviders() {
  return (
    <ConfigList
      kind="ModelProvider"
      title="Model providers"
      noun="model provider"
      link={modelProviderLink}
      load={listModelProviders}
      empty="This team has no model providers. Model providers are ModelProvider resources in the namespace, applied with kubectl or GitOps."
    />
  );
}

// ModelProviderDetail is one ModelProvider: its health, conditions,
// metadata and YAML, as the caller's role is shown it; its spec is in the
// YAML. A member and an admin also see the Agents that use it: those whose
// list item names it as modelRef.
export function ModelProviderDetail() {
  const { name = "" } = useParams();
  return (
    <ConfigDetail
      kind="ModelProvider"
      noun="model provider"
      title="Model providers"
      section="modelproviders"
      load={getModelProvider}
      spec={() => (
        <UsedByCard
          label="Agents"
          what="the agents"
          kind="Agent"
          load={listAgents}
          pick={(items) => items.filter((a) => a.modelRef === name).map((a) => a.name)}
          link={agentLink}
        />
      )}
    />
  );
}
