import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { archivePage } from "./Archive";
import { App } from "./App";
import type { Me, SnapshotSummary, Team } from "./api";
import { json, team } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const viewer: Me = { mode: "oidc", name: "lee", roles: { a: "viewer" } };

const summary = (n: number, extra: Partial<SnapshotSummary> = {}): SnapshotSummary => ({
  format: 1,
  uid: `uid-${n}`,
  namespace: "a",
  name: `task-${n}`,
  number: n,
  title: `Task ${n}`,
  phase: "Done",
  finishedAt: "2026-10-01T09:00:00Z",
  archivedAt: "2026-10-31T09:00:00Z",
  archiveAfter: "720h0m0s",
  ...extra,
});

// open renders team a's archive, with /me and the Team as given and the
// archive's answers in turn, the last one repeated; it returns the archive
// URLs read.
function open(me: Me, theTeam: Team, ...answers: (() => Promise<Response>)[]) {
  const reads: string[] = [];
  vi.stubGlobal("fetch", (url: string) => {
    if (url.endsWith("/me")) return json(me);
    if (url.endsWith("/teams")) return json({ items: [theTeam] });
    if (url.includes("/namespaces/a/archive?")) return answers[Math.min(reads.push(url) - 1, answers.length - 1)]();
    if (url.endsWith("/namespaces/a/team")) return json(theTeam);
    return json({ items: [] });
  });
  render(
    <MemoryRouter initialEntries={["/t/a/archive"]}>
      <App />
    </MemoryRouter>,
  );
  return reads;
}

const archiving = team({ spec: { displayName: "Team A", archiveAfter: "720h0m0s" } });
const page = (items: SnapshotSummary[], total: number, offset = 0) => () => json({ items, offset, limit: archivePage, total });
const offsets = (reads: string[]) => reads.map((u) => new URL(u, "http://x").searchParams.get("offset"));

describe("Archive", () => {
  it("lists the archived tasks newest first, as the server sends them, each linked to its page", async () => {
    const reads = open(
      viewer,
      archiving,
      page([summary(9, { assignee: { kind: "Agent", name: "writer" }, phase: "Failed" }), summary(4, { finishedAt: undefined })], 2),
    );
    const table = within(await screen.findByRole("table"));
    expect(screen.getByRole("heading", { name: "Archive" })).toBeTruthy();
    expect(table.getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["#", "Title", "Phase", "Assignee", "Finished", "Archived"]);
    const rows = table.getAllByRole("row").slice(1);
    expect(rows.map((r) => within(r).getAllByRole("cell")[0].textContent)).toEqual(["#9", "#4"]);
    const first = within(rows[0]).getAllByRole("cell");
    expect(within(first[1]).getByRole("link", { name: "Task 9" }).getAttribute("href")).toBe("/t/a/archive/uid-9");
    expect(first[2].textContent).toBe("Failed");
    expect(first[3].textContent).toBe("writer");
    expect(first[4].querySelector("time")?.getAttribute("datetime")).toBe("2026-10-01T09:00:00Z");
    expect(first[5].querySelector("time")?.getAttribute("datetime")).toBe("2026-10-31T09:00:00Z");
    expect(within(rows[1]).getAllByRole("cell")[4].textContent).toBe("");
    expect(new URL(reads[0], "http://x").searchParams.toString()).toBe(`offset=0&limit=${archivePage}`);
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  });

  it("loads the next page by offset while there are more than shown, and drops an item a later page repeats", async () => {
    const first = Array.from({ length: archivePage }, (_, i) => summary(200 - i));
    // A task archived between the reads moved the first page's last one down.
    const second = [first[archivePage - 1], summary(10)];
    const reads = open(viewer, archiving, page(first, archivePage + 1), page(first, archivePage + 2), page(second, archivePage + 2, archivePage));
    const more = await screen.findByRole("button", { name: "Load more" });
    act(() => more.focus());
    fireEvent.click(more);
    expect(await screen.findByText("Task 10")).toBeTruthy();
    expect(offsets(reads)).toEqual(["0", "0", String(archivePage)]);
    const rows = within(screen.getByRole("table")).getAllByRole("row").slice(1);
    expect(rows).toHaveLength(archivePage + 1);
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
    await vi.waitFor(() => expect(document.activeElement).toBe(screen.getByRole("region", { name: "Archived tasks" })));
  });

  it("offers no Load more when a full page is all there is, by the total", async () => {
    const all = Array.from({ length: archivePage }, (_, i) => summary(100 - i));
    open(viewer, archiving, page(all, archivePage));
    expect(await screen.findByText("Task 100")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  });

  it("says when there are no archived tasks", async () => {
    open(viewer, archiving, page([], 0));
    expect(await screen.findByText("No archived tasks.")).toBeTruthy();
    expect(screen.queryByText(/Archiving is off/)).toBeNull();
  });

  it("says archiving is off when the Team sets no archiveAfter", async () => {
    open(viewer, team({ spec: { displayName: "Team A", deleteFinishedAfter: "720h0m0s" } }), page([], 0));
    expect(await screen.findByText("Archiving is off for this team; an admin sets Archive after in Settings.")).toBeTruthy();
    expect(screen.getByText("No archived tasks.")).toBeTruthy();
  });

  it("reads again every 15 seconds", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const reads = open(viewer, archiving, page([], 0), page([summary(3)], 1));
      await vi.waitFor(() => expect(reads).toHaveLength(1));
      await act(() => vi.advanceTimersByTimeAsync(14000));
      expect(reads).toHaveLength(1);
      await act(() => vi.advanceTimersByTimeAsync(1000));
      expect(reads).toHaveLength(2);
      expect(await screen.findByText("Task 3")).toBeTruthy();
    } finally {
      cleanup();
      vi.useRealTimers();
    }
  });

  it("offers Retry after a failed read, and reads again on it", async () => {
    const reads = open(viewer, archiving, () => json({ error: { code: "unavailable", message: "try later" } }, 503), page([summary(1)], 1));
    expect(await screen.findByText("Could not load the archive: try later")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Task 1")).toBeTruthy();
    expect(reads).toHaveLength(2);
  });

  it("shows a 403 without Retry", async () => {
    open({ mode: "oidc", username: "kim" }, archiving, () => json({ error: { code: "no_role", message: "you have no role in a" } }, 403));
    expect(await screen.findByText("Could not load the archive: you have no role in a")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });
});
