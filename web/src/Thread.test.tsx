import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
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
    expect(items[3].textContent).toContain("waiting on this");
  });
});
