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
| `AgentAccess` | namespace | an agent outside Cadre allowed on its namespace's board, as a viewer or a member: in the cluster by ServiceAccount, or outside it with a token Cadre mints |

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
  roles:
    - role: admin
      subjects: [{ kind: user, name: lead@example.test }]
    - role: member
      subjects: [{ kind: group, name: backend }]
    - role: viewer
      subjects: [{ kind: group, name: everyone }]
status:
  nextNumber: 124
  slots:                      # the runs holding a slot of maxConcurrentRuns
    - { runId: 01JB..., task: fix-login-flake, acquiredAt: ... }
  running: 1                  # len(slots), for kubectl's printer column
```

Three roles:

1. **Opt-in.** Controllers only handle Tasks in namespaces that have a `Team`. The UI's team list is the `Team` list intersected with the namespaces the user may access. A Task applied in a namespace without a Team never runs.
2. **Counter.** `status.nextNumber` is claimed with a conditional write and copied to `Task.status.number`, so humans can say "#123". A counter that has not started, a new Team's or one made again in a namespace whose tasks outlived the old Team, starts above the highest number there, read once from the board index. A task numbered in the seconds before the Team was deleted, or whose numbering was in flight across the deletion, can be missed; its number is then shared, and `#N` is refused as ambiguous until one is renamed.
3. **Semaphore.** `status.slots` is the set of runs holding a slot, keyed by run id. A claim mints its run id, then adds it to the set with a conditional write on Team (only while the set has fewer than `maxConcurrentRuns` entries), then makes the conditional claim on the Task; if the Task claim loses, the run id is removed again. Taking the slot first and deciding afterwards would overshoot. Adding a run id that is already there, and removing one that is not, are no-ops, so every release can be retried safely and none can be counted twice.

**Roles.** `spec.roles` gives people a role on the team's board (the equivalent of Argo CD's `AppProject` roles). Each entry is a `role` (`admin`, `member` or `viewer`) and its `subjects`, at least one. A subject is a `kind` (`group` or `user`) and a non-empty `name`. A `user` subject matches the login's username, without case when the username is an email address (the default); a `group` subject matches any of its groups, exactly. Someone matching several entries has the strongest of their roles. The schema checks the role, the kind and the name. Roles take effect with logins (`--auth=oidc`); see [architecture.md](architecture.md#authorization).

| Role | May |
| --- | --- |
| `viewer` | read: list and get tasks, their threads, runs and events, the team's agents, and the Team |
| `member` | what a viewer may, and create tasks, comment, reply, approve or deny tool calls, cancel tasks |
| `admin` | what a member may, and editing the Team (its display name, run cap, archiving and roles) through the API, `cadre team set`, as well as with kubectl or GitOps; the only one who sees the Team's roles |

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
  thread: { revision: 42 }
  runs: { count: 3, lastId: 01JB... }
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
| `spec.parentRef` | another Task in the same namespace, never the task itself. A task's subtasks are listed with `GET /tasks?parent=<name>` (one level: their own subtasks are listed from their pages), which the task page shows and `cadre task list --parent` prints |
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
| `status.outbox[].kind` | `CreateSubtask`, whose `spec` is the subtask's `TaskSpec`; `RunEnded`, whose `run` is the ended run (id, mode, runner, step, reason `Finished`, `Interrupted` or `Cancelled`, outcome, end time); `AppendThread` and `WriteFile`, the second phase of a fenced write; `Notify`, whose `notify` is the event, the thread entry waited on, when it happened, and the Notifiers that took it so far (`delivered`) |

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
    # readinessProbe:           # a corev1.Probe; default a TCP connect to port
    #   httpGet: { path: /healthz, port: 8080 }
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
- `notifications` opts a ToolServer in as a Notifier channel. A Notifier is not scoped like an Agent's `toolRefs`: whoever may create Notifiers in the namespace may call any tool of an opted-in ToolServer that its `permissions` allow, with chosen static arguments, on every event they pick, and with the ToolServer's credential. Keep it off for a ToolServer whose allowed tools act (create issues, send mail) unless the people with RBAC on Notifiers may do that.
- The managed mode of ToolServer creates a Deployment and a Service only, both named after the ToolServer, labelled `cadre.dev/toolserver: <name>` and owned by it, so deleting the ToolServer deletes them. The Deployment runs one replica of `image` listening on `port`, ready (so the ToolServer `Available`) once a TCP connect to `port` succeeds, or `readinessProbe` passes when set; it has no liveness probe, so a slow server is not restarted. The ClusterIP Service maps `port` to it; `status.url` is `http://<name>.<namespace>.svc:<port><path>`, where Cadre reaches the server. A managed server takes no `auth`: it runs in the cluster and is reached unauthenticated. NetworkPolicy and ServiceAccount management may follow.

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
| ToolServer | `managed.readinessProbe` | exactly one of `exec`, `httpGet`, `tcpSocket` and `grpc`, and no `terminationGracePeriodSeconds`; a probe the API server still refuses for a Pod makes the ToolServer not Ready with `InvalidSpec` |
| ToolServer | `auth` | not with `managed`: a managed server is reached unauthenticated in the cluster |
| ToolServer | `metadata.name`, with `managed` | a DNS-1035 label, at most 63 characters, no dot, and starting with a letter: it names the Deployment and the Service, and a Service's name is such a label (newer API servers relax this; older ones do not). An external ToolServer's name may have dots. The rule refuses a new managed ToolServer, or an external one made managed, with such a name; one that was managed before the rule keeps working with its writes, and is not Ready (`InvalidName`): make it external (`url` instead of `managed`), which keeps the Agents that name it, or re-create it under another name |
| ToolServer | `transport` | `streamable_http`, the default |
| ToolServer | `auth.header`, `auth.prefix` | the request header carrying the Secret's value and what goes before it; default `Authorization` and `Bearer `, an empty prefix for a bare key such as `X-API-Key` |
| ToolServer | `permissions` | tool name or `"*"` → `allow`, `deny` or `ask` |
| ToolServer | `notifications` | `true` lets the namespace's Notifiers call the tools its `permissions` allow, with its credential; off by default |
| Agent | `modelRef` | required; a ModelProvider in the same namespace |
| Agent | `toolRefs` | ToolServers in the same namespace, a set |
| Agent | `runtime.type` | `builtin` (the default) or `runner` |
| Agent | `runtime.selector` | required, with at least one label, for `runner`; forbidden for `builtin` |
| Agent | `runtime.job` | for `runner` only: `runnerRef` (a Runner in the namespace with a `serviceAccount`, whose labels include the selector, or the Agent is not Ready), `template` (a PodTemplateSpec, checked by the API server when a Job is created), `activeDeadline` (default 1h), `maxPending` (default 2) |
| Agent | `runtime.execution` | `step` or `runtime`; `step` for `builtin` only. Left out, it is `step` for `builtin` and `runtime` for `runner`; there is no schema default, so an Agent stored with `runtime` before step became the default stays runtime |
| Runner | `serviceAccount` | `{name}` of the ServiceAccount, in the Runner's own namespace, that an in-cluster runner authenticates as. Without it the runner is outside the cluster and Cadre mints its token: exactly one of the two |
| Runner | `labels` | required, at least one: the labels the runner's claims are matched with, set here and never taken from the runner's request; the `cadre.dev/` prefix is reserved |
| Agent | `effort` | `low`, `medium`, `high` (the default), `xhigh` or `max`: how much thinking the builtin loop asks the model for. It is on the Agent, not the ModelProvider, because one model serves agents that need different depth |

Each reports a `Ready` condition and `observedGeneration` in its status:

| Resource | Ready when |
| --- | --- |
| ModelProvider | the referenced Secret has the key, or `workloadIdentity` is set |
| ToolServer | `InvalidName`, before anything else (`NoTeam` included), for a managed one whose name is not a DNS-1035 label, made before the CRD refused it: what runs for it is left as it is. An external server's Secret has its key, when `auth` is set. A managed one's Deployment is Available for its current spec (`Available`); until then `Deploying`, and `Forbidden` where cadre-server may not create its Deployment or Service (bind `config/rbac/workloads.yaml` in the namespace), `NameTaken` where a Deployment of its name is not Cadre's, `RolloutFailed` when the Deployment's rollout fails, and `InvalidSpec` when the API server refuses the Deployment or the Service made from the spec (a `readinessProbe` a Pod may not have, say), with its message. With `RolloutFailed` and `InvalidSpec` whatever was running keeps running while Ready is False |
| Agent | its ModelProvider and every ToolServer exist and are Ready, and a job-backed one's Runner holds its ServiceAccount (`ServiceAccountInUse` otherwise, naming the Runner that does); the reason names the first that is not |
| Runner | `ServiceAccount` for an in-cluster runner, while it is the oldest Runner in its namespace naming that ServiceAccount (then by name): a runner authenticated as it claims with one Runner's labels, so another naming it is not Ready (`ServiceAccountInUse`, naming the one that holds it) and takes its place when that one goes; `TokenMinted` once an outside runner's token exists. A Secret of the token's name that the Runner does not own, or one without a token, makes it not Ready (`SecretNotOwned`) and no token works until it is deleted. `InvalidName`, before anything else, for a name that is not a DNS label, made before the CRD refused it |
| AgentAccess | as a Runner: `ServiceAccount` while it is the oldest AgentAccess in its namespace naming that ServiceAccount (then by name), `ServiceAccountInUse` otherwise, naming the one that holds it; `TokenMinted` once a minted token exists; `SecretNotOwned` for a Secret of the token's name it does not own, or one without a token; `InvalidName` as a Runner. Ready is for humans: a request is decided from the spec and the token's hash, not from it |

Controllers only act in namespaces with a `Team`: elsewhere every resource here, and Schedules and Notifiers, are not Ready with reason `NoTeam`, and are checked again every minute. Secrets are read one by one when a resource is reconciled, never watched or cached, and the resources that reference them are re-checked every minute.

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
- A Runner without `serviceAccount` gets a token minted into the Secret `<name>-token`, labelled `cadre.dev/runner: <name>` and owned by the Runner. The Secret reaches the runner's machine out of band (`kubectl get secret`, a secret manager); Cadre never serves it. Deleting the Secret rotates the token: a new one is minted on the next reconcile, within a minute, and the old one stops working then. Switching a Runner to `serviceAccount` deletes its minted Secret, so switching back mints a fresh token. In a namespace whose Team is gone the Runner is not Ready (`NoTeam`) and its token stays valid, but claims nothing, since work is claimed only in Team namespaces; the same token works again once the Team is back.
- `cadre-runner` needs no Runner: its identity (`--builtin-runner`) may claim builtin agents in every Team namespace.
- A Runner's name names its Secret, `<name>-token`, and labels it, so it is a DNS label: at most 63 characters and no dot, which also keeps it from ever being an AgentAccess's (`<name>.access-token`). The CRD refuses another name at create. The rule came in before 1.0: a Runner made earlier with such a name is not Ready (`InvalidName`) and does nothing, its Secret left alone, until re-created under another name.

## AgentAccess

```yaml
apiVersion: cadre.dev/v1alpha1
kind: AgentAccess
metadata: { name: triage-bot, namespace: team-a }
spec:
  role: member                     # viewer or member; never admin
  # serviceAccount: { name: triage-bot }   # in the cluster: authenticate as this ServiceAccount, in this namespace
status:
  tokenSecret: triage-bot.access-token   # outside the cluster: the minted token, key token
  tokenHash: 3f9a...               # its SHA-256; a token with another hash is refused
  mintedAt: ...
```

An AgentAccess is an agent Cadre does not run, a bot or a scheduled job say, allowed on its namespace's board. It is created with kubectl or GitOps, where Kubernetes RBAC on the CRD decides who may, or by a Team admin through the web UI (below), the API or `cadre agentaccess set` (see [architecture.md](architecture.md#web-ui-and-api)), where the Team's roles do; the API never reads a minted token back, but a Team admin can replace it and be shown the new one, once (`cadre agentaccess token`). What a request authenticated as an AgentAccess may do is in [architecture.md, Identity and access](architecture.md#identity-and-access).

- **Identity and role.** The agent is `agent:<namespace>/<name>`, whichever way it authenticates. `spec.role` is its role on its own namespace's Team: `viewer` or `member`. The CRD refuses `admin`, so an agent can never edit the Team.
- **In the cluster** (`serviceAccount`), the agent authenticates as a ServiceAccount in the AgentAccess's namespace, with a projected token for the audience `cadre`, as an in-cluster runner does. A ServiceAccount is one AgentAccess's: the oldest naming it, then by name, decided from the specs. Another naming it is not Ready (`ServiceAccountInUse`, naming the one that holds it), and takes its place when that one goes.
- **Outside the cluster** (no `serviceAccount`), Cadre mints a token into the Secret `<name>.access-token`, labelled `cadre.dev/agentaccess: <name>` and owned by the AgentAccess. A Runner and an AgentAccess never share a Secret: a Runner's is `<name>-token`, an AgentAccess's `<name>.access-token`, and names have no dot. An AgentAccess's name names and labels its Secret, so it is a DNS label too: at most 63 characters and no dot, which the CRD checks at create (`InvalidName` for one that is not). The Secret lives in the Team namespace, and who may read it is that namespace's own Kubernetes RBAC. Cadre never reads it back; a Team admin can replace it, and is shown the new one once (`cadre agentaccess token`). Switching to `serviceAccount` deletes the minted Secret, so switching back mints a fresh token.
- **Revoke and rotate.** To revoke, delete the AgentAccess. To rotate, use **Rotate token** in the web UI or `cadre agentaccess token` (the old token stops at once), or delete the Secret: a new token is minted on the next reconcile, within a minute, and the old one stops then. Either way a credential stops working within a minute at most.
- **Without logins** (`--auth=none`) everyone is an admin and nobody authenticates: an AgentAccess is reconciled as usual but grants nothing.
- **In the web UI.** A Team admin manages AgentAccesses at **Settings**, **Agent access** (`/t/<namespace>/settings/access`), with logins only (`--auth=oidc`). A viewer or a member sees "Managing agent access needs the admin role.", and under `--auth=none` the page says it needs logins, as the server serves no AgentAccess routes there. The page lists each AgentAccess with its role, credential and readiness.
  - **Create:** **New agent access** asks for a Name (a DNS label: lower-case letters, digits and `-`, at most 63, starting and ending with a letter or digit), a Role (`viewer` or `member`, `member` by default) and a Credential: **Minted token** (the default; the reconciler mints the token into a Secret) or **ServiceAccount**, with the ServiceAccount's name. A refusal, such as a name already in use, shows the server's message and keeps what you typed. A created AgentAccess opens its page.
  - **Edit:** the page of one AgentAccess has **Edit**, for the role and the credential; the name cannot be changed. It carries a reminder: if AgentAccesses are applied from Git, change them there, since an edit here drifts from Git. The page re-reads every 15 seconds without touching what you are typing. If someone else changed the AgentAccess meanwhile, Save shows the server's message and keeps your draft; Save again to overwrite with your values, or Cancel. The reconciler writing the AgentAccess's status after a save is not a change by someone else, and does not show the message.
  - **Rotate token:** shown for an AgentAccess without a `serviceAccount`. It asks "Rotate the token? The old one stops working at once.", and then shows the new token once, in a box with a Copy button, with the name of its Secret. The token is kept in that page only: **Done**, leaving the page or coming back to it with Back hides it, and nothing can show it again; rotate again for another. An AgentAccess with a `serviceAccount` has no token to rotate.
  - **Reading a minted token without rotating:** the page names the Secret (`<name>.access-token`) and the key (`token`). Whoever may read Secrets in the namespace runs `kubectl -n <namespace> get secret <name>.access-token -o jsonpath='{.data.token}' | base64 -d`.
  - **Delete** asks "Delete *name*? Its agent loses access at once." and then returns to the list, which says "Deleted *name*.".

  The same operations are in the CLI (`cadre agentaccess list|set|token|delete`) and the API; the web UI's behaviour in detail is in [web-ui.md](web-ui.md).

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

### Notifier

A `Notifier` tells a channel when a task starts to wait on a human or ends. The channel is a webhook or a tool on a ToolServer, exactly one of them.

```yaml
apiVersion: cadre.dev/v1alpha1
kind: Notifier
metadata: { name: oncall, namespace: team-a }
spec:
  on: [Question, ToolApproval, Failed]
  webhook:
    url: https://hooks.example.test/cadre
    secretRef: { name: cadre-webhook, key: secret }
    headers: { X-Team: backend }
status:
  lastSent: "2026-10-25T09:00:03Z"
  conditions: [{ type: Ready, status: "True", reason: Ready }]
```

| Field | |
|---|---|
| `on` | required, at least one, each once: `Question` and `ToolApproval` (a task starting to wait, for that reason), `Review` (a run handing its work to a human: the `Review` phase, or a wait for a review), `Done`, `Failed` and `Cancelled` (a task ending) |
| `webhook.url` | required, http or https, without credentials (`user:password@`), which would travel as an `Authorization` header; every notification is POSTed there |
| `webhook.secretRef` | a Secret key holding a signing secret: every delivery then carries `X-Cadre-Signature: sha256=<hex HMAC-SHA256 of the body>` |
| `webhook.headers` | static headers sent with each delivery. An `Authorization` header is refused: headers are readable by anyone who can read the Notifier, so a credential does not belong there; the receiver checks the signature instead |
| `toolServer.name` | a ToolServer in the namespace that takes notifications (`spec.notifications: true`); instead of `webhook` |
| `toolServer.tool` | the MCP tool called. The ToolServer's permissions must allow it: `ask` or `deny` is refused, as a notification cannot wait for a human to approve the call |
| `toolServer.arguments` | static string arguments passed with every call, beside `notification` (a key of that name is refused) |

Ready is False with reason `InvalidURL` (no host, credentials in it, or not http or https), `SecretMissing` (the signing Secret or its key cannot be read), `ToolServerMissing`, `ToolServerNotReady` (not Ready for its current spec), `ToolServerNotForNotifications` (it does not set `notifications`), `ToolNotAllowed` (its permissions do not allow the tool), or `NoTeam`; otherwise it is True. A Notifier is looked at again when its ToolServer changes. The reconciler only decides Ready; it never delivers. `status.lastSent` is the last delivery, and `status.lastError` the last that failed, with its message (which names the webhook without user info or query) and time; a Notifier that is not Ready is not tried, and that is not recorded.

**What is sent.** A POST of `Content-Type: application/json`, with `Idempotency-Key: <id>`, `User-Agent: cadre-server/<version>`, the static headers and the signature, within 10 seconds, following no redirect. Only a 2xx counts as delivered. The body:

```json
{
  "id": "01JB...", "event": "Question", "at": "2026-10-25T09:00:00Z", "threadSeq": 7,
  "task": { "namespace": "team-a", "name": "fix-login", "number": 123, "title": "Fix login",
            "phase": "Waiting", "assignee": { "kind": "Agent", "name": "coder" } },
  "url": "https://cadre.example.test/t/team-a/tasks/fix-login"
}
```

`url` is the task's page at cadre-server's `--external-url`. `threadSeq` is the entry the task waits on, when it is known.

**Through a tool.** The tool is called through the same forwarder a run's tool calls take, with the ToolServer's own credentials, within the same 10 seconds. Its arguments are `{"notification": <the body above, as an object>, ...arguments}`. The forwarder checks again at the call, on the ToolServer read fresh, that it takes notifications and that its permissions say `allow` for the tool: a ToolServer changed to `ask` is refused at once, whatever the Notifier's Ready still says. A call that is not made, or answers with `isError`, is not a delivery. `lastError` names the ToolServer and the tool and, for a call without an answer, why (gone or not Ready, not allowed, not taking notifications, its credential unreadable, unreachable, or the call failed before the tool answered, as on a timeout), never the arguments or what the tool answered. A server that calls no ToolServers does not try a tool's notifications, and records nothing against the Notifier; dropped after 24 hours, such a notification's Event says it was never tried.

**What Notifier RBAC amounts to.** Creating a Notifier lets its author have cadre-server POST task summaries to any URL, and call the allowed tools of any ToolServer in the namespace that takes notifications. Grant it as an admin's lever.

**At least once.** A notification is an outbox item on the Task, written in the same conditional write as the transition it tells of, and only when a Notifier in the namespace listens for the event. The item names those Notifiers (`to`): it goes to them and no others, and one deleted since counts as having taken it. A Notifier counts as deleted only when a fresh read of the API server does not find it: one another server saw first, and this server's cache has not seen yet, is not, and is sent to once the cache has it; nor is one the read fails on. Such Notifiers are read once per pass, before a server takes its turn on the item. While they are all that is left of a notification, it costs no write, and if neither happens within the 24 hours below, it is dropped as one never tried, its Event naming those Notifiers; one with another Notifier still pending goes on as that one does, and its Event names both. A Notifier list that cannot be read costs the notification, never the transition. Notifications go out on their own loop, at the dispatcher's pace but apart from it and from the reclaim, a few tasks and a few deliveries at a time, so a webhook that does not answer delays nothing else. One server sends a notification at a time: it takes a turn on the item with a conditional write first, long enough for all its deliveries, and works from the task that write returns, so what another server sent or finished meanwhile is not sent again, and a question's entry placed meanwhile is in the body. Who took it is written as it is taken; the write that records the last of its Notifiers (counting deleted ones as taken) also removes the item, so a notification all take on the first try costs two writes on the Task, the turn and that record, and no ack is left to be lost after it. A server run without Notifiers acks notifications as having nobody to tell. A Notifier that took it is recorded on the item, so a failing one does not make the others receive it again; the item stays until all have taken it. A receiver may still get one twice, when a server dies between the delivery and the record: it deduplicates on `id`, which is also the `Idempotency-Key`. A notification not taken within 24 hours is dropped, with a Warning Event `NotificationDropped` on the Task naming the Notifiers that never took it, so a dead webhook does not pin the task's outbox. The drop is the one write that ends the server's turn, never under another server's live turn, and only the server whose write removed the item emits the Event: another dropping it at the same time finds it gone and says nothing. So a drop is told at least once, and twice only when a server cannot tell whether its own write landed and finds the item gone, which another server's drop could have done. Two cases are accepted exceptions: a server whose drop was written but whose read to check it also failed tells nobody, and a server whose uncertain drop was in fact refused, while another server's record of the last delivery removed the item, tells of a drop that did not happen. Cancelling a task keeps its notifications, and tells `Cancelled` too.

**Egress.** The URL is whatever the Notifier says, and cadre-server does not filter private addresses. Creating a Notifier needs Kubernetes RBAC on the CRD: it is an admin's lever, granted as such.

## Credentials

- **Ordinary credentials** are Kubernetes Secrets referenced with `secretRef`, optionally synced from Vault or a cloud secret manager by External Secrets Operator.
- **Cloud services** use workload identity: an Agent or ToolServer maps to a ServiceAccount bound to a GCP or AWS IAM identity, no key files.
- **Credentials never leave Cadre.** Tools execute on the Cadre side; LLM calls are made by Cadre's worker or a gateway. Runtime-mode runners never receive a provider key.
- Rotation is the Secret's business.
- **Specs are not secret.** A team's members read its Agents' specs and status through the API (`GET /api/v1/namespaces/<namespace>/agents/<name>`; see [architecture.md](architecture.md#web-ui-and-api)). Put no credential in a spec: not in a Notifier's webhook headers, URL or tool arguments, nor in a ToolServer's readiness probe. Use a `secretRef`.

## Multi-team

- One team per namespace. Kubernetes RBAC decides who may change `Agent`, `ToolServer` and `Task` resources through GitOps or kubectl.
- Board actions (approve, assign, comment) are governed by Cadre's own SSO and the per-team role mapping on `Team`; see [architecture.md](architecture.md#identity-and-access).
- Boards are separated by namespace.

## Versioning

- Start at `v1alpha1`; moving to a new version needs a conversion webhook.
- CRDs are public API. The first version keeps fields to the minimum; it is far easier to add a field than to remove one.
