# Execution model

## Two time scales

| Layer | What it is | Time scale |
| --- | --- | --- |
| task | a work item on the board, mostly waiting for a human | days |
| run | one stretch of an agent actually working: an LLM turn, tool calls, another turn | seconds to minutes |

**A run is always short.** When the agent has to wait for a human (a question, an approval, a tool that must ask first) the run ends and the waiting state is recorded on the task. When the human acts, a new run picks up where the last one stopped. The execution layer never needs to "pause for days".

## State transitions

Every transition is one conditional write on the Task (`resourceVersion` in the kubernetes driver). Transitions that carry a run_id also check `status.run.id`.

| Transition | Triggered by | Condition |
| --- | --- | --- |
| create → `Todo` | human, GitOps, an outbox CreateSubtask | a number claimed from Team |
| `Todo` → `Running` | a claim | Team semaphore has room; a new run_id and lease are minted |
| lease renewal | heartbeat | run_id matches, otherwise 409 and the runner must stop |
| `Running` → `Waiting` | finish with `Waiting{reason}` | run_id matches; the run ends |
| `Waiting` → `Todo` | human reply, or a decision on a tool call | `waiting` cleared; a decision names the call's thread entry and is refused if the task now waits on another |
| `Running` → `Review` / `Done` / `Failed` | finish | run_id matches |
| `Running` → `Todo` | lease expiry reclaim | `leaseUntil` has passed; the run is marked interrupted |
| any → `Cancelled` | human | an in-flight run gets 409 on its next heartbeat |

## Two modes

### Step mode

Cadre drives the loop. The builtin agent loop is cut into steps, and each step is a message:

```
llm_turn → choice (did the output contain tool_calls?)
             ├─ yes → exec_tools → llm_turn
             └─ no  → finish
```

- A worker processes one step, commits the next step's message, then acks.
- Any worker can take the next step: a new machine, a scale-down or a redeploy never interrupts a run.
- Fair scheduling is per step, rotating between teams.
- Provider-side prompt caches are keyed on the prompt prefix, so switching workers does not invalidate them.

Granularity: one LLM turn is one step. One batch of tool calls is one step, executed concurrently inside it, which matches how providers return several tool calls that must all be answered in one message. An external side-effecting tool without an idempotency key gets a step of its own (see below).

### Runtime mode

The loop lives in someone else's process: an ACP agent such as Claude Code, a kagent agent, a custom container. Cadre cannot cut it into steps, so:

- The whole run is bound to one runner, managed by a lease and run_id fencing.
- Tool calls still go to Cadre's per-run MCP endpoint.
- How the runner works internally is not Cadre's business.

Step-mode workers only run inside the cluster; a process outside the cluster is always a runtime-mode runner. Kubernetes Job, Agent Sandbox and a user's own machine are all runtime mode; they differ only in who starts the runner process. The builtin loop first ships as a runtime-mode runner (`cadre-runner`) and becomes step mode later.

### The builtin loop in runtime mode

`cadre-runner` claims builtin agents' tasks and runs each as one conversation with the agent's model:

- **It starts from the task.** The system prompt is a fixed Cadre preamble followed by the Agent's `instructions`. The first message is the task (title, description, labels), its thread oldest first, read with `thread_read`, and its file list. Each entry is clipped at 8 KiB, and the whole message is kept to about a quarter of the model's context window (200 KiB when the window is unknown): when the thread does not fit, the newest entries are kept and a note says how many older ones `thread_read` still has. Nothing else carries over from earlier runs: the thread is the memory, and a reclaimed run starts again from it.
- **The conversation is append-only.** What the model returns goes back unchanged, thinking blocks included, and the results of one turn's tool calls go back together in one message. Tools are listed once per run and sorted by name, so the prompt cache holds.
- **Tool calls go through the run's MCP endpoint**, one at a time in the order the model made them. A result marked `_meta: {"cadre.dev/runEnded": true}` (from `thread_ask`, a call that must ask a human first, or any tool answering that the run is no longer active) means the run is over: the runner stops at once, makes no further call and does not finish.
- **How a run ends.** A reply with no tool call is the agent's summary: it is posted to the thread and the run finishes `Review`. Reaching the turn limit (100) or 80% of the model's context window also posts a comment and finishes `Review`, so a human decides whether to run again. A refusal, a reply cut off at the output limit, a model error that retrying cannot fix, or one that keeps failing for five minutes, posts a comment and finishes `Failed`. The comment says what failed in one sentence; the error itself, which can carry request URLs, ids and response bodies, goes to the runner's log. The last comment is retried like the finish.
- **It reports every model response** on `/runner/v1/events`: input, output and prompt-cache tokens, the context size, and which tools were called. Reporting is best effort.
- **A run whose MCP endpoint stops answering is left to its lease**, logged as an error: at connect, while reading the task (retried for five minutes first), or on a tool call. A tool call is cut after 105 seconds, a little over the server's bound, and is not retried, because it may have taken effect with only its answer lost. Such a run still holds a Team slot until the lease runs out, so the runner waits before claiming again, 5 seconds after the first and twice as long after each one in a row, up to 5 minutes; a failing claim backs off the same way. Once a run has started, the endpoint answering 404 means the run is over, and the runner stops without a finish.
- **It stops at once when its task is gone**: a heartbeat answered `run_mismatch` or `not_found`, or a lease that ran out without a renewal getting through, cancels the run without a finish.

**Models.** `cadre-runner` speaks to ModelProviders of type `anthropic`, on Anthropic's API or a gateway at its `baseURL`, and `openai`, on OpenAI's API or any server at its `baseURL` that speaks Chat Completions; `bedrock` and `vertex` come next, and until then their agents' runs fail with a comment saying so. On the Messages API it streams every turn and puts one prompt-cache breakpoint on the request. It reads the model from the Models API once per run: its context window, its output cap (`max_tokens`), and whether it takes adaptive thinking and the Agent's `effort`, which it then asks for. When the Models API cannot describe the model (a gateway without it), it asks for both on the first request and leaves out what that request is refused for, for the rest of the run. On Anthropic's own API (no `baseURL`) it also turns on server-side fallbacks, so a refused request is served again by another model; a refusal it still gets ends the run. The API key is the ModelProvider's Secret only: nothing in the runner's environment can stand in for it. A failed API call is logged by status, error type and message, never with the response body.

On Chat Completions it streams every turn with usage, and asks for the Agent's `effort` as `reasoning_effort`; a server that rejects either on the first request gets it again without it, for the rest of the run. Its credential is the Secret only, as for `anthropic`: none of the `OPENAI_*` environment variables is read. These endpoints do not say a model's context window, so the first message keeps to 200 KiB, there is no 80% ceiling, and a server's context-length error ends the run; the output cap is the server's. A tool result's failure is said in its text, as the API has no error flag for it. A call whose arguments are not JSON, or of a tool kind other than a function, is not made: the provider answers it with an error for the model. A reply cut off at its output limit makes none of its calls. The history keeps the standard Chat Completions fields only, so a server's extra fields on an assistant message, such as `reasoning_content`, are not sent back: in M1, models that require it back on tool-calling turns are not supported.

**Deploying it.** `cadre-runner` runs in the cluster under its own ServiceAccount, and needs `get` on Secrets in each Team namespace whose builtin agents it runs: `config/runner/` holds what to apply: `serviceaccount.yaml` (the ServiceAccount, in `cadre-system`), `rbac.yaml` (a ClusterRole for that and a RoleBinding to copy into each such namespace) and `deployment.yaml` (an example Deployment; there is no published image yet). It reads each Secret fresh for every run and caches none. It authenticates to cadre-server with a projected ServiceAccount token for the audience `cadre` (the example Deployment mounts one, valid for ten minutes), read from `--token-file` before every request, as the kubelet rotates it. Without `--token-file` it uses `/var/run/secrets/cadre/token` when that file exists, and otherwise sends no token, saying so once: that works only against `--runner-auth=none`. `cadre runner fake` takes the same flag. Flags: `--server` (cadre-server's URL), `--runner` (its name, by default the host name), `--namespaces` (comma-separated, by default every Team namespace) and `--max-turns`.

## Runner protocol

Pull based. A runner is any process that speaks it. In-cluster runners authenticate with a ServiceAccount token, which Cadre verifies with TokenReview; out-of-cluster runners use a `Runner` resource and a secret.

- **Every request carries `Authorization: Bearer <token>`.** A missing or invalid credential gets `401 unauthenticated`, always with the same message; a valid one acting on a run it does not hold gets `403 forbidden`.
- **In-cluster tokens** are projected ServiceAccount tokens issued for the audience `cadre`, not the API server's: a token leaked from Cadre cannot be replayed against the API server, nor the other way round. cadre-server checks them with a TokenReview for that audience, and remembers a valid answer for a minute, or until the token expires if sooner, and a refusal for five seconds, so random tokens cannot turn into a flood of reviews; at most 1024 tokens are remembered, and an API server that cannot answer is never. The run is held by the authenticated identity, recorded as `status.run.runner`; the runner's own name is kept as `status.run.runnerName`. Heartbeats, events and finishes from any other identity are refused with 403, even with the right run id, and so is a retried finish of an ended run. A runner refused with 403 or 401 stops the run at once, as on `run_mismatch`.
- **Runners.** Any other runner needs a `Runner` in each Team namespace it serves (see [crds.md](crds.md#runner)). An in-cluster one authenticates with its ServiceAccount's projected token; one outside the cluster sends the token Cadre minted for its Runner, which is checked against the Runner's status on a fresh read and never cached, so a rotated token stops at once. Either way the runner claims only in its Runners' namespaces, matched with their labels; the labels and namespaces it sends do not widen that.
- **`--runner-auth=none`** turns all of this off, for tests and local development only; cadre-server logs a warning when it runs so. A run claimed that way holds no identity (`status.run.runner` is empty) and is not fenced on its sender, so switching to `required` lets the runs in flight finish; their runners must then authenticate to claim again.

```
POST /runner/v1/claim      labels → one task, a run_id and an MCP token; claimed by conditional write; 204 when there is no work
POST /runner/v1/heartbeat  run_id → extends the lease; 409 on run_id mismatch, the runner must stop
POST /runner/v1/events     run_id, seq → usage, context size, progress (for the UI and hand-off decisions)
POST /runner/v1/finish     run_id, outcome: Done | Review | Waiting{reason} | Failed
```

Claiming matches a task's agent to the runner:

- The agent must exist and be Ready for its current spec. Otherwise its tasks are passed over until it is.
- A `builtin` agent is claimed by a runner sending the label `cadre.dev/runtime: builtin`. `cadre-runner` always sends it. Only `cadre-runner`'s identity, `--builtin-runner` on cadre-server (by default `system:serviceaccount:cadre-system:cadre-runner`), may assert labels under `cadre.dev/`; from any other runner they are dropped, so its claims never match a builtin agent. That identity may claim builtin agents in every Team namespace; a namespace whose Secrets it may not read shows up as a Failed run saying so. The Helm chart may later turn this into a `Runner` per namespace, like other runners.
- A `runner` agent is claimed by any runner whose labels include every pair of its selector.
- The `cadre.dev/` label prefix is reserved, so a selector cannot match the builtin label.
- The index only proposes candidates. The agent is read fresh for the decision.

The claim response describes the agent: its instructions, its ToolServers, and a model block (type, model, endpoint, cloud location). The model block names the Secret key holding the credential, never its value: **provider keys never travel over the runner protocol.** `cadre-runner` reads the Secret itself with its own ServiceAccount, so in M1 the builtin runner runs inside the cluster only. Other runners bring their own loop and credentials and ignore the model block.

## Per-run MCP endpoint

`/mcp/run`, with the run's token in `Authorization: Bearer <token>`, the standard MCP transport authentication. The token maps to a run_id. It is never part of a URL, so it cannot leak into ingress, mesh or client access logs, or into a redirect.

The token is minted when a run is claimed and handed to the runner in the claim response (`mcp: {url, token}`), once. Only its SHA-256 is stored, in the Task's `status.run`, written in the same conditional write as the claim. The token is bound to that one run and dies with it: when the run ends (finish, cancel, lease-expiry reclaim) `status.run` is cleared and the token stops working, and a reclaimed task gets a fresh token with its new run. The endpoint keeps no session; every request resolves the token on a fresh read of the task, so any replica can serve it.

Tool names follow the model APIs' rule, `^[a-zA-Z0-9_-]{1,64}$`, so a runner hands them to a model unchanged. Forwarded ToolServer tools are named `<toolserver>__<tool>`; ToolServer names cannot contain `_`, so they never collide with the core tools.

Core tools:

| Tool | Behaviour |
| --- | --- |
| `thread_post` | append the agent's message to the thread; returns its sequence number |
| `thread_read` | the thread, oldest first, at most 200 entries after a sequence number |
| `thread_ask` | append the agent's question and end the run as `Waiting{Question}`, in one step; the runner must stop without calling finish |
| `task_create_subtask` | record the subtask in the outbox (fenced on the run); the dispatcher creates it, unassigned unless an assignee is given; returns its name and number |
| `task_files_read` / `task_files_write` | the latest version of a task file, or a new version of it: UTF-8 text, at most 1 MiB, paths up to 255 bytes |
| `task_files_list` | the task's files with their latest version and size |

plus the ToolServers forwarded from the Agent's `toolRefs`. Every tool checks the run_id; a run that has lost its task is always rejected, with a tool error telling the agent to stop.

The agent's ToolServers are forwarded through the same endpoint:

- Each ToolServer in the Agent's `toolRefs` that exists, is Ready and has a `url` offers its tools as `<toolserver>__<tool>`, both names reduced to the model APIs' characters (a `.` in the ToolServer's name becomes `_`). A name over 64 characters, a tool whose input schema is not an object schema, and every tool among several reducing to one name are skipped and logged.
- The tool list is built from cached reads of the Agent and its ToolServers. Tool lists are discovered with an MCP client, concurrently, within 10 seconds in all, and cached per ToolServer for a minute (a new generation lists again); a failure to list is remembered for 15 seconds. A ToolServer that cannot be reached offers no tools, which is logged and never breaks the core tools.
- `permissions` decide what is offered: `allow` is forwarded, `deny` is neither offered nor callable, and `ask` is offered but must ask a human first (below).
- Each call is fenced on the run, then reads its ToolServer fresh: one that is gone, not Ready or no longer allows the tool gets no call. The call is made with the ToolServer's credential, read fresh from its Secret for every call (trailing newlines trimmed) and never cached or logged; it is sent only to the ToolServer's own host, and redirects are not followed. A call may take 60 seconds; results keep text and structured content up to 1 MiB, cut on a character boundary, and media is dropped with a note.

A call of an `ask` tool is not made by the run. Like any forwarded call it first reads its ToolServer fresh, and a tool denied since the list was built is refused without asking anyone, so the run goes on. Otherwise it appends a ToolCall entry to the thread, recording the tool and its arguments under a fresh call id, and ends the run as `Waiting{ToolApproval}` naming that entry, in one step like `thread_ask`; the runner must stop without calling finish. A human then decides, with `POST /api/v1/namespaces/{ns}/tasks/{name}/approve` (`{author, approve, note}`) or `cadre task approve|deny`; a reply does not end this wait.

- **Approve** appends an Approved entry, makes the call Cadre-side on the ToolServer read fresh (one that is gone, not Ready or now denies the tool gets no call, and the result says so), appends a ToolResult entry with what the call returned, and sends the task back to `Todo`. The next run reads the result in the thread.
- **Deny** appends a Denied entry and sends the task back to `Todo`.
- **The earliest decision wins.** Decisions are create-only appends to the thread, so two humans deciding at once cannot both win: a request makes the call only when the winning Approved entry is its own. A request losing to the other decision gets 409 `invalid_transition`; one losing to the same decision just ends the wait.
- **An approved call is never repeated.** If the approver dies after its Approved entry but before the ToolResult, the call may or may not have happened. Approving again within two minutes of the Approved entry (the 60-second call bound plus a margin) gets 409 `in_progress`, saying when to try again; after that it records a ToolResult saying the outcome is unknown, without making the call, and ends the wait. The agent may ask again, which needs a fresh approval.
- Once the call is made, its result and the end of the wait are written even if the human's request goes away. An approval may take up to 90 seconds, like a request to the per-run endpoint.

What fencing guarantees for each tool in runtime mode, in M1:

| Tool | Guarantee |
| --- | --- |
| `task_create_subtask` | fully fenced: the subtask enters the outbox in a conditional write on the Task that checks the run |
| `thread_ask`, an `ask` tool | the run ends through Finish, which is fenced; the Question or ToolCall is appended just before it |
| `thread_post`, `thread_ask`'s Question, an `ask` tool's ToolCall, `task_files_write` | checked, then written: a fresh read must show the run still holding the task, then the log entry is appended. A run that loses its task in the moment between the two can still land that one entry. |

A two-phase write would close that window: the content written first, then a pointer to it committed in a run-fenced write. It is planned for M2.

Task files are stored as one log stream of versions per path, `<ns>/<task>/files/<base64url(path)>`, plus an index stream, `<ns>/<task>/files-index`, recording each write's path, version and size. A write appends the version first and the index entry second. Reads use the version streams, so a write whose index entry was lost is still read, and the path is indexed again by its next write.

## Correctness: three layers

A queue's ack only guarantees at-least-once delivery, so a step may execute twice. Three layers deal with that:

| Layer | Solves | Guaranteed? |
| --- | --- | --- |
| lock | reduces duplicate **execution** | no: locks expire, processes stall |
| fencing | our own state **commits** exactly once | yes |
| idempotency | external side effects **take effect** once | depends on the other side |

### Fencing
- Each run has a small record: `run_id` and the current `step`.
- Finishing step N advances with a conditional write: `WHERE run_id = $r AND step = N`. If the write fails someone else already advanced, and this result is dropped.
- **A lock without fencing is unsafe**: a stalled worker whose lock has expired wakes up believing it still holds the lock and writes anyway.
- Locks can therefore be simple and occasionally wrong; fencing is the guard.

### Internal side effects: exactly once
Creating a subtask, posting to the thread, writing a task file: these only write Cadre's own storage. The step commit writes the *intent* into the Task's embedded outbox in the **same conditional write** as `step + 1`. If fencing succeeds both land; if it fails both are dropped. A dispatcher then applies each outbox item with its pre-minted ULID (create-if-not-exists), so re-applying leaves no trace. "Exactly once" is thus an idempotent apply keyed on `run_id + step + item`. A subtask appears a beat after the step that created it; that is accepted.

### External side effects: at least once, plus idempotency
An external call cannot share our write, so the receiving side has to deduplicate:
- **It supports an idempotency key**: send `run_id + step + tool_call_id`.
- **It does not**: make the call its own step, in three parts:
  1. commit "about to call X";
  2. make the call;
  3. commit "result of X".

  On retry, "about to call X" without a result means the call **may already have happened**. Do not repeat it blindly: ask the other side first (does the post exist?), and if that is impossible mark the result unknown and hand it to a human.
- **The most dangerous side effects do not happen inside a run at all**: an agent only drafts a deliverable; after human approval a separate publisher claims and publishes it. A re-delivered run can at worst produce a duplicate draft, never a duplicate publication. (Publishers are plugins; see [architecture.md](architecture.md#plugins).)

## Context attach

Conversation history grows (a million-token context is several megabytes) and queue messages have size limits. So:
- A message carries only `run_id`, `step` and pointers.
- Conversation history and large tool results live in the storage driver's log or blob store and are attached on read.

## Timeouts

- Every step runs under a context with a timeout; nothing hangs forever.
- While a step runs, the ack deadline is extended up to "the step's timeout plus a margin", so the queue never re-delivers a step that is still running.
- A timed-out step counts as failed; the retry policy decides whether to retry the step or fail the run.
- Very long tools (video rendering, say) become asynchronous: submitting ends the step, and the completion callback produces the next step's message.

## Forward recovery

This is a forward-recovery saga: a chain of individually committed small steps, retried until they succeed, never rolled back. It holds because every step eventually succeeds (LLM calls and writes do). Real compensation, where a step can never succeed after earlier external side effects, is rare; add a cleanup action to the individual tool when it comes up.
