import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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

const list = "/namespaces/a/runners";
const one = "/namespaces/a/runners/ci";

const runners: AgentSummary[] = [
  { name: "ci", ready: true },
  { name: "laptop", ready: false, reason: "Pending", message: "its current spec has not been checked yet" },
  { name: "shared", ready: false, reason: "ServiceAccountInUse", message: "ServiceAccount ci is Runner ci's" },
];

describe("Runners", () => {
  it("shows each runner's readiness, reason and message, linked to its page", async () => {
    at("/t/a/runners", { [list]: [() => json({ items: runners })] });
    const rows = await screen.findAllByRole("row");
    expect(rows).toHaveLength(4);
    const [, ready, pending, notReady] = rows.map((r) => within(r));
    expect(ready.getByText("Ready")).toBeTruthy();
    expect(ready.getByRole("link", { name: "ci" }).getAttribute("href")).toBe("/t/a/runners/ci");
    expect(pending.getAllByText("Pending")).toHaveLength(1);
    expect(pending.getByText("its current spec has not been checked yet")).toBeTruthy();
    expect(notReady.getByText("ServiceAccountInUse")).toBeTruthy();
    expect(notReady.getByText("ServiceAccount ci is Runner ci's")).toBeTruthy();
    expect(screen.getByRole("region", { name: "Runners" })).toBeTruthy();
    expect(document.title).toBe("Runners · a · Cadre");
  });

  it("says how runners are added when there are none", async () => {
    at("/t/a/runners", { [list]: [() => json({ items: [] })] });
    expect(await screen.findByText(/This team has no runners/)).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows a banner with Retry when the runners cannot be read", async () => {
    at("/t/a/runners", { [list]: [() => json({ error: { code: "Unavailable", message: "not synced" } }, 503)] });
    expect(await screen.findByText("Could not load the runners: not synced")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });

  it("is a tab of the team nav, current on its pages", async () => {
    at("/t/a/runners/ci", {});
    const tab = within(screen.getByRole("navigation", { name: "Team" })).getByRole("link", { name: "Runners" });
    expect(tab.getAttribute("href")).toBe("/t/a/runners");
    expect(tab.getAttribute("aria-current")).toBe("page");
    expect(await screen.findByText(/Loading the runner/)).toBeTruthy();
  });

  it.each(["viewer", "member", "admin"])("is a tab for a %s", async (role) => {
    let me = false;
    at("/t/a/runners", {
      "/me": [() => ((me = true), json({ mode: "oidc", username: "dana@example.test", name: "Dana", roles: { a: role } }))],
      [list]: [() => json({ items: [] })],
    });
    expect(await screen.findByText(/This team has no runners/)).toBeTruthy();
    await waitFor(() => expect(me).toBe(true));
    await act(() => new Promise((r) => setTimeout(r, 0)));
    const nav = within(screen.getByRole("navigation", { name: "Team" }));
    expect(nav.getByRole("link", { name: "Runners" }).getAttribute("href")).toBe("/t/a/runners");
  });
});

// The Runner ci as each role is shown it (project() in internal/api/config.go):
// its spec changed since the controller last checked it, so it is Pending.
const viewer: ConfigResource = {
  name: "ci",
  ready: false,
  reason: "Pending",
  message: "its current spec has not been checked yet",
  kind: "Runner",
  generation: 2,
  observedGeneration: 1,
  conditions: [{ type: "Ready", status: "True", reason: "TokenMinted", message: "", observedGeneration: 1 }],
  metadata: { uid: "u-9", resourceVersion: "7", creationTimestamp: "2026-10-25T08:00:00Z", labels: { team: "a" } },
};

// member is also an admin's: nothing of a Runner is masked, and no role is
// served status.tokenHash.
const member: ConfigResource = {
  ...viewer,
  metadata: { ...viewer.metadata, annotations: { owner: "dana" } },
  spec: { labels: { pool: "gpu", zone: "eu" } },
  status: { observedGeneration: 1, tokenSecret: "ci-token", mintedAt: "2026-10-25T08:00:01Z" },
};

const yaml = async () => (await screen.findByRole("region", { name: "YAML text" })).textContent ?? "";

describe("RunnerDetail", () => {
  const open = (...answers: (() => Promise<Response>)[]) => at("/t/a/runners/ci", { [one]: answers });

  it("shows a skeleton while the runner is read", async () => {
    open(() => new Promise(() => {}));
    expect(await screen.findByText(/Loading the runner/)).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Conditions" })).toBeNull();
  });

  it("shows a viewer its health and metadata, with no spec", async () => {
    open(() => json(viewer));
    expect(await screen.findByText("Runner")).toBeTruthy();
    expect(screen.getByRole("heading", { level: 1, name: "ci" })).toBeTruthy();
    expect(screen.getByText("Pending", { selector: ".badge" })).toBeTruthy();
    expect(screen.getByText("its current spec has not been checked yet")).toBeTruthy();
    expect(document.title).toBe("ci · Runners · a · Cadre");
    const crumbs = within(screen.getByRole("navigation", { name: "Breadcrumb" }));
    expect(crumbs.getByRole("link", { name: "Runners" }).getAttribute("href")).toBe("/t/a/runners");
    const conditions = within(screen.getByRole("region", { name: "Conditions table" }));
    expect(conditions.getByText("TokenMinted")).toBeTruthy();
    expect(conditions.getByText("(older spec)")).toBeTruthy();
    expect(screen.getByText("u-9")).toBeTruthy();
    expect(screen.getByText("The spec and status are shown to the team's members and admins.")).toBeTruthy();
    expect(screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent)).toEqual(["Conditions", "Metadata", "YAML"]);
    expect(await yaml()).not.toContain("spec:");
    expect(screen.getByText("No spec; not a manifest to apply.")).toBeTruthy();
  });

  it("shows a member or admin the spec as a manifest, and the minted token's Secret only under Status", async () => {
    open(() => json(member));
    const text = await yaml();
    expect(text).toBe(
      "apiVersion: cadre.dev/v1alpha1\nkind: Runner\nmetadata:\n  name: ci\n  namespace: a\n  labels:\n    team: a\n  annotations:\n    owner: dana\nspec:\n  labels:\n    pool: gpu\n    zone: eu\n",
    );
    const status = screen.getByRole("region", { name: "Status" }).textContent;
    expect(status).toContain("tokenSecret: ci-token");
    expect(status).toContain("mintedAt:");
    expect(screen.getByRole("button", { name: "Copy manifest" })).toBeTruthy();
    expect(screen.queryByText(/hidden from members|not applyable/)).toBeNull();
    expect(screen.queryByText(/shown to the team's members and admins/)).toBeNull();
    expect(document.body.textContent).not.toMatch(/tokenHash/i);
  });

  it("says when there is no such runner, with a way back", async () => {
    open(() => json({ error: { code: "not_found", message: "not found" } }, 404));
    expect((await screen.findByText(/No runner named/)).textContent).toBe("No runner named ci in this team.");
    expect(screen.getByRole("link", { name: "All runners" }).getAttribute("href")).toBe("/t/a/runners");
  });

  it("shows a refusal, as in a namespace without a Team, without Retry", async () => {
    open(() => json({ error: { code: "no_role", message: "you have no role in namespace a" } }, 403));
    expect(await screen.findByText("Could not load the runner: you have no role in namespace a")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it("shows a failed read with Retry, which reads again", async () => {
    open(() => json({ error: { code: "Unavailable", message: "not synced" } }, 503), () => json(viewer));
    expect(await screen.findByText("Could not load the runner: not synced")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("region", { name: "Conditions table" })).toBeTruthy();
    await waitFor(() => expect(screen.queryByText(/Could not load the runner/)).toBeNull());
  });
});
