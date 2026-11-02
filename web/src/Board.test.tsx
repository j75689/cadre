import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter, useLocation, useNavigate, type NavigateFunction } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { phases, type Phase, type Task } from "./api";
import { Columns, latest, matches, searchDelayMs, type Column } from "./Board";
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

  it("shows what the server filtered as it is, and its total in the pill", () => {
    const cols = columns([task("a", "Todo"), task("b", "Done")], ["Done"]).map((c) => (c.phase === "Todo" ? { ...c, total: 120 } : c));
    renderBoard(cols, "nothing");
    const pill = (phase: string) => screen.getByRole("region", { name: phase }).querySelector(".count-pill")!.textContent;
    expect(pill("Todo")).toBe("120");
    expect(screen.getByText("Title of a")).toBeTruthy();
    // A column without a total, from a server without the filters, counts
    // what is loaded, and filters it here.
    expect(pill("Done")).toBe("1+");
    expect(screen.queryByText("Title of b")).toBeNull();
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

  it("matches a task that has every label, exactly", () => {
    const t = task("a", "Todo", { labels: ["ops", "db"] });
    expect(matches(t, "", "", ["ops", "db"])).toBe(true);
    expect(matches(t, "", "", ["ops", "web"])).toBe(false);
    expect(matches(t, "", "", ["op"])).toBe(false);
    expect(matches(task("b", "Todo"), "", "", ["ops"])).toBe(false);
  });
});

describe("latest", () => {
  it("keeps a task in two columns only in the one read last", () => {
    const cols = columns([task("mover", "Todo"), task("a", "Todo"), task("b", "Running")]);
    cols.find((c) => c.phase === "Running")!.tasks.push(task("mover", "Running"));
    const tasks = (cs: Column[]) => Object.fromEntries(cs.map((c) => [c.phase, c.tasks.map((t) => t.metadata.name)]));
    expect(tasks(latest(cols, ["Todo", "Running"])).Todo).toEqual(["a"]);
    expect(tasks(latest(cols, ["Todo", "Running"])).Running).toEqual(["b", "mover"]);
    expect(tasks(latest(cols, ["Running", "Todo"])).Todo).toEqual(["mover", "a"]);
    expect(tasks(latest(cols, ["Running", "Todo"])).Running).toEqual(["b"]);
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

  // urls are the board's reads of serveFiltered.
  let urls: URLSearchParams[] = [];

  // serveFiltered answers as a server with the filters: it filters by q,
  // assignee and labels, and counts the matches, or answers total[phase] if
  // set. It refuses the label "bad".
  function serveFiltered(tasks: Task[], total: Partial<Record<Phase, number>> = {}) {
    urls = [];
    vi.stubGlobal("fetch", (url: string) => {
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      if (url.endsWith("/teams")) return json({ items: [] });
      const p = new URL(url, "http://cadre.test").searchParams;
      urls.push(p);
      const q = p.get("q") ?? "";
      if (q.length > 200) return json({ error: { code: "invalid", message: `q is ${q.length} characters: at most 200` } }, 400);
      if (p.getAll("label").includes("bad")) return json({ error: { code: "invalid", message: "label bad is refused" } }, 400);
      const phase = p.get("phase") as Phase;
      const items = tasks.filter((t) => t.status?.phase === phase && matches(t, q, p.get("assignee") ?? "", p.getAll("label")));
      return json({ items, offset: 0, limit: 50, total: total[phase] ?? items.length });
    });
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
    fireEvent.change(screen.getByLabelText("Search the board"), { target: { value: "ops" } });
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
    expect((screen.getByLabelText("Search the board") as HTMLInputElement).value).toBe("title");
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

  it("sends the search trimmed to every phase once the typing pauses, and counts the board's matches", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    serveFiltered([task("a", "Todo", { labels: ["ops"] }), task("b", "Todo"), task("c", "Done", { labels: ["ops"] })], { Done: 80 });
    renderPage();
    await screen.findByText("Title of b");
    urls = [];
    const search = screen.getByLabelText("Search the board");
    fireEvent.change(search, { target: { value: " op" } });
    await act(() => vi.advanceTimersByTimeAsync(searchDelayMs - 100));
    fireEvent.change(search, { target: { value: " ops " } });
    await act(() => vi.advanceTimersByTimeAsync(searchDelayMs - 100));
    expect(urls).toEqual([]);
    await act(() => vi.advanceTimersByTimeAsync(150));
    await vi.waitFor(() => expect(urls).toHaveLength(7));
    expect(new Set(urls.map((u) => u.get("q")))).toEqual(new Set(["ops"]));
    expect(new Set(urls.map((u) => u.get("phase")))).toEqual(new Set(phases));
    await vi.waitFor(() => expect(screen.queryByText("Title of b")).toBeNull());
    expect(screen.getByText("Title of a")).toBeTruthy();
    expect(screen.getByText("81 tasks match")).toBeTruthy();
  });

  it("reads a link's search and assignee at once, and the whole board when they are cleared", async () => {
    serveFiltered([bot("fix-a", "Todo"), task("fix-b", "Todo"), bot("c", "Done")]);
    renderPage("/t/team-a?q=%20fix%20&assignee=bot-1");
    expect(await screen.findByText("Title of fix-a")).toBeTruthy();
    expect(new Set(urls.map((u) => `${u.get("q")}/${u.get("assignee")}`))).toEqual(new Set(["fix/bot-1"]));
    expect(screen.queryByText("Title of fix-b")).toBeNull();
    expect(screen.getByText("1 task matches")).toBeTruthy();
    act(() => {
      fireEvent.click(screen.getByRole("button", { name: "Clear the search" }));
      fireEvent.change(select("Assignee"), { target: { value: "" } });
    });
    expect(query()).toBe("");
    expect(await screen.findByText("Title of fix-b")).toBeTruthy();
    expect(screen.getByText("Title of c")).toBeTruthy();
    expect(urls.at(-1)!.has("q") || urls.at(-1)!.has("assignee")).toBe(false);
    expect(screen.queryByText(/match/)).toBeNull();
  });

  it("keeps the assignees read before as options while one is chosen", async () => {
    serveFiltered([bot("a", "Todo"), task("b", "Todo", { assignee: { kind: "User", name: "dana" } })]);
    renderPage();
    await screen.findByText("Title of b");
    fireEvent.change(select("Assignee"), { target: { value: "dana" } });
    await vi.waitFor(() => expect(screen.queryByText("Title of a")).toBeNull());
    expect([...select("Assignee").options].map((o) => o.text)).toEqual(["Anyone", "bot-1", "dana"]);
  });

  it("shows each column's total in its pill, as each read counts it", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const total: Partial<Record<Phase, number>> = { Todo: 120 };
    serveFiltered([task("a", "Todo")], total);
    renderPage("/t/team-a?q=title");
    await screen.findByText("Title of a");
    const pill = () => screen.getByRole("region", { name: "Todo" }).querySelector(".count-pill")!.textContent;
    expect(pill()).toBe("120");
    total.Todo = 121;
    await act(() => vi.advanceTimersByTimeAsync(5000));
    await vi.waitFor(() => expect(pill()).toBe("121"));
    expect(urls.at(-1)!.get("q")).toBe("title");
  });

  it("reads the first pages again when a filter changes, and offers more by the page, not the total", async () => {
    const offsets: string[] = [];
    vi.stubGlobal("fetch", (url: string) => {
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      if (url.endsWith("/teams")) return json({ items: [] });
      const q = new URL(url, "http://cadre.test").searchParams;
      const phase = q.get("phase") as Phase;
      const offset = Number(q.get("offset"));
      offsets.push(`${phase}@${offset}`);
      // Todo has a full first page and a short second one, and a task more
      // by the second read; Done says it has more than its one short page.
      const n = phase === "Todo" ? (offset === 0 ? 50 : 1) : phase === "Done" ? 1 : 0;
      const items = Array.from({ length: n }, (_, i) => bot(`${phase}-${offset + i}`, phase));
      return json({ items, offset, limit: 50, total: phase === "Done" ? 999 : phase === "Todo" ? 51 + offset / 50 : 0 });
    });
    renderPage();
    const todo = await screen.findByRole("region", { name: "Todo" });
    expect(within(screen.getByRole("region", { name: "Done" })).queryByRole("button", { name: "Load more" })).toBeNull();
    fireEvent.click(await within(todo).findByRole("button", { name: "Load more" }));
    await vi.waitFor(() => expect(offsets).toContain("Todo@50"));
    await vi.waitFor(() => expect(within(screen.getByRole("region", { name: "Todo" })).queryByRole("button", { name: "Load more" })).toBeNull());
    // The pill shows the total of the last page read.
    expect(screen.getByRole("region", { name: "Todo" }).querySelector(".count-pill")!.textContent).toBe("52");
    offsets.length = 0;
    fireEvent.change(select("Assignee"), { target: { value: "bot-1" } });
    await vi.waitFor(() => expect(screen.getByRole("region", { name: "Todo" }).querySelector(".count-pill")!.textContent).toBe("51"));
    expect(offsets.sort()).toEqual(phases.map((ph) => `${ph}@0`).sort());
  });

  it("says why the server refused the search, and shows the board again once it is cleared", async () => {
    serveFiltered([task("a", "Todo")]);
    renderPage(`/t/team-a?q=${"x".repeat(201)}`);
    expect(await screen.findByText("Could not filter the board: q is 201 characters: at most 200")).toBeTruthy();
    expect(screen.queryByText("Loading the board")).toBeNull();
    expect(screen.queryByRole("region", { name: "Todo" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Clear the search" }));
    expect(await screen.findByText("Title of a")).toBeTruthy();
    expect(screen.queryByText(/Could not filter/)).toBeNull();
  });

  it("shows the skeleton, not the last filter's cards, until a new filter's read is back", async () => {
    serveFiltered([task("b", "Todo", { assignee: { kind: "User", name: "dana" } }), task("c", "Todo", { assignee: { kind: "User", name: "carol" } })]);
    const served = globalThis.fetch;
    let go: () => void = () => {};
    const held = new Promise<void>((r) => (go = r));
    let asked = false;
    vi.stubGlobal("fetch", async (url: string) => {
      if (url.includes("assignee=carol")) {
        asked = true;
        await held;
      }
      return served(url);
    });
    renderPage();
    await screen.findByText("Title of c");
    fireEvent.change(select("Assignee"), { target: { value: "dana" } });
    expect(await screen.findByText("1 task matches")).toBeTruthy();
    expect(screen.queryByText("Title of c")).toBeNull();
    fireEvent.change(select("Assignee"), { target: { value: "carol" } });
    await vi.waitFor(() => expect(asked).toBe(true));
    expect(query()).toBe("?assignee=carol");
    expect(screen.getByText("Loading the board")).toBeTruthy();
    expect(screen.queryByText("Title of b")).toBeNull();
    expect(screen.queryByRole("region", { name: "Todo" })).toBeNull();
    expect(screen.queryByText(/match/)).toBeNull();
    await act(async () => go());
    expect(await screen.findByText("Title of c")).toBeTruthy();
    expect(screen.getByText("1 task matches")).toBeTruthy();
  });

  it("clears the refusal when the search changes, so a failed read shows the error and retry", async () => {
    serveFiltered([task("fix-a", "Todo")]);
    renderPage(`/t/team-a?q=${"x".repeat(201)}`);
    await screen.findByText("Could not filter the board: q is 201 characters: at most 200");
    const served = globalThis.fetch;
    vi.stubGlobal("fetch", async (url: string) => {
      if (url.includes("/tasks")) throw new TypeError("Failed to fetch");
      return served(url);
    });
    fireEvent.change(screen.getByLabelText("Search the board"), { target: { value: "fix" } });
    expect(await screen.findByText("Could not load the board: Failed to fetch")).toBeTruthy();
    expect(screen.queryByText(/Could not filter/)).toBeNull();
    vi.stubGlobal("fetch", served);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Title of fix-a")).toBeTruthy();
  });

  it("keeps the search in the URL in place of the history entry", async () => {
    serve([task("a", "Todo", { labels: ["ops"] }), task("b", "Todo")])();
    renderPage("/t/team-a?phase=Done", "/t/team-a");
    await screen.findByText("Title of b");
    const search = screen.getByLabelText("Search the board");
    fireEvent.change(search, { target: { value: "o" } });
    fireEvent.change(search, { target: { value: "ops" } });
    expect(query()).toBe("?q=ops");
    // One entry back is the one before the board, not a shorter query.
    act(() => navigate(-1));
    expect(query()).toBe("?phase=Done");
  });

  it("keeps an input method's composition in the search, and writes only the composed text", async () => {
    serve([task("a", "Todo", { title: "是務" }), task("b", "Todo")])();
    renderPage();
    await screen.findByText("Title of b");
    const search = screen.getByLabelText("Search the board") as HTMLInputElement;
    fireEvent.compositionStart(search);
    for (const value of ["ㄕ", "ㄕˋ", "是", "是ㄨ", "是ㄨˋ", "是務"]) {
      fireEvent.change(search, { target: { value } });
      expect(search.value).toBe(value);
      expect(query()).toBe("");
    }
    fireEvent.compositionEnd(search);
    expect(search.value).toBe("是務");
    expect(new URLSearchParams(query() ?? "").get("q")).toBe("是務");
    expect(screen.queryByText("Title of b")).toBeNull();
  });

  it("keeps every key typed before the URL has caught up", async () => {
    serve([task("a", "Todo", { title: "release notes" }), task("b", "Todo")])();
    renderPage();
    await screen.findByText("Title of b");
    const search = screen.getByLabelText("Search the board") as HTMLInputElement;
    // One act: the keys come faster than the router's transition commits.
    act(() => {
      for (const key of "release") fireEvent.change(search, { target: { value: search.value + key } });
    });
    expect(search.value).toBe("release");
    expect(query()).toBe("?q=release");
  });

  it("does not take back its own write when the URL catches up mid-composition", async () => {
    serve([task("a", "Todo")])();
    renderPage();
    await screen.findByText("Title of a");
    const search = screen.getByLabelText("Search the board") as HTMLInputElement;
    // "a" is written; the router commits it after the composition began.
    act(() => {
      fireEvent.change(search, { target: { value: "a" } });
      fireEvent.compositionStart(search);
      fireEvent.change(search, { target: { value: "aㄕ" } });
    });
    expect(query()).toBe("?q=a");
    expect(search.value).toBe("aㄕ");
    fireEvent.change(search, { target: { value: "a是" } });
    fireEvent.compositionEnd(search);
    expect(search.value).toBe("a是");
    expect(new URLSearchParams(query() ?? "").get("q")).toBe("a是");
  });

  it("shows the search of a link, Back and Forward, and keeps it when a filter changes", async () => {
    serve([task("a", "Todo"), task("b", "Todo")])();
    renderPage("/t/team-a?q=a", "/t/team-a");
    await screen.findByText("Title of b");
    const search = screen.getByLabelText("Search the board") as HTMLInputElement;
    expect(search.value).toBe("");
    act(() => navigate(-1));
    expect(search.value).toBe("a");
    act(() => navigate(1));
    expect(search.value).toBe("");
    act(() => navigate("/t/team-a?q=b"));
    expect(search.value).toBe("b");
    fireEvent.change(select("Phase"), { target: { value: "Todo" } });
    expect(search.value).toBe("b");
    expect(query()).toBe("?q=b&phase=Todo");
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

  it("opens the list with the search and the filters of the URL", async () => {
    const go = serve([bot("fix-a", "Todo"), bot("other", "Todo"), task("fix-b", "Todo"), bot("fix-c", "Done")]);
    go();
    renderPage("/t/team-a?q=fix&phase=Todo&assignee=bot-1&view=list");
    const list = await screen.findByRole("region", { name: "Tasks" });
    expect(within(list).getAllByRole("link").map((l) => l.textContent)).toEqual(["Title of fix-a"]);
    expect(screen.queryByRole("region", { name: "Todo" })).toBeNull();
    expect((screen.getByLabelText("Search the board") as HTMLInputElement).value).toBe("fix");
    expect(select("Phase").value).toBe("Todo");
    expect(select("Assignee").value).toBe("bot-1");
    expect(screen.getByRole("button", { name: "List" }).getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByRole("button", { name: "Board" }).getAttribute("aria-pressed")).toBe("false");
    expect(new Set(reads)).toEqual(new Set(["Todo"]));
  });

  it("switches between the board and the list, keeping the focus on the button", async () => {
    serve([task("a", "Todo")])();
    renderPage("/t/team-a?q=a");
    await screen.findByRole("region", { name: "Todo" });
    const listButton = screen.getByRole("button", { name: "List" });
    listButton.focus();
    fireEvent.click(listButton);
    expect(query()).toBe("?q=a&view=list");
    expect(screen.getByRole("region", { name: "Tasks" })).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Todo" })).toBeNull();
    expect(document.activeElement).toBe(listButton);
    fireEvent.click(screen.getByRole("button", { name: "Board" }));
    expect(query()).toBe("?q=a");
    expect(screen.getByRole("region", { name: "Todo" })).toBeTruthy();
  });

  it("reads the next page of each phase with more from the list's Load more", async () => {
    const offsets: string[] = [];
    vi.stubGlobal("fetch", (url: string) => {
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      if (url.endsWith("/teams")) return json({ items: [] });
      const q = new URL(url, "http://cadre.test").searchParams;
      const phase = q.get("phase") as Phase;
      const offset = Number(q.get("offset"));
      offsets.push(`${phase}@${offset}`);
      // Todo and Done have a full first page, and a short second one.
      const n = phase === "Todo" || phase === "Done" ? (offset === 0 ? 50 : 1) : 0;
      return json({ items: Array.from({ length: n }, (_, i) => task(`${phase}-${offset + i}`, phase)), offset, limit: 50 });
    });
    renderPage("/t/team-a?view=list");
    const more = await screen.findByRole("button", { name: "Load more" });
    expect(within(screen.getByRole("region", { name: "Tasks" })).getAllByRole("link")).toHaveLength(100);
    offsets.length = 0;
    fireEvent.click(more);
    await vi.waitFor(() => expect(screen.queryByRole("button", { name: "Load more" })).toBeNull());
    expect(offsets.filter((o) => !o.endsWith("@0")).sort()).toEqual(["Done@50", "Todo@50"]);
    expect(within(screen.getByRole("region", { name: "Tasks" })).getAllByRole("link")).toHaveLength(102);
  });

  it("shows a task that changed phase between the reads once, as the read back last has it", async () => {
    const answer = new Map<string, () => void>();
    vi.stubGlobal("fetch", async (url: string) => {
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      if (url.endsWith("/teams")) return json({ items: [] });
      const phase = new URL(url, "http://cadre.test").searchParams.get("phase") as Phase;
      await new Promise<void>((r) => answer.set(phase, r));
      // mover went from Running to Todo between the Running and Todo reads.
      const items = phase === "Running" || phase === "Todo" ? [task("mover", phase)] : [];
      return json({ items, offset: 0, limit: 50 });
    });
    renderPage("/t/team-a?q=mover&view=list");
    await vi.waitFor(() => expect(answer.size).toBe(7));
    for (const ph of [...phases.filter((p) => p !== "Todo"), "Todo"]) await act(async () => answer.get(ph)!());
    const list = await screen.findByRole("region", { name: "Tasks" });
    expect(within(list).getAllByRole("link")).toHaveLength(1);
    expect(within(list).getByText("Todo")).toBeTruthy();
    expect(screen.getByText("1 of 1 loaded tasks shown")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Board" }));
    expect(within(screen.getByRole("region", { name: "Todo" })).getByText("Title of mover")).toBeTruthy();
    expect(within(screen.getByRole("region", { name: "Running" })).queryByText("Title of mover")).toBeNull();
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

  it("shows the skeleton, not the last filter's failed refresh, until a new filter's read is back", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    serveFiltered([bot("a", "Todo"), task("c", "Todo", { assignee: { kind: "User", name: "carol" } })]);
    const served = globalThis.fetch;
    let down = false;
    let go: () => void = () => {};
    const held = new Promise<void>((r) => (go = r));
    let asked = false;
    vi.stubGlobal("fetch", async (url: string) => {
      if (down && url.includes("/tasks")) throw new TypeError("Failed to fetch");
      const who = new URL(url, "http://cadre.test").searchParams.get("assignee");
      if (who === "bot-1") {
        asked = true;
        await held;
      }
      return served(url);
    });
    renderPage();
    await screen.findByText("Title of a");
    fireEvent.change(select("Assignee"), { target: { value: "carol" } });
    await screen.findByText("1 task matches");
    down = true;
    await act(() => vi.advanceTimersByTimeAsync(5000));
    await vi.waitFor(() => expect(screen.getByRole("status").textContent).toBe("Could not refresh: Failed to fetch"));
    down = false;
    fireEvent.change(select("Assignee"), { target: { value: "bot-1" } });
    await vi.waitFor(() => expect(asked).toBe(true));
    expect(screen.getByText("Loading the board")).toBeTruthy();
    expect(screen.queryByText(/Could not/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    await act(async () => go());
    expect(await screen.findByText("Title of a")).toBeTruthy();
    expect(screen.queryByText(/Could not/)).toBeNull();
  });

  describe("labels", () => {
    const labelled = (name: string, phase: Phase, ...labels: string[]) => task(name, phase, { labels });
    const card = (name: string) => screen.getByRole("link", { name: `Title of ${name}` }).closest("article")!;

    it("sends the URL's labels, each once and none empty, on every phase's read with the search and assignee", async () => {
      serveFiltered([
        task("fix-a", "Todo", { labels: ["ops", "db"], assignee: { kind: "Agent", name: "bot-1" } }),
        task("fix-b", "Todo", { labels: ["ops"], assignee: { kind: "Agent", name: "bot-1" } }),
      ]);
      renderPage("/t/team-a?label=ops&label=&label=ops&label=db&q=fix&assignee=bot-1&view=list");
      const list = await screen.findByRole("region", { name: "Tasks" });
      expect(within(list).getAllByRole("link").map((l) => l.textContent)).toEqual(["Title of fix-a"]);
      expect(new Set(urls.map((u) => u.get("phase")))).toEqual(new Set(phases));
      expect(new Set(urls.map((u) => `${u.getAll("label").join(",")}/${u.get("q")}/${u.get("assignee")}`))).toEqual(new Set(["ops,db/fix/bot-1"]));
      const chips = within(screen.getByRole("group", { name: "Labels" })).getAllByRole("button", { name: /^Remove label / });
      expect(chips.map((c) => c.textContent)).toEqual(["ops", "db"]);
      expect(screen.getByText("1 task matches")).toBeTruthy();
    });

    it("adds a card's label to the URL once, and reads every column from its first page, showing the skeleton until then", async () => {
      serveFiltered([labelled("a", "Todo", "ops", "db"), labelled("b", "Done", "ops"), task("c", "Todo")], { Done: 40 });
      const served = globalThis.fetch;
      let go: () => void = () => {};
      const held = new Promise<void>((r) => (go = r));
      const asked: URLSearchParams[] = [];
      vi.stubGlobal("fetch", async (url: string) => {
        const p = new URL(url, "http://cadre.test").searchParams;
        if (p.has("label")) {
          asked.push(p);
          await held;
        }
        return served(url);
      });
      renderPage();
      await screen.findByText("Title of c");
      fireEvent.click(within(card("a")).getByRole("button", { name: "Filter by label ops" }));
      expect(query()).toBe("?label=ops");
      await vi.waitFor(() => expect(asked).toHaveLength(7));
      expect(asked.map((u) => `${u.get("phase")}@${u.get("offset")}`).sort()).toEqual(phases.map((ph) => `${ph}@0`).sort());
      expect(screen.getByText("Loading the board")).toBeTruthy();
      expect(screen.queryByText("Title of c")).toBeNull();
      await act(async () => go());
      expect(await screen.findByText("Title of b")).toBeTruthy();
      expect(screen.queryByText("Title of c")).toBeNull();
      expect(screen.getByRole("region", { name: "Done" }).querySelector(".count-pill")!.textContent).toBe("40");
      expect(screen.getByText("41 tasks match")).toBeTruthy();
      // A label already in the filter is not added again.
      const ops = within(card("b")).getByRole("button", { name: "Filter by label ops" }) as HTMLButtonElement;
      expect(ops.disabled).toBe(true);
      expect(ops.getAttribute("aria-pressed")).toBe("true");
      fireEvent.click(ops);
      expect(query()).toBe("?label=ops");
      fireEvent.click(within(card("a")).getByRole("button", { name: "Filter by label db" }));
      expect(query()).toBe("?label=ops&label=db");
      expect(await screen.findByText("Title of a")).toBeTruthy();
      expect(screen.queryByText("Title of b")).toBeNull();
    });

    it("removes a label with its chip and all of them with Clear labels, reading again", async () => {
      serveFiltered([labelled("a", "Todo", "ops", "db"), labelled("b", "Todo", "db"), task("c", "Todo")]);
      renderPage("/t/team-a?label=ops&label=db&phase=Todo");
      await screen.findByText("Title of a");
      expect(screen.queryByText("Title of b")).toBeNull();
      fireEvent.click(screen.getByRole("button", { name: "Remove label ops" }));
      expect(query()).toBe("?phase=Todo&label=db");
      expect(await screen.findByText("Title of b")).toBeTruthy();
      expect(urls.at(-1)!.getAll("label")).toEqual(["db"]);
      expect(screen.queryByText("Title of c")).toBeNull();
      fireEvent.click(screen.getByRole("button", { name: "Clear labels" }));
      expect(query()).toBe("?phase=Todo");
      expect(await screen.findByText("Title of c")).toBeTruthy();
      expect(urls.at(-1)!.has("label")).toBe(false);
      expect(screen.queryByRole("group", { name: "Labels" })).toBeNull();
    });

    it("keeps the focus on the labels' chips, or the search box once there are none, as a label's control goes away", async () => {
      serveFiltered([labelled("a", "Todo", "ops", "db", "qa", "sp")]);
      renderPage("/t/team-a?label=ops&label=db&label=qa");
      await screen.findByText("Title of a");
      const focused = () => document.activeElement?.getAttribute("aria-label") ?? document.activeElement?.id;
      const press = (button: HTMLElement) => {
        button.focus();
        fireEvent.click(button);
      };
      // A pill's label gets its chip, the last one.
      press(within(card("a")).getByRole("button", { name: "Filter by label sp" }));
      await vi.waitFor(() => expect(focused()).toBe("Remove label sp"));
      // A chip's removal gives the chip now in its place, else the one before.
      press(screen.getByRole("button", { name: "Remove label db" }));
      await vi.waitFor(() => expect(focused()).toBe("Remove label qa"));
      press(screen.getByRole("button", { name: "Remove label sp" }));
      await vi.waitFor(() => expect(focused()).toBe("Remove label qa"));
      press(screen.getByRole("button", { name: "Clear labels" }));
      await vi.waitFor(() => expect(focused()).toBe("board-search"));
      expect(query()).toBe("");
      // So does the last chip's removal.
      await screen.findByText("Title of a");
      press(within(card("a")).getByRole("button", { name: "Filter by label db" }));
      await vi.waitFor(() => expect(focused()).toBe("Remove label db"));
      press(screen.getByRole("button", { name: "Remove label db" }));
      await vi.waitFor(() => expect(focused()).toBe("board-search"));
    });

    it("sends at most ten labels, and says so instead of adding an eleventh", async () => {
      const ten = Array.from({ length: 10 }, (_, i) => `l${i}`);
      serveFiltered([labelled("a", "Todo", ...ten, "more")]);
      renderPage(`/t/team-a?${[...ten, "l10"].map((l) => `label=${l}`).join("&")}`);
      await screen.findByText("Title of a");
      expect(new Set(urls.map((u) => u.getAll("label").join(",")))).toEqual(new Set([ten.join(",")]));
      expect(screen.getByText("At most 10 labels")).toBeTruthy();
      expect(screen.getAllByRole("button", { name: /^Remove label / })).toHaveLength(10);
      const more = within(card("a")).getByRole("button", { name: "Filter by label more" }) as HTMLButtonElement;
      expect(more.disabled).toBe(true);
      fireEvent.click(more);
      expect(new URLSearchParams(query() ?? "").getAll("label")).toEqual([...ten, "l10"]);
      fireEvent.click(screen.getByRole("button", { name: "Remove label l0" }));
      await vi.waitFor(() => expect(screen.queryByText("At most 10 labels")).toBeNull());
      fireEvent.click(screen.getByRole("button", { name: "Clear labels" }));
      expect(query()).toBe("");
    });

    it("says why the server refused a label, until the label is removed", async () => {
      serveFiltered([labelled("a", "Todo", "ops")]);
      renderPage("/t/team-a?label=ops&label=bad");
      expect(await screen.findByText("Could not filter the board: label bad is refused")).toBeTruthy();
      expect(screen.queryByRole("region", { name: "Todo" })).toBeNull();
      fireEvent.click(screen.getByRole("button", { name: "Remove label bad" }));
      expect(await screen.findByText("Title of a")).toBeTruthy();
      expect(screen.queryByText(/Could not filter/)).toBeNull();
    });

    it("filters the loaded pages by the labels on a server without the filters", async () => {
      serve([labelled("a", "Todo", "ops", "db"), labelled("b", "Todo", "ops"), labelled("c", "Done", "db", "ops")])();
      renderPage("/t/team-a?label=ops&label=db");
      await screen.findByText("Title of a");
      expect(screen.getByText("Title of c")).toBeTruthy();
      expect(screen.queryByText("Title of b")).toBeNull();
      expect(screen.getByText("2 of 3 loaded tasks shown")).toBeTruthy();
    });

    it("shows each entry's labels on Back and Forward", async () => {
      serveFiltered([labelled("a", "Todo", "ops"), task("b", "Todo")]);
      renderPage("/t/team-a?label=ops", "/t/team-a");
      await screen.findByText("Title of b");
      expect(screen.queryByRole("group", { name: "Labels" })).toBeNull();
      act(() => navigate(-1));
      expect(screen.getByRole("button", { name: "Remove label ops" })).toBeTruthy();
      expect(await screen.findByText("Title of a")).toBeTruthy();
      expect(screen.queryByText("Title of b")).toBeNull();
      act(() => navigate(1));
      expect(screen.queryByRole("group", { name: "Labels" })).toBeNull();
      expect(await screen.findByText("Title of b")).toBeTruthy();
    });
  });
});
