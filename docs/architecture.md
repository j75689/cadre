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
- The web UI is a React and TypeScript app in `web/`, built with Vite into `internal/web/dist` and embedded in cadre-server with `go:embed`: cadre-server serves it beside its own routes, so there is no separate Deployment: GET at `/`, under `/t/` (the app's routes, which get `index.html`, served with `Cache-Control: no-store`, so a new build is picked up at once and no HTTP cache keeps it; Chrome still restores such a page from its back/forward cache unless a cookie changed, as Log out does, or the page received an answer that is `no-store` too, as a rotated token's is) and under `/assets/` (the build's hashed files, cached for a year). Every other path stays cadre-server's, so a wrong method on an API route is still a 405 and an unknown path a 404. `make ui` builds it (needs Node, see `web/.nvmrc`) and must run before `make build` for a cadre-server that carries it; the cadre-server image always does. A binary built without it serves a page at `/` saying how to build it.
- The UI is an operations console, designed in [web-ui.md](web-ui.md): semantic colour tokens, light and dark themes (the system's, or one picked in the top bar and kept in the browser), a system font stack and inline icons, so it fetches nothing from outside the cluster and works air-gapped.
- Team picker at `/`, from `GET /api/v1/teams`, which also fills the top bar's team switcher on every page. The team is in the URL, `/t/<namespace>`, so a board can be linked to.
- Board at `/t/<namespace>`: the seven phases as columns, each read with the API's phase filter and paging and in its order (priority, then number), a page of 50 at a time with "load more". A card shows the number, title, assignee, priority, labels and, while Waiting, the reason. Search and an assignee filter the loaded pages in the browser, say so, and count how many of the loaded tasks they show; a phase filter reads that one column only. The search, the filters, the view and the list's order are in the URL (`?q=`, `?phase=`, `?assignee=`, `?view=list`, `?sort=`; see [web-ui.md](web-ui.md#url-parameters)), so a filtered board can be linked to. The list view (`?view=list`) shows the loaded columns' tasks as one sortable table. The board polls every five seconds while the tab is visible (and reads again on the watch stream's hints, below), and its toolbar says when it was last read, or why the last read failed.
- Task page at `/t/<namespace>/tasks/<name>`: the spec (the description as plain text; markdown is not rendered yet), the parent of a subtask, the status (phase, number, what it waits for, the run in progress, conditions), the thread and the run history. The thread is read once from the start, then only past the last entry seen, every five seconds; the entry the task waits on is marked. A human can comment until the task ends, reply while it waits on a question or a review, approve or reject while it waits on a tool approval (with a note), and cancel it, after a confirmation, until it ends. The thread ends in one composer, which offers what the task waits for (a reply, or the approval) and turns into a comment on request; otherwise it is a comment. After an action the task and the thread are read again at once (the thread from the last entry held, so what others wrote meanwhile is not skipped); an action's error shows beside it, and a conflict (the task moved on) reads them again too. An ended run expands to its events, read once. While the task is Running, the run in progress shows its events and token totals, read again every four seconds while the tab is visible; `?run=<id>` opens the page at a run.
- New task at `/t/<namespace>/new`, linked from the board: title, description, an assignee (an Agent picked from the team's, a not-Ready one saying why, or a user by name), priority and labels; the server names the task. It is checked as the server would before it is sent.
- Agents at `/t/<namespace>/agents`, for every role: the team's Agents, whether each is Ready and if not why, read every 15 seconds, each with a link to the board filtered to its tasks, and its name to the Agent's page at `/t/<namespace>/agents/<name>`: its conditions, what it uses, its metadata and its spec and status as YAML to copy, as `GET /agents/<name>` shows them to the caller's role.
- Model providers at `/t/<namespace>/modelproviders`, for every role: the team's ModelProviders, whether each is Ready and if not why, read every 15 seconds, each name linking to its page at `/t/<namespace>/modelproviders/<name>`, laid out as an Agent's (conditions, metadata, its YAML to copy) without the Uses card, as `GET /modelproviders/<name>` shows it to the caller's role: a viewer gets health and metadata only, a member the spec with the masked parts of `baseURL` as `(set)`, an admin all of it. An Agent's page links its `spec.modelRef` to this page.
- Settings at `/t/<namespace>/settings`, for every role: the team's Team, read every 15 seconds. Every role sees its display name and namespace; its admins (everyone under `--auth=none`) also see its limits, roles and run slots, and edit it with the `resourceVersion` it was read at, so a conflicting save is shown, not lost. With logins, its admins also get the Agent access tab.
- A subtask's card and page link to its parent, and a task's page lists its subtasks.
- Agent access at `/t/<namespace>/settings/access`, with logins, for a team's admins: the team's AgentAccesses, and create, edit, rotate (the new token shown once) and delete over the routes below. How an operator uses it is in [crds.md](crds.md#agentaccess). Keyboard shortcuts (`?` lists them) are in the [README](../README.md#keyboard-shortcuts).
- Identity: with logins the UI asks `GET /api/v1/me` who it is, shows the user with a logout, and sends no author; any request answered 401 sends the browser to log in and back to the page. Without logins (`--auth=none`) it asks for a display name once, keeps it in the browser and sends it as the author, as `--as` does for the CLI. See [Identity and access](#identity-and-access).
- The routes the UI uses, under `/api/v1`: `GET /me`, `GET /teams`; under `/namespaces/<namespace>`: `GET /agents` (each Agent's name, whether it is Ready, and if not why), `GET /agents/<name>`, `GET /modelproviders`, `GET /modelproviders/<name>`, `GET /tasks` (`phase`, `parent`, `offset`, `limit`; `parent` lists a task's subtasks), `POST /tasks`, `GET /team` and `PUT /team` (creating the Team, for a bootstrap admin, or editing it, for its admins); for the Team's admins, with logins only, `GET /agentaccesses`, `GET /agentaccesses/<name>`, `PUT /agentaccesses/<name>`, `POST /agentaccesses/<name>/token` and `DELETE /agentaccesses/<name>`; and under `/tasks/<name>`: `GET`, `GET /thread` (`after`, `limit`), `POST /comments`, `POST /reply`, `POST /approve`, `POST /cancel`, `GET /runs` and `GET /runs/<run>/events`. Logging in and out is `GET /auth/login` and `POST /auth/logout`, outside `/api/v1`.
- The CLI uses these routes too: the Team's as `cadre team init|set`, the AgentAccesses' as `cadre agentaccess list|set|token|delete`. It also uses `GET /api/v1/auth` (how to log in) and `GET /tasks`' `number` (a task by its number, see below). A list of AgentAccesses is read from cadre-server's cache; one AgentAccess fresh from the API server, so a PUT right after a create or a delete is decided on what is there. A PUT makes an AgentAccess, 201, or, carrying the `resourceVersion` it was read at, replaces its spec, 200: one without it over an existing one is 409 `already_exists`, one from a stale version 409 `conflict`, and what the CRD refuses (an `admin` role, a dotted name, a namespace that does not exist) 400 with the API server's reason. If cadre-server's own ClusterRole lacks a verb on agentaccesses, the answer is 500 `server_forbidden`, logged as an error naming the verb: a mistake in the install, which no retry fixes. Each answer shows the spec, whether it is Ready and if not why, and the Secret a minted token is in: never the token, nor its hash. The token is read from that Secret by whoever may read Secrets in the namespace (`cadre agentaccess set` prints the `kubectl` command), or replaced: `POST /agentaccesses/<name>/token` (`cadre agentaccess token <name>`, which prints it alone on stdout) mints a new token for an AgentAccess without a `serviceAccount`, writes it into the Secret and its hash into the status at once, so the old token stops then and the reconciler finds nothing to do, and answers with it. That answer is the only place a token is ever shown, and it is never logged, nor stored (`Cache-Control: no-store`, so Chrome loads a console page that received one again on Back rather than restoring it from its back/forward cache, when the answer arrived before the page was left): a token is never read back, only replaced. A Normal Event `TokenRotated` on the AgentAccess names who rotated it. One with a `serviceAccount`, whose Secret is not its own, whose name is not a DNS label, or in a namespace without a Team, is a 400: none the reconciler would mint for. Of two rotations at once, one wins and the other is a 409 naming the Secret; one whose AgentAccess was switched to a `serviceAccount` meanwhile is a 400; and one that wrote the Secret but not the hash (the AgentAccess kept changing for a couple of seconds, or the write failed) is a 409 saying the new token is in the Secret, unshown, and the old one works until the reconciler's next pass, with its `TokenRotated` Event all the same. The answer waits, up to five seconds, for cadre-server's cache to hold the new hash, so the token works as soon as it is shown.
- Under `/namespaces/<namespace>`, `GET /agents/<name>` reads one Agent from cadre-server's cache (the controllers' informers, which every replica starts at boot), read-only: Agents are changed with kubectl or GitOps. It is a 404 for an Agent that is not in the namespace named; in a namespace without a Team it is a 403 `no_role` for all but a bootstrap admin, who gets a 404. Every role sees its name, whether it is Ready for its current spec and if not why (Pending, as in the list, while the controller has not checked the current spec), its `kind`, `generation`, `observedGeneration`, conditions and its uid, `resourceVersion`, creation time and labels. A member, and an admin (everyone under `--auth=none`), also see its annotations, spec and status; in its job template (`spec.runtime.job.template`) a member, an AgentAccess's agent included, sees `(set)` in place of each container's and init container's literal env values, command and arguments (an env var's name and `valueFrom` stay, as they name a Secret, not its value), and an admin sees them whole. Nobody sees its `managedFields`, nor the `kubectl.kubernetes.io/last-applied-configuration` annotation, which repeats the whole spec. ModelProviders are served the same way: `GET /modelproviders` lists the namespace's, by name, each with whether it is Ready and if not why, as `GET /agents` does (in a namespace without a Team, a 403 `no_role` for all but a bootstrap admin, who gets a 404), and `GET /modelproviders/<name>` reads one; in its `baseURL` a member sees `(set)` in place of the user info, of each query value (or a query part with no `=`) and of the fragment (`https://(set)@gw.example.test/v1?api-key=(set)#(set)`), and in place of the whole `baseURL` when it does not parse as a URL; its scheme, host, port and path are shown as they are, and an admin sees it whole. ToolServers are served the same way, as `GET /toolservers` and `GET /toolservers/<name>`; in its readiness probe (`spec.managed.readinessProbe`) a member sees `(set)` in place of each `httpGet.httpHeaders` value (the header names stay) and each `exec.command` entry, and in place of its `auth.prefix` unless that is empty or one word and a space (`Bearer `), and its `spec.url`, `spec.managed.path` and the probe's `httpGet.path` are masked as a `baseURL` is, as is `status.url` after the Service's address, so its path is masked as `spec.managed.path` is (`//(set)@evil/mcp`); its `auth.secretRef`, a `tcpSocket` or `grpc` probe and the rest of its spec are shown as they are, and an admin sees it whole. Schedules are served the same way, as `GET /schedules` and `GET /schedules/<name>`, with nothing masked: the task template (`spec.task`) is copied as it is into each Task the Schedule creates, which a viewer reads. Notifiers are served the same way, as `GET /notifiers` and `GET /notifiers/<name>`; a member sees `(set)` in place of each `spec.webhook.headers` value and each `spec.toolServer.arguments` value (the header and argument names stay), and its `spec.webhook.url` is masked as a `baseURL` is; its `on` events, `webhook.secretRef`, ToolServer and tool, and its status (`lastSent`, and `lastError`, whose message already names the webhook without its query) are shown as they are, and an admin sees it whole. Runners will be served the same way, as `GET /runners` and `GET /runners/<name>`; a Runner's token hash is nobody's to see.
- Planned: subtask tree, deliverable approval queue (approve, reject, publish; plugin, see below).
- The API serves the CLI and external systems; the board MCP endpoint, `POST /mcp`, lets external agents (Claude Code, for example) list and read tasks and their threads, create tasks, comment and reply. Its tools are `list_tasks`, `get_task`, `create_task`, `comment` and `reply`; each names the team's namespace; those acting on a task name it, or give its `#number`. A task is found by its number with `GET /namespaces/{ns}/tasks?number=N`, in the list's shape with zero items or one; `cadre`'s `#N` arguments and the board's tools use it. It is one read of the board index, by `status.number`. The index lags the store by a moment, so a miss is checked against the Team's `status.nextNumber`, read live: a number never given, or one given before the last 100, is not found at once, and one given a moment ago, among the last 100, to a task just created say, is looked for again every 100 ms for up to 2 seconds. A number that names more than one task, as it can after a Team is deleted and made again, is a 409 `conflict` naming them. A namespace is never listed for it, and a create's response names the task, which is what callers use when they have it.
  - **Authentication.** It takes a bearer ID token only, verified as the API verifies one, with no session cookie, so a page in a browser cannot act through it.
  - **Authorization.** Each tool call is decided by the caller's role on the team it names, as the API's routes are: a viewer reads, a member also writes, and a refusal is a tool error naming the role needed.
  - **Authorship.** What it writes is the caller's.
  - **Without logins.** With `--auth=none` it is open, like the API, and what it writes is `anonymous`'s.
  - **Connecting an agent.** `cadre auth token` prints the token `cadre login` stored, while it is good for more than a minute (refreshing it first, where the login has a refresh token), for example: `claude mcp add --transport http cadre https://cadre.example.test/mcp --header "Authorization: Bearer $(cadre auth token)"`.
  - **The limit.** An ID token lasts hours at most, so this suits an agent someone runs themselves and logs in for; an unattended agent needs a token of its own (#93).
- Stateless and horizontally scalable. Every state change is delegated to the Task Manager.
- Reading the ModelProviders (`GET /api/v1/namespaces/<namespace>/modelproviders` and `…/modelproviders/<name>`), like an Agent, below (`cadre auth token`, or no header under `--auth=none`). The list is `{"items":[…]}`, sorted by name, each item `name`, `ready` and, when it is not Ready, `reason` and `message`; the answer for one is an Agent's, with `"kind":"ModelProvider"`. It is decided by the caller's role on the Team:

  ```sh
  curl -s -H "Authorization: Bearer $(cadre auth token)" \
    "$CADRE_SERVER/api/v1/namespaces/demo/modelproviders"
  curl -s -H "Authorization: Bearer $(cadre auth token)" \
    "$CADRE_SERVER/api/v1/namespaces/demo/modelproviders/gateway"
  ```

  The list (`bad` and `gateway` are Ready, `claude` is not yet):

  ```json
  {"items":[{"name":"bad","ready":true},{"name":"claude","ready":false,"reason":"SecretNotFound","message":"Secret anthropic does not exist"},{"name":"gateway","ready":true}]}
  ```

  A viewer, for `claude` once its Secret is there:

  ```json
  {"name":"claude","ready":true,"kind":"ModelProvider","generation":2,"observedGeneration":2,"conditions":[{"type":"Ready","status":"True","observedGeneration":2,"lastTransitionTime":"2026-10-30T07:56:04Z","reason":"SecretFound","message":""}],"metadata":{"uid":"ee494702-c726-4f01-adf1-d8197d5934d2","resourceVersion":"297","creationTimestamp":"2026-10-30T07:55:15Z","labels":{"tier":"gold"}}}
  ```

  A member gets the same, and `metadata.annotations`, `spec` and `status` besides:

  ```json
  {"…":"…","metadata":{"…":"…","annotations":{"owner":"platform"}},"spec":{"type":"anthropic","model":"claude-opus-5-5","auth":{"secretRef":{"name":"anthropic","key":"apiKey"}}},"status":{"observedGeneration":2,"conditions":[{"type":"Ready","status":"True","…":"…"}]}}
  ```

  Only `spec.baseURL` is masked, and only for a member. For `https://svc:s3cret@gw.example.test:8443/v1?api-key=abc&debug#frag` a member reads `https://(set)@gw.example.test:8443/v1?api-key=(set)&(set)#(set)`: the user info, each query value, a query part with no `=` and the fragment are `(set)`, the scheme, host, port, path and query keys are not. A `baseURL` that does not parse as a URL (`https://gw.example.test/%zz?k=v`) is `(set)` whole. An admin gets the real value. `auth` is a reference to a Secret, not a credential, and every member sees it as it is. The failures are an Agent's: 401 `unauthenticated`, 403 `no_role` (a namespace without a Team included, for all but a bootstrap admin, who gets 404 `not_found` there, for the list as for one) and 404 `not_found` for a ModelProvider that is not there.
- Reading the ToolServers (`GET /api/v1/namespaces/<namespace>/toolservers` and `…/toolservers/<name>`), like a ModelProvider, above. The list is `{"items":[…]}`, sorted by name, each item `name`, `ready` and, when it is not Ready, `reason` and `message`; the answer for one is an Agent's, with `"kind":"ToolServer"`:

  ```sh
  curl -s -H "Authorization: Bearer $(cadre auth token)" \
    "$CADRE_SERVER/api/v1/namespaces/demo/toolservers"
  curl -s -H "Authorization: Bearer $(cadre auth token)" \
    "$CADRE_SERVER/api/v1/namespaces/demo/toolservers/github"
  ```

  The list (`docs` and `shell` are managed and their Deployments have not finished rolling out; `badauth` and `github` are Ready):

  ```json
  {"items":[{"name":"badauth","ready":true},{"name":"docs","ready":false,"reason":"Deploying","message":"its Deployment has not finished rolling out its current spec"},{"name":"github","ready":true},{"name":"shell","ready":false,"reason":"Deploying","message":"its Deployment has not finished rolling out its current spec"}]}
  ```

  A viewer, for `github`:

  ```json
  {"name":"github","ready":true,"kind":"ToolServer","generation":3,"observedGeneration":3,"conditions":[{"type":"Ready","status":"True","observedGeneration":3,"lastTransitionTime":"2026-10-30T15:11:37Z","reason":"SecretFound","message":""}],"metadata":{"uid":"afafe371-23de-432d-80c5-8fefc42dc57e","resourceVersion":"314","creationTimestamp":"2026-10-30T15:10:35Z","labels":{"tier":"gold"}}}
  ```

  A member gets the same, and `metadata.annotations`, `spec` and `status` besides (the API writes `&` as `\u0026`):

  ```json
  {"…":"…","metadata":{"…":"…","annotations":{"owner":"platform"}},"spec":{"transport":"streamable_http","url":"https://(set)@mcp.example.test:8443/mcp?api-key=(set)\u0026(set)#(set)","auth":{"secretRef":{"name":"github","key":"token"},"header":"Authorization","prefix":"Bearer "}},"status":{"observedGeneration":3,"conditions":[{"type":"Ready","status":"True","…":"…"}]}}
  ```

  An admin reads that `url` as `https://svc:s3cret@mcp.example.test:8443/mcp?api-key=abc&debug#frag`. What a member sees of each place a credential can be, for the ToolServers above:

  | Field | Stored | A member reads |
  | --- | --- | --- |
  | `spec.url` (`github`) | `https://svc:s3cret@mcp.example.test:8443/mcp?api-key=abc&debug#frag` | `https://(set)@mcp.example.test:8443/mcp?api-key=(set)&(set)#(set)` |
  | `spec.managed.path` (`docs`) | `/mcp?key=abc` | `/mcp?key=(set)` |
  | `status.url` (`docs`) | `http://docs.demo.svc:8080/mcp?key=abc` | `http://docs.demo.svc:8080/mcp?key=(set)` |
  | `spec.managed.readinessProbe.httpGet.path` (`docs`) | `/healthz?token=abc` | `/healthz?token=(set)` |
  | `…httpGet.httpHeaders` (`docs`) | `{"name":"X-Probe-Key","value":"abc"}` | `{"name":"X-Probe-Key","value":"(set)"}` |
  | `…readinessProbe.exec.command` (`shell`) | `["check","--token=abc"]` | `["(set)","(set)"]` |
  | `spec.auth.prefix` (`badauth`) | `sk_abc ` | `(set)` |
  | `spec.auth.prefix` (`github`) | `Bearer ` | `Bearer ` |

  A `url` or path that does not parse as a URL is `(set)` whole, as a ModelProvider's `baseURL` is. An `auth.prefix` is shown only when it is empty or letters and a space (`Bearer `). Still shown to a member as they are: the host, port and path of a `url`, the Service's address in `status.url`, `managed.image`, `auth.secretRef`, `auth.header`, the probe's `port` and a `tcpSocket` or `grpc` probe; so keep a credential out of those too. The failures are the Agent's: 401 `unauthenticated`, 403 `no_role` (a namespace without a Team included, for all but a bootstrap admin, who gets 404 `not_found` there, for the list as for one) and 404 `not_found` for a ToolServer that is not there, or is in another namespace.
- Reading one Agent (`GET /api/v1/namespaces/<namespace>/agents/<name>`), with an ID token from `cadre auth token` (with logins; without them, `--auth=none`, send no header). The answer is an object, `name`, `ready`, `reason` and `message` as in the list, then `kind`, `generation`, `observedGeneration`, `conditions` and `metadata` (`uid`, `resourceVersion`, `creationTimestamp`, and `labels` and, for a member and up, `annotations`, when there are any), and for a member and up `spec` and `status`. Which of them an answer carries is decided by the caller's role on the Team:

  ```sh
  curl -s -H "Authorization: Bearer $(cadre auth token)" \
    "$CADRE_SERVER/api/v1/namespaces/demo/agents/writer"
  ```

  A viewer (an Agent that is not Ready, as the list shows):

  ```json
  {"name":"writer","ready":false,"reason":"ModelProviderNotFound","message":"ModelProvider claude does not exist","kind":"Agent","generation":1,"observedGeneration":1,"conditions":[{"type":"Ready","status":"False","observedGeneration":1,"lastTransitionTime":"2026-10-30T00:33:26Z","reason":"ModelProviderNotFound","message":"ModelProvider claude does not exist"}],"metadata":{"uid":"e0ca9698-dc16-45a5-9bef-291fad590b36","resourceVersion":"261","creationTimestamp":"2026-10-30T00:33:23Z","labels":{"tier":"gold"}}}
  ```

  A member gets the same, and `metadata.annotations`, `spec` and `status` besides:

  ```json
  {"…":"…","metadata":{"…":"…","labels":{"tier":"gold"},"annotations":{"owner":"platform"}},"spec":{"description":"writes things","modelRef":"claude","effort":"high","runtime":{"type":"builtin"}},"status":{"observedGeneration":1,"conditions":[{"type":"Ready","status":"False","…":"…"}]}}
  ```

  For an Agent with a job template, a member sees each container's and init container's env `value`, `command` and `args` as `(set)`: `"env":[{"name":"GH_TOKEN","value":"(set)"},{"name":"FROM_SECRET","valueFrom":{"secretKeyRef":{"name":"gh","key":"token"}}}]`, `"command":["(set)","(set)"]`. An admin gets the real values. Only those fields are masked: `envFrom`, probes' and lifecycle hooks' commands, and the rest of the spec are shown to a member as they are, so keep a credential out of them too. The failures are the API's usual: 401 `unauthenticated` without a token, 403 `no_role` for a caller with no role on the Team (a namespace without a Team included, for all but a bootstrap admin, who gets 404 `not_found`), and 404 `not_found` for an Agent that is not there.
- Upgrading to a cadre-server that serves it: nothing to apply. The route reads the `agents`, `modelproviders`, `toolservers`, `schedules`, `notifiers` and `runners` that cadre-server's ClusterRole already allows (`get`, `list`, `watch`), and adds no flag, value or CRD change. cadre-server starts the informers for those six kinds at boot, so a replica that does not lead answers too. What changes is who reads what: a member can now read the spec of every Agent, ModelProvider, ToolServer, Schedule and Notifier in the team's namespace, with the masking above. Before upgrading, move any credential an Agent's spec holds into a Secret, and refer to it with `valueFrom`; a ModelProvider's or ToolServer's key belongs in `auth.secretRef`, not in its URL, and a Notifier signs its deliveries with `webhook.secretRef`.

### Task Manager
- The only place that changes task state: claim, lease, finish, hand-off, wait, approve.
- Every transition is a single-object conditional write (see [execution.md](execution.md)).
- Side effects (dispatching a run, notifying) go through an outbox embedded in the task object and written in the same conditional write.
- Notifiers tell a webhook, or call a tool on a ToolServer through the same forwarder runs use, when a task needs a human or ends ([crds.md](crds.md#notifier)).
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
- UI events are watch events on Task resources and only say "something changed, refetch". `thread.revision` on the Task status makes thread appends visible through the same watch. No Redis, no message queue. The web UI reads again on the stream below and polls when it cannot have it. A browser profile holds 2 streams at most, opened by one of its tabs for all of them (a refused one does not count), so that over HTTP/1.1 a stream per tab does not take the 6 connections a browser keeps to a host (see [web-ui.md](web-ui.md#live-updates)).
- **The watch stream.** `GET /api/v1/namespaces/<namespace>/watch` is a server-sent event stream (`text/event-stream`, `Cache-Control: no-cache`, `X-Accel-Buffering: no`) of hints that the namespace's board changed, for a caller who may read it (`401` without a login, `403` without a role, as every route). A `HEAD` gets the same status and headers, `?task=` included, and returns at once, holding no stream. Its events carry JSON and no `id`, as they are never replayed:
  - `hello` first: `{"tasks":true,"config":true,"runs":false,"lifetimeSeconds":300}`, what the stream will carry. `tasks` is false on a store that gives no change notifications (both of today's drivers give them); `config` is true where cadre-server watches the configuration kinds (always, as it starts; false only in a test server without them), and a page that sees it false polls. `runs` is true when the stream names a task with `?task=<name>` and this replica can give it run hints: the store gives change notifications and the stream fits under the cap below. It does not wait for the task to have a run: a stream opened while the task is queued gets the hints of the run it starts. A `retry: 3000` comes with it.
  - `task` `{"name","number","phase","parent"}`: a Task changed in a way the board shows (its phase, priority, title, assignee, labels, waiting reason, number, parent, thread revision or run). A renewed lease is not a change. A hint carries no spec and no thread: read the task again. Changes to one task within 250 ms are one hint.
  - `config` `{"kind","name"}`: an Agent, ModelProvider, ToolServer, Schedule, Notifier, Runner or the Team of the namespace was applied, changed (spec, status or metadata) or deleted. Every replica sees it from the informers it already keeps, and a resync or relist that brings nothing new is no hint. AgentAccess is not hinted: only those who manage access may list it. The hint carries no spec and no status: read the resource again. Changes to one resource within 250 ms are one hint, and two resources of one kind each get theirs. What is already there when the stream opens is no hint (read it once, then follow), and neither is another namespace's change. To act on a hint, read the resource: `GET /api/v1/namespaces/<namespace>/agents/<name>` for an `Agent`, `.../modelproviders/<name>` for a `ModelProvider`, `.../toolservers/<name>` for a `ToolServer`, `.../schedules/<name>` for a `Schedule`, `.../notifiers/<name>` for a `Notifier`, `.../team` for the `Team` (its `name` is `team`); a resource that is gone answers 404. A Runner has no read route yet, so a page that shows one polls.
  - `run` `{"task","run","seq"}`, on a stream with `runs` true: events up to `seq` were appended to the task's current run. It carries no event: read the run's events again. A run's events are log appends, which no store notifies, so each replica lists the keys of the run's events every 500 ms, with one tailer per task and run however many streams watch it, so a hint comes within about a second of the append. When the task's run changes, the stream follows the new run; when the run ends, or the last stream of the task closes, the tailer stops. A store that gives no change notifications gives no run hints either, as the stream could not follow the run. Each replica gives run hints to at most 200 streams at once, whether their tasks have a run or not; one more gets `runs` false, and its page polls the run as it does without a stream. `?task=` naming no task of the namespace is a 404.
  - `resync` `{}`: this stream fell behind (256 hints waiting) and lost what it held: read the board again.
  - `: ping`, a comment, every 20 seconds. Each ping checks the caller's role again, from the board index; a role taken away ends the stream with `end` `{"reason":"no_role"}`, and the browser's reconnect is a 403.
  - `end` `{"reason"}` before the server closes the stream: `lifetime` after 300 seconds ±20 %, when the browser reconnects (so an expired session ends at that reconnect, a 401); `shutdown` when the replica stops; `no_role`.

  Hints are lossy, like the driver's: one may be late, merged or lost, a reconnect does not replay what was missed, and a page reads what it shows from the API. The stream is not bound by the 30-second request timeout; each write is given 30 seconds instead. Each replica serves at most `--watch-max-streams` streams at once (default 2000; the chart's `server.watchMaxStreams`), and answers one more with a 503, after which a client polls instead. A proxy in front must not buffer the stream: nginx honours `X-Accel-Buffering: no`. It should also let a connection sit idle for longer than the 20-second ping.

  To watch a namespace from a shell, with an ID token from `cadre auth token` (with logins; without them, `--auth=none`, send no header), `-N` so curl does not buffer:

  ```sh
  curl -N -H "Authorization: Bearer $(cadre auth token)" \
    "$CADRE_SERVER/api/v1/namespaces/demo/watch"
  ```

  It prints the stream as it comes, here after a member created the Task `release-notes` and an admin then changed the ModelProvider `claude` (the Agent `bot` and the Team would each give a `config` like it; an AgentAccess gives none):

  ```
  retry: 3000
  event: hello
  data: {"tasks":true,"config":true,"runs":false,"lifetimeSeconds":300}

  event: task
  data: {"name":"release-notes","number":1,"phase":"Todo"}

  event: config
  data: {"kind":"ModelProvider","name":"claude"}

  : ping

  ```

  Read the Task again with `GET /api/v1/namespaces/demo/tasks/release-notes`. Without a token the answer is `401` `unauthenticated`; with no role on the Team, `403` `no_role`; past the limit, `503` `unavailable` ("this replica serves 1 watch streams already: retry later; the board polls meanwhile", with `--watch-max-streams=1`). `--watch-max-streams=0`, or less, stops cadre-server at start: `--watch-max-streams 0: must be at least 1`.

  To follow one Task's run, name it with `?task=`:

  ```sh
  curl -N -H "Authorization: Bearer $(cadre auth token)" \
    "$CADRE_SERVER/api/v1/namespaces/demo/watch?task=release-notes"
  ```

  `hello` now says `"runs":true`, and a `run` comes each time events were appended to the Task's current run, here after the runner sent its first:

  ```
  retry: 3000
  event: hello
  data: {"tasks":true,"config":true,"runs":true,"lifetimeSeconds":300}

  event: run
  data: {"task":"release-notes","run":"run-1","seq":1}

  ```

  Read the run's events again with `GET /api/v1/namespaces/demo/tasks/release-notes/runs/run-1/events`. `seq` is the last event the hint covers: events appended close together come as one hint. A Task that does not exist is a `404` `not_found` ("not found"), for a `HEAD` too, after the role check. At 200 streams with `?task=` on a replica, the next one is still served, with `"runs":false` in its `hello` and no `run` events: it polls the run's events itself, and takes run hints again when it reconnects under the cap, as a closed stream frees its slot. Streams without `?task=` do not count towards the 200.
- Upgrading to a cadre-server that gives run hints: nothing to apply. There is no flag for the 200 streams (`MaxRunTails`) or the 500 ms interval, and no RBAC, CRD or chart change. A replica lists the keys of the run's events in the log store every 500 ms for each Task and run that a stream watches, so at the cap that is at most 200 lists every 500 ms against the store. The web UI's run in progress opens its stream with `?task=`; until the replica serves run hints, it polls the run's events.
- Upgrading to a cadre-server that serves the stream: nothing to apply. It adds one flag, `--watch-max-streams` (default 2000; the chart's `server.watchMaxStreams`, unset by default, and when set at least 1), and reads the Tasks the informers already hold: no RBAC or CRD change. A cadre-server that sends `config` hints (`"config":true` in `hello`) adds no flag, value, RBAC or CRD change either: it hangs a handler on the Agent, ModelProvider, ToolServer, Schedule, Notifier, Runner and Team informers it already keeps. A client that read `"config":false` from `hello` and polls can keep polling, or follow the hints. The web UI uses it once served: a page shows a change within a second instead of at its next poll, and polls as before where the stream is refused (a server without it answers `404`).

## Identity and access

Two layers, as in Argo CD:

- **Kubernetes RBAC** governs the GitOps path: who may apply `Agent`, `ToolServer`, `Task` resources.
- **Cadre's own SSO**: OIDC (or Dex), groups mapped to per-team roles, a policy file for board actions (who may approve, assign, or only view), and a local bootstrap admin. Board users such as reviewers do not need a kubeconfig.

### Authentication

People log in with any OIDC provider; there is no provider-specific code. This covers the web UI, the API and the CLI. The board MCP endpoint takes the same ID tokens, as a bearer token only. Agents outside Cadre that no one logs in for, a bot or a scheduled job, authenticate as an [`AgentAccess`](crds.md#agentaccess) on both, with a bearer token too. Runners (ServiceAccount tokens checked with TokenReview, or minted Runner tokens) and the per-run MCP endpoint authenticate their own way and are not affected.

- **Modes.** `--auth=oidc` requires a login for every `/api/v1` route except `GET /api/v1/auth`. `--auth=none` requires nothing: a request's author is whatever the client names, so anyone may act as anyone; cadre-server logs a warning, as for `--runner-auth=none`. The default is `oidc` in a cluster and `none` outside one.
- **Flags.**
  - `--oidc-issuer`: the provider's discovery document is read from it at start, and its keys are cached and refreshed. A provider that cannot be reached at start, or answers 5xx, does not stop cadre-server: discovery is retried in the background, with a pause growing to 30 seconds, each failure logged. Until it succeeds, logins and bearer tokens are answered 503 ("logins are not ready"); existing sessions keep working, and the pod stays ready, so runners are not affected. A misconfiguration (a 4xx, an issuer that does not match, a malformed document) stops cadre-server at start instead.
  - `--oidc-client-id` and `--oidc-client-secret-file`: the web UI's client, which is confidential.
  - `--oidc-cli-client-id`: a public client for `cadre login`, by default the same client. ID tokens for either client are accepted. Most providers will not let one client be both confidential (for the browser) and public (for a CLI without a secret), hence the second client.
  - `--oidc-scopes`: default `openid profile email groups`.
  - `--oidc-username-claim`: default `email`. A token without it is refused. With `email` as the username, a token whose `email_verified` is false is refused too: anyone could have registered an unverified address. A token without `email_verified` is accepted. The `sub` claim is kept too, as the stable key.
  - `--oidc-groups-claim`: default `groups`.
  - `--oidc-offline-access`: default true. Whether `cadre login` may ask for `offline_access`, and keep a refresh token, where the provider offers it; `GET /api/v1/auth` says so as `offlineAccess`. With `false` the CLI keeps ID tokens only, as before refresh tokens.
  - `--session-key-file`: 32 bytes, raw or 64 hex characters, required with `oidc`. Every replica must have the same key. Until the Helm chart makes it, create the Secret by hand: `kubectl -n cadre-system create secret generic cadre-session-key --from-literal=key=$(openssl rand -hex 32)`, and mount it.
  - `--external-url`: where browsers reach cadre-server, such as its ingress, by default `--public-url`. It must be an origin (scheme, host and port, no path), since the login and the UI are at the root (which is where runners reach it, often a Service inside the cluster). The callback is `<--external-url>/auth/callback`, cookies are `Secure` when it is https, and the origin check below compares against it.
- **Browser.**
  - `GET /auth/login?next=<path>` starts the authorization code flow with PKCE. `next` must be a path on this site, checked after cleaning it as the redirect will: a backslash, another host, a path under `/auth/`, or one longer than 2000 bytes becomes `/`. Behind an https external URL, a login started over plain http is refused with a page naming the https URL, since its cookies are `Secure` and the browser would drop them. The state, nonce, PKCE verifier and `next` are kept, encrypted, in a cookie for ten minutes.
  - `GET /auth/callback` checks the state, exchanges the code, and verifies the ID token: signature, issuer, audience, expiry and nonce.
  - It then sets the session cookie: the subject, username, display name (the `name` claim) and groups, encrypted and authenticated with the session key, so any replica reads it. It expires with the ID token, and after 12 hours at most. There is no refresh: an expired session logs in again.
  - The cookie is `HttpOnly`, `SameSite=Lax` and path `/`. Behind https it is also `Secure` and named with the `__Host-` prefix. A session that would not fit a browser cookie (about 4 KB, which a user in hundreds of groups can exceed) is refused with a message saying so, rather than silently dropped by the browser; have the provider send fewer groups.
  - Beside the session the callback sets `cadre-just-logged-in` for a minute, readable by the page. A page that gets a 401 while it is there knows the browser dropped the session (it blocks or cleared cookies). It shows that instead of sending the browser round the login again. A logout clears the marker too.
  - `POST /auth/logout` clears the cookie in this browser. It revokes nothing: a session cookie copied elsewhere, or a bearer token, stays valid until it expires (12 hours at most). The emergency lever is rotating the session key, which ends every session at once; bearer tokens then still last until their own expiry.
  - A change made with the cookie (any method but GET or HEAD) must carry an `Origin`, or failing that a `Referer`, of the public URL, or it is refused with 403.
- **CLI.**
  - `cadre login` asks `GET /api/v1/auth` for the issuer, client and scopes. It then runs the same code flow with PKCE in the browser, with the callback on a listener at `http://127.0.0.1:<port>/callback`: the loopback address itself, since `localhost` may resolve to another one. A request to it without this login's state is turned away and the wait goes on.
  - It keeps the ID token in `$XDG_CONFIG_HOME/cadre/credentials` (by default `~/.config`), mode 0600, by server URL. Where the server allows it (`--oidc-offline-access`) and the provider's discovery document lists `offline_access` in `scopes_supported`, the login asks for it too, and keeps the refresh token beside the ID token, with the issuer, client and subject it is for: a long-lived secret in that file. Elsewhere (Google, for one) no refresh token is asked for or kept.
  - Every request to that server then sends `Authorization: Bearer <ID token>`, verified like the browser's; bearer requests need no `Origin`. Once the token expires the CLI says so and asks for cadre login again.
  - With a refresh token, a command whose ID token expires within a minute refreshes it first, with no browser; `cadre auth token` too. The answer must carry an ID token for the same issuer, client and subject; it is kept, with the refresh token if the provider rotated it. A refresh the provider refuses (`invalid_grant`: a revoked or used refresh token), or one whose answer has no ID token, one that does not verify, or one for another user, drops the refresh token, and the CLI asks for cadre login again, saying the provider's reason. Any other failure (the provider unreachable, throttling, a proxy, a misconfigured client) keeps the refresh token: a command goes on with the ID token while it lasts and fails, saying what failed, once it has expired; `cadre auth token` still wants a minute. A refresh reads, refreshes and writes the login under a lock on the credentials file (`credentials.lock`, flock), and reads it again first: two cadre processes never spend one refresh token, and one logged out meanwhile stays logged out. The file is written whole to a temporary file of its own and renamed.
  - `cadre logout` forgets the login, and revokes its refresh token at the provider's revocation endpoint (RFC 7009) where discovery names one. Where it does not, forgetting it, or deleting the credentials file, is all the revoking there is: the provider's own session controls end it.
  - Against a server without logins, `cadre login` forgets any login kept for it.
  - `--as` is refused by a server with logins ("identity comes from login") and still names the author with `--auth=none`.
- **Agents.** With `--auth=oidc`, a bearer token on `/mcp` or the API is routed by its shape before anything is checked.
  - **An ID token.** A JWT whose unverified `iss` is the OIDC issuer (as the verifier counts it: Google's tokens may say `accounts.google.com` for `https://accounts.google.com`) is an ID token, verified as above and nothing else: one that fails is refused, never tried as another kind. So a person's ID token never reaches the API server in a TokenReview.
  - **A ServiceAccount token.** Any other JWT is checked with a TokenReview for the audience `cadre`, as a runner's is, the answer remembered for a minute. It acts as the AgentAccess holding the ServiceAccount: the oldest in the ServiceAccount's namespace naming it, then by name, decided from the specs. A ServiceAccount no AgentAccess names gets a 401: a Runner's and `--builtin-runner`'s are no exception, they just have no AgentAccess.
  - **A minted token.** A two-part token is an AgentAccess's minted token. The AgentAccess it names must have no `serviceAccount` and hold the token's hash, checked against the cache first, then read fresh. A miss or a mismatch in the cache is final, so a made-up token costs no read of the API server. A token that passes is confirmed by a fresh read on every request, never cached, so a deleted AgentAccess stops at once. A token of the same shape minted for a Runner of the same name is never taken for it.
  - **Anything else** is refused unread.
  - **Errors.** Every refusal is the same 401, and a refusal of a minted token is remembered for five seconds, by the token's hash, so random tokens cannot turn into a flood of reads. An API server that cannot be read is a 503, not a refusal, and is not remembered.
  - **Not Ready.** No request looks at an AgentAccess's Ready condition, which lags its spec; the spec and the token's hash decide. So deleting the AgentAccess revokes it at once (a ServiceAccount's once the cache has seen it), and deleting its Secret rotates the token within a minute; see [crds.md](crds.md#agentaccess).
  - **The issuer assumption.** The cluster's ServiceAccount issuer must not be the OIDC issuer. If `--oidc-issuer` pointed at the API server, ServiceAccount tokens would be verified as ID tokens, and refused.
  - **Identity.** The agent is `agent:<namespace>/<name>`: the author of what it writes (a `User` author of that name), and what `GET /api/v1/me` returns, with `agent: {namespace, name, role}`. No login can pass for one: an ID token whose username starts with `agent:`, in any case, is refused.
- **Identity in the API.** The author of a comment, reply or approval is the caller's username. The `author` fields of those requests count only with `--auth=none`. `GET /api/v1/me` returns the mode and the identity.

### Authorization

With logins, what a person may do on a team's board is decided by their role on the team. The matrix is fixed: there is no policy file. It is enforced in the API, so the UI and the CLI cannot get around it.

- **Roles** come from `Team.spec.roles` (see [crds.md](crds.md#team)): `viewer` reads, `member` also creates tasks, comments, replies, approves or denies tool calls and cancels, and `admin` also edits the Team through the API (`PUT /namespaces/<namespace>/team` with the `resourceVersion` it read: its display name, run cap, archiving and roles, replaced whole; `cadre team set`), as kubectl or GitOps may too; and manages its AgentAccesses through the API (`cadre agentaccess`), listing them included: the action is `manage agent access`, an admin's. An edit made from a version the Team has moved on from is refused (409 `conflict`). Where no bootstrap admin is set, an edit that would leave a Team that has an admin with none is refused, since nobody could give it one back. Users are matched by username (without case when it is an email address) and groups by the groups claim. The strongest matching role counts.
- **Freshness.** Every request is first decided on the Team in the board index, seconds behind the store at worst while its watch is healthy. A refusal there is final: a lagging index can grant too much, never too little. A write the index allows is then confirmed on the Team read from the store, so taking a role away refuses that person's next write at once, and only people the index lets write cost a store read.
- **Bootstrap admins**, `--admin-users` and `--admin-groups` (comma-separated; a group's name may hold spaces), are admin on every team, see every team, and may create teams: creating one (`PUT /api/v1/namespaces/<namespace>/team` where there is none yet) needs them, since nobody has a role on a team that does not exist. With neither flag set, only people named in a Team's roles can do anything, and nobody can create a team through the API; cadre-server says so in its log at start. The flags need `--auth=oidc`: cadre-server refuses to start with them and `--auth=none`.
- **403 and 404.** A request on a team the caller has no role on, or in a namespace without a Team, is refused with 403. The message names the role it needs and the caller's, so a stranger learns nothing about what exists. A refusal for the role has the error code `no_role`. A 404 means a missing object on a team the caller may see. `GET /api/v1/teams` lists only the teams the caller has a role on (every team for a bootstrap admin). It shows a team's `roles`, status, annotations and managed fields only to its admins, since `kubectl apply` keeps the roles in an annotation. `GET /api/v1/me` adds `roles`, the caller's role by namespace, and `admin` for a bootstrap admin.
- **The UI** leaves out what the role does not allow, says the role ("You are a viewer of team-a"), and shows each team's role in the picker. A `no_role` refusal of an action means the role changed since the page read it: the UI shows the server's message and reads `/me` again.
- **Agents** have their AgentAccess's role, `viewer` or `member`, on the Team of its namespace and on no other: anywhere else they have no role, as a stranger has none (403 `no_role`), and they can never edit a Team. The role is read from the AgentAccess's spec on each request. No entry of `Team.spec.roles`, not even one naming `agent:...`, and no bootstrap admin entry applies to them.
- **Without logins** (`--auth=none`) nothing is enforced: everyone is an admin of every team, and `/me` says `admin: true`. An AgentAccess grants nothing there: nobody authenticates. Nor are the AgentAccess routes served (404): anyone could make one, and it would be a live credential once logins are on.

To try roles by group with Dex, `hack/dex/groups.yaml` switches Dex to its mock connector. It logs in at once as `kilgore@kilgore.trout`, in the group `authors`.

**Developing with Dex.** `hack/dex/dex.yaml` runs Dex in a local cluster with one user (`dev@example.test`, password `password`), the web client `cadre` and the public client `cadre-cli`. To use it:

1. `kubectl apply -f hack/dex/dex.yaml`
2. `kubectl -n dex port-forward svc/dex 5556:5556`
3. Write a session key: `openssl rand -hex 32 > /tmp/cadre-session-key`.
4. Run cadre-server outside the cluster, at its default `http://localhost:8080`, with `--auth=oidc --oidc-issuer=http://localhost:5556/dex --oidc-client-id=cadre --oidc-cli-client-id=cadre-cli --oidc-client-secret-file=<a file holding cadre-dev-not-a-secret> --session-key-file=/tmp/cadre-session-key`.

The browser and cadre-server then both reach the issuer at the same address. Dex's static users have no groups.

## Installation

- **The Helm chart**, `charts/cadre`, installs Cadre into one namespace: `helm install cadre oci://ghcr.io/j75689/charts/cadre -n cadre-system --create-namespace`. Its [README](../charts/cadre/README.md) has the values and examples.
  - **What it installs.** cadre-server: a Deployment of two replicas with leader election in the release namespace, a Service, an optional Ingress, a ServiceAccount, and the ClusterRole of `config/rbac/role.yaml` bound to it. cadre-runner (on by default): a Deployment and a ServiceAccount whose token is projected for the audience `cadre`. The ClusterRoles of `config/rbac/workloads.yaml` and `config/runner/rbac.yaml`. The CRDs. With logins, the session key.
  - **The values that matter.** `auth.oidc.issuer` and `auth.oidc.clientId` (logins are on by default; `auth.mode: none` is for local development), `auth.oidc.clientSecret`, `auth.admins` (the only ones who may create teams), `storage.s3` (the bucket is required; keys from a Secret or the AWS default chain), and `ingress` (its host becomes the external URL logins return to; without an Ingress, logins need `server.externalURL`). `server.publicURL`, where runners reach cadre-server, defaults to the Service.
  - **The session key** is the Secret `<release>-session-key`, made on first install and kept across upgrades and uninstalls; `auth.sessionKey.existingSecret` names one of your own instead, which tools that render with `helm template` need, since the chart cannot see the existing key there.
  - **The CRDs** are in the chart's `crds/`, which Helm installs once and never upgrades: before upgrading, `kubectl apply -f charts/cadre/crds` from the new version. `make manifests` copies them there, with the RBAC files, and CI fails when the copies drift.
  - **Per-Team RBAC.** The workloads ClusterRole (runner Jobs, managed ToolServers) and the runner's (the builtin agents' model credentials) grant nothing until bound in a Team namespace, so cadre reaches only the namespaces given to it; the chart's README has the RoleBindings. `workloads.clusterWide: true` binds the first everywhere instead.
- **`install.yaml`**, attached to each GitHub release: the chart rendered with its CRDs, `auth.mode=none` and the bucket `CHANGE-ME`, behind a header saying so. It has no logins, so everyone is an admin: a way to try Cadre, not to run it. `make chart-dist RELEASE=vX.Y.Z` makes it, and the chart package, into `dist/`.
- **A `v*` tag publishes** a commit that is on main and passed main's CI there (the checks and the install in kind); nothing is run again: the three images; then the chart, at version `X.Y.Z` with `appVersion` the tag (so its images default to that tag), to `oci://ghcr.io/j75689/charts/cadre`; then the GitHub release, with generated notes, `install.yaml`, the CLI's archives and `cadre_X.Y.Z_checksums.txt`. The archives are `cadre_X.Y.Z_<os>_<arch>.tar.gz` for darwin and linux and `.zip` for windows, each on amd64 and arm64, holding the binary and `LICENSE`. `make cli-dist RELEASE=vX.Y.Z` builds them into `dist/`, without a VCS stamp, with `hack/archive`, which writes the same bytes on every host (sorted entries, owner 0:0, the commit's time), so a rebuild of a tag gives the same checksums. A release tag has no `+build` part, since GitHub renames a `+` in asset names.
- **The `cadre` CLI** is installed on a workstation in one of these ways:
  - `hack/install.sh`, piped from main (it is not attached to releases). It picks the archive for the OS and architecture, refuses one whose checksum does not match or is not listed, and installs into `CADRE_INSTALL_DIR`, else `/usr/local/bin` when it is writable, else `~/.local/bin`. It never runs sudo. `CADRE_VERSION` picks a release; the default is the latest, never a pre-release, found from GitHub's redirect rather than its rate-limited API.
  - Downloaded by hand from the release.
  - `go install github.com/j75689/cadre/cmd/cadre@<version>`.
  - `make install-cadre-cli` from a checkout.
  - The `ghcr.io/j75689/cadre` image.

  `cadre version` prints the release, the version set with `-ldflags` (`make install-cadre-cli` sets `git describe`), or, for a `go install`, the module version Go recorded.
- Images: `ghcr.io/j75689/cadre-server`, `ghcr.io/j75689/cadre-runner` and `ghcr.io/j75689/cadre` (the CLI), for linux/amd64 and linux/arm64, built from the one `Dockerfile` (`make image CMD=...` locally) on distroless/static as a non-root user. A `v*` tag publishes `vX.Y.Z`, `vX.Y` and `latest`; every push to main publishes `main` and `main-<short sha>`. Each binary prints its version with `--version`: `vX.Y.Z` for a release, `main-<short sha>` for a `main` image. The packages pushed by this repository's workflow have been public from their first push; if a pull is refused, set the package to public in its settings.
- **Checking an install end to end.** `make e2e` builds the images at the current commit and runs `hack/e2e/run.sh`. The script:
  - installs the chart in a kind cluster, with Dex (password grant on) and SeaweedFS;
  - checks that the admin, with an ID token and no browser, can create a Team and a task through the API, and that the task gets a number;
  - checks that the UI is served and that a request without a token is refused.
  - checks an agent outside Cadre end to end (`hack/e2e/agent.yaml`): a pod of AgentAccess `bot`'s ServiceAccount, with a projected token for the audience `cadre`, creates a task and comments through `/mcp`, the comment authored `agent:team-a/bot`, through a real TokenReview; a pod of Runner `ci`'s ServiceAccount, which no AgentAccess names, gets a 401.

  CI runs it on every pull request and on main. The `cadre` CLI's `login` needs a browser and is covered by its tests against a fake provider instead.
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
internal/storage/        driver interface, conformance/, kubernetes/, memory/; postgres/ planned
internal/runner/         runner protocol
internal/mcp/            per-run and board MCP endpoints
internal/controller/     Team, Task, Agent, ToolServer reconcilers
docs/                    architecture and design
```
