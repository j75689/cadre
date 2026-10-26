import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter, useLocation, useNavigate, type NavigateFunction } from "react-router";
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

  it("matches the assignee exactly, and a task without one never", () => {
    const t = task("a", "Todo", { assignee: { kind: "Agent", name: "bot-1" } });
    expect(matches(t, "", "bot-1")).toBe(true);
    expect(matches(t, "", "bot")).toBe(false);
    expect(matches(task("b", "Todo"), "", "bot-1")).toBe(false);
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
  // reads are the phases the board has read, in order.
  let reads: string[] = [];

  // serve answers the board's reads; the first read waits for go.
  function serve(tasks: Task[]) {
    let go: () => void = () => {};
    const ready = new Promise<void>((r) => (go = r));
    reads = [];
    vi.stubGlobal("fetch", async (url: string) => {
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      if (url.endsWith("/teams")) return json({ items: [] });
      const phase = new URL(url, "http://cadre.test").searchParams.get("phase");
      reads.push(phase ?? "");
      await ready;
      return json({ items: tasks.filter((t) => t.status?.phase === phase), offset: 0, limit: 50 });
    });
    return () => go();
  }

  let navigate: NavigateFunction;
  function Navigator() {
    navigate = useNavigate();
    return <span data-testid="query">{useLocation().search}</span>;
  }

  // renderPage opens the board at the last of the urls, after the others.
  function renderPage(...urls: string[]) {
    localStorage.setItem("cadre.identity", "dana");
    const entries = urls.length > 0 ? urls : ["/t/team-a"];
    render(
      <MemoryRouter initialEntries={entries} initialIndex={entries.length - 1}>
        <App />
        <Navigator />
      </MemoryRouter>,
    );
  }

  const query = () => screen.getByTestId("query").textContent;
  const select = (name: string) => screen.getByLabelText(name) as HTMLSelectElement;
  const bot = (name: string, phase: Phase) => task(name, phase, { assignee: { kind: "Agent", name: "bot-1" } });

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
    expect(screen.getByText("2 of 3 loaded tasks shown")).toBeTruthy();
    expect(screen.queryByText("Title of b")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Clear the search" }));
    expect(screen.queryByText(/shown$/)).toBeNull();
    expect(screen.getByText("Title of b")).toBeTruthy();
  });

  it("opens with the search and the filters of the URL, reading only the phase asked for", async () => {
    const go = serve([bot("a", "Todo"), task("b", "Todo", { title: "fix b" }), bot("c", "Done")]);
    go();
    renderPage("/t/team-a?q=title&phase=Todo&assignee=bot-1");
    expect(await screen.findByText("Title of a")).toBeTruthy();
    expect((screen.getByLabelText("Search the loaded page") as HTMLInputElement).value).toBe("title");
    expect(select("Phase").value).toBe("Todo");
    expect(select("Assignee").value).toBe("bot-1");
    expect(screen.getAllByRole("region").map((r) => r.getAttribute("aria-label"))).toEqual(["Todo"]);
    expect(screen.queryByText("fix b")).toBeNull();
    expect(screen.getByText("1 of 2 loaded tasks shown")).toBeTruthy();
    expect(new Set(reads)).toEqual(new Set(["Todo"]));
  });

  it("keeps an assignee the loaded tasks do not have as an option", async () => {
    serve([bot("a", "Todo")])();
    renderPage("/t/team-a?assignee=dana");
    await screen.findByRole("region", { name: "Todo" });
    expect(select("Assignee").value).toBe("dana");
    expect([...select("Assignee").options].map((o) => o.text)).toEqual(["Anyone", "bot-1", "dana"]);
    expect(screen.queryByText("Title of a")).toBeNull();
  });

  it("counts what an assignee alone shows of what is loaded", async () => {
    serve([bot("a", "Todo"), task("b", "Todo"), bot("c", "Done")])();
    renderPage("/t/team-a?assignee=bot-1");
    await screen.findByText("Title of a");
    expect(screen.getByText("2 of 3 loaded tasks shown")).toBeTruthy();
  });

  it("reads every phase for an unknown phase, and leaves it in the URL", async () => {
    serve([bot("a", "Todo")])();
    renderPage("/t/team-a?phase=Bogus");
    await screen.findByText("Title of a");
    expect(select("Phase").value).toBe("");
    expect(screen.getAllByRole("region")).toHaveLength(7);
    expect(query()).toBe("?phase=Bogus");
  });

  it("reads a phase in any case, and the select clears it", async () => {
    serve([bot("a", "Todo"), task("b", "Done")])();
    renderPage("/t/team-a?phase=todo");
    await screen.findByText("Title of a");
    expect(select("Phase").value).toBe("Todo");
    expect(new Set(reads)).toEqual(new Set(["Todo"]));
    fireEvent.change(select("Phase"), { target: { value: "" } });
    expect(query()).toBe("");
    expect(await screen.findByText("Title of b")).toBeTruthy();
  });

  it("keeps the search in the URL in place of the history entry", async () => {
    serve([task("a", "Todo", { labels: ["ops"] }), task("b", "Todo")])();
    renderPage("/t/team-a?phase=Done", "/t/team-a");
    await screen.findByText("Title of b");
    const search = screen.getByLabelText("Search the loaded page");
    fireEvent.change(search, { target: { value: "o" } });
    fireEvent.change(search, { target: { value: "ops" } });
    expect(query()).toBe("?q=ops");
    // One entry back is the one before the board, not a shorter query.
    act(() => navigate(-1));
    expect(query()).toBe("?phase=Done");
  });

  it("sets and clears the filters in the URL, keeping the focus on the control", async () => {
    serve([bot("a", "Todo"), task("b", "Done")])();
    renderPage("/t/team-a");
    await screen.findByText("Title of b");
    const phase = select("Phase");
    phase.focus();
    fireEvent.change(phase, { target: { value: "Todo" } });
    fireEvent.change(select("Assignee"), { target: { value: "bot-1" } });
    expect(query()).toBe("?phase=Todo&assignee=bot-1");
    await vi.waitFor(() => expect(reads.at(-1)).toBe("Todo"));
    expect(screen.queryByText("Title of b")).toBeNull();
    fireEvent.change(select("Assignee"), { target: { value: "" } });
    expect(query()).toBe("?phase=Todo");
    expect(document.activeElement).toBe(phase);
  });

  it("shows only the chosen phase's column while its read is on the way", async () => {
    const tasks = [task("a", "Todo"), task("b", "Done")];
    serve(tasks)();
    renderPage("/t/team-a");
    await screen.findByText("Title of b");
    const go = serve(tasks);
    fireEvent.change(select("Phase"), { target: { value: "Todo" } });
    await vi.waitFor(() => expect(reads).toEqual(["Todo"]));
    expect(screen.getAllByRole("region").map((r) => r.getAttribute("aria-label"))).toEqual(["Todo"]);
    expect(screen.queryByText("Title of b")).toBeNull();
    await act(async () => go());
    expect(screen.getAllByRole("region").map((r) => r.getAttribute("aria-label"))).toEqual(["Todo"]);
    expect(screen.getByText("Title of a")).toBeTruthy();
  });

  it("clears two filters in one handler", async () => {
    serve([bot("a", "Todo")])();
    renderPage("/t/team-a?phase=Todo&assignee=bot-1&q=a");
    await screen.findByText("Title of a");
    act(() => {
      fireEvent.change(select("Phase"), { target: { value: "" } });
      fireEvent.change(select("Assignee"), { target: { value: "" } });
    });
    expect(query()).toBe("?q=a");
  });

  it("shows each entry's filters on Back and Forward", async () => {
    serve([bot("a", "Todo"), task("b", "Done")])();
    renderPage("/t/team-a?phase=Done", "/t/team-a?assignee=bot-1");
    await screen.findByText("Title of a");
    expect(screen.queryByText("Title of b")).toBeNull();
    act(() => navigate(-1));
    expect(select("Phase").value).toBe("Done");
    expect(select("Assignee").value).toBe("");
    expect(await screen.findByText("Title of b")).toBeTruthy();
    expect(screen.queryByText("Title of a")).toBeNull();
    act(() => navigate(1));
    expect(select("Phase").value).toBe("");
    expect(select("Assignee").value).toBe("bot-1");
    expect(await screen.findByText("Title of a")).toBeTruthy();
    expect(screen.queryByText("Title of b")).toBeNull();
  });

  it("opens the board filtered to the agent from the Agents page's link", async () => {
    serve([bot("a", "Todo"), task("b", "Todo")])();
    vi.stubGlobal(
      "fetch",
      ((serveBoard) => (url: string) =>
        url.endsWith("/agents") ? json({ items: [{ name: "bot-1", ready: true }] }) : serveBoard(url))(globalThis.fetch),
    );
    renderPage("/t/team-a/agents");
    fireEvent.click(await screen.findByRole("link", { name: "Tasks of bot-1" }));
    expect(await screen.findByText("Title of a")).toBeTruthy();
    expect(select("Assignee").value).toBe("bot-1");
    expect(screen.queryByText("Title of b")).toBeNull();
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
