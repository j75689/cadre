# Architecture

## Overview

```
                 Users (browser, CLI, external agents over MCP)
                                  │
┌─────────────────────────── Cadre ───────────────────────────────────┐
│  Web UI (board, threads, approvals) + API + MCP endpoint             │
│                 │ every state change goes through the Task Manager   │
│  Task Manager (state machine: claim, fencing, finish, wait, approve) │
│                 │                                                    │
│  Controllers (sync CRDs, schedules, dispatch runs)                   │
│                 │                                                    │
│  Storage driver: kubernetes | postgres                               │
└───────┬─────────────────────────────────┬───────────────────────────┘
        │ CRDs (configuration)            │ runs
  Task / Team                       step mode:    Cadre's own workers
  Agent / ModelProvider / ToolServer runtime mode: a runner process
  Schedule / Notifier                              (Kubernetes Job, Agent Sandbox,
                                                    kagent, a user's own machine)
                                  │
             LLM traffic → the provider directly, or through a gateway
```

Cadre is a cluster-level installation that serves many teams. One team is one namespace, opted in with a `Team` resource. The shape is Argo CD's: CRDs as the source of configuration, controllers that reconcile them, and the project's own API server and web UI on top.

## Non-goals

Cadre is not a SaaS, not a workflow engine, not an LLM gateway and not a sandbox. Each of those layers has good projects already (see [Ecosystem](#ecosystem)). Cadre covers the one layer none of them do: whose task this is, how far it has got, when a human needs to look, who continues after the human answers, and whether output needs approval before it leaves.

## Components

### Web UI and API
- Board: columns by phase, ordering by priority, search, subtask tree.
- Thread: the conversation between humans and the agent, tool call records, files.
- Approval queue: deliverable drafts, approve, reject, publish (plugin, see below).
- The API serves the CLI and external systems; the MCP endpoint lets external agents (Claude Code, for example) create tasks, comment and read progress.
- Stateless and horizontally scalable. Every state change is delegated to the Task Manager.

### Task Manager
- The only place that changes task state: claim, lease, finish, hand-off, wait, approve.
- Every transition is a single-object conditional write (see [execution.md](execution.md)).
- Side effects (dispatching a run, notifying) go through an outbox embedded in the task object and written in the same conditional write.
- Stateless: any instance can handle any task, so it scales by adding replicas.

In phase one the API and the Task Manager ship in one binary, `cadre-server`, because both are stateless and splitting them has no benefit yet.

### Controllers
- Sync CRDs into Cadre's configuration: create an `Agent` and the board can assign to it.
- Schedules: read `Schedule` resources and create tasks when due.
- Dispatch: hand claimed tasks to the right execution mode.

### Execution modes

There are two modes, not a list of backends:

| Mode | Who drives the loop | Where it runs | State |
| --- | --- | --- | --- |
| **step** | Cadre | Cadre's own worker Deployment | one step per message; any worker can take the next step |
| **runtime** | someone else's agent (an ACP agent such as Claude Code, kagent, a custom container) | a runner process: Kubernetes Job, Agent Sandbox, a user's own machine | the whole run is bound to one runner, managed by lease and run_id fencing |

Step-mode workers always run inside the cluster, as a pool of interchangeable pods that talk to the Task Manager, never to storage directly. Anything that runs outside the cluster is an external runner in runtime mode, whatever loop it contains.

Runtime-mode backends differ only in who starts the runner process. They all speak the same pull-based runner protocol and all call tools through the per-run MCP endpoint. In step mode, Agent Sandbox is the place where tools that need a shell execute: a kind of ToolServer, not an execution backend.

### Per-run tool server
- Every run gets its own MCP endpoint (`/mcp/run`, authenticated by the run's bearer token) exposing Cadre's own tools (post to the thread, ask a question, create a subtask, read and write task files) plus the ToolServers the agent is allowed to use.
- Tools execute on the Cadre side with the team's credentials. Credentials never reach the execution environment.

### LLM access
- `ModelProvider` resources describe how to reach a model: Anthropic, an OpenAI-compatible endpoint with a custom base URL, Amazon Bedrock or Google Vertex via workload identity.
- Two modes: direct, where Cadre's worker holds the secret, or through a gateway such as agentgateway for rate limiting, per-team metering, provider fallback and guardrails. The gateway is optional.
- Runtime-mode runners never receive provider keys; they only receive the per-run MCP endpoint.

### Events
- Work hints ("run X has a next step") are optional and may be lost or duplicated. Correctness rests on conditional writes alone. The kubernetes driver gets hints for free from informer watches; a message queue is a later latency optimisation. Today they number a task applied with kubectl, and create a run's subtasks, as soon as the watch sees the change; the periodic scan (`--dispatch-interval`) stays as the fallback.
- UI events are watch events on Task resources and only say "something changed, refetch". `thread.revision` on the Task status makes thread appends visible through the same watch. No Redis, no message queue.

## Identity and access

Two layers, as in Argo CD:

- **Kubernetes RBAC** governs the GitOps path: who may apply `Agent`, `ToolServer`, `Task` resources.
- **Cadre's own SSO**: OIDC (or Dex), groups mapped to per-team roles, a policy file for board actions (who may approve, assign, or only view), and a local bootstrap admin. Board users such as reviewers do not need a kubeconfig.

## Installation

- A Helm chart and a plain `install.yaml`, cluster level.
- Local development uses the Kubernetes built into Docker Desktop or OrbStack, with SeaweedFS as the object store. There is no single-machine mode outside Kubernetes.
- An operator and a top-level `Cadre` resource arrive together with the postgres driver, which needs managed databases and migrations.

### Enterprise checklist
- Exportable audit log.
- All images mirrorable to an internal registry.
- HTTP proxy support for LLM egress.
- CLI with `login` and contexts.

## Plugins

The core does two things: task orchestration and the agent runner. Anything else is a plugin, and a plugin is one of exactly two shapes:

1. An MCP `ToolServer`, for anything an agent should be able to do.
2. A separate controller that watches Task events and owns its own CRD, for anything that reacts to the board: publishers, deliverable approval, notifiers, schedulers.

## Ecosystem

| Project | Role | Relation to Cadre |
| --- | --- | --- |
| kagent | agent execution, agents as CRDs | a runtime-mode backend, integrated over A2A |
| agentgateway | LLM, MCP and A2A traffic and policy | an optional gateway |
| Agent Sandbox (kubernetes-sigs) | isolated, warm execution environments | a runtime-mode backend, or a ToolServer for shell tools in step mode |
| Argo CD | GitOps | syncs Cadre's CRDs |
| CloudNativePG, Crossplane | databases | create the database for the postgres driver |
| **Cadre** | tasks, board, human-agent collaboration, approvals, schedules | the missing layer |

## Code layout

```
api/v1alpha1/            CRD types
cmd/cadre/               CLI
cmd/cadre-server/        API, Task Manager, per-run MCP
cmd/cadre-runner/        runner for the builtin loop
internal/task/           aggregate and state machine, pure logic, no storage access
internal/storage/        driver interface, conformance/, kubernetes/, postgres/
internal/runner/         runner protocol
internal/mcp/            per-run MCP endpoint
internal/controller/     Team, Task, Agent, ToolServer reconcilers
docs/                    architecture and design
```
