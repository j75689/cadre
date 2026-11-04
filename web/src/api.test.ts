import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { APIError, deleteAgentAccess, loginURL, putAgentAccess, putTeam, readTimedOut, readTimeoutMs, rotateAgentAccessToken, runEvents } from "./api";
import { access, json, team } from "./testing";

describe("loginURL", () => {
  it("comes back to the page it left", () => {
    history.replaceState(null, "", "/t/team-a/tasks/x?tab=runs");
    expect(loginURL()).toBe("/auth/login?next=%2Ft%2Fteam-a%2Ftasks%2Fx%3Ftab%3Druns");
  });
});

// sent stubs fetch to answer reply, and returns the requests it was sent.
function sent(reply: () => Promise<Response>) {
  const calls: { url: string; init?: RequestInit }[] = [];
  vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    return reply();
  });
  return calls;
}

const bodyOf = (init?: RequestInit) => JSON.parse(String(init?.body));

describe("the Team and its AgentAccesses", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("resolves a 204 DELETE to undefined", async () => {
    const calls = sent(() => Promise.resolve(new Response(null, { status: 204 })));
    await expect(deleteAgentAccess("a", "bot")).resolves.toBeUndefined();
    expect(calls[0].url).toBe("/api/v1/namespaces/a/agentaccesses/bot");
    expect(calls[0].init?.method).toBe("DELETE");
  });

  it("sends a Team's version beside its spec's fields", async () => {
    const calls = sent(() => json(team()));
    await putTeam("a", { displayName: "Team A", maxConcurrentRuns: 2 }, "7");
    expect(calls[0].url).toBe("/api/v1/namespaces/a/team");
    expect(calls[0].init?.method).toBe("PUT");
    expect(bodyOf(calls[0].init)).toEqual({ displayName: "Team A", maxConcurrentRuns: 2, resourceVersion: "7" });
  });

  it("creates a Team without a version", async () => {
    const calls = sent(() => json(team(), 201));
    await putTeam("a", { displayName: "Team A" });
    expect(bodyOf(calls[0].init)).toEqual({ displayName: "Team A" });
  });

  it("sends an AgentAccess's spec under spec, beside its version", async () => {
    const calls = sent(() => json(access()));
    await putAgentAccess("a", "bot", { role: "viewer" }, "3");
    expect(calls[0].url).toBe("/api/v1/namespaces/a/agentaccesses/bot");
    expect(bodyOf(calls[0].init)).toEqual({ spec: { role: "viewer" }, resourceVersion: "3" });
  });

  it("makes a 409 an APIError with the server's code", async () => {
    sent(() => json({ error: { code: "conflict", message: "AgentAccess bot changed since it was read" } }, 409));
    const err = await putAgentAccess("a", "bot", { role: "member" }, "2").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(APIError);
    expect(err).toMatchObject({ status: 409, code: "conflict", message: "AgentAccess bot changed since it was read" });
  });

  it("makes a non-JSON error without a status text an APIError with its status for a message", async () => {
    sent(() => Promise.resolve(new Response("<html>Bad gateway</html>", { status: 502 })));
    const err = await runEvents("a", "x", "r1").catch((e: unknown) => e);
    expect(err).toMatchObject({ status: 502, code: "", message: "HTTP 502" });
  });

  it("makes a JSON error with an empty message an APIError with its status for a message", async () => {
    sent(() => json({ error: { code: "unavailable", message: "" } }, 503));
    const err = await runEvents("a", "x", "r1").catch((e: unknown) => e);
    expect(err).toMatchObject({ status: 503, code: "unavailable", message: "HTTP 503" });
  });

  it("rotates a token with a POST to its token", async () => {
    const calls = sent(() => json({ token: "t0ken", tokenSecret: "bot-token" }));
    await expect(rotateAgentAccessToken("a", "bot")).resolves.toEqual({ token: "t0ken", tokenSecret: "bot-token" });
    expect(calls[0].url).toBe("/api/v1/namespaces/a/agentaccesses/bot/token");
    expect(calls[0].init?.method).toBe("POST");
  });
});

// hung stubs fetch with one that never answers, but stops, as fetch does,
// when its signal is aborted (or already was).
function hung() {
  const calls: { url: string; init?: RequestInit }[] = [];
  vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    const aborted = () => new DOMException("aborted", "AbortError");
    if (init?.signal?.aborted) return Promise.reject(aborted());
    return new Promise<Response>((_, reject) => init?.signal?.addEventListener("abort", () => reject(aborted())));
  });
  return calls;
}

describe("runEvents", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("aborts the fetch when its signal is aborted", async () => {
    const calls = hung();
    const ctrl = new AbortController();
    const read = runEvents("a", "x", "r1", ctrl.signal).catch((e: unknown) => e);
    expect(calls[0].init?.signal?.aborted).toBe(false);
    ctrl.abort();
    expect(calls[0].init?.signal?.aborted).toBe(true);
    expect(await read).toMatchObject({ name: "AbortError" });
  });

  it("aborts the fetch at once when its signal is aborted before the call", async () => {
    const calls = hung();
    const ctrl = new AbortController();
    ctrl.abort();
    const read = runEvents("a", "x", "r1", ctrl.signal).catch((e: unknown) => e);
    expect(calls[0].init?.signal?.aborted).toBe(true);
    expect(await read).toMatchObject({ name: "AbortError" });
  });
});

describe("a request that never answers", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it("fails as a read once readTimeoutMs has passed", async () => {
    hung();
    let err: unknown;
    void runEvents("a", "x", "r1").catch((e: unknown) => (err = e));
    await vi.advanceTimersByTimeAsync(readTimeoutMs - 1);
    expect(err).toBeUndefined();
    await vi.advanceTimersByTimeAsync(1);
    expect(err).toBeInstanceOf(Error);
    expect((err as Error).message).toBe(readTimedOut);
  });

  it("is given up on as a read while its body never comes", async () => {
    vi.stubGlobal("fetch", (_url: string, init?: RequestInit) =>
      Promise.resolve({ ok: true, status: 200, json: () => new Promise((_, reject) => init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")))) }),
    );
    let err: unknown;
    void runEvents("a", "x", "r1").catch((e: unknown) => (err = e));
    await vi.advanceTimersByTimeAsync(readTimeoutMs);
    expect((err as Error)?.message).toBe(readTimedOut);
  });

  it("is waited for as a write", async () => {
    const calls = hung();
    let settled = false;
    void putTeam("a", { displayName: "Team A" }).finally(() => (settled = true)).catch(() => {});
    await vi.advanceTimersByTimeAsync(10 * readTimeoutMs);
    expect(settled).toBe(false);
    expect(calls[0].init?.signal).toBeUndefined();
  });
});
