// The parts of cadre-server's /api/v1 the board reads. Same origin: in
// development the Vite dev server proxies /api to a local cadre-server.

export const phases = ["Todo", "Running", "Waiting", "Review", "Done", "Cancelled", "Failed"] as const;
export type Phase = (typeof phases)[number];

export interface Assignee {
  kind: "Agent" | "User";
  name: string;
}

export interface TaskSpec {
  title: string;
  description?: string;
  assignee?: Assignee;
  priority?: string;
  parentRef?: { name: string };
  labels?: string[];
}

export const terminalPhases: readonly Phase[] = ["Done", "Cancelled", "Failed"];

export interface Condition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  lastTransitionTime?: string;
  observedGeneration?: number;
}

export interface TaskStatus {
  number?: number;
  phase?: Phase;
  waiting?: { reason: string; since?: string; threadSeq?: number; itemId?: string };
  run?: { id: string; mode?: string; runnerName?: string; step?: number; attempt?: number; leaseUntil?: string };
  runs?: { count?: number; lastId?: string };
  conditions?: Condition[];
  [field: string]: unknown;
}

export interface Task {
  metadata: { name: string; namespace: string; creationTimestamp?: string; labels?: Record<string, string> };
  spec: TaskSpec;
  status?: TaskStatus;
}

export interface TaskList {
  items: Task[];
  offset: number;
  // The limit that applied: the server caps what it is asked for.
  limit: number;
}

export interface ObjectMeta {
  name: string;
  namespace: string;
  resourceVersion?: string;
  annotations?: Record<string, string>;
  labels?: Record<string, string>;
  managedFields?: { manager?: string; operation?: string }[];
}

export type TeamRoleName = "admin" | "member" | "viewer";

export interface RoleSubject {
  kind: "user" | "group";
  name: string;
}

export interface TeamRole {
  role: TeamRoleName;
  subjects: RoleSubject[];
}

export interface TeamSpec {
  displayName?: string;
  maxConcurrentRuns?: number;
  // archiveAfter is a Go duration, such as "168h0m0s".
  archiveAfter?: string;
  roles?: TeamRole[];
}

// Slot is a run holding one of a Team's maxConcurrentRuns.
export interface Slot {
  runId: string;
  task: string;
  acquiredAt: string;
}

// Team is a namespace's Team. Only an admin of it is shown its roles,
// status, annotations and managedFields.
export interface Team {
  metadata: ObjectMeta;
  spec?: TeamSpec;
  status?: { running?: number; slots?: Slot[]; nextNumber?: number };
}

// An agent acts with at most member: it can never manage the Team.
export type AgentRole = "member" | "viewer";

export interface AgentAccessSpec {
  role: AgentRole;
  serviceAccount?: { name: string };
}

// AgentAccess is an AgentAccess as the API shows it. Its token is never
// shown: tokenSecret names the Secret it is kept in.
export interface AgentAccess {
  name: string;
  resourceVersion: string;
  spec: AgentAccessSpec;
  ready: boolean;
  reason?: string;
  message?: string;
  tokenSecret?: string;
  mintedAt?: string;
}

// AgentAccessToken is a token just minted: shown in this answer only.
export interface AgentAccessToken {
  token: string;
  tokenSecret: string;
}

export interface ToolCall {
  id: string;
  name?: string;
  toolServer?: string;
  tool?: string;
  arguments?: unknown;
  isError?: boolean;
}

// ThreadEntry is one entry of a task's thread, as the manager writes it.
export interface ThreadEntry {
  seq: number;
  author: { kind: string; name: string };
  kind: "Comment" | "Question" | "Answer" | "ToolCall" | "Approved" | "Denied" | "ToolResult" | string;
  body: string;
  at: string;
  runId?: string;
  call?: ToolCall;
}

export interface RunRecord {
  id: string;
  mode: string;
  runner?: string;
  runnerName?: string;
  step?: number;
  attempt?: number;
  reason: string;
  outcome?: string;
  endedAt: string;
}

export interface RunEvent {
  runId: string;
  seq: number;
  progress?: string;
  inputTokens?: number;
  outputTokens?: number;
  // Prompt-cache tokens, apart from inputTokens.
  cacheReadInputTokens?: number;
  cacheCreationInputTokens?: number;
  contextTokens?: number;
}

export interface AgentSummary {
  name: string;
  ready: boolean;
  reason?: string;
  message?: string;
}

// ConfigResource is a configuration resource, such as an Agent, as the
// caller may see it: a viewer gets no spec, status or annotations, a member
// gets some of the spec's values as "(set)", an admin all of it.
export interface ConfigResource extends AgentSummary {
  kind: string;
  generation: number;
  observedGeneration: number;
  conditions: Condition[];
  metadata: {
    uid: string;
    resourceVersion: string;
    creationTimestamp: string;
    labels?: Record<string, string>;
    annotations?: Record<string, string>;
  };
  spec?: Record<string, unknown>;
  status?: Record<string, unknown>;
}

export class APIError extends Error {
  constructor(
    readonly status: number,
    message: string,
    // code is the server's error code, such as "no_role".
    readonly code = "",
  ) {
    super(message);
  }
}

// loginURL is where a request found unauthenticated sends the browser, to
// come back to the page it was on.
export function loginURL(): string {
  return `/auth/login?next=${encodeURIComponent(location.pathname + location.search)}`;
}

// auth sends the browser to log in; tests replace it. justLoggedIn reads
// the marker the login's callback leaves for a minute: a 401 then means the
// browser did not keep the session cookie, and logging in again would only
// go round in a circle.
export const auth = {
  toLogin: () => location.assign(loginURL()),
  justLoggedIn: () => document.cookie.split(";").some((c) => c.trim().startsWith("cadre-just-logged-in=")),
};

export const sessionDropped =
  "You logged in, but this browser did not keep the session: it blocks or cleared cookies for this site. Allow them and reload.";

async function request<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  const init: RequestInit = { method, headers: { Accept: "application/json" }, signal };
  if (body !== undefined) {
    init.body = JSON.stringify(body);
    init.headers = { ...init.headers, "Content-Type": "application/json" };
  }
  const res = await fetch("/api/v1" + path, init);
  if (res.status === 401) {
    if (auth.justLoggedIn()) throw new APIError(401, sessionDropped);
    auth.toLogin();
  }
  if (!res.ok) {
    let message = res.statusText;
    let code = "";
    try {
      const body = await res.json();
      message = body?.error?.message ?? message;
      code = body?.error?.code ?? "";
    } catch {
      // not JSON: keep the status text
    }
    throw new APIError(res.status, message, code);
  }
  // A DELETE answers 204, with no body to read.
  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}

const get = <T,>(path: string, signal?: AbortSignal) => request<T>("GET", path, undefined, signal);
const post = <T,>(path: string, body: unknown) => request<T>("POST", path, body);
const put = <T,>(path: string, body: unknown) => request<T>("PUT", path, body);
const del = (path: string) => request<void>("DELETE", path);

// Me is who the server takes this browser for: with a login, the user; with
// --auth=none, nobody, and the UI asks for a name instead.
export interface Me {
  mode: "oidc" | "none";
  username?: string;
  name?: string;
  groups?: string[];
  // roles is the caller's role on each team they may see, by namespace;
  // admin says they are a bootstrap admin.
  roles?: Record<string, string>;
  admin?: boolean;
}

export const getMe = (signal?: AbortSignal) => get<Me>("/me", signal);

export async function logout(): Promise<void> {
  await fetch("/auth/logout", { method: "POST" });
}

export const listTeams = (signal?: AbortSignal) => get<{ items: Team[] }>("/teams", signal);

export function listTasks(ns: string, phase: Phase, offset: number, limit: number, signal?: AbortSignal) {
  const q = new URLSearchParams({ phase, offset: String(offset), limit: String(limit) });
  return get<TaskList>(`/namespaces/${encodeURIComponent(ns)}/tasks?${q}`, signal);
}

// listSubtasks reads a page of parent's subtasks, in every phase.
export function listSubtasks(ns: string, parent: string, offset: number, limit: number, signal?: AbortSignal) {
  const q = new URLSearchParams({ parent, offset: String(offset), limit: String(limit) });
  return get<TaskList>(`/namespaces/${encodeURIComponent(ns)}/tasks?${q}`, signal);
}

export const getTask = (ns: string, name: string, signal?: AbortSignal) =>
  get<Task>(`/namespaces/${encodeURIComponent(ns)}/tasks/${encodeURIComponent(name)}`, signal);

const taskPath = (ns: string, name: string) => `/namespaces/${encodeURIComponent(ns)}/tasks/${encodeURIComponent(name)}`;

// readThread reads the entries after sequence after, at most limit.
export function readThread(ns: string, name: string, after: number, limit: number, signal?: AbortSignal) {
  const q = new URLSearchParams({ after: String(after), limit: String(limit) });
  return get<{ items: ThreadEntry[] }>(`${taskPath(ns, name)}/thread?${q}`, signal);
}

// An action names its author only without a login: with one the server
// knows who acts, and "" leaves the field out.
export const comment = (ns: string, name: string, author: string, body: string) =>
  post<ThreadEntry>(`${taskPath(ns, name)}/comments`, { author: author || undefined, body });

export const reply = (ns: string, name: string, author: string, body: string) =>
  post<{ task: Task; entry: ThreadEntry }>(`${taskPath(ns, name)}/reply`, { author: author || undefined, body });

export const approve = (ns: string, name: string, author: string, ok: boolean, note: string) =>
  post<{ task: Task; decision: ThreadEntry; result?: ThreadEntry }>(`${taskPath(ns, name)}/approve`, { author: author || undefined, approve: ok, note: note || undefined });

export const cancel = (ns: string, name: string) => post<Task>(`${taskPath(ns, name)}/cancel`, {});

export const listRuns = (ns: string, name: string, signal?: AbortSignal) => get<{ items: RunRecord[] }>(`${taskPath(ns, name)}/runs`, signal);

export const runEvents = (ns: string, name: string, run: string, signal?: AbortSignal) =>
  get<{ items: RunEvent[] }>(`${taskPath(ns, name)}/runs/${encodeURIComponent(run)}/events`, signal);

export const listAgents = (ns: string, signal?: AbortSignal) =>
  get<{ items: AgentSummary[] }>(`/namespaces/${encodeURIComponent(ns)}/agents`, signal);

export const getAgent = (ns: string, name: string, signal?: AbortSignal) =>
  get<ConfigResource>(`/namespaces/${encodeURIComponent(ns)}/agents/${encodeURIComponent(name)}`, signal);

export const listModelProviders = (ns: string, signal?: AbortSignal) =>
  get<{ items: AgentSummary[] }>(`/namespaces/${encodeURIComponent(ns)}/modelproviders`, signal);

export const getModelProvider = (ns: string, name: string, signal?: AbortSignal) =>
  get<ConfigResource>(`/namespaces/${encodeURIComponent(ns)}/modelproviders/${encodeURIComponent(name)}`, signal);

export const listToolServers = (ns: string, signal?: AbortSignal) =>
  get<{ items: AgentSummary[] }>(`/namespaces/${encodeURIComponent(ns)}/toolservers`, signal);

export const getToolServer = (ns: string, name: string, signal?: AbortSignal) =>
  get<ConfigResource>(`/namespaces/${encodeURIComponent(ns)}/toolservers/${encodeURIComponent(name)}`, signal);

export const listSchedules = (ns: string, signal?: AbortSignal) =>
  get<{ items: AgentSummary[] }>(`/namespaces/${encodeURIComponent(ns)}/schedules`, signal);

export const getSchedule = (ns: string, name: string, signal?: AbortSignal) =>
  get<ConfigResource>(`/namespaces/${encodeURIComponent(ns)}/schedules/${encodeURIComponent(name)}`, signal);

export const listNotifiers = (ns: string, signal?: AbortSignal) =>
  get<{ items: AgentSummary[] }>(`/namespaces/${encodeURIComponent(ns)}/notifiers`, signal);

export const getNotifier = (ns: string, name: string, signal?: AbortSignal) =>
  get<ConfigResource>(`/namespaces/${encodeURIComponent(ns)}/notifiers/${encodeURIComponent(name)}`, signal);

export const getTeam = (ns: string, signal?: AbortSignal) => get<Team>(`/namespaces/${encodeURIComponent(ns)}/team`, signal);

// putTeam replaces a Team's spec, made from the Team at resourceVersion;
// without one it creates the Team. The spec's fields go beside the version,
// not under a "spec" key.
export const putTeam = (ns: string, spec: TeamSpec, resourceVersion?: string) =>
  put<Team>(`/namespaces/${encodeURIComponent(ns)}/team`, { ...spec, resourceVersion });

const accessPath = (ns: string, name: string) =>
  `/namespaces/${encodeURIComponent(ns)}/agentaccesses/${encodeURIComponent(name)}`;

export const listAgentAccesses = (ns: string, signal?: AbortSignal) =>
  get<{ items: AgentAccess[] }>(`/namespaces/${encodeURIComponent(ns)}/agentaccesses`, signal);

export const getAgentAccess = (ns: string, name: string, signal?: AbortSignal) => get<AgentAccess>(accessPath(ns, name), signal);

// putAgentAccess replaces an AgentAccess's spec, made from the one at
// resourceVersion; without one it creates the AgentAccess.
export const putAgentAccess = (ns: string, name: string, spec: AgentAccessSpec, resourceVersion?: string) =>
  put<AgentAccess>(accessPath(ns, name), { spec, resourceVersion });

export const deleteAgentAccess = (ns: string, name: string) => del(accessPath(ns, name));

// rotateAgentAccessToken mints a new token, which stops the last at once.
export const rotateAgentAccessToken = (ns: string, name: string) =>
  post<AgentAccessToken>(`${accessPath(ns, name)}/token`, {});

export const createTask = (ns: string, spec: TaskSpec) =>
  post<Task>(`/namespaces/${encodeURIComponent(ns)}/tasks`, { spec });

// ulidTime is when a ULID was minted: a run's id says when it started.
export function ulidTime(id: string): Date | undefined {
  const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";
  if (id.length !== 26) return undefined;
  let ms = 0;
  for (const c of id.slice(0, 10).toUpperCase()) {
    const v = alphabet.indexOf(c);
    if (v < 0) return undefined;
    ms = ms * 32 + v;
  }
  return new Date(ms);
}
