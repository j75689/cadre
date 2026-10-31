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
// It returns how many times each path was read.
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
  return reads;
}

const list = "/namespaces/a/schedules";
const one = "/namespaces/a/schedules/nightly";

// As schedules.go in internal/controller reports them: a suspended one is Ready.
const schedules: AgentSummary[] = [
  { name: "nightly", ready: true, reason: "Scheduled", message: "next at 2026-11-01T03:00:00Z" },
  { name: "paused", ready: true, reason: "Suspended" },
  { name: "broken", ready: false, reason: "InvalidSchedule", message: "unknown time zone Mars/Base" },
  { name: "fresh", ready: false, reason: "Pending", message: "its current spec has not been checked yet" },
];

describe("Schedules", () => {
  it("shows each schedule's readiness, reason and message, linked to its page", async () => {
    at("/t/a/schedules", { [list]: [() => json({ items: schedules })] });
    const rows = await screen.findAllByRole("row");
    expect(rows).toHaveLength(5);
    const [, nightly, paused, broken, fresh] = rows.map((r) => within(r));
    expect(nightly.getByText("Ready")).toBeTruthy();
    expect(nightly.getByText("Scheduled")).toBeTruthy();
    expect(nightly.getByRole("link", { name: "nightly" }).getAttribute("href")).toBe("/t/a/schedules/nightly");
    expect(paused.getByText("Ready")).toBeTruthy();
    expect(paused.getByText("Suspended")).toBeTruthy();
    expect(broken.getByText("InvalidSchedule")).toBeTruthy();
    expect(broken.getByText("unknown time zone Mars/Base")).toBeTruthy();
    expect(fresh.getAllByText("Pending")).toHaveLength(1);
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

// The Schedule nightly as each role is shown it (project() in internal/api/config.go):
// nothing is masked, so a member's is an admin's.
const viewer: ConfigResource = {
  name: "nightly",
  ready: true,
  reason: "Suspended",
  kind: "Schedule",
  generation: 3,
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
    expect(within(screen.getByRole("region", { name: "Conditions table" })).getByText("(older spec)")).toBeTruthy();
    expect(screen.getByText("u-7")).toBeTruthy();
    expect(screen.getByText("The spec and status are shown to the team's members and admins.")).toBeTruthy();
    expect(screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent)).toEqual(["Conditions", "Metadata", "YAML"]);
    expect(await yaml()).not.toContain("spec:");
    expect(screen.getByText("No spec; not a manifest to apply.")).toBeTruthy();
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

// schedule is a Schedule whose tasks go to assignee.
const schedule = (name: string, assignee?: { kind: string; name: string }): ConfigResource => ({
  ...member,
  name,
  spec: { cron: "@daily", task: { title: name, ...(assignee && { assignee }) } },
});

describe("An Agent's Used by", () => {
  const agent = "/namespaces/a/agents/bot";
  const bot: ConfigResource = { ...viewer, name: "bot", kind: "Agent", spec: { modelRef: "claude" } };
  const usedBy = async () => within((await screen.findByRole("heading", { name: "Used by" })).closest("section")!);

  it("links the Schedules whose tasks go to the agent, and only those", async () => {
    at("/t/a/agents/bot", {
      [agent]: [() => json(bot)],
      [list]: [() => json({ items: ["nightly", "weekly", "other", "user", "gone", "none"].map((name) => ({ name, ready: true })) })],
      [one]: [() => json(schedule("nightly", { kind: "Agent", name: "bot" }))],
      "/schedules/weekly": [() => json(schedule("weekly", { kind: "Agent", name: "bot" }))],
      "/schedules/other": [() => json(schedule("other", { kind: "Agent", name: "writer" }))],
      "/schedules/user": [() => json(schedule("user", { kind: "User", name: "bot" }))],
      "/schedules/gone": [() => json({ error: { code: "not_found", message: "not found" } }, 404)],
      "/schedules/none": [() => json(schedule("none"))],
    });
    const card = await usedBy();
    await waitFor(() => expect(card.getAllByRole("link").map((l) => [l.textContent, l.getAttribute("href")])).toEqual([
      ["nightly", "/t/a/schedules/nightly"],
      ["weekly", "/t/a/schedules/weekly"],
    ]));
    expect(screen.queryByText(/Could not load the schedules/)).toBeNull();
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
      [list]: [() => json({ error: { code: "Unavailable", message: "not synced" } }, 503), () => json({ items: [{ name: "nightly", ready: true }] })],
      [one]: [() => json(schedule("nightly", { kind: "Agent", name: "bot" }))],
    });
    expect(await screen.findByText("Could not load the schedules: not synced")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await (await usedBy()).findByRole("link", { name: "nightly" })).toBeTruthy();
    expect(screen.queryByText(/Could not load the schedules/)).toBeNull();
  });

  it("is not shown to a viewer, who reads no Schedule", async () => {
    const reads = at("/t/a/agents/bot", { [agent]: [() => json({ ...bot, spec: undefined })], [list]: [() => json({ items: [] })] });
    expect(await screen.findByText("The spec and status are shown to the team's members and admins.")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Used by" })).toBeNull();
    expect(reads[list]).toBeUndefined();
  });
});
