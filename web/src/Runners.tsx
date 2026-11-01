import { getRunner, listRunners } from "./api";
import { ConfigDetail } from "./ConfigDetail";
import { ConfigList } from "./ConfigList";
import { runnerLink } from "./links";

// Runners is the team's runners: whether each is ready, and why not.
export function Runners() {
  return (
    <ConfigList
      kind="Runner"
      title="Runners"
      noun="runner"
      link={runnerLink}
      load={listRunners}
      empty="This team has no runners. Runners are Runner resources in the namespace, applied with kubectl or GitOps."
    />
  );
}

// RunnerDetail is one Runner: its health, conditions, metadata and YAML,
// as the caller's role is shown it; its spec (ServiceAccount, labels) and
// its minted token's Secret and time (status.tokenSecret, status.mintedAt)
// are in the YAML. The server serves no role the token's hash.
export function RunnerDetail() {
  return <ConfigDetail kind="Runner" noun="runner" title="Runners" section="runners" load={getRunner} />;
}
