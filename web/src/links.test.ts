import { act, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { MemoryRouter, useLocation, useNavigationType } from "react-router";
import { describe, expect, it } from "vitest";
import { boardLink, taskLink, teamPath, useParam } from "./links";

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

describe("useParam", () => {
  const at = (url: string) => ({ children }: { children: ReactNode }) => createElement(MemoryRouter, { initialEntries: [url] }, children);
  const use = () => ({ param: useParam("run"), location: useLocation(), action: useNavigationType() });

  it("reads a query parameter, empty when unset", () => {
    expect(renderHook(use, { wrapper: at("/t/a?run=R1") }).result.current.param[0]).toBe("R1");
    expect(renderHook(use, { wrapper: at("/t/a") }).result.current.param[0]).toBe("");
  });

  it("sets it in place of the history entry, and deletes it when empty", () => {
    const { result } = renderHook(use, { wrapper: at("/t/a?q=x") });
    act(() => result.current.param[1]("R 2"));
    expect(result.current.location.search).toBe("?q=x&run=R+2");
    expect(result.current.action).toBe("REPLACE");
    act(() => result.current.param[1](""));
    expect(result.current.location.search).toBe("?q=x");
    expect(result.current.param[0]).toBe("");
  });

  it("keeps the same setter from one render to the next", () => {
    const { result, rerender } = renderHook(use, { wrapper: at("/t/a?run=R1") });
    const set = result.current.param[1];
    rerender();
    act(() => set("R2"));
    expect(result.current.param[1]).toBe(set);
  });

  it("keeps both of two sets made in one event", () => {
    const useTwo = () => ({ phase: useParam("phase"), assignee: useParam("assignee"), location: useLocation() });
    const { result } = renderHook(useTwo, { wrapper: at("/t/a?phase=Todo&assignee=bot&q=x") });
    act(() => {
      result.current.phase[1]("");
      result.current.assignee[1]("");
    });
    expect(result.current.location.search).toBe("?q=x");
    act(() => {
      result.current.phase[1]("Done");
      result.current.assignee[1]("dana");
    });
    expect(result.current.location.search).toBe("?q=x&phase=Done&assignee=dana");
  });
});
