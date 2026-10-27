import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Phase, Task } from "./api";
import { sorted, TaskTable } from "./TaskTable";

afterEach(cleanup);

function task(name: string, phase: Phase, number: number | undefined, spec: Partial<Task["spec"]> = {}, created?: string): Task {
  return {
    metadata: { name, namespace: "a", creationTimestamp: created },
    spec: { title: `Title of ${name}`, ...spec },
    status: { phase, number },
  };
}

const tasks = [
  task("b", "Done", 2, { priority: "low", assignee: { kind: "Agent", name: "bot-1" } }, "2026-10-20T09:00:00Z"),
  task("a", "Todo", 3, { priority: "urgent" }, "2026-10-21T09:00:00Z"),
  task("c", "Running", 1, {}, "2026-10-19T09:00:00Z"),
];

function Query() {
  return <span data-testid="query">{useLocation().search}</span>;
}

function renderTable(url = "/t/a?view=list", more = false, onMore = () => {}) {
  render(
    <MemoryRouter initialEntries={[url]}>
      <TaskTable ns="a" tasks={tasks} more={more} onMore={onMore} />
      <Query />
    </MemoryRouter>,
  );
}

const titles = () => screen.getAllByRole("link").map((l) => l.textContent);
const header = (name: string) => screen.getByRole("button", { name }).closest("th")!;

describe("TaskTable", () => {
  it("shows a row per task with its fields, in the API's order by default", () => {
    renderTable();
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["#", "Title", "Phase", "Assignee", "Priority", "Created"]);
    expect(titles()).toEqual(["Title of a", "Title of c", "Title of b"]);
    expect(header("Priority").getAttribute("aria-sort")).toBe("ascending");
    const row = screen.getByRole("link", { name: "Title of b" }).closest("tr")!;
    expect(screen.getByRole("link", { name: "Title of b" }).getAttribute("href")).toBe("/t/a/tasks/b");
    for (const text of ["#2", "Done", "bot-1", "low"]) expect(within(row).getByText(text)).toBeTruthy();
    expect(within(row).getByText((_, el) => el?.tagName === "TIME").getAttribute("datetime")).toBe("2026-10-20T09:00:00Z");
    // medium shows too, in a table.
    expect(within(screen.getByRole("link", { name: "Title of c" }).closest("tr")!).getByText("medium")).toBeTruthy();
  });

  it("sorts by a header, ascending then descending, and keeps it in the URL", () => {
    renderTable();
    fireEvent.click(screen.getByRole("button", { name: "Title" }));
    expect(screen.getByTestId("query").textContent).toBe("?view=list&sort=title");
    expect(header("Title").getAttribute("aria-sort")).toBe("ascending");
    expect(header("Priority").getAttribute("aria-sort")).toBeNull();
    expect(titles()).toEqual(["Title of a", "Title of b", "Title of c"]);
    fireEvent.click(screen.getByRole("button", { name: "Title" }));
    expect(screen.getByTestId("query").textContent).toBe("?view=list&sort=-title");
    expect(header("Title").getAttribute("aria-sort")).toBe("descending");
    expect(titles()).toEqual(["Title of c", "Title of b", "Title of a"]);
  });

  it("opens sorted as the URL says, and back to the default without a key", () => {
    renderTable("/t/a?sort=-number");
    expect(titles()).toEqual(["Title of a", "Title of b", "Title of c"]);
    fireEvent.click(screen.getByRole("button", { name: "Priority" }));
    expect(screen.getByTestId("query").textContent).toBe("");
    fireEvent.click(screen.getByRole("button", { name: "Priority" }));
    expect(screen.getByTestId("query").textContent).toBe("?sort=-priority");
    expect(titles()).toEqual(["Title of b", "Title of c", "Title of a"]);
  });

  it("reads an unknown sort as the default", () => {
    renderTable("/t/a?sort=bogus");
    expect(titles()).toEqual(["Title of a", "Title of c", "Title of b"]);
    expect(header("Priority").getAttribute("aria-sort")).toBe("ascending");
  });

  it("offers one Load more while a column has more", () => {
    const onMore = vi.fn();
    renderTable("/t/a", true, onMore);
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(onMore).toHaveBeenCalledTimes(1);
    cleanup();
    renderTable("/t/a", false);
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  });
});

describe("sorted", () => {
  it("sorts by phase, assignee and created, the number breaking ties, a new task last", () => {
    const names = (sort: string, list = tasks) => sorted(list, sort).map((t) => t.metadata.name);
    expect(names("phase")).toEqual(["a", "c", "b"]);
    expect(names("created")).toEqual(["c", "b", "a"]);
    expect(names("-created")).toEqual(["a", "b", "c"]);
    // No assignee sorts first; c and a tie, by number.
    expect(names("assignee")).toEqual(["c", "a", "b"]);
    expect(names("number", [...tasks, task("new", "Todo", undefined)])).toEqual(["c", "b", "a", "new"]);
  });

  it("sorts a priority it does not know after low", () => {
    const names = sorted([...tasks, task("odd", "Todo", 4, { priority: "someday" })], "priority").map((t) => t.metadata.name);
    expect(names).toEqual(["a", "c", "b", "odd"]);
  });
});
