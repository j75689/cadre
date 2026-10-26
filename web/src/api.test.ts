import { afterEach, describe, expect, it, vi } from "vitest";
import { APIError, deleteAgentAccess, loginURL, putAgentAccess, putTeam, rotateAgentAccessToken, runEvents } from "./api";
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

  it("rotates a token with a POST to its token", async () => {
    const calls = sent(() => json({ token: "t0ken", tokenSecret: "bot-token" }));
    await expect(rotateAgentAccessToken("a", "bot")).resolves.toEqual({ token: "t0ken", tokenSecret: "bot-token" });
    expect(calls[0].url).toBe("/api/v1/namespaces/a/agentaccesses/bot/token");
    expect(calls[0].init?.method).toBe("POST");
  });
});

describe("runEvents", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("passes its signal to fetch", async () => {
    const calls = sent(() => json({ items: [] }));
    const ctrl = new AbortController();
    await runEvents("a", "x", "r1", ctrl.signal);
    expect(calls[0].init?.signal).toBe(ctrl.signal);
  });
});
