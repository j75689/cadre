import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { activityPage } from "./Activity";
import { App } from "./App";
import type { Activity, Me } from "./api";
import { json, team } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const admin: Me = { mode: "oidc", name: "dana", roles: { a: "admin" } };

const item = (reason: string, kind: string, name: string, extra: Partial<Activity> = {}): Activity => ({
  time: "2026-11-01T05:30:00Z",
  type: "Normal",
  reason,
  action: reason,
  note: `${reason} note`,
  regarding: { kind, name },
  reportedBy: "cadre-server",
  count: 1,
  ...extra,
});

// open renders path in team a, with /me as given and the activity's
// answers in turn, the last one repeated; it returns the activity URLs read.
function open(path: string, me: Me, ...answers: (() => Promise<Response>)[]) {
  const reads: string[] = [];
  vi.stubGlobal("fetch", (url: string) => {
    if (url.endsWith("/me")) return json(me);
    if (url.endsWith("/teams")) return json({ items: [team()] });
    if (url.includes("/namespaces/a/activity?")) return answers[Math.min(reads.push(url) - 1, answers.length - 1)]();
    if (url.endsWith("/namespaces/a/team")) return json(team());
    return json({ items: [] });
  });
  render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
  return reads;
}

const page = (items: Activity[], offset = 0) => () => json({ items, offset, limit: activityPage });

describe("Activity", () => {
  it("lists the Events newest first, as the server sends them, with links to what they are about", async () => {
    const reads = open(
      "/t/a/settings/activity",
      admin,
      page([
        item("MissedTicks", "Schedule", "nightly", { type: "Warning", count: 7, reportedBy: "cadre-schedule" }),
        item("TokenRotated", "AgentAccess", "ci-bot"),
        item("TeamUpdated", "Team", "team"),
        item("JobRefused", "Agent", "writer"),
        item("Noted", "ModelProvider", "claude"),
        item("Noted", "ToolServer", "github"),
        item("Noted", "Notifier", "slack"),
        item("Noted", "Runner", "ci"),
        item("Noted", "Task", "01jtask"),
        item("Noted", "Widget", "w"),
      ]),
    );
    const table = within(await screen.findByRole("table"));
    const rows = table.getAllByRole("row").slice(1);
    expect(rows.map((r) => within(r).getAllByRole("cell")[2].textContent)).toEqual([
      "MissedTicks",
      "TokenRotated",
      "TeamUpdated",
      "JobRefused",
      "Noted",
      "Noted",
      "Noted",
      "Noted",
      "Noted",
      "Noted",
    ]);
    const first = within(rows[0]).getAllByRole("cell");
    expect(first[0].querySelector("time")?.getAttribute("title")).toBe(new Date("2026-11-01T05:30:00Z").toLocaleString());
    expect(first[1].textContent).toBe("Warning");
    expect(first[3].textContent).toBe("MissedTicks note");
    expect(first[4].textContent).toBe("Schedule/nightly");
    expect(first[5].textContent).toBe("cadre-schedule");
    expect(first[6].textContent).toBe("×7");
    expect(within(rows[1]).getAllByRole("cell")[1].textContent).toBe("Normal");
    expect(within(rows[1]).getAllByRole("cell")[6].textContent).toBe("");
    const href = (name: string) => table.getByRole("link", { name }).getAttribute("href");
    expect(href("nightly")).toBe("/t/a/schedules/nightly");
    expect(href("ci-bot")).toBe("/t/a/settings/access/ci-bot");
    expect(href("team")).toBe("/t/a/settings");
    expect(href("writer")).toBe("/t/a/agents/writer");
    expect(href("claude")).toBe("/t/a/modelproviders/claude");
    expect(href("github")).toBe("/t/a/toolservers/github");
    expect(href("slack")).toBe("/t/a/notifiers/slack");
    expect(href("ci")).toBe("/t/a/runners/ci");
    expect(href("01jtask")).toBe("/t/a/tasks/01jtask");
    expect(table.queryByRole("link", { name: "w" })).toBeNull();
    expect(table.getByText("w")).toBeTruthy();
    expect(screen.getByText("Recent activity (about the last hour); changes made with kubectl or GitOps are not listed.")).toBeTruthy();
    expect(reads).toEqual([`/api/v1/namespaces/a/activity?offset=0&limit=${activityPage}`]);
  });

  it("loads the next page while a page comes back full, and stops at a short one", async () => {
    const full = Array.from({ length: activityPage }, (_, i) => item(`R${i}`, "Agent", `a${i}`));
    const reads = open("/t/a/settings/activity", admin, page(full), page(full), page([item("Last", "Agent", "z")], activityPage));
    fireEvent.click(await screen.findByRole("button", { name: "Load more" }));
    expect(await screen.findByText("Last")).toBeTruthy();
    expect(screen.getAllByRole("row")).toHaveLength(activityPage + 2);
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
    expect(reads.map((u) => new URL(u, "http://x").searchParams.get("offset"))).toEqual(["0", "0", String(activityPage)]);
  });

  it("reads no further than a short page when it reads the shown pages again", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const full = Array.from({ length: activityPage }, (_, i) => item(`R${i}`, "Agent", `a${i}`));
      const reads = open("/t/a/settings/activity", admin, page(full), page(full), page([item("Last", "Agent", "z")], activityPage), page([item("Only", "Agent", "o")]));
      fireEvent.click(await screen.findByRole("button", { name: "Load more" }));
      expect(await screen.findByText("Last")).toBeTruthy();
      // The events older than an hour are gone: the first page is short now.
      await act(() => vi.advanceTimersByTimeAsync(15000));
      expect(await screen.findByText("Only")).toBeTruthy();
      expect(reads.map((u) => new URL(u, "http://x").searchParams.get("offset"))).toEqual(["0", "0", String(activityPage), "0"]);
    } finally {
      cleanup();
      vi.useRealTimers();
    }
  });

  it("reads again every 15 seconds", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const reads = open("/t/a/settings/activity", admin, page([]));
      await vi.waitFor(() => expect(reads).toHaveLength(1));
      await act(() => vi.advanceTimersByTimeAsync(14000));
      expect(reads).toHaveLength(1);
      await act(() => vi.advanceTimersByTimeAsync(1000));
      expect(reads).toHaveLength(2);
    } finally {
      cleanup();
      vi.useRealTimers();
    }
  });

  it("says when there is no activity", async () => {
    open("/t/a/settings/activity", admin, page([]));
    expect(await screen.findByText("No activity in the last hour.")).toBeTruthy();
  });

  it("offers Retry after a failed read, and reads again on it", async () => {
    const reads = open("/t/a/settings/activity", admin, () => json({ error: { code: "unavailable", message: "try later" } }, 503), page([item("TeamUpdated", "Team", "team")]));
    expect(await screen.findByText("Could not load the activity: try later")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("TeamUpdated")).toBeTruthy();
    expect(reads).toHaveLength(2);
  });

  it("says the server's ClusterRole needs upgrading on server_forbidden, without Retry", async () => {
    open("/t/a/settings/activity", admin, () => json({ error: { code: "server_forbidden", message: "cadre-server may not list Events" } }, 500));
    expect(await screen.findByText(/its ClusterRole predates the activity/)).toBeTruthy();
    expect(screen.getByText("config/rbac/role.yaml")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it("shows a 403 without Retry, to a member who follows a link", async () => {
    open("/t/a/settings/activity", { mode: "oidc", name: "lee", roles: { a: "member" } }, () =>
      json({ error: { code: "no_role", message: "needs the admin role" } }, 403),
    );
    expect(await screen.findByText("Could not load the activity: needs the admin role")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it.each([
    ["viewer", { mode: "oidc", name: "lee", roles: { a: "viewer" } }],
    ["member", { mode: "oidc", name: "lee", roles: { a: "member" } }],
  ] as const)("hides the Activity tab from a %s", async (role, me) => {
    open("/t/a/settings", me, page([]));
    // The role line shows once /me is read.
    expect(await screen.findByText(`You are a ${role} of a.`)).toBeTruthy();
    await act(() => new Promise((r) => setTimeout(r, 0)));
    expect(within(screen.getByRole("navigation", { name: "Settings" })).queryByRole("link", { name: "Activity" })).toBeNull();
  });

  it.each([
    ["an admin", admin],
    ["a bootstrap admin", { mode: "oidc", name: "root", admin: true }],
    ["anyone without logins", { mode: "none", admin: true }],
  ] as const)("shows the Activity tab to %s", async (_, me) => {
    open("/t/a/settings", me, page([]));
    const nav = within(screen.getByRole("navigation", { name: "Settings" }));
    expect((await nav.findByRole("link", { name: "Activity" })).getAttribute("href")).toBe("/t/a/settings/activity");
  });
});
