import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
    expect(roles.getByText("user:lee").closest("td")?.classList.contains("table-text")).toBe(true);

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
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
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

  it("shows a 403 without Retry", async () => {
    open({ mode: "oidc", username: "kim" }, () => json({ error: { code: "no_role", message: "you have no role in a" } }, 403));
    expect(await screen.findByText(/Could not load the team: you have no role in a/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
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

// edit opens team a's settings as an admin and the Team card's form. The
// Team's reads and PUTs are answered in turn, the last one repeated; sent
// holds the PUTs' bodies.
async function edit(reads: (() => Promise<Response>)[], puts: (() => Promise<Response>)[] = []) {
  const sent: Record<string, unknown>[] = [];
  let n = 0;
  vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
    if (url.endsWith("/me")) return json({ mode: "oidc", name: "dana", roles: { a: "admin" } });
    if (url.endsWith("/namespaces/a/team") && init?.method === "PUT") {
      sent.push(JSON.parse(String(init.body)));
      return puts[Math.min(sent.length - 1, puts.length - 1)]();
    }
    if (url.endsWith("/namespaces/a/team")) return reads[Math.min(n++, reads.length - 1)]();
    return json({ items: [] });
  });
  render(
    <MemoryRouter initialEntries={["/t/a/settings"]}>
      <App />
    </MemoryRouter>,
  );
  fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
  const form = within(screen.getByRole("form", { name: "Edit the team" }));
  return { form, sent, reads: () => n };
}

const conflict = () => json({ error: { code: "conflict", message: "the Team of a changed since it was read" } }, 409);

describe("Editing the Team", () => {
  it("saves the whole spec from the version read, and shows the saved values", async () => {
    const after = team({ metadata: { name: "team", namespace: "a", resourceVersion: "8" }, spec: { ...full.spec, displayName: "Team B", maxConcurrentRuns: 3, archiveAfter: "30m0s" } });
    const { form, sent } = await edit([() => json(full)], [() => json(after)]);
    const name = form.getByLabelText("Display name");
    await waitFor(() => expect(document.activeElement).toBe(name));
    fireEvent.change(name, { target: { value: "Team B" } });
    fireEvent.change(form.getByLabelText(/^Max concurrent runs/), { target: { value: "3" } });
    fireEvent.change(form.getByLabelText(/^Archive after/), { target: { value: "30m" } });
    fireEvent.click(form.getByRole("button", { name: "Save" }));

    const card = within(screen.getByRole("region", { name: "Team" }));
    expect(await card.findByText("Team B")).toBeTruthy();
    expect(card.getByText("3")).toBeTruthy();
    expect(card.getByText("30m0s")).toBeTruthy();
    expect(card.getByRole("status").textContent).toBe("Saved");
    expect(sent).toEqual([{ displayName: "Team B", maxConcurrentRuns: 3, archiveAfter: "30m", roles: full.spec!.roles, resourceVersion: "7" }]);
    await waitFor(() => expect(document.activeElement).toBe(card.getByRole("button", { name: "Edit" })));
  });

  it("edits the roles and their subjects", async () => {
    const { form, sent } = await edit([() => json(full)], [() => json(full)]);
    const viewer = within(form.getByRole("group", { name: "Role 2" }));
    fireEvent.click(viewer.getByRole("button", { name: "Remove subject 1" }));
    fireEvent.click(viewer.getByRole("button", { name: "Add subject" }));
    fireEvent.click(viewer.getAllByRole("radio", { name: "group" })[1]);
    fireEvent.change(viewer.getByLabelText("Name of subject 2"), { target: { value: " sre " } });
    fireEvent.click(form.getByRole("button", { name: "Add role" }));
    const added = within(form.getByRole("group", { name: "Role 3" }));
    fireEvent.change(added.getByLabelText("Role"), { target: { value: "member" } });
    fireEvent.change(added.getByLabelText("Name of subject 1"), { target: { value: "kim" } });
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    await screen.findByText("Saved");
    expect(sent[0].roles).toEqual([
      { role: "admin", subjects: [{ kind: "user", name: "dana" }] },
      { role: "viewer", subjects: [{ kind: "user", name: "lee" }, { kind: "group", name: "sre" }] },
      { role: "member", subjects: [{ kind: "user", name: "kim" }] },
    ]);
  });

  it("keeps the edits after a 409, shows the current Team, and saves again from its version", async () => {
    const theirs = team({ metadata: { name: "team", namespace: "a", resourceVersion: "9" }, spec: { ...full.spec, displayName: "Team C" } });
    const { form, sent, reads } = await edit([() => json(full), () => json(theirs)], [conflict, () => json(theirs)]);
    fireEvent.change(form.getByLabelText("Display name"), { target: { value: "Team B" } });
    fireEvent.click(form.getByRole("button", { name: "Save" }));

    const alert = await form.findByText(/Someone else changed the team/);
    expect(alert.textContent).toMatch(/the Team of a changed since it was read/);
    await waitFor(() => expect(document.activeElement).toBe(alert.closest("[role=alert]")));
    expect(reads()).toBe(2);
    expect((form.getByLabelText("Display name") as HTMLInputElement).value).toBe("Team B");
    const current = within(await form.findByRole("region", { name: "Current" }));
    expect(current.getByText("Team C")).toBeTruthy();

    fireEvent.click(form.getByRole("button", { name: "Save" }));
    await screen.findByText("Saved");
    expect(sent.map((b) => b.resourceVersion)).toEqual(["7", "9"]);
    expect(sent[1].displayName).toBe("Team B");
  });

  it("saves from the version it opened with, even after a newer read", async () => {
    const theirs = team({ metadata: { name: "team", namespace: "a", resourceVersion: "9" }, spec: { ...full.spec, maxConcurrentRuns: 5 } });
    const { form, sent, reads } = await edit([() => json(full), () => json(theirs)], [conflict]);
    fireEvent.change(form.getByLabelText("Display name"), { target: { value: "Team B" } });
    document.dispatchEvent(new Event("visibilitychange")); // the page reads the Team again
    await waitFor(() => expect(reads()).toBe(2));
    await screen.findByText(/of 5 in use/); // the newer read has landed
    expect((form.getByLabelText(/^Max concurrent runs/) as HTMLInputElement).value).toBe("2");
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    await form.findByText(/Someone else changed the team/);
    expect(sent[0].resourceVersion).toBe("7");
  });

  it("cannot save a Team deleted since it was read", async () => {
    const { form } = await edit([() => json(full), () => json({ error: { code: "not_found", message: "no Team in a" } }, 404)], [conflict]);
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    expect(await form.findByText(/nothing to save to/)).toBeTruthy();
    expect((form.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("says why the server refused, after the roles", async () => {
    const refused = () => json({ error: { code: "invalid", message: "the Team would have no admin, and no bootstrap admin is set to give it one back: keep an admin role" } }, 400);
    const { form, sent } = await edit([() => json(full)], [refused]);
    fireEvent.click(within(form.getByRole("group", { name: "Role 1" })).getByRole("button", { name: "Remove role" }));
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    const alert = await form.findByRole("alert");
    expect(alert.textContent).toMatch(/keep an admin role/);
    expect(alert.previousElementSibling).toBe(form.getByRole("group", { name: "Roles" }));
    await waitFor(() => expect(document.activeElement).toBe(alert));
    expect(sent).toHaveLength(1);
  });

  it("checks a duration before sending it", async () => {
    const { form, sent } = await edit([() => json(full)]);
    fireEvent.change(form.getByLabelText(/^Archive after/), { target: { value: "a week" } });
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    expect(form.getByRole("alert").textContent).toMatch(/duration such as 168h/);
    expect(sent).toHaveLength(0);
  });

  it("warns about a Team managed by GitOps, and saves only once confirmed", async () => {
    const applied = team({ ...full, metadata: { ...full.metadata, annotations: { "kubectl.kubernetes.io/last-applied-configuration": "{}" } } });
    const { form, sent } = await edit([() => json(applied)], [() => json(applied)]);
    expect(form.getByText(/This Team looks managed by GitOps \(it was last written by kubectl apply\)/)).toBeTruthy();
    const save = form.getByRole("button", { name: "Save" });
    fireEvent.click(save);
    const confirm = within(form.getByRole("group", { name: "Save anyway?" }));
    fireEvent.click(confirm.getByRole("button", { name: "Keep editing" }));
    expect(form.queryByRole("group", { name: "Save anyway?" })).toBeNull();
    await waitFor(() => expect(document.activeElement).toBe(save));
    expect(sent).toHaveLength(0);

    fireEvent.click(save);
    fireEvent.click(within(form.getByRole("group", { name: "Save anyway?" })).getByRole("button", { name: "Save anyway" }));
    await screen.findByText("Saved");
    expect(sent).toHaveLength(1);
  });

  it("goes back to the details on Cancel, with the focus on Edit", async () => {
    const { form, sent } = await edit([() => json(full)]);
    fireEvent.change(form.getByLabelText("Display name"), { target: { value: "Team B" } });
    fireEvent.click(form.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("form", { name: "Edit the team" })).toBeNull();
    const card = within(screen.getByRole("region", { name: "Team" }));
    expect(card.getByText("Team A")).toBeTruthy();
    await waitFor(() => expect(document.activeElement).toBe(card.getByRole("button", { name: "Edit" })));
    expect(sent).toHaveLength(0);
  });
});
