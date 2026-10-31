import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ConfigResource } from "./api";
import { App } from "./App";
import { json } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

// The Agent bot as each role is shown it (project() in internal/api/config.go).
const viewer: ConfigResource = {
  name: "bot",
  ready: false,
  reason: "Pending",
  message: "its current spec has not been checked yet",
  kind: "Agent",
  generation: 4,
  observedGeneration: 3,
  conditions: [
    { type: "Ready", status: "True", reason: "Ready", message: "all references resolve", lastTransitionTime: "2026-10-25T09:00:00Z", observedGeneration: 3 },
  ],
  metadata: { uid: "u-1", resourceVersion: "42", creationTimestamp: "2026-10-25T08:00:00Z", labels: { team: "a" } },
};

const container = (env: object, command: string[]) => ({ runtime: { job: { template: { spec: { containers: [{ name: "agent", env: [env], command }] } } } } });

const member: ConfigResource = {
  ...viewer,
  metadata: { ...viewer.metadata, annotations: { owner: "dana" } },
  spec: { modelRef: "claude", toolRefs: ["git", "web"], ...container({ name: "TOKEN", value: "(set)" }, ["(set)"]) },
  status: { observedGeneration: 3 },
};

const admin: ConfigResource = { ...member, spec: { modelRef: "claude", ...container({ name: "TOKEN", value: "hunter2" }, ["run", "--fast"]) } };

// open renders the page of agent bot in team a; the agent answers in turn,
// the last one repeated, and the other calls answer nothing.
function open(...answers: (() => Promise<Response>)[]) {
  let reads = 0;
  vi.stubGlobal("fetch", (url: string) => {
    if (url.endsWith("/namespaces/a/agents/bot")) return answers[Math.min(reads++, answers.length - 1)]();
    if (url.endsWith("/namespaces/a/agents")) return json({ items: [{ name: "bot", ready: true }] });
    return new Promise(() => {});
  });
  render(
    <MemoryRouter initialEntries={["/t/a/agents/bot"]}>
      <App />
    </MemoryRouter>,
  );
}

const yaml = async () => (await screen.findByRole("region", { name: "YAML text" })).textContent ?? "";

describe("AgentDetail", () => {
  it("shows a skeleton while the agent is read", async () => {
    open(() => new Promise(() => {}));
    expect(await screen.findByText(/Loading the agent/)).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Conditions" })).toBeNull();
  });

  it("heads the page with the name, the kind and the readiness as the server says it", async () => {
    open(() => json(viewer));
    expect(screen.getByRole("heading", { level: 1, name: "bot" })).toBeTruthy();
    expect(await screen.findByText("Agent")).toBeTruthy();
    // Pending is the badge's, from readiness(): not repeated as a reason.
    expect(screen.getAllByText("Pending")).toHaveLength(1);
    expect(screen.getByText("its current spec has not been checked yet")).toBeTruthy();
    expect(document.title).toBe("bot · Agents · a · Cadre");
    const crumbs = within(screen.getByRole("navigation", { name: "Breadcrumb" }));
    expect(crumbs.getByRole("link", { name: "Agents" }).getAttribute("href")).toBe("/t/a/agents");
    expect(within(screen.getByRole("navigation", { name: "Team" })).getByRole("link", { name: "Agents" }).getAttribute("aria-current")).toBe("page");
  });

  it("shows a not-ready reason beside its message", async () => {
    open(() => json({ ...viewer, reason: "ModelMissing", message: "ModelProvider claude not found" }));
    expect(await screen.findByText("ModelMissing:")).toBeTruthy();
    expect(screen.getByText("ModelProvider claude not found")).toBeTruthy();
  });

  it("lists the conditions with the generation each is about", async () => {
    open(() => json(viewer));
    const table = within(await screen.findByRole("region", { name: "Conditions table" }));
    const [, row] = table.getAllByRole("row").map((r) => within(r));
    expect(row.getByText("True")).toBeTruthy();
    expect(row.getByText("all references resolve")).toBeTruthy();
    expect(row.getByText(/3 of 4/)).toBeTruthy();
    expect(row.getByText("(older spec)")).toBeTruthy();
    expect(screen.getByText("4, observed 3")).toBeTruthy();
  });

  it("shows a viewer its health and metadata, and says who sees the spec", async () => {
    open(() => json(viewer));
    expect(await screen.findByText("The spec and status are shown to the team's members and admins.")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Uses" })).toBeNull();
    expect(screen.queryByText("Annotations")).toBeNull();
    expect(screen.getByText("team=a")).toBeTruthy();
    expect(screen.getByText("u-1")).toBeTruthy();
    expect(screen.getByText("42")).toBeTruthy();
    const text = await yaml();
    expect(text).toContain("metadata:\n  uid: u-1\n");
    expect(text).not.toContain("spec:");
    expect(text).not.toContain("status:");
  });

  it("shows a member what the agent uses, and masked values as (set)", async () => {
    open(() => json(member));
    const uses = within((await screen.findByRole("heading", { name: "Uses" })).closest("section")!);
    expect(uses.getByText("claude")).toBeTruthy();
    expect(uses.getByText("git")).toBeTruthy();
    expect(uses.getByText("web")).toBeTruthy();
    expect(screen.getByText("owner=dana")).toBeTruthy();
    const text = await yaml();
    expect(text).toMatch(/- name: TOKEN\n\s+value: \(set\)\n/);
    expect(text).toMatch(/command:\n\s+- \(set\)\n/);
    expect(text).toContain("status:\n  observedGeneration: 3\n");
    expect(screen.getByText(/are set but hidden from members; admins see them/)).toBeTruthy();
  });

  it("shows an admin the whole spec, with no note on masked values", async () => {
    open(() => json(admin));
    const text = await yaml();
    expect(text).toContain("value: hunter2\n");
    expect(text).toMatch(/command:\n\s+- run\n\s+- --fast\n/);
    expect(screen.queryByText(/hidden from members/)).toBeNull();
    const uses = within(screen.getByRole("heading", { name: "Uses" }).closest("section")!);
    expect(uses.getByText("None")).toBeTruthy();
  });

  it("adds no note for (set) in an admin's text", async () => {
    open(() => json({ ...admin, spec: { instructions: "Reply in this form:\nstatus: (set)\n" } }));
    expect(await yaml()).toContain("  status: (set)\n");
    expect(screen.queryByText(/hidden from members/)).toBeNull();
  });

  it("adds no note for a URL in an admin's text that looks masked", async () => {
    open(() => json({ ...admin, spec: { instructions: "Reply with the form at https://forms.example.test/?status=(set)" } }));
    expect(await yaml()).toContain("https://forms.example.test/?status=(set)");
    expect(screen.queryByText(/hidden from members/)).toBeNull();
  });

  it("copies the YAML", async () => {
    const writeText = vi.fn(() => Promise.resolve());
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    open(() => json(member));
    const text = await yaml();
    fireEvent.click(screen.getByRole("button", { name: "Copy" }));
    expect(await screen.findByText("Copied")).toBeTruthy();
    expect(writeText).toHaveBeenCalledWith(text);
  });

  it("selects the YAML to copy by hand without a clipboard", async () => {
    vi.stubGlobal("navigator", { ...navigator, clipboard: undefined });
    open(() => json(member));
    const text = await yaml();
    fireEvent.click(screen.getByRole("button", { name: "Copy" }));
    expect(await screen.findByText("Press Ctrl+C to copy it.")).toBeTruthy();
    expect(getSelection()?.toString()).toBe(text);
  });

  it("says when there is no such agent, with a way back", async () => {
    open(() => json({ error: { code: "not_found", message: "agent bot not found" } }, 404));
    expect((await screen.findByText(/No agent named/)).textContent).toBe("No agent named bot in this team.");
    expect(screen.getByRole("link", { name: "All agents" }).getAttribute("href")).toBe("/t/a/agents");
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it("shows a refusal without Retry", async () => {
    open(() => json({ error: { code: "no_role", message: "you have no role in namespace a" } }, 403));
    expect(await screen.findByText("Could not load the agent: you have no role in namespace a")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it("shows a failed read with Retry, which reads again", async () => {
    open(() => json({ error: { code: "Unavailable", message: "not synced" } }, 503), () => json(viewer));
    expect(await screen.findByText("Could not load the agent: not synced")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("region", { name: "Conditions table" })).toBeTruthy();
    await waitFor(() => expect(screen.queryByText(/Could not load the agent/)).toBeNull());
  });

  it("is opened from the Agents list", async () => {
    vi.stubGlobal("fetch", (url: string) => {
      if (url.endsWith("/namespaces/a/agents")) return json({ items: [{ name: "bot", ready: true }] });
      if (url.endsWith("/namespaces/a/agents/bot")) return json(viewer);
      return new Promise(() => {});
    });
    render(
      <MemoryRouter initialEntries={["/t/a/agents"]}>
        <App />
      </MemoryRouter>,
    );
    const link = await screen.findByRole("link", { name: "bot" });
    expect(link.getAttribute("href")).toBe("/t/a/agents/bot");
    fireEvent.click(link);
    expect(await screen.findByRole("heading", { level: 1, name: "bot" })).toBeTruthy();
    expect(await screen.findByRole("region", { name: "Conditions table" })).toBeTruthy();
  });
});
