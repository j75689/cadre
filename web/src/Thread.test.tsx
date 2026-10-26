import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Thread } from "./Thread";
import { entry } from "./testing";

afterEach(cleanup);

describe("Thread", () => {
  it("shows every entry kind with its author, and marks the one waited on", () => {
    const call = { id: "c1", name: "search__web", arguments: { q: "cadre" } };
    render(
      <Thread
        waitingOn={4}
        entries={[
          entry(1, "Comment"),
          entry(2, "Question", { author: { kind: "Agent", name: "writer" } }),
          entry(3, "Answer"),
          entry(4, "ToolCall", { author: { kind: "Agent", name: "writer" }, call }),
          entry(5, "Approved", { call }),
          entry(6, "Denied", { call }),
          entry(7, "ToolResult", { call: { ...call, isError: true } }),
        ]}
      />,
    );
    const items = screen.getAllByRole("listitem");
    expect(items.map((li) => li.querySelector(".kind")?.textContent)).toEqual(["comment", "question", "answer", "tool call", "approved", "denied", "tool result"]);
    expect(items[1].textContent).toContain("agent writer");
    expect(items[3].textContent).toContain('"q": "cadre"');
    expect(items[6].textContent).toContain("failed");
    const marked = items.filter((li) => li.getAttribute("aria-current") === "true");
    expect(marked).toEqual([items[3]]);
    expect(items[3].textContent).toContain("Waiting on this");
  });
});

describe("Thread entries", () => {
  it("tag each kind with an icon and its word, in its tone", () => {
    const call = { id: "c1", name: "search__web", arguments: { q: "cadre" } };
    render(<Thread entries={[entry(1, "Question"), entry(2, "Approved", { call }), entry(3, "Denied", { call })]} />);
    const tags = screen.getAllByRole("listitem").map((li) => li.querySelector(".kind")!);
    expect(tags.map((t) => [t.textContent, t.querySelector("svg") !== null])).toEqual([
      ["question", true],
      ["approved", true],
      ["denied", true],
    ]);
    expect(tags.map((t) => t.className)).toEqual(["kind chip tone-waiting", "kind chip tone-done", "kind chip tone-danger"]);
  });

  it("keep a tool call's arguments in a closed <details>, summarised by the tool", () => {
    const call = { id: "c1", name: "search__web", arguments: { q: "cadre" }, isError: true };
    render(<Thread entries={[entry(1, "ToolCall", { call }), entry(2, "ToolResult", { call })]} />);
    const [callEntry, result] = screen.getAllByRole("listitem");
    const details = callEntry.querySelector("details")!;
    expect(details.open).toBe(false);
    expect(details.querySelector("summary")!.textContent).toBe("search__webfailed, arguments");
    expect(details.querySelector("pre")!.textContent).toBe(JSON.stringify(call.arguments, null, 2));
    // A result names the tool, with no arguments to open.
    expect(result.querySelector("details")).toBeNull();
    expect(result.querySelector("code")!.textContent).toBe("search__web");
  });

  it("show the time relative within a day, and the full date in its title", () => {
    const at = new Date(Date.now() - 5 * 60 * 1000).toISOString();
    render(<Thread entries={[entry(1, "Comment", { at }), entry(2, "Comment", { at: "2026-01-02T03:04:05Z" })]} />);
    const [recent, old] = screen.getAllByRole("listitem").map((li) => li.querySelector("time")!);
    expect(recent.textContent).toBe("5 min ago");
    expect(recent.getAttribute("title")).toBe(new Date(at).toLocaleString());
    expect(old.textContent).toBe(new Date("2026-01-02T03:04:05Z").toLocaleString());
  });

  it("say when there is nothing yet", () => {
    render(<Thread entries={[]} />);
    expect(screen.getByText("Nothing in the thread yet.")).toBeTruthy();
  });

  it("scroll the entry waited on into view once for each wait", () => {
    const scroll = vi.fn();
    Element.prototype.scrollIntoView = scroll;
    const { rerender } = render(<Thread entries={[entry(1, "Comment")]} waitingOn={2} />);
    expect(scroll).not.toHaveBeenCalled();
    rerender(<Thread entries={[entry(1, "Comment"), entry(2, "Question")]} waitingOn={2} />);
    expect(scroll).toHaveBeenCalledTimes(1);
    rerender(<Thread entries={[entry(1, "Comment"), entry(2, "Question"), entry(3, "Comment")]} waitingOn={2} />);
    expect(scroll).toHaveBeenCalledTimes(1);
    // A later wait, on a later entry, is scrolled to in its turn.
    const later = [entry(1, "Comment"), entry(2, "Question"), entry(3, "Comment"), entry(4, "Question")];
    rerender(<Thread entries={later} waitingOn={4} />);
    expect(scroll).toHaveBeenCalledTimes(2);
    rerender(<Thread entries={[...later, entry(5, "Comment")]} waitingOn={4} />);
    expect(scroll).toHaveBeenCalledTimes(2);
  });
});
