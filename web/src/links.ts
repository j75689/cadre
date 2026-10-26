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

// taskLink is a task's page, opened at one of its runs when run is set.
export function taskLink(ns: string, name: string, run?: string): string {
  return withQuery(`${teamPath(ns)}/tasks/${encodeURIComponent(name)}`, [[params.run, run]]);
}
