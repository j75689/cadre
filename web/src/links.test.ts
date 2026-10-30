import { act, cleanup, fireEvent, render, renderHook, screen } from "@testing-library/react";
import { createElement, useEffect, type ReactNode } from "react";
import { Link, MemoryRouter, useLocation, useNavigationType } from "react-router";
import { afterEach, describe, expect, it } from "vitest";
import { agentLink, boardLink, taskLink, teamPath, useParam } from "./links";

describe("links", () => {
  it("encodes the team's name", () => {
    expect(teamPath("a b/c")).toBe("/t/a%20b%2Fc");
  });

  it("builds the board's link with the set filters only", () => {
    expect(boardLink("a")).toBe("/t/a");
    expect(boardLink("a", { assignee: "Agent/Engineer", phase: "Todo", view: "list" })).toBe("/t/a?assignee=Agent%2FEngineer&phase=Todo&view=list");
    expect(boardLink("a", { phase: "Done" })).toBe("/t/a?phase=Done");
  });

  it("builds an agent's link", () => {
    expect(agentLink("a b", "x/y")).toBe("/t/a%20b/agents/x%2Fy");
  });

  it("builds a task's link, at a run when one is given", () => {
    expect(taskLink("a b", "x/y")).toBe("/t/a%20b/tasks/x%2Fy");
    expect(taskLink("a", "x", "run 1")).toBe("/t/a/tasks/x?run=run+1");
  });
});

describe("useParam", () => {
  afterEach(cleanup);
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

  it("keeps the same setter while the query is the same", () => {
    const { result, rerender } = renderHook(use, { wrapper: at("/t/a?run=R1") });
    const set = result.current.param[1];
    rerender();
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

  it("sets from the query a link navigated to, when a child's effect sets", () => {
    // The child normalises the view the parent's link leaves invalid, with
    // the parent's setter. Its effect runs before the parent's, so a setter
    // of the last render would set on ?phase=Todo and undo the navigation.
    function Child({ view, setView }: { view: string; setView: (v: string) => void }) {
      useEffect(() => {
        if (view !== "board" && view !== "list") setView("board");
      }, [view, setView]);
      return null;
    }
    function Parent() {
      const [phase] = useParam("phase");
      const [view, setView] = useParam("view");
      const location = useLocation();
      return createElement(
        "div",
        null,
        createElement("output", null, location.search),
        createElement(Link, { to: "?phase=Done&view=bogus" }, phase),
        createElement(Child, { view, setView }),
      );
    }
    render(createElement(MemoryRouter, { initialEntries: ["/t/a?phase=Todo&view=board"] }, createElement(Parent)));
    act(() => fireEvent.click(screen.getByRole("link", { name: "Todo" })));
    expect(screen.getByRole("status").textContent).toBe("?phase=Done&view=board");
  });
});
