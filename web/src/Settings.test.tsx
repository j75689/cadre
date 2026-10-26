import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import type { Me } from "./api";
import { json, team } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const full = team({
  spec: {
    displayName: "Team A",
    maxConcurrentRuns: 2,
    archiveAfter: "168h0m0s",
    roles: [
      { role: "admin", subjects: [{ kind: "user", name: "dana" }] },
      { role: "viewer", subjects: [{ kind: "group", name: "ops" }, { kind: "user", name: "lee" }] },
    ],
  },
  status: { running: 1, slots: [{ runId: "01JRUN", task: "01jtask", acquiredAt: "2026-10-25T09:00:00Z" }] },
});

// What project() leaves a viewer or a member.
const stripped = team({ spec: { displayName: "Team A" }, status: {} });

// open renders the settings of team a, with /me as given and the Team's
// answers in turn, the last one repeated.
function open(me: Me, ...answers: (() => Promise<Response>)[]) {
  let reads = 0;
  vi.stubGlobal("fetch", (url: string) => {
    if (url.endsWith("/me")) return json(me);
    if (url.endsWith("/namespaces/a/team")) return answers[Math.min(reads++, answers.length - 1)]();
    return json({ items: [] });
  });
  render(
    <MemoryRouter initialEntries={["/t/a/settings"]}>
      <App />
    </MemoryRouter>,
  );
  return () => reads;
}

describe("Settings", () => {
  it("shows an admin the limits, the roles and the run slots", async () => {
    open({ mode: "oidc", name: "dana", roles: { a: "admin" } }, () => json(full));
    const card = within(await screen.findByRole("region", { name: "Team" }));
    expect(card.getByText("Team A")).toBeTruthy();
    expect(card.getByText("2")).toBeTruthy();
    expect(card.getByText("168h0m0s")).toBeTruthy();

    const roles = within(screen.getByRole("table", { name: "Roles" }));
    expect(roles.getByText("user:dana")).toBeTruthy();
    expect(roles.getByText("group:ops")).toBeTruthy();
    expect(roles.getByText("user:lee")).toBeTruthy();

    const slots = within(screen.getByRole("region", { name: "Run slots" }));
    expect(within(screen.getByRole("table", { name: "Run slots" })).getAllByRole("row")).toHaveLength(2);
    expect(slots.getByText("1 of 2 in use")).toBeTruthy();
    expect(slots.getByRole("link", { name: "01jtask" }).getAttribute("href")).toBe("/t/a/tasks/01jtask?run=01JRUN");
    expect(slots.getByText("01JRUN")).toBeTruthy();
  });

  // An idle Team comes without slots (omitempty), and often without roles.
  it("shows an admin a Team without roles or slots", async () => {
    open({ mode: "oidc", name: "dana", roles: { a: "admin" } }, () => json(team({ spec: { maxConcurrentRuns: 1 }, status: {} })));
    expect(await screen.findByText("No roles are given on this team.")).toBeTruthy();
    expect(screen.getByText("No run holds a slot.")).toBeTruthy();
    expect(screen.getByText("0 of 1 in use")).toBeTruthy();
  });

  it("shows an admin the server's defaults for unset limits", async () => {
    open({ mode: "oidc", name: "dana", roles: { a: "admin" } }, () => json(team({ status: { running: 1 } })));
    const card = within(await screen.findByRole("region", { name: "Team" }));
    expect(card.getByText("(default)").parentElement?.textContent).toBe("1 (default)");
    expect(card.getByText("never")).toBeTruthy();
    expect(screen.getByText("1 of 1 in use")).toBeTruthy();
  });

  it("shows an admin the limits under --auth=none", async () => {
    open({ mode: "none", admin: true }, () => json(full));
    expect(await screen.findByRole("heading", { name: "Roles" })).toBeTruthy();
    expect(screen.queryByText(/to its admins only/)).toBeNull();
  });

  it.each(["viewer", "member"])("shows a %s the name only, from the stripped Team", async (role) => {
    open({ mode: "oidc", name: "lee", roles: { a: role } }, () => json(stripped));
    expect(await screen.findByText("This page shows a team's limits, roles and run slots to its admins only.")).toBeTruthy();
    const card = within(screen.getByRole("region", { name: "Team" }));
    expect(card.getByText("Team A")).toBeTruthy();
    expect(card.getByText("a")).toBeTruthy();
    expect(card.queryByText("Max concurrent runs")).toBeNull();
    expect(screen.queryByRole("heading", { name: "Roles" })).toBeNull();
    expect(screen.queryByRole("heading", { name: "Run slots" })).toBeNull();
  });

  it("says when the namespace has no Team", async () => {
    open({ mode: "none", admin: true }, () => json({ error: { code: "not_found", message: "no Team in a" } }, 404));
    expect(await screen.findByText(/This namespace has no Team/)).toBeTruthy();
    expect(screen.getByText("cadre team init -n a")).toBeTruthy();
  });

  it("shows a failed read and reads again on Retry", async () => {
    const reads = open(
      { mode: "none", admin: true },
      () => json({ error: { code: "internal", message: "the cache is not synced" } }, 500),
      () => json(full),
    );
    expect(await screen.findByText(/Could not load the team: the cache is not synced/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("heading", { name: "Roles" })).toBeTruthy();
    expect(screen.queryByText(/Could not load the team/)).toBeNull();
    expect(reads()).toBe(2);
  });

  it("is the current tab in the team nav", async () => {
    open({ mode: "none", admin: true }, () => json(full));
    const nav = within(screen.getByRole("navigation", { name: "Team" }));
    const tab = nav.getByRole("link", { name: "Settings" });
    expect(tab.getAttribute("aria-current")).toBe("page");
    expect(tab.getAttribute("href")).toBe("/t/a/settings");
    expect(nav.getByRole("link", { name: "Board" }).getAttribute("aria-current")).toBeNull();
    await screen.findByRole("heading", { name: "Roles" });
  });
});
