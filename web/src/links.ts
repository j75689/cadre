import { useCallback, useRef, useState, type ChangeEvent, type CompositionEvent } from "react";
import { NavigationType, useNavigationType, useSearchParams } from "react-router";

// The UI's URLs: the team's pages and the query parameters they read, so a
// link built on one page is the one another page understands.

export const params = { q: "q", phase: "phase", assignee: "assignee", view: "view", sort: "sort", run: "run" } as const;

export const teamPath = (ns: string) => `/t/${encodeURIComponent(ns)}`;

// withQuery appends the set values as a query string, none when all are unset.
function withQuery(path: string, values: [string, string | undefined][]): string {
  const query = new URLSearchParams();
  for (const [k, v] of values) if (v) query.set(k, v);
  const s = query.toString();
  return s ? `${path}?${s}` : path;
}

// boardLink is the team's board, filtered to an assignee or a phase, or as
// a list.
export function boardLink(ns: string, f: { assignee?: string; phase?: string; view?: "list" } = {}): string {
  return withQuery(teamPath(ns), [
    [params.assignee, f.assignee],
    [params.phase, f.phase],
    [params.view, f.view],
  ]);
}

// agentLink is an Agent's page.
export const agentLink = (ns: string, name: string) => `${teamPath(ns)}/agents/${encodeURIComponent(name)}`;

// modelProviderLink is a ModelProvider's page.
export const modelProviderLink = (ns: string, name: string) => `${teamPath(ns)}/modelproviders/${encodeURIComponent(name)}`;

// toolServerLink is a ToolServer's page.
export const toolServerLink = (ns: string, name: string) => `${teamPath(ns)}/toolservers/${encodeURIComponent(name)}`;

// taskLink is a task's page, opened at one of its runs when run is set.
export function taskLink(ns: string, name: string, run?: string): string {
  return withQuery(`${teamPath(ns)}/tasks/${encodeURIComponent(name)}`, [[params.run, run]]);
}

// pending is what the sets of the current event made of the query:
// react-router hands setSearchParams the params of the last render, so a
// second set in one event would drop the first.
let pending: URLSearchParams | undefined;

// useParam is a query parameter of the page and its setter, the same while
// the query is. Setting one replaces the history entry, and an empty value
// deletes the parameter. Sets in one event all apply.
//
// An effect that sets a parameter must check its value first: the setter
// changes whenever the query does, so the effect runs again after every
// navigation, and setting unchecked would undo the user's choice.
export function useParam(key: string): [string, (value: string) => void] {
  const [search, setSearch] = useSearchParams();
  // The setter follows setSearch, not a ref updated in an effect: a child's
  // effect runs before its parent's, and would set from the query before a
  // navigation, undoing it.
  const set = useCallback(
    (value: string) =>
      setSearch(
        (prev) => {
          const next = new URLSearchParams(pending ?? prev);
          if (value) next.set(key, value);
          else next.delete(key);
          if (!pending) queueMicrotask(() => (pending = undefined));
          pending = next;
          return next;
        },
        { replace: true },
      ),
    [key, setSearch],
  );
  return [search.get(key) ?? "", set];
}

// useTextParam is useParam for a text box: the parameter, its setter, and
// the props of the box. The router applies a URL change in a transition, so
// a box whose value is the parameter loses keys and breaks an input
// method's composition. The box keeps its own value instead and writes it
// to the parameter, not while a composition is on but at its end. It takes
// the parameter's value only when a navigation other than a replace, as
// its own writes are, changes it: Back, Forward, a link.
export function useTextParam(key: string) {
  const [param, setParam] = useParam(key);
  const navigation = useNavigationType();
  const [value, setValue] = useState(param);
  const [seen, setSeen] = useState(param);
  const composing = useRef(false);
  if (param !== seen) {
    setSeen(param);
    if (navigation !== NavigationType.Replace) setValue(param);
  }
  const set = (v: string) => {
    setValue(v);
    if (!composing.current) setParam(v);
  };
  const box = {
    value,
    onChange: (e: ChangeEvent<HTMLInputElement>) => set(e.target.value),
    onCompositionStart: () => (composing.current = true),
    onCompositionEnd: (e: CompositionEvent<HTMLInputElement>) => {
      composing.current = false;
      set(e.currentTarget.value);
    },
  };
  return [param, set, box] as const;
}
