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

const list = "/namespaces/a/modelproviders";
const one = "/namespaces/a/modelproviders/claude";

const providers: AgentSummary[] = [
  { name: "claude", ready: true },
  { name: "gpt", ready: false, reason: "Pending", message: "its current spec has not been checked yet" },
  { name: "local", ready: false, reason: "SecretMissing", message: "secret llm-key not found" },
];

describe("ModelProviders", () => {
  it("shows each provider's readiness, reason and message, linked to its page", async () => {
    at("/t/a/modelproviders", { [list]: [() => json({ items: providers })] });
    const rows = await screen.findAllByRole("row");
    expect(rows).toHaveLength(4);
    const [, ready, pending, notReady] = rows.map((r) => within(r));
    expect(ready.getByText("Ready")).toBeTruthy();
    expect(ready.getByRole("link", { name: "claude" }).getAttribute("href")).toBe("/t/a/modelproviders/claude");
    expect(pending.getAllByText("Pending")).toHaveLength(1);
    expect(notReady.getByText("SecretMissing")).toBeTruthy();
    expect(notReady.getByText("secret llm-key not found")).toBeTruthy();
    expect(screen.getByRole("region", { name: "Model providers" })).toBeTruthy();
    expect(document.title).toBe("Model providers · a · Cadre");
  });

  it("says how providers are added when there are none", async () => {
    at("/t/a/modelproviders", { [list]: [() => json({ items: [] })] });
    expect(await screen.findByText(/This team has no model providers/)).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows a banner with Retry when the providers cannot be read", async () => {
    at("/t/a/modelproviders", { [list]: [() => json({ error: { code: "Unavailable", message: "not synced" } }, 503)] });
    expect(await screen.findByText("Could not load the model providers: not synced")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });

  it("is a tab of the team nav, current on its pages", async () => {
    at("/t/a/modelproviders/claude", {});
    const tab = within(screen.getByRole("navigation", { name: "Team" })).getByRole("link", { name: "Model providers" });
    expect(tab.getAttribute("href")).toBe("/t/a/modelproviders");
    expect(tab.getAttribute("aria-current")).toBe("page");
    expect(await screen.findByText(/Loading the model provider/)).toBeTruthy();
  });
});

// The ModelProvider claude as each role is shown it (project() in internal/api/config.go).
const viewer: ConfigResource = {
  name: "claude",
  ready: true,
  kind: "ModelProvider",
  generation: 2,
  observedGeneration: 1,
  conditions: [{ type: "Ready", status: "True", reason: "Ready", message: "secret found", observedGeneration: 1 }],
  metadata: { uid: "u-9", resourceVersion: "7", creationTimestamp: "2026-10-25T08:00:00Z", labels: { team: "a" } },
};

const spec = { type: "openai", model: "m", auth: { secretRef: { name: "llm", key: "api-key" } } };

const member: ConfigResource = {
  ...viewer,
  metadata: { ...viewer.metadata, annotations: { owner: "dana" } },
  spec: { ...spec, baseURL: "https://(set)@gw.example.test/v1?api-key=(set)&(set)" },
  status: { observedGeneration: 1 },
};

const admin: ConfigResource = { ...member, spec: { ...spec, baseURL: "https://u:sk@gw.example.test/v1?api-key=sk" } };

const yaml = async () => (await screen.findByRole("region", { name: "YAML text" })).textContent ?? "";

describe("ModelProviderDetail", () => {
  const open = (...answers: (() => Promise<Response>)[]) => at("/t/a/modelproviders/claude", { [one]: answers });

  it("shows a skeleton while the provider is read", async () => {
    open(() => new Promise(() => {}));
    expect(await screen.findByText(/Loading the model provider/)).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Conditions" })).toBeNull();
  });

  it("shows a viewer its health and metadata, with no spec card", async () => {
    open(() => json(viewer));
    expect(await screen.findByText("ModelProvider")).toBeTruthy();
    expect(screen.getByRole("heading", { level: 1, name: "claude" })).toBeTruthy();
    expect(screen.getByText("Ready", { selector: ".badge" })).toBeTruthy();
    expect(document.title).toBe("claude · Model providers · a · Cadre");
    const crumbs = within(screen.getByRole("navigation", { name: "Breadcrumb" }));
    expect(crumbs.getByRole("link", { name: "Model providers" }).getAttribute("href")).toBe("/t/a/modelproviders");
    expect(within(screen.getByRole("region", { name: "Conditions table" })).getByText("(older spec)")).toBeTruthy();
    expect(screen.getByText("u-9")).toBeTruthy();
    expect(screen.getByText("The spec and status are shown to the team's members and admins.")).toBeTruthy();
    expect(screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent)).toEqual(["Conditions", "Metadata", "YAML"]);
    expect(await yaml()).not.toContain("spec:");
  });

  it("shows a member the base URL masked as served, with the note", async () => {
    open(() => json(member));
    expect(await yaml()).toContain("baseURL: https://(set)@gw.example.test/v1?api-key=(set)&(set)\n");
    expect(screen.getByText(/are set but hidden from members; admins see them/)).toBeTruthy();
    expect(screen.getByText("owner=dana")).toBeTruthy();
    expect(screen.queryByText(/shown to the team's members and admins/)).toBeNull();
  });

  it.each([
    ["the user info", "https://(set)@gw.example.test/v1"],
    ["a fragment", "https://gw.example.test/v1#(set)"],
    ["a URL that does not parse", "(set)"],
  ])("notes a base URL masked in %s", async (_, baseURL) => {
    open(() => json({ ...member, spec: { ...spec, baseURL } }));
    expect(await yaml()).toContain(`baseURL: ${baseURL}\n`);
    expect(screen.getByText(/are set but hidden from members/)).toBeTruthy();
  });

  it("shows an admin the whole base URL, with no note", async () => {
    open(() => json(admin));
    expect(await yaml()).toContain("baseURL: https://u:sk@gw.example.test/v1?api-key=sk\n");
    expect(screen.queryByText(/hidden from members/)).toBeNull();
  });

  it("says when there is no such provider, with a way back", async () => {
    open(() => json({ error: { code: "not_found", message: "modelprovider claude not found" } }, 404));
    expect((await screen.findByText(/No model provider named/)).textContent).toBe("No model provider named claude in this team.");
    expect(screen.getByRole("link", { name: "All model providers" }).getAttribute("href")).toBe("/t/a/modelproviders");
  });

  it("shows a refusal, as in a namespace without a Team, without Retry", async () => {
    open(() => json({ error: { code: "no_role", message: "you have no role in namespace a" } }, 403));
    expect(await screen.findByText("Could not load the model provider: you have no role in namespace a")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it("shows a failed read with Retry, which reads again", async () => {
    open(() => json({ error: { code: "Unavailable", message: "not synced" } }, 503), () => json(viewer));
    expect(await screen.findByText("Could not load the model provider: not synced")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("region", { name: "Conditions table" })).toBeTruthy();
    await waitFor(() => expect(screen.queryByText(/Could not load the model provider/)).toBeNull());
  });

  it("is opened from an Agent's modelRef", async () => {
    at("/t/a/agents/bot", {
      "/namespaces/a/agents/bot": [() => json({ ...viewer, name: "bot", kind: "Agent", spec: { modelRef: "claude" } })],
      [one]: [() => json(viewer)],
    });
    const uses = within((await screen.findByRole("heading", { name: "Uses" })).closest("section")!);
    const link = uses.getByRole("link", { name: "claude" });
    expect(link.getAttribute("href")).toBe("/t/a/modelproviders/claude");
    fireEvent.click(link);
    expect(await screen.findByRole("heading", { level: 1, name: "claude" })).toBeTruthy();
    expect(await screen.findByText("ModelProvider")).toBeTruthy();
  });
});
