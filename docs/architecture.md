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
- The web UI is a React and TypeScript app in `web/`, built with Vite into `internal/web/dist` and embedded in cadre-server with `go:embed`: cadre-server serves it beside its own routes, so there is no separate Deployment: GET at `/`, under `/t/` (the app's routes, which get `index.html`, served uncached) and under `/assets/` (the build's hashed files, cached for a year). Every other path stays cadre-server's, so a wrong method on an API route is still a 405 and an unknown path a 404. `make ui` builds it (needs Node, see `web/.nvmrc`) and must run before `make build` for a cadre-server that carries it; the cadre-server image always does. A binary built without it serves a page at `/` saying how to build it.
- Team picker at `/`, from `GET /api/v1/teams`. The team is in the URL, `/t/<namespace>`, so a board can be linked to.
- Board at `/t/<namespace>`: the seven phases as columns, each read with the API's phase filter and paging and in its order (priority, then number), a page of 50 at a time with "load more". A card shows the number, title, assignee, priority, labels and, while Waiting, the reason. Search filters the loaded pages in the browser, and says so. The board polls every five seconds while the tab is visible.
- Task page at `/t/<namespace>/tasks/<name>`: the spec (the description as plain text; markdown is not rendered yet), the parent of a subtask, the status (phase, number, what it waits for, the run in progress, conditions), the thread and the run history. The thread is read once from the start, then only past the last entry seen, every five seconds; the entry the task waits on is marked. A human can comment until the task ends, reply while it waits on a question or a review, approve or reject while it waits on a tool approval (with a note), and cancel it, after a confirmation, until it ends. After an action the task and the thread are read again at once (the thread from the last entry held, so what others wrote meanwhile is not skipped); an action's error shows beside it, and a conflict (the task moved on) reads them again too. A run expands to its events, read once.
- New task at `/t/<namespace>/new`, linked from the board: title, description, an assignee (an Agent picked from the team's, a not-Ready one saying why, or a user by name), priority and labels; the server names the task. It is checked as the server would before it is sent.
- A subtask's card and page link to its parent. Listing a task's subtasks needs a parent filter in the storage index, which comes later.
- Identity, until sign-in: the UI asks for a display name once and keeps it in the browser; it is the author of what the user does, as `--as` is for the CLI. Nothing checks it. OIDC replaces it.
- The routes the UI uses, under `/api/v1`: `GET /teams`; under `/namespaces/<namespace>`: `GET /agents` (each Agent's name, whether it is Ready, and if not why), `GET /tasks` (`phase`, `offset`, `limit`), `POST /tasks`, and under `/tasks/<name>`: `GET`, `GET /thread` (`after`, `limit`), `POST /comments`, `POST /reply`, `POST /approve`, `POST /cancel`, `GET /runs` and `GET /runs/<run>/events`.
- Planned: subtask tree, deliverable approval queue (approve, reject, publish; plugin, see below).
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
- They run on one cadre-server replica at a time, elected with a Lease (`--leader-elect`, on by default in a cluster): starting a Job is not a conditional write, so two replicas would start Jobs for the same tasks. The Task Manager, the APIs and the MCP endpoint serve on every replica.

### Execution modes

There are two modes, not a list of backends:

| Mode | Who drives the loop | Where it runs | State |
| --- | --- | --- | --- |
| **step** | Cadre | Cadre's own worker Deployment | one step per message; any worker can take the next step |
| **runtime** | someone else's agent (an ACP agent such as Claude Code, kagent, a custom container) | a runner process: Kubernetes Job, Agent Sandbox, a user's own machine | the whole run is bound to one runner, managed by lease and run_id fencing |

Step-mode workers always run inside the cluster, as a pool of interchangeable pods that talk to the Task Manager, never to storage directly. Anything that runs outside the cluster is an external runner in runtime mode, whatever loop it contains.

Runtime-mode backends differ only in who starts the runner process. For the Kubernetes Job backend, cadre-server starts the Jobs itself (see [execution.md](execution.md#runtime-mode)): it watches Jobs cluster-wide, since an informer cannot be granted per namespace, but its cache keeps only those labelled `cadre.dev/agent`, and it may create and delete them only in the Team namespaces where `config/rbac/workloads.yaml` is bound. It watches the Deployments of managed ToolServers the same way: cluster-wide and read-only, caching only those labelled `cadre.dev/toolserver`, and creating, updating and deleting them and their Services only where that file is bound. They all speak the same pull-based runner protocol and all call tools through the per-run MCP endpoint. In step mode, Agent Sandbox is the place where tools that need a shell execute: a kind of ToolServer, not an execution backend.

### Per-run tool server
- Every run gets its own MCP endpoint (`/mcp/run`, authenticated by the run's bearer token) exposing Cadre's own tools (post to the thread, ask a question, create a subtask, read and write task files) plus the ToolServers the agent is allowed to use.
- Tools execute on the Cadre side with the team's credentials. Credentials never reach the execution environment.

### LLM access
- `ModelProvider` resources describe how to reach a model: Anthropic, an OpenAI-compatible endpoint with a custom base URL, Amazon Bedrock or Google Vertex via workload identity.
- Two modes: direct, where Cadre's worker holds the secret, or through a gateway such as agentgateway for rate limiting, per-team metering, provider fallback and guardrails. The gateway is optional.
- Runtime-mode runners never receive provider keys; they only receive the per-run MCP endpoint.

### Events
- Work hints ("run X has a next step") are optional and may be lost or duplicated. Correctness rests on conditional writes alone. The kubernetes driver gets hints for free from informer watches; a message queue is a later latency optimisation. Today they number a task applied with kubectl, and create a run's subtasks, as soon as the watch sees the change; the periodic scan (`--dispatch-interval`) stays as the fallback.
- UI events are watch events on Task resources and only say "something changed, refetch". Until the API exposes them, the UI polls. `thread.revision` on the Task status makes thread appends visible through the same watch. No Redis, no message queue.

## Identity and access

Two layers, as in Argo CD:

- **Kubernetes RBAC** governs the GitOps path: who may apply `Agent`, `ToolServer`, `Task` resources.
- **Cadre's own SSO**: OIDC (or Dex), groups mapped to per-team roles, a policy file for board actions (who may approve, assign, or only view), and a local bootstrap admin. Board users such as reviewers do not need a kubeconfig.

## Installation

- A Helm chart and a plain `install.yaml`, cluster level.
- Images: `ghcr.io/j75689/cadre-server`, `ghcr.io/j75689/cadre-runner` and `ghcr.io/j75689/cadre` (the CLI), for linux/amd64 and linux/arm64, built from the one `Dockerfile` (`make image CMD=...` locally) on distroless/static as a non-root user. A `v*` tag publishes `vX.Y.Z`, `vX.Y` and `latest`; every push to main publishes `main` and `main-<short sha>`. Each binary prints its version with `--version`: `vX.Y.Z` for a release, `main-<short sha>` for a `main` image. The packages pushed by this repository's workflow have been public from their first push; if a pull is refused, set the package to public in its settings.
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
