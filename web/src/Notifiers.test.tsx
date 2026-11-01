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

const list = "/namespaces/a/notifiers";
const one = "/namespaces/a/notifiers/slack";

const notifiers: AgentSummary[] = [
  { name: "slack", ready: true },
  { name: "pager", ready: false, reason: "Pending", message: "its current spec has not been checked yet" },
  { name: "chat", ready: false, reason: "SecretMissing", message: 'secret "chat-signing" not found' },
];

describe("Notifiers", () => {
  it("shows each notifier's readiness, reason and message, linked to its page", async () => {
    at("/t/a/notifiers", { [list]: [() => json({ items: notifiers })] });
    const rows = await screen.findAllByRole("row");
    expect(rows).toHaveLength(4);
    const [, ready, pending, notReady] = rows.map((r) => within(r));
    expect(ready.getByText("Ready")).toBeTruthy();
    expect(ready.getByRole("link", { name: "slack" }).getAttribute("href")).toBe("/t/a/notifiers/slack");
    expect(pending.getAllByText("Pending")).toHaveLength(1);
    expect(notReady.getByText("SecretMissing")).toBeTruthy();
    expect(notReady.getByText('secret "chat-signing" not found')).toBeTruthy();
    expect(screen.getByRole("region", { name: "Notifiers" })).toBeTruthy();
    expect(document.title).toBe("Notifiers · a · Cadre");
  });

  it("says how notifiers are added when there are none", async () => {
    at("/t/a/notifiers", { [list]: [() => json({ items: [] })] });
    expect(await screen.findByText(/This team has no notifiers/)).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows a banner with Retry when the notifiers cannot be read", async () => {
    at("/t/a/notifiers", { [list]: [() => json({ error: { code: "Unavailable", message: "not synced" } }, 503)] });
    expect(await screen.findByText("Could not load the notifiers: not synced")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });

  it("is a tab of the team nav, current on its pages", async () => {
    at("/t/a/notifiers/slack", {});
    const tab = within(screen.getByRole("navigation", { name: "Team" })).getByRole("link", { name: "Notifiers" });
    expect(tab.getAttribute("href")).toBe("/t/a/notifiers");
    expect(tab.getAttribute("aria-current")).toBe("page");
    expect(await screen.findByText(/Loading the notifier/)).toBeTruthy();
  });
});

// The Notifier slack as each role is shown it (project() in internal/api/config.go).
const viewer: ConfigResource = {
  name: "slack",
  ready: true,
  kind: "Notifier",
  generation: 2,
  observedGeneration: 1,
  conditions: [{ type: "Ready", status: "True", reason: "Ready", message: "", observedGeneration: 1 }],
  metadata: { uid: "u-9", resourceVersion: "7", creationTimestamp: "2026-10-25T08:00:00Z", labels: { team: "a" } },
};

const webhook = { secretRef: { name: "slack-signing", key: "secret" } };

const member: ConfigResource = {
  ...viewer,
  metadata: { ...viewer.metadata, annotations: { owner: "dana" } },
  spec: { on: ["Question", "Done"], webhook: { ...webhook, url: "https://hooks.slack.com/(set)", headers: { "X-Team": "(set)" } } },
  status: { observedGeneration: 1, lastError: { message: "(set)", at: "2026-10-31T10:00:00Z" } },
};

const admin: ConfigResource = {
  ...member,
  spec: { on: ["Question", "Done"], webhook: { ...webhook, url: "https://hooks.slack.com/services/T0/B0/xyz", headers: { "X-Team": "ops" } } },
  status: { observedGeneration: 1, lastError: { message: "POST https://hooks.slack.com/services/T0/B0/xyz: 404", at: "2026-10-31T10:00:00Z" } },
};

// tool is a Notifier's spec that calls a tool on the ToolServer chat.
const tool = (args: Record<string, string>) => ({ on: ["Review"], toolServer: { name: "chat", tool: "post_message", arguments: args } });

const yaml = async () => (await screen.findByRole("region", { name: "YAML text" })).textContent ?? "";

describe("NotifierDetail", () => {
  const open = (...answers: (() => Promise<Response>)[]) => at("/t/a/notifiers/slack", { [one]: answers });

  it("shows a skeleton while the notifier is read", async () => {
    open(() => new Promise(() => {}));
    expect(await screen.findByText(/Loading the notifier/)).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Conditions" })).toBeNull();
  });

  it("shows a viewer its health and metadata, with no spec card", async () => {
    open(() => json(viewer));
    expect(await screen.findByText("Notifier")).toBeTruthy();
    expect(screen.getByRole("heading", { level: 1, name: "slack" })).toBeTruthy();
    expect(screen.getByText("Ready", { selector: ".badge" })).toBeTruthy();
    expect(document.title).toBe("slack · Notifiers · a · Cadre");
    const crumbs = within(screen.getByRole("navigation", { name: "Breadcrumb" }));
    expect(crumbs.getByRole("link", { name: "Notifiers" }).getAttribute("href")).toBe("/t/a/notifiers");
    expect(within(screen.getByRole("region", { name: "Conditions table" })).getByText("(older spec)")).toBeTruthy();
    expect(screen.getByText("u-9")).toBeTruthy();
    expect(screen.getByText("The spec and status are shown to the team's members and admins.")).toBeTruthy();
    expect(screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent)).toEqual(["Conditions", "Metadata", "YAML"]);
    expect(await yaml()).not.toContain("spec:");
    expect(screen.getByText("No spec; not a manifest to apply.")).toBeTruthy();
  });

  it("shows a member the masked values as served, with the note, and lastError only under Status", async () => {
    open(() => json(member));
    const text = await yaml();
    expect(text).toContain("url: https://hooks.slack.com/(set)\n");
    expect(text).toContain("X-Team: (set)\n");
    expect(text).not.toContain("lastError");
    expect(screen.getByRole("region", { name: "Status" }).textContent).toContain("lastError:\n  message: (set)\n");
    expect(screen.getByText(/are set but hidden from members; admins see them/)).toBeTruthy();
    expect(screen.getByText("Contains hidden values; not applyable as is.")).toBeTruthy();
    expect(screen.getByText("owner=dana")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Uses" })).toBeNull();
  });

  it.each([
    ["its webhook url's path", { on: ["Done"], webhook: { url: "https://hooks.slack.com/(set)" } }],
    ["its webhook url's path and query", { on: ["Done"], webhook: { url: "https://hooks.example.test:8443/(set)?token=(set)" } }],
    ["its webhook url's query, at /", { on: ["Done"], webhook: { url: "https://hooks.example.test/?token=(set)" } }],
    ["a webhook url that does not parse", { on: ["Done"], webhook: { url: "(set)" } }],
    ["a webhook header value", { on: ["Done"], webhook: { url: "https://hooks.example.test/", headers: { "X-Team": "(set)" } } }],
    ["a tool argument", tool({ channel: "(set)" })],
  ])("notes a value masked in %s", async (_, spec) => {
    open(() => json({ ...member, spec }));
    await yaml();
    expect(screen.getByText(/are set but hidden from members/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Copy" }).getAttribute("aria-describedby")).toBe("yaml-note");
    expect(screen.getByText("Contains hidden values; not applyable as is.")).toBeTruthy();
  });

  it("shows an admin the whole spec and lastError, with no note, as a manifest", async () => {
    open(() => json(admin));
    const text = await yaml();
    expect(text.slice(0, text.indexOf("spec:\n"))).toBe(
      "apiVersion: cadre.dev/v1alpha1\nkind: Notifier\nmetadata:\n  name: slack\n  namespace: a\n  labels:\n    team: a\n  annotations:\n    owner: dana\n",
    );
    expect(text).toContain("url: https://hooks.slack.com/services/T0/B0/xyz\n");
    expect(screen.getByRole("region", { name: "Status" }).textContent).toContain("message: \"POST https://hooks.slack.com/services/T0/B0/xyz: 404\"");
    expect(screen.queryByText(/hidden from members|not applyable/)).toBeNull();
    expect(screen.getByRole("button", { name: "Copy manifest" })).toBeTruthy();
  });

  it("adds no note for an admin's (set) outside a masked value", async () => {
    open(() => json({ ...admin, spec: tool({ text: "https://h/(set)?k=(set)" }) }));
    expect(await yaml()).toContain("text: https://h/(set)?k=(set)\n");
    expect(screen.queryByText(/hidden/)).toBeNull();
  });

  it("links the ToolServer it calls a tool on", async () => {
    at("/t/a/notifiers/slack", {
      [one]: [() => json({ ...admin, spec: tool({ channel: "#ops" }) })],
      "/namespaces/a/toolservers/chat": [() => json({ ...viewer, name: "chat", kind: "ToolServer" })],
    });
    const uses = within((await screen.findByRole("heading", { name: "Uses" })).closest("section")!);
    expect(uses.getByText("post_message")).toBeTruthy();
    const link = uses.getByRole("link", { name: "chat" });
    expect(link.getAttribute("href")).toBe("/t/a/toolservers/chat");
    fireEvent.click(link);
    expect(await screen.findByRole("heading", { level: 1, name: "chat" })).toBeTruthy();
    expect(await screen.findByText("ToolServer")).toBeTruthy();
  });

  it("says when there is no such notifier, with a way back", async () => {
    open(() => json({ error: { code: "not_found", message: "notifier slack not found" } }, 404));
    expect((await screen.findByText(/No notifier named/)).textContent).toBe("No notifier named slack in this team.");
    expect(screen.getByRole("link", { name: "All notifiers" }).getAttribute("href")).toBe("/t/a/notifiers");
  });

  it("shows a refusal, as in a namespace without a Team, without Retry", async () => {
    open(() => json({ error: { code: "no_role", message: "you have no role in namespace a" } }, 403));
    expect(await screen.findByText("Could not load the notifier: you have no role in namespace a")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it("shows a failed read with Retry, which reads again", async () => {
    open(() => json({ error: { code: "Unavailable", message: "not synced" } }, 503), () => json(viewer));
    expect(await screen.findByText("Could not load the notifier: not synced")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("region", { name: "Conditions table" })).toBeTruthy();
    await waitFor(() => expect(screen.queryByText(/Could not load the notifier/)).toBeNull());
  });
});
