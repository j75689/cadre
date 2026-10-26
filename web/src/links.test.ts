import { describe, expect, it } from "vitest";
import { boardLink, taskLink, teamPath } from "./links";

describe("links", () => {
  it("encodes the team's name", () => {
    expect(teamPath("a b/c")).toBe("/t/a%20b%2Fc");
  });

  it("builds the board's link with the set filters only", () => {
    expect(boardLink("a")).toBe("/t/a");
    expect(boardLink("a", { assignee: "Agent/Engineer", phase: "Todo", view: "list" })).toBe("/t/a?assignee=Agent%2FEngineer&phase=Todo&view=list");
    expect(boardLink("a", { phase: "Done" })).toBe("/t/a?phase=Done");
  });

  it("builds a task's link, at a run when one is given", () => {
    expect(taskLink("a b", "x/y")).toBe("/t/a%20b/tasks/x%2Fy");
    expect(taskLink("a", "x", "run 1")).toBe("/t/a/tasks/x?run=run+1");
  });
});
