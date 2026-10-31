import { vi } from "vitest";
import type { AgentAccess, Phase, Task, Team, ThreadEntry } from "./api";
import { restart } from "./live";

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

// FakeLocks stands in for navigator.locks: a lock goes to the next request
// once the promise of the one holding it settles.
export class FakeLocks {
  private last = new Map<string, Promise<unknown>>();
  request(name: string, granted: () => Promise<unknown>) {
    const held = (this.last.get(name) ?? Promise.resolve()).then(granted);
    this.last.set(name, held);
    return held;
  }
}

// FakeBroadcastChannel stands in for the browser's between the tabs of a
// test: a message goes to every other open channel of its name, a moment
// later.
export class FakeBroadcastChannel {
  static all: FakeBroadcastChannel[] = [];
  onmessage: ((e: MessageEvent) => void) | null = null;
  constructor(readonly name: string) {
    FakeBroadcastChannel.all.push(this);
  }
  postMessage(data: unknown) {
    const copy = structuredClone(data);
    for (const c of FakeBroadcastChannel.all) {
      if (c === this || c.name !== this.name) continue;
      queueMicrotask(() => FakeBroadcastChannel.all.includes(c) && c.onmessage?.(new MessageEvent("message", { data: copy })));
    }
  }
  close() {
    FakeBroadcastChannel.all = FakeBroadcastChannel.all.filter((c) => c !== this);
  }
}

// fakeTabs gives the tabs of a test fakes of EventSource, Web Locks and
// BroadcastChannel (none for those set false), and makes this tab a new
// one.
export function fakeTabs({ locks = true, channel = true } = {}) {
  FakeEventSource.instances = [];
  FakeBroadcastChannel.all = [];
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal("BroadcastChannel", channel ? FakeBroadcastChannel : undefined);
  Object.defineProperty(navigator, "locks", { value: locks ? new FakeLocks() : undefined, configurable: true });
  restart();
}
