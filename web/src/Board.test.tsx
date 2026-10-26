import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { phases, type Phase, type Task } from "./api";
import { Columns, matches, type Column } from "./Board";
import { json } from "./testing";

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
    const title = screen.getByRole("link", { name: "Title of c" });
    expect(title.getAttribute("href")).toBe("/t/team-a/tasks/c");
    const card = screen.getByRole("article");
    for (const text of ["#12", "urgent", "writer", "Waiting: Question", "ops"]) {
      expect(within(card).getByText(text)).toBeTruthy();
    }
  });

  it("links a subtask to its parent", () => {
    renderBoard(columns([task("child", "Todo", { parentRef: { name: "epic" } })]));
    expect(screen.getByRole("link", { name: "epic" }).getAttribute("href")).toBe("/t/team-a/tasks/epic");
    expect(screen.getByText(/subtask of/)).toBeTruthy();
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

describe("Card", () => {
  it("marks a waiting card, pulses a running one, and shows only urgent, high and low", () => {
    renderBoard(
      columns([
        task("w", "Waiting", { priority: "high" }, 1),
        task("r", "Running", { priority: "low" }, 2),
        task("t", "Todo", {}, 3),
      ]),
    );
    const card = (name: string) => screen.getByRole("link", { name: `Title of ${name}` }).closest("article")!;
    expect(card("w").classList.contains("waiting")).toBe(true);
    expect(within(card("w")).getByText("high")).toBeTruthy();
    expect(within(card("r")).getByRole("img", { name: "running" })).toBeTruthy();
    expect(within(card("r")).getByText("low")).toBeTruthy();
    expect(card("t").querySelector(".chip")).toBeNull();
    expect(card("t").classList.contains("waiting")).toBe(false);
  });

  it("says an empty column has nothing", () => {
    renderBoard(columns([task("a", "Todo")]));
    expect(within(screen.getByRole("region", { name: "Review" })).getByText("Nothing here")).toBeTruthy();
    expect(within(screen.getByRole("region", { name: "Todo" })).queryByText("Nothing here")).toBeNull();
  });
});

describe("Board", () => {
  // serve answers the board's reads; the first read waits for go.
  function serve(tasks: Task[]) {
    let go: () => void = () => {};
    const ready = new Promise<void>((r) => (go = r));
    vi.stubGlobal("fetch", async (url: string) => {
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      if (url.endsWith("/teams")) return json({ items: [] });
      await ready;
      const phase = new URL(url, "http://cadre.test").searchParams.get("phase");
      return json({ items: tasks.filter((t) => t.status?.phase === phase), offset: 0, limit: 50 });
    });
    return () => go();
  }

  function renderPage() {
    localStorage.setItem("cadre.identity", "dana");
    render(
      <MemoryRouter initialEntries={["/t/team-a"]}>
        <App />
      </MemoryRouter>,
    );
  }

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it("shows skeleton columns, then the columns and a live status", async () => {
    const go = serve([task("a", "Todo")]);
    renderPage();
    expect(screen.getByText("Loading the board")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Todo" })).toBeNull();
    expect(screen.queryByRole("status")).toBeNull();
    await act(async () => go());
    expect(await screen.findByRole("region", { name: "Todo" })).toBeTruthy();
    expect(screen.queryByText("Loading the board")).toBeNull();
    expect(screen.getByRole("status").textContent).toBe("Up to date");
    expect(screen.getByText("Updated just now")).toBeTruthy();
  });

  it("keeps the live region's text while only the time since the last read moves", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const go = serve([task("a", "Todo")]);
    go();
    renderPage();
    await screen.findByText("Title of a");
    const status = screen.getByRole("status");
    const seen = [status.textContent];
    for (let i = 0; i < 4; i++) {
      await act(() => vi.advanceTimersByTimeAsync(5000));
      seen.push(screen.getByRole("status").textContent);
    }
    expect(new Set(seen)).toEqual(new Set(["Up to date"]));
    // The time is shown, outside the region.
    expect(status.contains(screen.getByText(/^Updated /))).toBe(false);
  });

  it("counts what the search shows of what is loaded, and clears it", async () => {
    const go = serve([task("a", "Todo", { labels: ["ops"] }), task("b", "Todo"), task("c", "Done", { labels: ["ops"] })]);
    go();
    renderPage();
    await screen.findByText("Title of a");
    expect(screen.queryByText(/shown$/)).toBeNull();
    fireEvent.change(screen.getByLabelText("Search the loaded page"), { target: { value: "ops" } });
    expect(screen.getByText("2 of 3 shown")).toBeTruthy();
    expect(screen.queryByText("Title of b")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Clear the search" }));
    expect(screen.queryByText(/shown$/)).toBeNull();
    expect(screen.getByText("Title of b")).toBeTruthy();
  });

  it("keeps the columns and says so in the status when a poll fails", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let down = false;
    vi.stubGlobal("fetch", (url: string) => {
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      if (url.endsWith("/teams")) return json({ items: [] });
      if (down) return json({ error: { code: "Unavailable", message: "not synced" } }, 503);
      return json({ items: url.includes("phase=Todo") ? [task("a", "Todo")] : [], offset: 0, limit: 50 });
    });
    renderPage();
    await screen.findByText("Title of a");
    down = true;
    await act(() => vi.advanceTimersByTimeAsync(5000));
    await vi.waitFor(() => expect(screen.getByRole("status").textContent).toBe("Could not refresh: not synced"));
    expect(screen.getByText("Title of a")).toBeTruthy();
  });
});
