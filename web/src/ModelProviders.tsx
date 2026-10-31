import { getModelProvider, listModelProviders } from "./api";
import { ConfigDetail } from "./ConfigDetail";
import { ConfigList } from "./ConfigList";
import { modelProviderLink } from "./links";

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
// YAML. The Agents that use it (spec.modelRef) are not shown: the Agents
// list is summary-only and a viewer gets no Agent spec, so it needs an
// API that answers them. TODO(used-by): a Used by card here.
export function ModelProviderDetail() {
  return <ConfigDetail kind="ModelProvider" noun="model provider" title="Model providers" section="modelproviders" load={getModelProvider} />;
}
