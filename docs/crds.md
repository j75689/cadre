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
    attempt: 1                # step mode: workers that took the current step; fences the worker's events
    stepFree: false           # step mode: the step waits for a worker
    stepItems: [{ id: 01JB..., after: 41 }]   # step mode: outbox items the current step recorded
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
| `status.run.attempt` | step mode: how many workers took the current step, reset when it is committed. A worker's heartbeats, records, commits, releases and finish carry the attempt it was given and are refused (`attempt_mismatch`) once the step was freed or taken again. The run fails when the third one dies or gives up |
| `status.run.stepFree` | step mode: the step waits for a worker; `runner` and `runnerName` are empty, `lastRunner` and `lastRunnerName` keep the last worker, and `leaseUntil` is the idle deadline (1 hour by default), past which the run fails |
| `status.run.stepItems` | step mode: the outbox items the current step recorded, and where their entries are looked for; a step done again records each once. Cleared when the step is committed |
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
    # path: /mcp                # the MCP endpoint's path on the server; default /mcp
status:
  url: http://searxng.team-a.svc:8080/mcp   # set by the controller: the Service's address and the path
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
    # execution: runtime        # builtin: step (the default) runs a step at a time, runtime one worker per run (see execution.md)
    # selector: { pool: claude-code }   # runner: which runner pool may claim this agent's tasks
```

- `runtime.type: builtin` is Cadre's own LLM loop. `runner` hands the task to any runner whose labels match the selector (see [execution.md](execution.md)).
- `permissions` on a ToolServer decide per tool whether a call is allowed, denied or must ask a human first (`ask` ends the run with `Waiting{ToolApproval}`). Without `permissions` every tool is allowed. With them the list is an allowlist: `"*"` covers tools not listed by name, and a tool matching neither is denied.
- The managed mode of ToolServer creates a Deployment and a Service only, both named after the ToolServer, labelled `cadre.dev/toolserver: <name>` and owned by it, so deleting the ToolServer deletes them. The Deployment runs one replica of `image` listening on `port`; the ClusterIP Service maps `port` to it; `status.url` is `http://<name>.<namespace>.svc:<port><path>`, where Cadre reaches the server. A managed server takes no `auth`: it runs in the cluster and is reached unauthenticated. NetworkPolicy and ServiceAccount management may follow.

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
| ToolServer | `managed.path` | the MCP endpoint's path, starting with `/`; default `/mcp` |
| ToolServer | `auth` | not with `managed`: a managed server is reached unauthenticated in the cluster |
| ToolServer | `transport` | `streamable_http`, the default |
| ToolServer | `auth.header`, `auth.prefix` | the request header carrying the Secret's value and what goes before it; default `Authorization` and `Bearer `, an empty prefix for a bare key such as `X-API-Key` |
| ToolServer | `permissions` | tool name or `"*"` → `allow`, `deny` or `ask` |
| Agent | `modelRef` | required; a ModelProvider in the same namespace |
| Agent | `toolRefs` | ToolServers in the same namespace, a set |
| Agent | `runtime.type` | `builtin` (the default) or `runner` |
| Agent | `runtime.selector` | required, with at least one label, for `runner`; forbidden for `builtin` |
| Agent | `runtime.job` | for `runner` only: `runnerRef` (a Runner in the namespace with a `serviceAccount`, whose labels include the selector, or the Agent is not Ready), `template` (a PodTemplateSpec, checked by the API server when a Job is created), `activeDeadline` (default 1h), `maxPending` (default 2) |
| Agent | `runtime.execution` | `step` or `runtime`; `step` for `builtin` only. Left out, it is `step` for `builtin` and `runtime` for `runner`; there is no schema default, so an Agent stored with `runtime` before step became the default stays runtime |
| Runner | `serviceAccount` | `{name}` of the ServiceAccount, in the Runner's own namespace, that an in-cluster runner authenticates as. Without it the runner is outside the cluster and Cadre mints its token: exactly one of the two |
| Runner | `labels` | required, at least one: the labels the runner's claims are matched with, set here and never taken from the runner's request; the `cadre.dev/` prefix is reserved |
| Agent | `effort` | `low`, `medium`, `high` (the default), `xhigh` or `max`: how much thinking the builtin loop asks the model for. It is on the Agent, not the ModelProvider, because one model serves agents that need different depth |

All three report a `Ready` condition and `observedGeneration` in their status:

| Resource | Ready when |
| --- | --- |
| ModelProvider | the referenced Secret has the key, or `workloadIdentity` is set |
| ToolServer | an external server's Secret has its key, when `auth` is set. A managed one's Deployment is Available for its current spec (`Available`); until then `Deploying`, and `Forbidden` where cadre-server may not create its Deployment or Service (bind `config/rbac/workloads.yaml` in the namespace), `NameTaken` where a Deployment of its name is not Cadre's |
| Agent | its ModelProvider and every ToolServer exist and are Ready; the reason names the first that is not |
| Runner | `ServiceAccount` for an in-cluster runner; `TokenMinted` once an outside runner's token exists. A Secret of the token's name that the Runner does not own, or one without a token, makes it not Ready (`SecretNotOwned`) and no token works until it is deleted |

Controllers only act in namespaces with a `Team`. Secrets are read one by one when a resource is reconciled, never watched or cached, and the resources that reference them are re-checked every minute.

## Runner

```yaml
apiVersion: cadre.dev/v1alpha1
kind: Runner
metadata: { name: laptop, namespace: team-a }
spec:
  labels: { pool: claude-code }   # claims match agents whose runtime.selector this includes
  # serviceAccount: { name: runner }   # in the cluster: authenticate as this ServiceAccount, in this namespace
status:
  tokenSecret: laptop-token       # outside the cluster: the minted token, key token
  tokenHash: 3f9a...              # its SHA-256; a token with another hash is refused
  mintedAt: ...
```

- A Runner lives in the Team namespace it serves, and lets its runner claim there only: the namespace is the tenant boundary. Its ServiceAccount is in that namespace too, so a Team cannot enlist another Team's runner; an in-cluster runner serving several Teams runs as a ServiceAccount, and a pod, in each. Letting a ServiceAccount consent to Runners in other namespaces is a later relaxation.
- The claim's labels come from the Runner; what the runner sends is ignored, so it cannot claim agents meant for another pool.
- A Runner without `serviceAccount` gets a token minted into the Secret `<name>-token`, labelled `cadre.dev/runner: <name>` and owned by the Runner. The Secret reaches the runner's machine out of band (`kubectl get secret`, a secret manager); Cadre never serves it. Deleting the Secret rotates the token: a new one is minted on the next reconcile, within a minute, and the old one stops working then. Switching a Runner to `serviceAccount` deletes its minted Secret, so switching back mints a fresh token.
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
  task:                         # a TaskSpec, without parentRef
    title: "Today's AI trends thread"
    assignee: { kind: Agent, name: writer }
    labels: [thread]
status:
  lastScheduleTime: "2026-10-24T01:00:00Z"
  lastTask: daily-thread-202610240100
  nextScheduleTime: "2026-10-25T01:00:41Z"
  activeSince: "2026-10-01T03:12:09Z"
  conditions: [{ type: Ready, status: "True", reason: Scheduled }]
```

| Field | |
|---|---|
| `cron` | required; a standard five-field cron expression, or a descriptor such as `@daily`; no `TZ=` or `CRON_TZ=` prefix, and ticks at least a minute apart (`@every 1m` or longer) |
| `timezone` | the IANA zone `cron` is read in; default UTC; `Local` is refused. As with a Kubernetes CronJob, on a day the clocks go back a time inside the repeated hour ticks twice (two tasks), and on a day they go forward a time inside the skipped hour does not tick |
| `jitter` | delays each tick's task by up to this long, by an amount fixed by the Schedule's UID and the tick; keep it shorter than the interval between ticks |
| `suspend` | no task is created while it is true |
| `task` | the spec of every task created; `parentRef` is refused, since a scheduled task has no parent |

The name is at most 50 characters, so that its tasks' names fit 63.

Ready is False with reason `InvalidSchedule` when the cron or the zone is refused, or the task is invalid or refused by the store (the message says which), and `NoTeam` when the namespace has no Team. Otherwise it is True with reason `Scheduled`, or `Suspended`.

**Naming and idempotency.** A tick is a time `cron` fires in `timezone`. Its task is named `<schedule>-<tick in UTC as YYYYMMDDhhmm>`, labelled `cadre.dev/schedule: <schedule>` and annotated `cadre.dev/scheduled-at` with the tick in RFC 3339. It is created through the storage driver, so it works under any driver, and a second create of the same name fails as already existing: that is what makes a tick fire once across restarts and overlapping leaders. The reconciler keeps no other memory. A task has no owner reference: tasks are history, and deleting a Schedule leaves them.

**Missed ticks.** A tick fires once its fire time (the tick plus its jitter) has come, and ticks fire in order: one whose fire time has not come holds back the later ones, and one whose jitter outlasts the next tick fires before it. When several have come since the last one fired (cadre-server was down, or no leader was elected), only the latest fires; the others are skipped, and an Event says how many. A daily Schedule three days behind creates one task, not three. Ticks before the Schedule's creation never fire.

**Suspend.** A tick that passes while the Schedule is suspended never fires, even once resumed; the first to fire is the first after the resume is seen. The same holds while it cannot run (`InvalidSchedule`, `NoTeam`). `status.activeSince` records when it last became able to fire, and is cleared while it cannot.

There is no concurrency policy: a tick's task is created whether or not the previous one is finished.

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
