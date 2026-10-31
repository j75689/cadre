import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { AgentSummary, ConfigResource } from "./api";
import { App } from "./App";
import { json } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

// at opens path in team a; the calls to the paths in answers get their
// answers in turn, the last one repeated, and the other calls never answer.
function at(path: string, answers: Record<string, (() => Promise<Response>)[]>) {
  const reads: Record<string, number> = {};
  vi.stubGlobal("fetch", (url: string) => {
    const key = Object.keys(answers).find((k) => url.endsWith(k));
    if (!key) return new Promise(() => {});
    const n = (reads[key] = (reads[key] ?? -1) + 1);
    return answers[key][Math.min(n, answers[key].length - 1)]();
  });
  render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
}

const list = "/namespaces/a/toolservers";
const one = "/namespaces/a/toolservers/github";

const servers: AgentSummary[] = [
  { name: "github", ready: true },
  { name: "jira", ready: false, reason: "Pending", message: "its current spec has not been checked yet" },
  { name: "search", ready: false, reason: "Unreachable", message: "dial tcp: connection refused" },
];

describe("ToolServers", () => {
  it("shows each tool server's readiness, reason and message, linked to its page", async () => {
    at("/t/a/toolservers", { [list]: [() => json({ items: servers })] });
    const rows = await screen.findAllByRole("row");
    expect(rows).toHaveLength(4);
    const [, ready, pending, notReady] = rows.map((r) => within(r));
    expect(ready.getByText("Ready")).toBeTruthy();
    expect(ready.getByRole("link", { name: "github" }).getAttribute("href")).toBe("/t/a/toolservers/github");
    expect(pending.getAllByText("Pending")).toHaveLength(1);
    expect(notReady.getByText("Unreachable")).toBeTruthy();
    expect(notReady.getByText("dial tcp: connection refused")).toBeTruthy();
    expect(screen.getByRole("region", { name: "Tool servers" })).toBeTruthy();
    expect(document.title).toBe("Tool servers · a · Cadre");
  });

  it("says how tool servers are added when there are none", async () => {
    at("/t/a/toolservers", { [list]: [() => json({ items: [] })] });
    expect(await screen.findByText(/This team has no tool servers/)).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows a banner with Retry when the tool servers cannot be read", async () => {
    at("/t/a/toolservers", { [list]: [() => json({ error: { code: "Unavailable", message: "not synced" } }, 503)] });
    expect(await screen.findByText("Could not load the tool servers: not synced")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });

  it("is a tab of the team nav, current on its pages", async () => {
    at("/t/a/toolservers/github", {});
    const tab = within(screen.getByRole("navigation", { name: "Team" })).getByRole("link", { name: "Tool servers" });
    expect(tab.getAttribute("href")).toBe("/t/a/toolservers");
    expect(tab.getAttribute("aria-current")).toBe("page");
    expect(await screen.findByText(/Loading the tool server/)).toBeTruthy();
  });
});

// The ToolServer github as each role is shown it (project() in internal/api/config.go).
const viewer: ConfigResource = {
  name: "github",
  ready: true,
  kind: "ToolServer",
  generation: 2,
  observedGeneration: 1,
  conditions: [{ type: "Ready", status: "True", reason: "SecretFound", message: "", observedGeneration: 1 }],
  metadata: { uid: "u-9", resourceVersion: "7", creationTimestamp: "2026-10-25T08:00:00Z", labels: { team: "a" } },
};

const auth = { secretRef: { name: "gh", key: "token" }, header: "Authorization" };

const member: ConfigResource = {
  ...viewer,
  metadata: { ...viewer.metadata, annotations: { owner: "dana" } },
  spec: { url: "https://(set)@mcp.example.test/mcp?api-key=(set)", auth: { ...auth, prefix: "(set)" } },
  status: { observedGeneration: 1, url: "http://github.a.svc:8080//(set)@evil/mcp" },
};

const admin: ConfigResource = {
  ...member,
  spec: { url: "https://u:sk@mcp.example.test/mcp?api-key=sk", auth: { ...auth, prefix: "token sk-" } },
  status: { observedGeneration: 1, url: "http://github.a.svc:8080/mcp" },
};

// managed is a managed ToolServer's spec, with its probe.
const managed = (path: string, probe: Record<string, unknown>) => ({ managed: { image: "ghcr.io/x/mcp:1", port: 8080, path, readinessProbe: probe } });

const yaml = async () => (await screen.findByRole("region", { name: "YAML text" })).textContent ?? "";

describe("ToolServerDetail", () => {
  const open = (...answers: (() => Promise<Response>)[]) => at("/t/a/toolservers/github", { [one]: answers });

  it("shows a skeleton while the tool server is read", async () => {
    open(() => new Promise(() => {}));
    expect(await screen.findByText(/Loading the tool server/)).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Conditions" })).toBeNull();
  });

  it("shows a viewer its health and metadata, with no spec card", async () => {
    open(() => json(viewer));
    expect(await screen.findByText("ToolServer")).toBeTruthy();
    expect(screen.getByRole("heading", { level: 1, name: "github" })).toBeTruthy();
    expect(screen.getByText("Ready", { selector: ".badge" })).toBeTruthy();
    expect(document.title).toBe("github · Tool servers · a · Cadre");
    const crumbs = within(screen.getByRole("navigation", { name: "Breadcrumb" }));
    expect(crumbs.getByRole("link", { name: "Tool servers" }).getAttribute("href")).toBe("/t/a/toolservers");
    expect(within(screen.getByRole("region", { name: "Conditions table" })).getByText("(older spec)")).toBeTruthy();
    expect(screen.getByText("u-9")).toBeTruthy();
    expect(screen.getByText("The spec and status are shown to the team's members and admins.")).toBeTruthy();
    expect(screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent)).toEqual(["Conditions", "Metadata", "YAML"]);
    expect(await yaml()).not.toContain("spec:");
    expect(screen.getByText("No spec; not a manifest to apply.")).toBeTruthy();
  });

  it("shows a member the masked values as served, with the note, and status.url only under Status", async () => {
    open(() => json(member));
    const text = await yaml();
    expect(text).toContain("url: https://(set)@mcp.example.test/mcp?api-key=(set)\n");
    expect(text).toContain("prefix: (set)\n");
    expect(text).not.toContain("status");
    expect(text).not.toContain("svc:8080");
    expect(screen.getByRole("region", { name: "Status" }).textContent).toContain("url: http://github.a.svc:8080//(set)@evil/mcp");
    expect(screen.getByText(/are set but hidden from members; admins see them/)).toBeTruthy();
    expect(screen.getByText("Contains hidden values; not applyable as is.")).toBeTruthy();
    expect(screen.getByText("owner=dana")).toBeTruthy();
    expect(screen.queryByText(/shown to the team's members and admins/)).toBeNull();
  });

  it.each([
    ["its url's user info", { url: "https://(set)@mcp.example.test/mcp" }],
    ["its url's query value", { url: "https://mcp.example.test/mcp?k=(set)" }],
    ["a url that does not parse", { url: "(set)" }],
    ["its managed path", managed("//(set)@mcp.internal/mcp", {})],
    ["its probe's path", managed("/mcp", { httpGet: { path: "//(set)@h/healthz", port: 8080 } })],
    ["a probe header value", managed("/mcp", { httpGet: { path: "/healthz", port: 8080, httpHeaders: [{ name: "X-Key", value: "(set)" }] } })],
    ["a probe exec command", managed("/mcp", { exec: { command: ["(set)", "(set)"] } })],
    ["its auth prefix", { url: "https://mcp.example.test/mcp", auth: { ...auth, prefix: "(set)" } }],
  ])("notes a value masked in %s", async (_, spec) => {
    open(() => json({ ...member, spec }));
    await yaml();
    expect(screen.getByText(/are set but hidden from members/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Copy" }).getAttribute("aria-describedby")).toBe("yaml-note");
    expect(screen.getByText("Contains hidden values; not applyable as is.")).toBeTruthy();
  });

  it("shows an admin the whole spec, with no note, as a manifest", async () => {
    open(() => json(admin));
    const text = await yaml();
    expect(text.slice(0, text.indexOf("spec:\n"))).toBe(
      "apiVersion: cadre.dev/v1alpha1\nkind: ToolServer\nmetadata:\n  name: github\n  namespace: a\n  labels:\n    team: a\n  annotations:\n    owner: dana\n",
    );
    expect(text).toContain("url: https://u:sk@mcp.example.test/mcp?api-key=sk\n");
    expect(text).toContain("prefix: token sk-\n");
    expect(screen.getByRole("region", { name: "Status" }).textContent).toContain("url: http://github.a.svc:8080/mcp");
    expect(screen.queryByText(/hidden from members|not applyable/)).toBeNull();
    expect(screen.getByRole("button", { name: "Copy manifest" })).toBeTruthy();
  });

  it("adds no note for a URL-like (set) outside an admin's masked URLs", async () => {
    open(() => json({ ...admin, spec: { ...managed("/mcp", {}), env: "//(set)@x/y?k=(set)" } }));
    expect(await yaml()).toContain("env: //(set)@x/y?k=(set)\n");
    expect(screen.queryByText(/hidden/)).toBeNull();
  });

  it("says when there is no such tool server, with a way back", async () => {
    open(() => json({ error: { code: "not_found", message: "toolserver github not found" } }, 404));
    expect((await screen.findByText(/No tool server named/)).textContent).toBe("No tool server named github in this team.");
    expect(screen.getByRole("link", { name: "All tool servers" }).getAttribute("href")).toBe("/t/a/toolservers");
  });

  it("shows a refusal, as in a namespace without a Team, without Retry", async () => {
    open(() => json({ error: { code: "no_role", message: "you have no role in namespace a" } }, 403));
    expect(await screen.findByText("Could not load the tool server: you have no role in namespace a")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it("shows a failed read with Retry, which reads again", async () => {
    open(() => json({ error: { code: "Unavailable", message: "not synced" } }, 503), () => json(viewer));
    expect(await screen.findByText("Could not load the tool server: not synced")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("region", { name: "Conditions table" })).toBeTruthy();
    await waitFor(() => expect(screen.queryByText(/Could not load the tool server/)).toBeNull());
  });

  it("is opened from an Agent's toolRefs", async () => {
    at("/t/a/agents/bot", {
      "/namespaces/a/agents/bot": [() => json({ ...viewer, name: "bot", kind: "Agent", spec: { modelRef: "claude", toolRefs: ["github", "jira"] } })],
      [one]: [() => json(viewer)],
    });
    const uses = within((await screen.findByRole("heading", { name: "Uses" })).closest("section")!);
    expect(uses.getByRole("link", { name: "jira" }).getAttribute("href")).toBe("/t/a/toolservers/jira");
    const link = uses.getByRole("link", { name: "github" });
    expect(link.getAttribute("href")).toBe("/t/a/toolservers/github");
    fireEvent.click(link);
    expect(await screen.findByRole("heading", { level: 1, name: "github" })).toBeTruthy();
    expect(await screen.findByText("ToolServer")).toBeTruthy();
  });
});
