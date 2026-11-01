import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { AgentSummary, ConfigResource } from "./api";
import { App } from "./App";
import { json } from "./testing";

// fetched is every path the page has requested, answered or not.
const fetched: string[] = [];

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  fetched.length = 0;
});

// at opens path in team a; the calls to the paths in answers get their
// answers in turn, the last one repeated, and the other calls never answer.
// It returns how many times each path was read.
function at(path: string, answers: Record<string, (() => Promise<Response>)[]>) {
  const reads: Record<string, number> = {};
  vi.stubGlobal("fetch", (url: string) => {
    fetched.push(url);
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
  return reads;
}

const list = "/namespaces/a/schedules";
const one = "/namespaces/a/schedules/nightly";

// As GET /schedules answers a member (the Schedule row's summary in
// internal/api/config.go): why when not Ready, Suspended for a suspended Ready
// one (paused), and whom the tasks go to. fresh is suspended too, but its
// current spec is unchecked, so it is Pending. A viewer's has no assignee.
const schedules: AgentSummary[] = [
  { name: "nightly", ready: true, assignee: { kind: "Agent", name: "bot" } },
  { name: "paused", ready: true, reason: "Suspended", assignee: { kind: "User", name: "ops@example.com" } },
  { name: "broken", ready: false, reason: "InvalidSchedule", message: "unknown time zone Mars/Base" },
  { name: "fresh", ready: false, reason: "Pending", message: "its current spec has not been checked yet", assignee: { kind: "Agent", name: "bot" } },
];

const viewerSchedules: AgentSummary[] = schedules.map(({ name, ready, reason, message }) => ({ name, ready, reason, message }));

describe("Schedules", () => {
  it.each([
    ["a member", schedules],
    ["a viewer", viewerSchedules],
  ])("shows %s each schedule's readiness and reason as served, linked to its page", async (_, items) => {
    at("/t/a/schedules", { [list]: [() => json({ items })] });
    const rows = await screen.findAllByRole("row");
    expect(rows).toHaveLength(5);
    const [, nightly, paused, broken, fresh] = rows.map((r) => within(r));
    expect(nightly.getByText("Ready")).toBeTruthy();
    expect(nightly.getByRole("link", { name: "nightly" }).getAttribute("href")).toBe("/t/a/schedules/nightly");
    expect(paused.getByText("Ready")).toBeTruthy();
    expect(paused.getByText("Suspended")).toBeTruthy();
    expect(broken.getByText("InvalidSchedule")).toBeTruthy();
    expect(broken.getByText("unknown time zone Mars/Base")).toBeTruthy();
    expect(fresh.getAllByText("Pending")).toHaveLength(1);
    expect(fresh.queryByText("Suspended")).toBeNull();
    expect(screen.getByRole("region", { name: "Schedules" })).toBeTruthy();
    expect(document.title).toBe("Schedules · a · Cadre");
  });

  it("says how schedules are added when there are none", async () => {
    at("/t/a/schedules", { [list]: [() => json({ items: [] })] });
    expect(await screen.findByText(/This team has no schedules/)).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows a banner with Retry when the schedules cannot be read", async () => {
    at("/t/a/schedules", { [list]: [() => json({ error: { code: "Unavailable", message: "not synced" } }, 503)] });
    expect(await screen.findByText("Could not load the schedules: not synced")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });

  it("reads them again every 15 seconds without a watch stream", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const reads = at("/t/a/schedules", { [list]: [() => json({ items: schedules })] });
      await screen.findAllByRole("row");
      expect(reads[list]).toBe(0);
      await act(() => vi.advanceTimersByTimeAsync(15000));
      expect(reads[list]).toBe(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("is a tab of the team nav, current on its pages", async () => {
    at("/t/a/schedules/nightly", {});
    const tab = within(screen.getByRole("navigation", { name: "Team" })).getByRole("link", { name: "Schedules" });
    expect(tab.getAttribute("href")).toBe("/t/a/schedules");
    expect(tab.getAttribute("aria-current")).toBe("page");
    expect(await screen.findByText(/Loading the schedule/)).toBeTruthy();
  });
});

// The Schedule nightly, suspended, as each role is shown it (configView() in
// internal/api/config.go): Ready with no reason, which only its Ready condition
// has; nothing is masked, so a member's is an admin's.
const viewer: ConfigResource = {
  name: "nightly",
  ready: true,
  kind: "Schedule",
  generation: 2,
  observedGeneration: 2,
  conditions: [{ type: "Ready", status: "True", reason: "Suspended", message: "", observedGeneration: 2 }],
  metadata: { uid: "u-7", resourceVersion: "9", creationTimestamp: "2026-10-25T08:00:00Z", labels: { team: "a" } },
};

const member: ConfigResource = {
  ...viewer,
  metadata: { ...viewer.metadata, annotations: { owner: "dana" } },
  spec: { cron: "0 3 * * *", timezone: "Asia/Taipei", suspend: true, task: { title: "Triage", description: "token: (set)", assignee: { kind: "Agent", name: "bot" } } },
  status: { observedGeneration: 2, lastTask: "nightly-202610310300" },
};

const yaml = async () => (await screen.findByRole("region", { name: "YAML text" })).textContent ?? "";

describe("ScheduleDetail", () => {
  const open = (...answers: (() => Promise<Response>)[]) => at("/t/a/schedules/nightly", { [one]: answers });

  it("shows a skeleton while the schedule is read", async () => {
    open(() => new Promise(() => {}));
    expect(await screen.findByText(/Loading the schedule/)).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Conditions" })).toBeNull();
  });

  it("shows a viewer its health and metadata, with no spec", async () => {
    open(() => json(viewer));
    expect(await screen.findByText("Schedule")).toBeTruthy();
    expect(screen.getByRole("heading", { level: 1, name: "nightly" })).toBeTruthy();
    expect(screen.getByText("Ready", { selector: ".badge" })).toBeTruthy();
    expect(screen.getByText("Suspended:")).toBeTruthy();
    expect(document.title).toBe("nightly · Schedules · a · Cadre");
    const crumbs = within(screen.getByRole("navigation", { name: "Breadcrumb" }));
    expect(crumbs.getByRole("link", { name: "Schedules" }).getAttribute("href")).toBe("/t/a/schedules");
    expect(screen.getByText("u-7")).toBeTruthy();
    expect(screen.getByText("The spec and status are shown to the team's members and admins.")).toBeTruthy();
    expect(screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent)).toEqual(["Conditions", "Metadata", "YAML"]);
    expect(await yaml()).not.toContain("spec:");
    expect(screen.getByText("No spec; not a manifest to apply.")).toBeTruthy();
  });

  // Its spec changed since the condition: the server says Pending, and the
  // page does not take the older condition's Suspended for it.
  it("shows Pending as the server says it, not the older condition's reason", async () => {
    open(() => json({ ...viewer, ready: false, reason: "Pending", message: "its current spec has not been checked yet", generation: 3 }));
    expect(await screen.findByText("its current spec has not been checked yet")).toBeTruthy();
    expect(screen.getByText("Pending", { selector: ".badge" })).toBeTruthy();
    expect(screen.queryByText("Suspended:")).toBeNull();
    expect(within(screen.getByRole("region", { name: "Conditions table" })).getByText("(older spec)")).toBeTruthy();
  });

  // A member's and an admin's answers are the same: the task template is not masked.
  it("shows a member the whole spec as a manifest, the status under it", async () => {
    const writeText = vi.fn(() => Promise.resolve());
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    open(() => json(member));
    const text = await yaml();
    expect(text).toBe(
      "apiVersion: cadre.dev/v1alpha1\nkind: Schedule\nmetadata:\n  name: nightly\n  namespace: a\n  labels:\n    team: a\n  annotations:\n    owner: dana\n" +
        'spec:\n  cron: "0 3 * * *"\n  timezone: Asia/Taipei\n  suspend: true\n  task:\n    title: Triage\n    description: "token: (set)"\n    assignee:\n      kind: Agent\n      name: bot\n',
    );
    expect(screen.getByRole("region", { name: "Status" }).textContent).toContain("lastTask: nightly-202610310300");
    expect(screen.queryByText(/hidden from members|not applyable|not a manifest/)).toBeNull();
    expect(screen.getByText("owner=dana")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Copy manifest" }));
    expect(await screen.findByText("Copied")).toBeTruthy();
    expect(writeText).toHaveBeenCalledWith(text);
  });

  it("says when there is no such schedule, with a way back", async () => {
    open(() => json({ error: { code: "not_found", message: "schedule nightly not found" } }, 404));
    expect((await screen.findByText(/No schedule named/)).textContent).toBe("No schedule named nightly in this team.");
    expect(screen.getByRole("link", { name: "All schedules" }).getAttribute("href")).toBe("/t/a/schedules");
  });

  it("shows a refusal, as in a namespace without a Team, without Retry", async () => {
    open(() => json({ error: { code: "no_role", message: "you have no role in namespace a" } }, 403));
    expect(await screen.findByText("Could not load the schedule: you have no role in namespace a")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it("shows a failed read with Retry, which reads again", async () => {
    open(() => json({ error: { code: "Unavailable", message: "not synced" } }, 503), () => json(viewer));
    expect(await screen.findByText("Could not load the schedule: not synced")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("region", { name: "Conditions table" })).toBeTruthy();
    await waitFor(() => expect(screen.queryByText(/Could not load the schedule/)).toBeNull());
  });
});

describe("An Agent's Used by", () => {
  const agent = "/namespaces/a/agents/bot";
  const bot: ConfigResource = { ...viewer, name: "bot", kind: "Agent", spec: { modelRef: "claude" } };
  const usedBy = async () => within((await screen.findByRole("heading", { name: "Used by" })).closest("section")!);

  // schedules is the paths of Schedule reads, the list's and each one's.
  const schedules = () => fetched.filter((u) => /\/namespaces\/a\/schedules(\/|$)/.test(u));

  it("links the Schedules whose tasks go to the agent, and only those, from one list read", async () => {
    const assigned = (name: string, assignee?: { kind: string; name: string }): AgentSummary => ({ name, ready: true, ...(assignee && { assignee }) });
    at("/t/a/agents/bot", {
      [agent]: [() => json(bot)],
      [list]: [
        () =>
          json({
            items: [
              assigned("nightly", { kind: "Agent", name: "bot" }),
              assigned("none"),
              assigned("other", { kind: "Agent", name: "writer" }),
              assigned("user", { kind: "User", name: "bot" }),
              { ...assigned("weekly", { kind: "Agent", name: "bot" }), reason: "Suspended" },
            ],
          }),
      ],
      [one]: [() => json(member)],
    });
    const card = await usedBy();
    await waitFor(() => expect(card.getAllByRole("link").map((l) => [l.textContent, l.getAttribute("href")])).toEqual([
      ["nightly", "/t/a/schedules/nightly"],
      ["weekly", "/t/a/schedules/weekly"],
    ]));
    expect(screen.queryByText(/Could not load the schedules/)).toBeNull();
    // No Schedule is read on its own, now or later in the same poll.
    await act(() => new Promise((r) => setTimeout(r, 50)));
    expect(schedules()).toEqual(["/api/v1/namespaces/a/schedules"]);
    fireEvent.click(card.getByRole("link", { name: "nightly" }));
    expect(await screen.findByRole("heading", { level: 1, name: "nightly" })).toBeTruthy();
  });

  it("says None when no Schedule creates tasks for it", async () => {
    at("/t/a/agents/bot", { [agent]: [() => json(bot)], [list]: [() => json({ items: [] })] });
    expect(await (await usedBy()).findByText("None")).toBeTruthy();
  });

  it("shows a failed read with Retry, which reads again", async () => {
    at("/t/a/agents/bot", {
      [agent]: [() => json(bot)],
      [list]: [() => json({ error: { code: "Unavailable", message: "not synced" } }, 503), () => json({ items: [{ name: "nightly", ready: true, assignee: { kind: "Agent", name: "bot" } }] })],
    });
    expect(await screen.findByText("Could not load the schedules: not synced")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await (await usedBy()).findByRole("link", { name: "nightly" })).toBeTruthy();
    expect(screen.queryByText(/Could not load the schedules/)).toBeNull();
  });

  it("is not shown to a viewer, who reads no Schedule", async () => {
    at("/t/a/agents/bot", { [agent]: [() => json({ ...bot, spec: undefined })], [list]: [() => json({ items: [] })] });
    expect(await screen.findByText("The spec and status are shown to the team's members and admins.")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Used by" })).toBeNull();
    await act(() => new Promise((r) => setTimeout(r, 50)));
    expect(schedules()).toEqual([]);
  });
});
