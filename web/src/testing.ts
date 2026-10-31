import type { AgentAccess, Phase, Task, Team, ThreadEntry } from "./api";

// Fixtures for the UI's tests.

export function task(phase: Phase, extra: Partial<NonNullable<Task["status"]>> = {}, spec: Partial<Task["spec"]> = {}): Task {
  return {
    metadata: { name: "x", namespace: "a" },
    spec: { title: "Task x", ...spec },
    status: { phase, ...extra },
  };
}

export function entry(seq: number, kind: string, extra: Partial<ThreadEntry> = {}): ThreadEntry {
  return { seq, kind, author: { kind: "User", name: "dana" }, body: `${kind} ${seq}`, at: "2026-10-25T09:00:00Z", ...extra };
}

export function team(extra: Partial<Team> = {}): Team {
  return { metadata: { name: "team", namespace: "a", resourceVersion: "7" }, spec: { displayName: "Team A" }, ...extra };
}

export function access(extra: Partial<AgentAccess> = {}): AgentAccess {
  return { name: "bot", resourceVersion: "3", spec: { role: "member" }, ready: true, ...extra };
}

export const json = (body: unknown, status = 200) => Promise.resolve(new Response(JSON.stringify(body), { status }));

// FakeEventSource stands in for the browser's: a test opens, fails and
// feeds the streams the UI opened, kept in instances. A closed one sends
// nothing more, as the browser's.
export class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  static instances: FakeEventSource[] = [];
  readyState = FakeEventSource.CONNECTING;
  constructor(readonly url: string) {
    super();
    FakeEventSource.instances.push(this);
  }
  // emit sends an event with data as its JSON.
  emit(event: string, data: unknown = {}) {
    if (this.closed) return;
    this.readyState = FakeEventSource.OPEN;
    this.dispatchEvent(new MessageEvent(event, { data: JSON.stringify(data) }));
  }
  // fail is a dropped connection that EventSource retries, or with refused
  // a refused open (401, 403, 404, 503) that closes it.
  fail(refused = false) {
    if (this.closed) return;
    this.readyState = refused ? FakeEventSource.CLOSED : FakeEventSource.CONNECTING;
    this.dispatchEvent(new Event("error"));
  }
  close() {
    this.readyState = FakeEventSource.CLOSED;
  }
  get closed() {
    return this.readyState === FakeEventSource.CLOSED;
  }
  // open is the streams not closed.
  static open() {
    return FakeEventSource.instances.filter((s) => !s.closed);
  }
}
