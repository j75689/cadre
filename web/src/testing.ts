import type { Phase, Task, ThreadEntry } from "./api";

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

export const json = (body: unknown, status = 200) => Promise.resolve(new Response(JSON.stringify(body), { status }));
