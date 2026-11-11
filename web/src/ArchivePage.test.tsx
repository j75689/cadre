import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import type { Snapshot, Team } from "./api";
import { entry, json, team } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const stepRun = "01JSTEPRUN0000000000000000";
const runtimeRun = "01JRUNTIME0000000000000000";

// The provider's JSON as stored: the page shows it as it came, < included.
const steps = [
  { step: 0, kind: "start", provider: "anthropic", model: "claude-opus-5-5", request: { system: "be <brief>" }, at: "2026-10-01T08:00:00Z" },
  { step: 1, kind: "llm_turn", text: "Done <b>now</b>", calls: [{ id: "c1", name: "thread_post" }, { id: "c2", name: "task_files_write" }], at: "2026-10-01T08:01:00Z" },
];

const snapshot: Snapshot = {
  format: 1,
  uid: "u1",
  namespace: "a",
  name: "ship",
  number: 7,
  title: "Ship it",
  phase: "Done",
  priority: "high",
  assignee: { kind: "Agent", name: "writer" },
  labels: ["web"],
  finishedAt: "2026-10-01T09:00:00Z",
  archivedAt: "2026-10-31T09:00:00Z",
  archiveAfter: "720h0m0s",
  task: {
    metadata: { name: "ship", namespace: "a" },
    spec: { title: "Ship it", description: "Ship the archive pages.", assignee: { kind: "Agent", name: "writer" }, priority: "high", labels: ["web"] },
    status: { number: 7, phase: "Done" },
  },
  thread: [entry(1, "Comment", { body: "Please ship" }), entry(2, "Comment", { body: "Shipped" })],
  runs: [
    { id: stepRun, mode: "step", runnerName: "builtin", reason: "finished", outcome: "Done", endedAt: "2026-10-01T08:02:00Z" },
    { id: runtimeRun, mode: "runtime", runnerName: "ci", reason: "exited", outcome: "Review", endedAt: "2026-09-30T08:02:00Z" },
  ],
  events: {
    [runtimeRun]: [
      { runId: runtimeRun, seq: 1, progress: "building" },
      { runId: runtimeRun, seq: 2, inputTokens: 1200, outputTokens: 300 },
    ],
  },
  transcripts: { [stepRun]: steps },
  files: [{ path: "notes/plan.md", version: 3, size: 11, content: "# The plan" }],
};

// open renders the archived task u1 of team a, the Team and the snapshot
// answered as given; it returns the snapshot reads.
function open(snap: () => Promise<Response>, theTeam: () => Promise<Response> = () => json(team())) {
  const reads: string[] = [];
  vi.stubGlobal("fetch", (url: string) => {
    if (url.endsWith("/me")) return json({ mode: "oidc", name: "lee", roles: { a: "viewer" } });
    if (url.endsWith("/teams")) return json({ items: [team()] });
    if (url.endsWith("/namespaces/a/archive/u1")) {
      reads.push(url);
      return snap();
    }
    if (url.endsWith("/namespaces/a/team")) return theTeam();
    return json({ items: [] });
  });
  render(
    <MemoryRouter initialEntries={["/t/a/archive/u1"]}>
      <App />
    </MemoryRouter>,
  );
  return reads;
}

const withRetention = (archiveRetention: string): Team => team({ spec: { displayName: "Team A", archiveAfter: "720h0m0s", archiveRetention } });

describe("ArchivePage", () => {
  it("shows the header, the description and the thread of the snapshot, read once", async () => {
    const reads = open(() => json(snapshot));
    const heading = await screen.findByRole("heading", { level: 1 });
    expect(heading.textContent).toBe("#7 Ship it");
    const head = heading.closest<HTMLElement>(".title-block")!;
    expect(within(head).getByText("Done")).toBeTruthy();
    expect(within(head).getByText("high")).toBeTruthy();
    expect(within(head).getByText("web")).toBeTruthy();
    expect(within(head).getByText("writer")).toBeTruthy();
    expect(screen.getByText("Ship the archive pages.")).toBeTruthy();
    expect(screen.getByText("Please ship")).toBeTruthy();
    expect(screen.getByText("Shipped")).toBeTruthy();
    expect(within(screen.getByRole("navigation", { name: "Breadcrumb" })).getByRole("link", { name: "Archive" }).getAttribute("href")).toBe("/t/a/archive");
    await new Promise((r) => setTimeout(r, 20));
    expect(reads).toHaveLength(1);
  });

  it.each([
    ["the Team's retention", () => json(withRetention("2160h0m0s")), "2160h0m0s"],
    ["90 days when the Team sets none", () => json(team()), "90 days"],
    ["90 days when the Team cannot be read", () => json({ error: { code: "no_role", message: "no" } }, 403), "90 days"],
  ] as [string, () => Promise<Response>, string][])("says it is archived and read-only, with %s", async (_, theTeam, retention) => {
    open(() => json(snapshot), theTeam);
    const banner = await screen.findByText(/^Archived/);
    await waitFor(() => expect(banner.textContent).toMatch(new RegExp(`; read-only, removed ${retention} after the archive\\.$`)));
    expect(banner.querySelector("time")?.getAttribute("datetime")).toBe("2026-10-31T09:00:00Z");
  });

  it("has no actions: no composer, no cancel, no buttons at all", async () => {
    open(() => json(snapshot));
    await screen.findByRole("heading", { level: 1 });
    const main = within(screen.getByRole("main"));
    expect(main.queryByRole("region", { name: "Actions" })).toBeNull();
    expect(main.queryAllByRole("textbox")).toHaveLength(0);
    expect(main.queryAllByRole("button")).toHaveLength(0);
    expect(screen.queryByText(/Subtasks/)).toBeNull();
    expect(screen.queryByText(/Run in progress/)).toBeNull();
  });

  it("lists the runs with their events and tokens, without links", async () => {
    open(() => json(snapshot));
    const runs = within(await screen.findByRole("region", { name: "Runs" }));
    const items = runs.getAllByRole("listitem").filter((li) => li.classList.contains("archive-run"));
    expect(items.map((li) => li.querySelector("code")?.textContent)).toEqual([stepRun, runtimeRun]);
    const step = within(items[0]);
    expect(step.getByText("step on builtin: finished")).toBeTruthy();
    expect(step.getByText("No events.")).toBeTruthy();
    const runtime = within(items[1]);
    expect(runtime.getByText("runtime on ci: exited")).toBeTruthy();
    expect(runtime.getByText("Review")).toBeTruthy();
    expect(runtime.getByText(/progress: building/)).toBeTruthy();
    expect(runtime.getByText("1,200 in · 300 out")).toBeTruthy();
    expect(runs.queryAllByRole("link")).toHaveLength(0);
  });

  it("collapses a step run's transcript, and shows its steps and their JSON once opened", async () => {
    open(() => json(snapshot));
    const runs = within(await screen.findByRole("region", { name: "Runs" }));
    const summary = runs.getByText("Transcript (2 steps)");
    const details = summary.closest("details")!;
    expect(details.open).toBe(false);
    expect(runs.queryByText("Done <b>now</b>")).toBeNull();

    details.open = true;
    fireEvent(details, new Event("toggle"));
    const transcript = within(await waitFor(() => details.querySelector<HTMLElement>("ol.transcript")!));
    const rows = transcript.getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(rows[0].querySelector(".line")?.textContent).toMatch(/^#0 start /);
    expect(within(rows[0]).getByText("model claude-opus-5-5")).toBeTruthy();
    expect(rows[1].querySelector(".line")?.textContent).toMatch(/^#1 llm_turn /);
    expect(rows[1].querySelector("time")?.getAttribute("datetime")).toBe("2026-10-01T08:01:00Z");
    expect(within(rows[1]).getByText("Done <b>now</b>")).toBeTruthy();
    expect(within(rows[1]).getByText("calls thread_post, task_files_write")).toBeTruthy();
    expect(within(rows[1]).queryByText(/^model/)).toBeNull();
    // Each step's JSON is collapsed in it, as stored.
    const json0 = within(rows[0]).getByText("JSON of step 0").closest("details")!;
    expect(json0.open).toBe(false);
    expect(JSON.parse(json0.querySelector("pre")!.textContent!)).toEqual(steps[0]);
    expect(json0.querySelector("pre")!.textContent).toContain('"system": "be <brief>"');
  });

  it("shows no transcript for a runtime run", async () => {
    open(() => json(snapshot));
    const runs = within(await screen.findByRole("region", { name: "Runs" }));
    const runtime = runs.getAllByRole("listitem").find((li) => li.querySelector("code")?.textContent === runtimeRun)!;
    expect(within(runtime).queryByText(/^Transcript/)).toBeNull();
    expect(runs.getAllByText(/^Transcript/)).toHaveLength(1);
  });

  it("shows the files collapsed, with path, version, size and content", async () => {
    open(() => json(snapshot));
    const files = within(await screen.findByRole("region", { name: "Files" }));
    const details = files.getByText("notes/plan.md").closest("details")!;
    expect(details.open).toBe(false);
    expect(within(details).getByText("version 3, 11 bytes")).toBeTruthy();
    expect(details.querySelector("pre")?.textContent).toBe("# The plan");
  });

  it("says an archived task that is not there is gone, with a link back", async () => {
    open(() => json({ error: { code: "not_found", message: "snapshot u1: not found" } }, 404));
    expect(await screen.findByText("This archived task is gone or never existed.")).toBeTruthy();
    expect(screen.getByRole("link", { name: "All archived tasks" }).getAttribute("href")).toBe("/t/a/archive");
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it("says an archived task with a uid the server refuses is gone, without Retry", async () => {
    open(() => json({ error: { code: "invalid", message: 'uid "a/b" must be 1 to 64 letters, digits and dashes' } }, 400));
    expect(await screen.findByText("This archived task is gone or never existed.")).toBeTruthy();
    expect(screen.queryByText(/Could not load/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it("says an archived task is gone when Retry finds it gone, and drops the error", async () => {
    let n = 0;
    const reads = open(() =>
      n++ === 0 ? json({ error: { code: "internal", message: "boom" } }, 500) : json({ error: { code: "not_found", message: "snapshot u1: not found" } }, 404),
    );
    expect(await screen.findByText("Could not load the archived task: boom")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("This archived task is gone or never existed.")).toBeTruthy();
    expect(screen.getByRole("link", { name: "All archived tasks" }).getAttribute("href")).toBe("/t/a/archive");
    expect(screen.queryByText(/Could not load/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(reads).toHaveLength(2);
  });

  it("shows stored HTML as text, never as elements", async () => {
    const html = '<img src=x onerror="alert(1)"><script>alert(2)</script>';
    const step = { step: 0, kind: "llm_turn", text: html, request: { system: html }, at: "2026-10-01T08:00:00Z" };
    open(() =>
      json({
        ...snapshot,
        task: { ...snapshot.task, spec: { ...snapshot.task.spec, description: html } },
        thread: [entry(1, "Comment", { body: html })],
        transcripts: { [stepRun]: [step] },
        files: [{ path: "notes/x.html", version: 1, size: html.length, content: html }],
      }),
    );
    const main = (await screen.findByRole("heading", { level: 1 })).closest("main")!;
    const details = within(main).getByText("Transcript (1 step)").closest("details")!;
    details.open = true;
    fireEvent(details, new Event("toggle"));
    const transcript = await waitFor(() => details.querySelector<HTMLElement>("ol.transcript")!);

    expect(within(main).getByRole("region", { name: "Description" }).querySelector("pre")?.textContent).toBe(html);
    expect(within(within(main).getByRole("region", { name: "Thread" })).getByText(html)).toBeTruthy();
    expect(within(transcript).getByText(html)).toBeTruthy();
    expect(JSON.parse(transcript.querySelector("pre")!.textContent!)).toEqual(step);
    expect(within(main).getByRole("region", { name: "Files" }).querySelector("pre")?.textContent).toBe(html);
    expect(main.querySelectorAll("img, script")).toHaveLength(0);
  });

  it("offers Retry after a failed read, and reads again on it", async () => {
    let n = 0;
    const reads = open(() => (n++ === 0 ? json({ error: { code: "unavailable", message: "try later" } }, 503) : json(snapshot)));
    expect(await screen.findByText("Could not load the archived task: try later")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect((await screen.findByRole("heading", { level: 1 })).textContent).toBe("#7 Ship it");
    expect(screen.queryByText(/Could not load/)).toBeNull();
    expect(reads).toHaveLength(2);
  });

  it("shows a 403 without Retry", async () => {
    open(() => json({ error: { code: "no_role", message: "you have no role in a" } }, 403));
    expect(await screen.findByText("Could not load the archived task: you have no role in a")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });
});
