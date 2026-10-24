# CRDs

Principle: **configuration lives in CRDs, data lives in the storage driver.** CRDs hold what changes rarely and belongs under GitOps. Thread messages, conversation history, run history and files are never CRDs (see [storage.md](storage.md)).

Two exceptions are deliberate: `Task` and `Team` are CRDs even though tasks change often, because `kubectl get tasks -n team-a` is worth it and because a task's control state is small. Their bulky data still lives in object storage.

All YAML is illustrative. The API group is provisionally `cadre.dev/v1alpha1`.

## Inventory

| CRD | Scope | Purpose |
| --- | --- | --- |
| `Team` | namespace | opt-in marker for a team's namespace, task numbering, concurrency limit, role mapping |
| `Task` | namespace | a work item on the board |
| `Agent` | namespace | prompt, model, tools, permissions, who runs it |
| `ModelProvider` | namespace | a model, its endpoint and credentials |
| `ToolServer` | namespace | one MCP server, managed by Cadre or external |
| `Schedule` | namespace | create a task on a cron schedule |
| `Notifier` | namespace | whom to notify when a human is needed |
| `Runner` | namespace | a runner allowed to claim in its namespace: in the cluster by ServiceAccount, or outside it with a token Cadre mints |

Deliverables, approval policies and publishers are plugins with their own CRDs, outside the core.

Integrations are never built into the core as connectors. Every external integration is a `ToolServer`.

## Team

One per namespace, always named `team`.

```yaml
apiVersion: cadre.dev/v1alpha1
kind: Team
metadata: { name: team, namespace: team-a }
spec:
  displayName: Backend
  maxConcurrentRuns: 5
  archiveAfter: 720h
status:
  nextNumber: 124
  slots:                      # the runs holding a slot of maxConcurrentRuns
    - { runId: 01JB..., task: fix-login-flake, acquiredAt: ... }
  running: 1                  # len(slots), for kubectl's printer column
```

Three roles:

1. **Opt-in.** Controllers only handle Tasks in namespaces that have a `Team`. The UI's team list is the `Team` list intersected with the namespaces the user may access. A Task applied in a namespace without a Team never runs.
2. **Counter.** `status.nextNumber` is claimed with a conditional write and copied to `Task.status.number`, so humans can say "#123".
3. **Semaphore.** `status.slots` is the set of runs holding a slot, keyed by run id. A claim mints its run id, then adds it to the set with a conditional write on Team (only while the set has fewer than `maxConcurrentRuns` entries), then makes the conditional claim on the Task; if the Task claim loses, the run id is removed again. Taking the slot first and deciding afterwards would overshoot. Adding a run id that is already there, and removing one that is not, are no-ops, so every release can be retried safely and none can be counted twice.

Role mapping for SSO (the equivalent of Argo CD's `AppProject` roles) hangs here.

| Field | Rule |
| --- | --- |
| `metadata.name` | must be `team` |
| `spec.maxConcurrentRuns` | at least 1, default 1 |
| `spec.archiveAfter` | a positive duration, such as `720h` |
| `status.nextNumber`, `status.slots`, `status.running` | written by the Task Manager only; `slots` is a list keyed by `runId`; `running` is `len(slots)`, written with it |

## Task

Iron rule: humans and GitOps write `spec`; only the Task Manager writes `status`, through the status subresource. `kubectl edit` and the state machine never overwrite each other, and spec changes never touch the run's fencing fields.

```yaml
apiVersion: cadre.dev/v1alpha1
kind: Task
metadata: { name: fix-login-flake, namespace: team-a }
spec:
  title: Fix the flaky login test
  description: |
    ...
  assignee: { kind: Agent, name: coder }     # or kind: User
  priority: high
  parentRef: { name: q4-stability }
  labels: [backend]
status:
  number: 123
  phase: Running              # Todo | Running | Waiting | Review | Done | Cancelled | Failed
  waiting: { reason: Question, since: ..., threadSeq: 41 }   # only while Waiting
  run:                        # only while Running
    id: 01JB...               # ULID; the run the per-run MCP token maps to
    mode: runtime             # runtime | step
    runner: system:serviceaccount:cadre-system:cadre-runner   # who holds the run: the runner's authenticated identity
    runnerName: cadre-runner-7f3a   # the name it gave itself, for display
    step: 7                   # advances in step mode only
    leaseUntil: ...
  outbox:                     # written with the state change; applied idempotently, then cleared
    - { id: 01JB..., kind: CreateSubtask, spec: {...} }
    - { id: 01JB..., kind: Notify, ... }
  thread: { revision: 42, store: s3://cadre/team-a/fix-login-flake/thread/ }
  runs: { count: 3, lastId: 01JB..., store: s3://.../runs/ }
  conditions: [...]
```

- **Status stays small.** Only control state and pointers. Thread contents, conversation history and run history live in object storage; etcd caps one object at 1.5 MB.
- **Exactly seven phases.** `waiting.reason` (`Question`, `ToolApproval`, `Review`) carries the detail. Fewer fields make the move past `v1alpha1` cheaper.
- **Task is the single aggregate.** The run's control state lives inside the Task, so "the run finished, change the task's phase" is one write on one object.
- `thread.revision` is the thread's sequencer: every append advances it with a conditional write, so watching the Task is enough to learn the thread changed.

| Field | Values |
| --- | --- |
| `spec.title` | required |
| `spec.assignee.kind` | `Agent` or `User`; an unassigned task is never claimed |
| `spec.priority` | `low`, `medium`, `high`, `urgent`; default `medium` |
| `spec.parentRef` | another Task in the same namespace, never the task itself |
| `status.phase` | the seven phases above |
| `status.waiting.reason` | `Question`, `ToolApproval`, `Review` |
| `status.waiting.threadSeq` | a thread sequence; sequences start at 1 |
| `status.run.id`, `status.outbox[].id` | ULIDs |
| `status.run.mode` | `runtime` or `step` |
| `status.run.runner` | who holds the run: the runner's authenticated identity (`system:serviceaccount:<ns>:<name>` for an in-cluster runner); only requests from it may renew or finish the run. It is empty for a run claimed with `--runner-auth=none`, and such a run is not fenced on its sender |
| `status.run.runnerName` | the name the runner gave itself, for display |
| `status.outbox[].kind` | `CreateSubtask`, whose `spec` is the subtask's `TaskSpec`; `RunEnded`, whose `run` is the ended run (id, mode, runner, step, reason `Finished`, `Interrupted` or `Cancelled`, outcome, end time); `Notify` is added once its payload is defined |

A `CreateSubtask` item names the subtask after its own id, lowercased (a valid DNS-1123 label). Re-applying the item is then a create of an object that already exists, which is what makes the dispatcher idempotent.

A transition that ends a run (finish, lease-expiry reclaim, cancel) writes a `RunEnded` item in the same conditional write, with the run's id as the item id. The dispatcher appends it to the run history keyed on that id and then acks it, so the history always agrees with the Task: a write that loses the race leaves no record. Cancelling a task drops its pending `CreateSubtask` items but keeps `RunEnded` items.

## Agent, ModelProvider, ToolServer

```yaml
apiVersion: cadre.dev/v1alpha1
kind: ModelProvider
metadata: { name: claude, namespace: team-a }
spec:
  type: anthropic               # anthropic | openai | bedrock | vertex
  model: claude-opus-5-5
  auth: { secretRef: { name: anthropic, key: api-key } }
  # baseURL: https://llm-gateway.internal/v1     # openai-compatible endpoints and gateways
  # workloadIdentity: true                        # bedrock and vertex: no key, bind the ServiceAccount
---
apiVersion: cadre.dev/v1alpha1
kind: ToolServer
metadata: { name: github, namespace: team-a }
spec:
  transport: streamable_http
  url: https://api.githubcopilot.com/mcp/
  auth: { secretRef: { name: github-token, key: token } }
  permissions: { create_pull_request: ask, "*": allow }
---
apiVersion: cadre.dev/v1alpha1
kind: ToolServer
metadata: { name: searxng, namespace: team-a }
spec:
  managed:                      # the controller creates a Deployment and a Service
    image: ghcr.io/searxng/searxng:2026.10.1
    port: 8080
---
apiVersion: cadre.dev/v1alpha1
kind: Agent
metadata: { name: writer, namespace: team-a }
spec:
  description: Writes one AI trends thread a day
  modelRef: claude
  toolRefs: [searxng, news-trends]
  instructions: |
    ...
  runtime:
    type: builtin               # builtin | runner
    # selector: { pool: claude-code }   # runner: which runner pool may claim this agent's tasks
```

- `runtime.type: builtin` is Cadre's own LLM loop. `runner` hands the task to any runner whose labels match the selector (see [execution.md](execution.md)).
- `permissions` on a ToolServer decide per tool whether a call is allowed, denied or must ask a human first (`ask` ends the run with `Waiting{ToolApproval}`). Without `permissions` every tool is allowed. With them the list is an allowlist: `"*"` covers tools not listed by name, and a tool matching neither is denied.
- The managed mode of ToolServer creates a Deployment and a Service only. NetworkPolicy and ServiceAccount management may follow.

| Resource | Field | Rule |
| --- | --- | --- |
| ModelProvider | `type` | `anthropic`, `openai` (any OpenAI-compatible endpoint), `bedrock` or `vertex` |
| ModelProvider | `model` | required |
| ModelProvider | `auth.secretRef` | required for `anthropic` and `openai`; `bedrock` and `vertex` need it or `workloadIdentity: true` |
| ModelProvider | `baseURL` | an `http(s)` URL, for `anthropic` and `openai` only |
| ModelProvider | `workloadIdentity` | for `bedrock` and `vertex` only |
| ModelProvider | `region` | required for `bedrock`, forbidden otherwise |
| ModelProvider | `project`, `location` | required for `vertex`, forbidden otherwise |
| ToolServer | `url`, `managed` | exactly one of them |
| ToolServer | `transport` | `streamable_http`, the default |
| ToolServer | `auth.header`, `auth.prefix` | the request header carrying the Secret's value and what goes before it; default `Authorization` and `Bearer `, an empty prefix for a bare key such as `X-API-Key` |
| ToolServer | `permissions` | tool name or `"*"` → `allow`, `deny` or `ask` |
| Agent | `modelRef` | required; a ModelProvider in the same namespace |
| Agent | `toolRefs` | ToolServers in the same namespace, a set |
| Agent | `runtime.type` | `builtin` (the default) or `runner` |
| Agent | `runtime.selector` | required, with at least one label, for `runner`; forbidden for `builtin` |
| Runner | `serviceAccount` | `{namespace, name}` of the ServiceAccount an in-cluster runner authenticates as. Without it the runner is outside the cluster and Cadre mints its token: exactly one of the two |
| Runner | `labels` | the labels the runner's claims are matched with, set here and never taken from the runner's request; the `cadre.dev/` prefix is reserved |
| Agent | `effort` | `low`, `medium`, `high` (the default), `xhigh` or `max`: how much thinking the builtin loop asks the model for. It is on the Agent, not the ModelProvider, because one model serves agents that need different depth |

All three report a `Ready` condition and `observedGeneration` in their status:

| Resource | Ready when |
| --- | --- |
| ModelProvider | the referenced Secret has the key, or `workloadIdentity` is set |
| ToolServer | an external server's Secret has its key, when `auth` is set; a managed ToolServer is not Ready (`ManagedNotSupported`) until managed servers arrive in M2 |
| Agent | its ModelProvider and every ToolServer exist and are Ready; the reason names the first that is not |
| Runner | always, once reconciled: `ServiceAccount` for an in-cluster runner, `TokenMinted` once an outside runner's token exists |

Controllers only act in namespaces with a `Team`. Secrets are read one by one when a resource is reconciled, never watched or cached, and the resources that reference them are re-checked every minute.

## Runner

```yaml
apiVersion: cadre.dev/v1alpha1
kind: Runner
metadata: { name: laptop, namespace: team-a }
spec:
  labels: { pool: claude-code }   # claims match agents whose runtime.selector this includes
  # serviceAccount: { namespace: ci, name: runner }   # in the cluster: authenticate as this ServiceAccount
status:
  tokenSecret: laptop-token       # outside the cluster: the minted token, key token
  tokenHash: 3f9a...              # its SHA-256; a token with another hash is refused
  mintedAt: ...
```

- A Runner lives in the Team namespace it serves, and lets its runner claim there only: the namespace is the tenant boundary. A runner serving several Teams has a Runner in each, naming the same ServiceAccount.
- The claim's labels come from the Runner; what the runner sends is ignored, so it cannot claim agents meant for another pool.
- A Runner without `serviceAccount` gets a token minted into the Secret `<name>-token`, labelled `cadre.dev/runner: <name>` and owned by the Runner. The Secret reaches the runner's machine out of band (`kubectl get secret`, a secret manager); Cadre never serves it. Deleting the Secret rotates the token: a new one is minted on the next reconcile, within a minute, and the old one stops working then.
- `cadre-runner` needs no Runner: its identity (`--builtin-runner`) may claim builtin agents in every Team namespace.

## Schedule and Notifier

```yaml
apiVersion: cadre.dev/v1alpha1
kind: Schedule
metadata: { name: daily-thread, namespace: team-a }
spec:
  cron: "0 9 * * *"
  timezone: Asia/Taipei
  jitter: 60s                   # avoid everyone firing on the hour
  task: { assignee: writer, title: "Today's AI trends thread" }
```

`Notifier` names a channel (an MCP ToolServer or a webhook) and when to use it: a question for a human, a tool approval, a review.

## Credentials

- **Ordinary credentials** are Kubernetes Secrets referenced with `secretRef`, optionally synced from Vault or a cloud secret manager by External Secrets Operator.
- **Cloud services** use workload identity: an Agent or ToolServer maps to a ServiceAccount bound to a GCP or AWS IAM identity, no key files.
- **Credentials never leave Cadre.** Tools execute on the Cadre side; LLM calls are made by Cadre's worker or a gateway. Runtime-mode runners never receive a provider key.
- Rotation is the Secret's business.

## Multi-team

- One team per namespace. Kubernetes RBAC decides who may change `Agent`, `ToolServer` and `Task` resources through GitOps or kubectl.
- Board actions (approve, assign, comment) are governed by Cadre's own SSO and the per-team role mapping on `Team`; see [architecture.md](architecture.md#identity-and-access).
- Boards are separated by namespace.

## Versioning

- Start at `v1alpha1`; moving to a new version needs a conversion webhook.
- CRDs are public API. The first version keeps fields to the minimum; it is far easier to add a field than to remove one.
