# Cadre

**A self-hosted, cloud-native platform for humans and AI agents to work together.**
Declare agents, models and tools as Kubernetes CRDs. Run the work on a board. Keep every credential inside the cluster.

> Status: M0, M1 and M2 complete (October 2026). Agents, models and tool servers are CRDs; `cadre-runner` runs builtin agents on Claude or any OpenAI-compatible endpoint, a step at a time by default, so any worker continues a run; runner agents' work can run in Kubernetes Jobs; Cadre can run a ToolServer itself; runs reach their tools through a per-run MCP endpoint; runners authenticate with ServiceAccount tokens. cadre-server serves the web console: the board (also as a list), task pages with live runs, Agents, Model providers, Tool servers, Notifiers, Runners, and the team's Settings with its agent access ([Web UI](#web-ui)). See [Roadmap](#roadmap).

## What it is

Agent infrastructure on Kubernetes has settled into layers: agent runtimes (kagent), traffic and policy (agentgateway), isolated execution (Agent Sandbox), workflow engines (Argo Workflows, Temporal). Every one of them answers "how do the machines run". None of them answer:

- whose task is this, and how far has it got;
- when does a human need to look, and who continues after the human answers;
- does this output need approval before it leaves the building.

Cadre is that layer: a board where humans and a group of agents share the work, shaped like Argo CD. CRDs are the source of configuration, controllers reconcile them, and the project ships its own API server, CLI and web UI.

- **Board.** Tasks, subtasks, threads, phases. One board per team, one team per namespace.
- **Human in the loop.** Agents ask questions in the thread and are woken when a human answers. Tools can be set to ask first. Output can require approval.
- **Dispatch and execution.** Tasks are handed to agents and executed by Cadre's own loop or by any runner that speaks the runner protocol: a Kubernetes Job, an Agent Sandbox, kagent, or a developer's laptop running Claude Code. A task never runs twice.
- **Configuration as code.** `Agent`, `ModelProvider`, `ToolServer`, `Schedule` are CRDs, managed with GitOps.

## What it is not

Not a SaaS. Not a workflow engine. Not an LLM gateway. Not a sandbox. Cadre integrates those instead of rebuilding them, and keeps its own core to two things: task orchestration and the agent runner. Everything else is a plugin: an MCP tool server, or a controller that watches Task events.

## Design principles

1. **Configuration in CRDs, data in a storage driver.** Rarely changing, GitOps-friendly things are CRDs; tasks, threads and runs go to a pluggable store.
2. **The core needs three storage operations.** Single-object conditional write, append-only log, rebuildable index. A small installation needs no database.
3. **Fencing guarantees correctness; locks only reduce rework.** Every transition is a conditional write carrying a run id.
4. **Runs are short.** When an agent waits for a human the run ends; state lives on the task; a new run continues later.
5. **Do not reinvent.** LLM traffic can go through agentgateway, isolation comes from Agent Sandbox, every integration is an MCP server.
6. **Forward only.** Failed runs retry or continue; no compensation. The few tools with external side effects use idempotency keys and reconciliation.

## Documentation

| Document | Contents |
| --- | --- |
| [docs/architecture.md](docs/architecture.md) | components, execution modes, identity, installation, plugins, ecosystem |
| [docs/crds.md](docs/crds.md) | `Team`, `Task`, `Agent`, `ModelProvider`, `ToolServer`, `Runner`, `AgentAccess`, `Schedule`, `Notifier`, credentials, multi-team |
| [docs/execution.md](docs/execution.md) | state transitions, step and runtime modes, runner protocol, per-run MCP, fencing and idempotency |
| [docs/storage.md](docs/storage.md) | the three operations, the kubernetes and postgres drivers, conformance tests |

## Web UI

cadre-server serves the web console at `/`: **Teams**, then a team's **Board** (its columns, or one sortable table under **List**), task pages with their threads, actions and live runs, **New task**, **Agents** (each Agent's name opens its page: conditions, what it uses, its metadata and its manifest to copy), **Model providers**, **Tool servers**, **Notifiers** and **Runners** (each with its page, read-only: GitOps owns them), and **Settings**: the **Team** and, with logins, **Agent access** for its admins. Page by page: [docs/web-ui.md](docs/web-ui.md#pages). The web UI is built into cadre-server (`make ui` before `make build`; the image carries it). Developing it: [web/README.md](web/README.md).

### Keyboard shortcuts

Press `?`, or the **Keyboard shortcuts** button in the top bar, to see them in the console.

| Key | Does | Where |
|---|---|---|
| `?` | shows the shortcuts | any page, also while a dialog is open |
| `/` | focuses the board's search | the board, `/t/<namespace>` |
| `c` | opens New task | a team's pages, for members and admins; not on New task itself |
| `g` then `b` | goes to the board | a team's pages; `b` must follow `g` within 1 second; not on the board itself |

- A shortcut is a single key. With Ctrl, Cmd or Alt it is left to the browser; the exception is AltGr, which still works for `/` and `?` on layouts that type them with it.
- None works while you type in a field (or compose with an input method), or while a dialog is open, except `?`. Escape closes the dialog.
- `c` and `g` `b` leave the page, so they also do nothing while the focus is in a form, or while the page holds an unsaved draft (a new task, a changed Settings form, a reply or comment with text, a tool call note). Your text is never lost to a stray key.

## Installation

With the Helm chart, the supported way:

```sh
helm install cadre oci://ghcr.io/j75689/charts/cadre -n cadre-system --create-namespace
```

with an OIDC issuer and client, the URL browsers reach it at (an Ingress host or `server.externalURL`), bootstrap admins and an S3 bucket set: see the chart's [README](charts/cadre/README.md) for the values, the per-Team RBAC and upgrading the CRDs.

Or, to try Cadre, with the `install.yaml` attached to each [release](https://github.com/j75689/cadre/releases): the chart rendered without logins, so anyone who reaches cadre-server is an admin, and with the bucket `CHANGE-ME` to replace. Its header says how to apply it.

### Install the CLI

```sh
curl -fsSL https://raw.githubusercontent.com/j75689/cadre/main/hack/install.sh | sh
```

installs `cadre` from the latest release, checked against the release's checksums, into `/usr/local/bin` when it is writable, else `~/.local/bin`. `CADRE_VERSION=v1.2.3` picks a release and `CADRE_INSTALL_DIR` the directory, for example `curl -fsSL … | CADRE_VERSION=v1.2.3 CADRE_INSTALL_DIR=$HOME/bin sh`.

Other ways to install it:

- **By hand.** Download `cadre_<version>_<os>_<arch>.tar.gz` (on Windows, the `.zip`) and `cadre_<version>_checksums.txt` from a [release](https://github.com/j75689/cadre/releases), check them with `sha256sum -c --ignore-missing cadre_<version>_checksums.txt` (on macOS, `shasum -a 256 -c --ignore-missing cadre_<version>_checksums.txt`), and put `cadre` on your `PATH`.
- **With Go 1.27 or later:** `go install github.com/j75689/cadre/cmd/cadre@latest`, or `@vX.Y.Z`.
- **From a clone:** `make install-cadre-cli`, which installs into `go env GOBIN`, or `GOPATH/bin` when that is unset.
- **As an image,** for CI and containers: `docker run --rm ghcr.io/j75689/cadre:latest version`.

Then point it at your server and log in:

```sh
export CADRE_SERVER=https://cadre.example.test   # or --server on each command
cadre login
cadre version
```

`cadre --help` lists the rest.

## Images

`ghcr.io/j75689/cadre-server`, `ghcr.io/j75689/cadre-runner` and `ghcr.io/j75689/cadre`, for linux/amd64 and linux/arm64: `main` follows the main branch, `vX.Y.Z` each release. See [Installation](docs/architecture.md#installation).

## Roadmap

| Milestone | Scope | Done when |
| --- | --- | --- |
| **M0** state machine (done) | `Task` and `Team` CRDs, storage interface, conformance suite, kubernetes driver, every transition, outbox dispatcher, `cadre` CLI with a fake runner | two managers survive the chaos test; no task runs twice |
| **M1** runner (done) | `Agent`, `ModelProvider`, `ToolServer`, runner protocol, per-run MCP, `cadre-runner` builtin loop (Anthropic, OpenAI-compatible) | `kubectl apply` a Task, the agent asks a question, a human answers from the CLI, the agent finishes |
| **M2** execution (done) | builtin loop in step mode, the default for builtin agents since October 2026 (`runtime.execution: runtime` is the fallback), Job backend, managed ToolServers | a rolling update of workers never interrupts a run |
| **M3** platform (done) | web UI, OIDC and policy RBAC, `Schedule`, `Notifier`, Helm chart | a team uses Cadre day to day without kubectl |
| **M4** extensions | postgres driver, operator, plugin interface, deliverables and publishers, Agent Sandbox, agentgateway | a publisher attaches as a plugin |

## Contributing

Cadre is early: the design documents under `docs/` are the spec, and the code follows them. Read [CONTRIBUTING.md](CONTRIBUTING.md) and open an issue or a pull request. Everyone participating is expected to follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## Name

A cadre is a small group of trained core members. Cadre runs a squad made of humans and agents.

## License

[Apache License 2.0](LICENSE).
