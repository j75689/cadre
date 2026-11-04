import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { rolesPollMs } from "./Identity";
import { FakeEventSource, fakeTabs, json, team } from "./testing";
import { hintDelayMs } from "./usePolling";

// roles is what /me says the caller is, by namespace, as an admin changes
// it; me counts the reads of /me. A Team read without a role on a is a 403
// no_role, and one with a role other than admin comes without its roles,
// as cadre-server answers them.
let roles: Record<string, string> = {};
let me = 0;

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  roles = { a: "viewer" };
  me = 0;
  vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
    if (url.endsWith("/me")) {
      me++;
      return json({ mode: "oidc", username: "dana@example.test", name: "Dana", roles: { ...roles } });
    }
    if (url.endsWith("/teams")) return json({ items: [team()] });
    if (url.endsWith("/namespaces/a/team") && init?.method === "PUT") {
      return json({ error: { code: "no_role", message: `editing a's Team needs the admin role; you are a ${roles.a}` } }, 403);
    }
    if (url.endsWith("/namespaces/a/team")) {
      if (!roles.a) return json({ error: { code: "no_role", message: "you have no role on a" } }, 403);
      return json(roles.a === "admin" ? team({ spec: { displayName: "Team A", roles: [{ role: "admin", subjects: [{ kind: "user", name: "dana@example.test" }] }] } }) : team());
    }
    if (url.includes("/tasks?")) return json({ items: [], offset: 0, limit: 50 });
    return json({ items: [] });
  });
});
afterEach(async () => {
  cleanup();
  await vi.advanceTimersByTimeAsync(0);
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

// open renders path in team a and, with a stream, gives it a hello
// serving everything; it returns the stream.
async function open(path: string, role: RegExp) {
  render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
  expect(await screen.findByText(role)).toBeTruthy();
  await act(() => vi.advanceTimersByTimeAsync(0));
  const stream = FakeEventSource.open()[0];
  if (stream) {
    await act(async () => stream.emit("hello", { tasks: true, config: true, runs: false, lifetimeSeconds: 300 }));
    await act(() => vi.advanceTimersByTimeAsync(500));
  }
  return stream;
}

// send gives the stream its events together and counts the reads of /me
// they bring within a second.
async function send(stream: FakeEventSource, ...events: [string, object][]) {
  const before = me;
  await act(async () => events.forEach(([event, data]) => stream.emit(event, data)));
  await act(() => vi.advanceTimersByTimeAsync(hintDelayMs + 50));
  return me - before;
}

const teamHint: [string, object] = ["config", { kind: "Team", name: "team" }];

describe("Roles while live", () => {
  beforeEach(() => fakeTabs());

  it("are read again once for a burst of Team hints, and for no other hint", async () => {
    const stream = await open("/t/a", /You are a viewer of a/);
    // Once for the page and every component on it, once for the hello.
    expect(me).toBe(2);
    expect(screen.queryByRole("link", { name: "New task" })).toBeNull();

    expect(await send(stream, ["config", { kind: "Agent", name: "writer" }], ["task", { name: "y", number: 2, phase: "Todo" }])).toBe(0);
    roles = { a: "member" };
    expect(await send(stream, teamHint, teamHint, ["config", { kind: "Agent", name: "writer" }], teamHint)).toBe(1);
    expect(screen.getByText("You are a member of a.")).toBeTruthy();
    expect(screen.getByRole("link", { name: "New task" })).toBeTruthy();
  });

  it("are read again on a resync and on the hello of a reconnect", async () => {
    const stream = await open("/t/a", /You are a viewer of a/);
    roles = { a: "member" };
    expect(await send(stream, ["resync", {}])).toBe(1);
    expect(screen.getByText("You are a member of a.")).toBeTruthy();
    roles = { a: "admin" };
    await act(async () => stream.fail());
    expect(await send(stream, ["hello", { tasks: true, config: true, runs: false }])).toBe(1);
    expect(screen.getByText("You are an admin of a.")).toBeTruthy();
  });

  it("are not read again for another page of the team", async () => {
    await open("/t/a", /You are a viewer of a/);
    const before = me;
    fireEvent.click(screen.getByRole("link", { name: "Settings" }));
    expect(await screen.findByRole("heading", { name: "Settings" })).toBeTruthy();
    await act(() => vi.advanceTimersByTimeAsync(1000));
    expect(me).toBe(before);
  });

  it("give a member made an admin Settings' Edit and Activity, and take them back", async () => {
    roles = { a: "member" };
    const stream = await open("/t/a/settings", /You are a member of a/);
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    roles = { a: "admin" };
    expect(await send(stream, teamHint)).toBe(1);
    expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Activity" })).toBeTruthy();

    roles = { a: "viewer" };
    expect(await send(stream, teamHint)).toBe(1);
    expect(screen.getByText("You are a viewer of a.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Activity" })).toBeNull();
  });

  it("move the focus from a tab the new role takes away to the heading", async () => {
    roles = { a: "admin" };
    const stream = await open("/t/a/settings", /You are an admin of a/);
    act(() => screen.getByRole("link", { name: "Activity" }).focus());
    roles = { a: "member" };
    expect(await send(stream, teamHint)).toBe(1);
    expect(screen.queryByRole("link", { name: "Activity" })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("heading", { name: "Settings" }));
  });

  it("move the focus from New task to the heading when the role goes altogether", async () => {
    roles = { a: "member" };
    const stream = await open("/t/a", /You are a member of a/);
    act(() => screen.getByRole("link", { name: "New task" }).focus());
    roles = {};
    await send(stream, teamHint);
    expect(screen.queryByRole("link", { name: "New task" })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("heading", { level: 1 }));
  });

  // gate holds the answers to the URLs ending in suffix until released.
  function gate(suffix: string) {
    const inner = globalThis.fetch;
    let release = () => {};
    const held = new Promise<void>((r) => (release = r));
    vi.stubGlobal("fetch", (url: string, init?: RequestInit) => (url.endsWith(suffix) ? held.then(() => inner(url, init)) : inner(url, init)));
    return () => act(async () => release());
  }

  it("move the focus from Edit to Settings' alert when /me says the role went before the Team's 403", async () => {
    roles = { a: "admin" };
    const stream = await open("/t/a/settings", /You are an admin of a/);
    act(() => screen.getByRole("button", { name: "Edit" }).focus());
    // As in Chrome: the /me answer is shown before the Team's 403 comes.
    const release = gate("/namespaces/a/team");
    roles = {};
    await send(stream, teamHint);
    expect(screen.getByText("You have no role on a.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    await release();
    const alert = (await screen.findByText(/Your role on a is gone or has changed/)).closest<HTMLElement>("[role=alert]")!;
    await act(() => vi.advanceTimersByTimeAsync(500));
    expect(document.activeElement).toBe(alert);
  });

  it("move the focus from a tab to Settings' alert when the Team's 403 comes before /me", async () => {
    roles = { a: "admin" };
    const stream = await open("/t/a/settings", /You are an admin of a/);
    act(() => screen.getByRole("link", { name: "Activity" }).focus());
    const release = gate("/me");
    roles = {};
    await send(stream, teamHint);
    const alert = (await screen.findByText(/Your role on a is gone or has changed/)).closest<HTMLElement>("[role=alert]")!;
    expect(screen.getByRole("link", { name: "Activity" })).toBeTruthy();
    await release();
    expect(await screen.findByText("You have no role on a.")).toBeTruthy();
    await act(() => vi.advanceTimersByTimeAsync(500));
    expect(screen.queryByRole("link", { name: "Activity" })).toBeNull();
    expect(document.activeElement).toBe(alert);
  });

  it("leave the focus on what stays when /me says the role went before the Team's 403", async () => {
    roles = { a: "admin" };
    const stream = await open("/t/a/settings", /You are an admin of a/);
    const crumb = within(screen.getByRole("navigation", { name: "Breadcrumb" })).getAllByRole("link")[0];
    act(() => crumb.focus());
    const release = gate("/namespaces/a/team");
    roles = {};
    await send(stream, teamHint);
    expect(screen.getByText("You have no role on a.")).toBeTruthy();
    await release();
    expect(await screen.findByText(/Your role on a is gone or has changed/)).toBeTruthy();
    await act(() => vi.advanceTimersByTimeAsync(500));
    expect(document.activeElement).toBe(crumb);
  });

  it("leave the focus where it was put after /me took Edit, when the Team's 403 comes", async () => {
    roles = { a: "admin" };
    const stream = await open("/t/a/settings", /You are an admin of a/);
    act(() => screen.getByRole("button", { name: "Edit" }).focus());
    const release = gate("/namespaces/a/team");
    roles = {};
    await send(stream, teamHint);
    expect(screen.getByText("You have no role on a.")).toBeTruthy();
    const crumb = within(screen.getByRole("navigation", { name: "Breadcrumb" })).getAllByRole("link")[0];
    act(() => crumb.focus());
    await release();
    expect(await screen.findByText(/Your role on a is gone or has changed/)).toBeTruthy();
    await act(() => vi.advanceTimersByTimeAsync(500));
    expect(document.activeElement).toBe(crumb);
  });

  it("show Settings again to a caller whose role was taken and given back, the focus off the body", async () => {
    roles = { a: "admin" };
    const stream = await open("/t/a/settings", /You are an admin of a/);
    roles = {};
    await send(stream, teamHint);
    const alert = (await screen.findByText(/Your role on a is gone or has changed/)).closest<HTMLElement>("[role=alert]")!;
    expect(await screen.findByText("You have no role on a.")).toBeTruthy();
    alert.focus();

    roles = { a: "admin" };
    await send(stream, teamHint);
    expect(screen.queryByText(/Your role on a is gone or has changed/)).toBeNull();
    expect(screen.getByText("You are an admin of a.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy();
    expect(document.activeElement).toBe(screen.getByRole("heading", { name: "Settings" }));
  });

  it("keep Settings' alert for a role that changed but did not go", async () => {
    roles = { a: "admin" };
    await open("/t/a/settings", /You are an admin of a/);
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    roles = { a: "member" };
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Save" })));
    expect(await screen.findByText("You are a member of a.")).toBeTruthy();
    await act(() => vi.advanceTimersByTimeAsync(1000));
    expect(screen.getByText(/Your role on a is gone or has changed/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
  });
});

describe("Roles without a stream", () => {
  beforeEach(() => fakeTabs({ locks: false }));

  it("are read again every 15 s, not at the board's 5 s", async () => {
    await open("/t/a", /You are a viewer of a/);
    expect(FakeEventSource.open()).toHaveLength(0);
    expect(me).toBe(1);
    roles = { a: "member" };
    await act(() => vi.advanceTimersByTimeAsync(14000));
    expect(me).toBe(1);
    await act(() => vi.advanceTimersByTimeAsync(1000));
    expect(me).toBe(2);
    expect(screen.getByText("You are a member of a.")).toBeTruthy();
    expect(screen.getByRole("link", { name: "New task" })).toBeTruthy();
  });

  it("are not read again without logins", async () => {
    const inner = globalThis.fetch;
    vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
      if (!url.endsWith("/me")) return inner(url, init);
      me++;
      return json({ mode: "none", admin: true });
    });
    render(
      <MemoryRouter initialEntries={["/t/a"]}>
        <App />
      </MemoryRouter>,
    );
    expect(await screen.findByRole("link", { name: "New task" })).toBeTruthy();
    await act(() => vi.advanceTimersByTimeAsync(2 * rolesPollMs));
    expect(me).toBe(1);
  });

  it("keep the roles they had when a read again fails", async () => {
    await open("/t/a", /You are a viewer of a/);
    const inner = globalThis.fetch;
    vi.stubGlobal("fetch", (url: string) => (url.endsWith("/me") ? json({ error: { code: "unavailable", message: "down" } }, 503) : inner(url)));
    await act(() => vi.advanceTimersByTimeAsync(rolesPollMs));
    expect(screen.getByText("You are a viewer of a.")).toBeTruthy();
    expect(screen.queryByText(/Could not tell who you are/)).toBeNull();
  });

  it("show Settings to a caller refused it once they are given a role", async () => {
    roles = {};
    await open("/t/a/settings", /You have no role on a/);
    expect(await screen.findByText(/Could not load the team/)).toBeTruthy();
    roles = { a: "admin" };
    await act(() => vi.advanceTimersByTimeAsync(rolesPollMs));
    expect(screen.queryByText(/Could not load the team/)).toBeNull();
    expect(screen.getByText("You are an admin of a.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy();
  });

  // A member is sent the Team without its roles (project()): saved as is,
  // it would take every role away.
  it("give a member made an admin Edit only on the Team read as an admin", async () => {
    roles = { a: "member" };
    const admins = [{ role: "admin" as const, subjects: [{ kind: "user" as const, name: "dana@example.test" }] }];
    let release = () => {};
    const held = new Promise<void>((resolve) => (release = resolve));
    const sent: { roles?: unknown[] }[] = [];
    const inner = globalThis.fetch;
    vi.stubGlobal("fetch", async (url: string, init?: RequestInit) => {
      if (!url.endsWith("/namespaces/a/team")) return inner(url, init);
      if (init?.method === "PUT") {
        sent.push(JSON.parse(String(init.body)));
        return json(team({ spec: { displayName: "Team A", roles: admins } }));
      }
      if (roles.a !== "admin") return json(team());
      await held;
      return json(team({ metadata: { name: "team", namespace: "a", resourceVersion: "8" }, spec: { displayName: "Team A", roles: admins } }));
    });
    await open("/t/a", /You are a member of a/);
    await act(() => vi.advanceTimersByTimeAsync(7000));
    fireEvent.click(screen.getByRole("link", { name: "Settings" }));
    expect(await screen.findByText("This page shows a team's limits, roles and run slots to its admins only.")).toBeTruthy();

    roles = { a: "admin" };
    await act(() => vi.advanceTimersByTimeAsync(rolesPollMs - 7000));
    expect(screen.getByText("You are an admin of a.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Roles" })).toBeNull();

    await act(async () => release());
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Save" })));
    expect(sent).toHaveLength(1);
    expect(sent[0].roles).toEqual(admins);
  });

  // The role is taken and given back between two reads of /me (#468): the
  // Team was read without it, and /me says nothing changed. Saving that
  // Team would take every role away.
  it("give an admin Edit only on a Team the server sent with its roles", async () => {
    roles = { a: "admin" };
    let rv = 7;
    const admins = [{ role: "admin" as const, subjects: [{ kind: "user" as const, name: "dana@example.test" }] }, { role: "member" as const, subjects: [{ kind: "user" as const, name: "erin@example.test" }] }];
    const give = (role: string) => {
      roles = { a: role };
      rv++;
    };
    const sent: { roles?: unknown[]; resourceVersion?: string }[] = [];
    const inner = globalThis.fetch;
    vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
      if (!url.endsWith("/namespaces/a/team")) return inner(url, init);
      if (init?.method === "PUT") {
        const body = JSON.parse(String(init.body));
        sent.push(body);
        if (body.resourceVersion !== String(rv)) return json({ error: { code: "conflict", message: "the Team of a changed since it was read" } }, 409);
        rv++;
        return json(team({ metadata: { name: "team", namespace: "a", resourceVersion: String(rv) }, spec: { displayName: body.displayName, roles: body.roles } }));
      }
      const metadata = { name: "team", namespace: "a", resourceVersion: String(rv) };
      return json(roles.a === "admin" ? team({ metadata, spec: { displayName: "Team A", roles: admins } }) : team({ metadata }));
    });
    await open("/t/a", /You are an admin of a/);
    await act(() => vi.advanceTimersByTimeAsync(6000));
    give("member");
    fireEvent.click(screen.getByRole("link", { name: "Settings" }));
    await act(() => vi.advanceTimersByTimeAsync(500));
    give("admin");
    await act(() => vi.advanceTimersByTimeAsync(rolesPollMs - 6000));
    expect(screen.getByText("You are an admin of a.")).toBeTruthy();
    expect(screen.getByRole("region", { name: "Team" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Roles" })).toBeNull();
    expect(screen.queryByText("No roles are given on this team.")).toBeNull();

    // Settings' next read is an admin's.
    await act(() => vi.advanceTimersByTimeAsync(6000));
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Save" })));
    await screen.findByText("Saved");
    expect(sent).toEqual([expect.objectContaining({ resourceVersion: "9", roles: admins })]);
  });

  // The Team read as an admin has its roles, and the read again for the new
  // role has not come back yet.
  it("take Edit away on the role /me gives before the Team is read again", async () => {
    roles = { a: "admin" };
    const admins = [{ role: "admin" as const, subjects: [{ kind: "user" as const, name: "dana@example.test" }] }];
    let reads = 0;
    const inner = globalThis.fetch;
    vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
      if (!url.endsWith("/namespaces/a/team")) return inner(url, init);
      if (reads++ > 0) return new Promise<Response>(() => {});
      return json(team({ spec: { displayName: "Team A", roles: admins } }));
    });
    await open("/t/a/settings", /You are an admin of a/);
    expect(await screen.findByRole("button", { name: "Edit" })).toBeTruthy();
    roles = { a: "member" };
    await act(() => vi.advanceTimersByTimeAsync(rolesPollMs));
    expect(screen.getByText("You are a member of a.")).toBeTruthy();
    expect(reads).toBeGreaterThan(1);
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Roles" })).toBeNull();
  });
});
