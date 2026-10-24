import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it } from "vitest";
import { phases, type Phase, type Task } from "./api";
import { Columns, matches, type Column } from "./Board";

afterEach(cleanup);

function task(name: string, phase: Phase, extra: Partial<Task["spec"]> = {}, number?: number): Task {
  return {
    metadata: { name, namespace: "team-a" },
    spec: { title: `Title of ${name}`, ...extra },
    status: { phase, number, ...(phase === "Waiting" ? { waiting: { reason: "Question" } } : {}) },
  };
}

function columns(tasks: Task[], more: Phase[] = []): Column[] {
  return phases.map((phase) => ({ phase, tasks: tasks.filter((t) => t.status?.phase === phase), more: more.includes(phase) }));
}

function renderBoard(cols: Column[], search = "") {
  render(
    <MemoryRouter>
      <Columns ns="team-a" columns={cols} search={search} onMore={() => {}} />
    </MemoryRouter>,
  );
}

describe("Columns", () => {
  it("puts each task in its phase's column, in all seven", () => {
    renderBoard(columns([task("a", "Todo"), task("b", "Running"), task("c", "Waiting"), task("d", "Done"), task("e", "Todo")]));
    expect(screen.getAllByRole("region").map((r) => r.getAttribute("aria-label"))).toEqual([...phases]);
    expect(within(screen.getByRole("region", { name: "Todo" })).getAllByRole("link")).toHaveLength(2);
    expect(within(screen.getByRole("region", { name: "Running" })).getByText("Title of b")).toBeTruthy();
    expect(within(screen.getByRole("region", { name: "Review" })).queryAllByRole("link")).toHaveLength(0);
  });

  it("shows the card's fields and links to the task", () => {
    renderBoard(columns([task("c", "Waiting", { priority: "urgent", assignee: { kind: "Agent", name: "writer" }, labels: ["ops"] }, 12)]));
    const card = screen.getByRole("link");
    expect(card.getAttribute("href")).toBe("/t/team-a/tasks/c");
    for (const text of ["#12", "urgent", "agent writer", "waiting: Question", "ops"]) {
      expect(within(card).getByText(text)).toBeTruthy();
    }
  });

  it("offers more only where the last page was full", () => {
    renderBoard(columns([task("a", "Done")], ["Done"]));
    expect(within(screen.getByRole("region", { name: "Done" })).getByRole("button", { name: "Load more" })).toBeTruthy();
    expect(within(screen.getByRole("region", { name: "Todo" })).queryByRole("button")).toBeNull();
  });

  it("filters the loaded page by the search", () => {
    renderBoard(columns([task("a", "Todo", { labels: ["ops"] }), task("b", "Todo")]), "ops");
    expect(screen.getAllByRole("link")).toHaveLength(1);
  });
});

describe("matches", () => {
  it("searches title, name, assignee, labels and number", () => {
    const t = task("fix-login", "Todo", { assignee: { kind: "User", name: "dana" }, labels: ["auth"] }, 7);
    for (const q of ["title of", "FIX-LOGIN", "dana", "auth", "#7", ""]) expect(matches(t, q)).toBe(true);
    expect(matches(t, "nothing")).toBe(false);
  });
});
