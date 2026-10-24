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
}

export interface TaskStatus {
  number?: number;
  phase?: Phase;
  waiting?: { reason: string; since?: string; threadSeq?: number };
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

export interface Team {
  metadata: { name: string; namespace: string };
  spec?: { displayName?: string };
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
  contextTokens?: number;
}

export interface AgentSummary {
  name: string;
  ready: boolean;
  reason?: string;
  message?: string;
}

export class APIError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

// loginURL is where a request found unauthenticated sends the browser, to
// come back to the page it was on.
export function loginURL(): string {
  return `/auth/login?next=${encodeURIComponent(location.pathname + location.search)}`;
}

// toLogin sends the browser to log in; tests replace it.
export const auth = {
  toLogin: () => location.assign(loginURL()),
};

async function request<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  const init: RequestInit = { method, headers: { Accept: "application/json" }, signal };
  if (body !== undefined) {
    init.body = JSON.stringify(body);
    init.headers = { ...init.headers, "Content-Type": "application/json" };
  }
  const res = await fetch("/api/v1" + path, init);
  if (res.status === 401) auth.toLogin();
  if (!res.ok) {
    let message = res.statusText;
    try {
      const body = await res.json();
      message = body?.error?.message ?? message;
    } catch {
      // not JSON: keep the status text
    }
    throw new APIError(res.status, message);
  }
  return res.json() as Promise<T>;
}

const get = <T,>(path: string, signal?: AbortSignal) => request<T>("GET", path, undefined, signal);
const post = <T,>(path: string, body: unknown) => request<T>("POST", path, body);

// Me is who the server takes this browser for: with a login, the user; with
// --auth=none, nobody, and the UI asks for a name instead.
export interface Me {
  mode: "oidc" | "none";
  username?: string;
  name?: string;
  groups?: string[];
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

export const runEvents = (ns: string, name: string, run: string) =>
  get<{ items: RunEvent[] }>(`${taskPath(ns, name)}/runs/${encodeURIComponent(run)}/events`);

export const listAgents = (ns: string, signal?: AbortSignal) =>
  get<{ items: AgentSummary[] }>(`/namespaces/${encodeURIComponent(ns)}/agents`, signal);

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
