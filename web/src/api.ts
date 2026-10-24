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

export interface TaskStatus {
  number?: number;
  phase?: Phase;
  waiting?: { reason: string; since?: string };
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

export class APIError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

async function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch("/api/v1" + path, { headers: { Accept: "application/json" }, signal });
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

export const listTeams = (signal?: AbortSignal) => get<{ items: Team[] }>("/teams", signal);

export function listTasks(ns: string, phase: Phase, offset: number, limit: number, signal?: AbortSignal) {
  const q = new URLSearchParams({ phase, offset: String(offset), limit: String(limit) });
  return get<TaskList>(`/namespaces/${encodeURIComponent(ns)}/tasks?${q}`, signal);
}

export const getTask = (ns: string, name: string, signal?: AbortSignal) =>
  get<Task>(`/namespaces/${encodeURIComponent(ns)}/tasks/${encodeURIComponent(name)}`, signal);
