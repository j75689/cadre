import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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

const admin: Me = { mode: "oidc", name: "dana", roles: { a: "admin" } };

// edit opens team a's settings as an admin and the Team card's form. The
// Team's reads and PUTs, and /me, are answered in turn, the last one
// repeated; sent holds the PUTs' bodies.
async function edit(reads: (() => Promise<Response>)[], puts: (() => Promise<Response>)[] = [], mes: Me[] = [admin]) {
  const sent: Record<string, unknown>[] = [];
  let n = 0;
  let m = 0;
  vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
    if (url.endsWith("/me")) return json(mes[Math.min(m++, mes.length - 1)]);
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
const noRole = () => json({ error: { code: "no_role", message: "to manage the team on a needs the admin role; you have no role there" } }, 403);
const noTeam = () => json({ error: { code: "not_found", message: "no Team in a" } }, 404);

describe("Editing the Team", () => {
  it("is data-dirty only while the draft differs from the Team", async () => {
    const { form } = await edit([() => json(full)]);
    const name = form.getByLabelText("Display name");
    expect(document.querySelector("[data-dirty]")).toBeNull();
    fireEvent.change(name, { target: { value: "Team B" } });
    expect(document.querySelector("[data-dirty]")).not.toBeNull();
  });

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
    expect(alert.textContent).toMatch(/Saving again replaces their values/);
    await waitFor(() => expect(document.activeElement).toBe(alert.closest("[role=alert]")));
    expect(reads()).toBe(2);
    expect((form.getByLabelText(/^Display name/) as HTMLInputElement).value).toBe("Team B");
    const current = within(await form.findByRole("region", { name: "Current" }));
    expect(current.getByText("Team C")).toBeTruthy();

    fireEvent.click(form.getByRole("button", { name: "Save" }));
    await screen.findByText("Saved");
    expect(sent.map((b) => b.resourceVersion)).toEqual(["7", "9"]);
    expect(sent[1].displayName).toBe("Team B");
  });

  it("marks the fields someone else changed, unless the form holds their value", async () => {
    const theirs = team({
      metadata: { name: "team", namespace: "a", resourceVersion: "9" },
      spec: { displayName: "Team C", maxConcurrentRuns: 5, archiveAfter: "30m0s", roles: [full.spec!.roles![0]] },
    });
    const { form, sent } = await edit([() => json(full), () => json(theirs)], [conflict, () => json(theirs)]);
    fireEvent.change(form.getByLabelText("Display name"), { target: { value: "Team B" } });
    fireEvent.change(form.getByLabelText(/^Archive after/), { target: { value: "30m" } });
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    await form.findByRole("region", { name: "Current" });

    const note = (label: RegExp) => {
      const ids = form.getByLabelText(label).getAttribute("aria-describedby") ?? "";
      return ids.split(" ").map((id) => document.getElementById(id)?.textContent).find((t) => t?.startsWith("Changed by someone else"));
    };
    expect(note(/^Display name/)).toBe("Changed by someone else: Team C");
    expect(note(/^Max concurrent runs/)).toBe("Changed by someone else: 5");
    expect(note(/^Archive after/)).toBeUndefined(); // 30m is 30m0s
    const roles = form.getByRole("group", { name: "Roles" });
    expect(document.getElementById(roles.getAttribute("aria-describedby")!)?.textContent).toBe("Changed by someone else: admin: user:dana");

    fireEvent.click(form.getByRole("button", { name: "Save" }));
    await screen.findByText("Saved");
    expect(sent[1]).toMatchObject({ displayName: "Team B", maxConcurrentRuns: 2, archiveAfter: "30m", roles: full.spec!.roles });
  });

  it("does not mark a field only this user changed", async () => {
    const theirs = team({ metadata: { name: "team", namespace: "a", resourceVersion: "9" }, spec: { ...full.spec, displayName: "Team C" } });
    const { form } = await edit([() => json(full), () => json(theirs)], [conflict]);
    fireEvent.change(form.getByLabelText(/^Max concurrent runs/), { target: { value: "3" } });
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    await form.findByRole("region", { name: "Current" });
    expect(form.getByLabelText(/^Display name/).getAttribute("aria-describedby")).toBe("changed-displayName");
    expect(form.getByLabelText(/^Max concurrent runs/).getAttribute("aria-describedby")).toBe("max-runs-help");
    expect(form.queryAllByText(/Changed by someone else/)).toHaveLength(1);
  });

  it("shows another 409 as the error it is, not as someone else's change", async () => {
    const exists = () => json({ error: { code: "already_exists", message: "team a: already exists" } }, 409);
    const { form, reads } = await edit([() => json(full)], [exists]);
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    expect((await form.findByRole("alert")).textContent).toBe("team a: already exists");
    expect(form.queryByText(/Someone else changed the team/)).toBeNull();
    expect(reads()).toBe(1);
  });

  it("saves from the version it opened with, even after a newer read", async () => {
    const theirs = team({ metadata: { name: "team", namespace: "a", resourceVersion: "9" }, spec: { ...full.spec, maxConcurrentRuns: 5 } });
    const { form, sent, reads } = await edit([() => json(full), () => json(theirs)], [conflict]);
    fireEvent.change(form.getByLabelText("Display name"), { target: { value: "Team B" } });
    document.dispatchEvent(new Event("visibilitychange")); // the page reads the Team again
    await waitFor(() => expect(reads()).toBe(2));
    await screen.findByText(/of 5 in use/); // the newer read has landed
    await act(async () => {}); // and its effects have run
    expect((form.getByLabelText(/^Max concurrent runs/) as HTMLInputElement).value).toBe("2");
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    await form.findByText(/Someone else changed the team/);
    expect(sent[0]).toMatchObject({ resourceVersion: "7", displayName: "Team B", maxConcurrentRuns: 2 });
  });

  it("cannot save a Team deleted since it was read", async () => {
    const { form } = await edit([() => json(full), noTeam], [conflict]);
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

  it("checks a duration before sending it, and focuses each failure", async () => {
    const { form, sent } = await edit([() => json(full)]);
    const save = form.getByRole("button", { name: "Save" });
    for (const v of ["a week", "0s", "-1h"]) {
      fireEvent.change(form.getByLabelText(/^Archive after/), { target: { value: v } });
      save.focus();
      fireEvent.click(save);
      expect(form.getByRole("alert").textContent).toMatch(/duration longer than 0, such as 168h/);
      await waitFor(() => expect(document.activeElement).toBe(form.getByRole("alert"))); // the same message too
    }
    fireEvent.change(form.getByLabelText(/^Archive after/), { target: { value: "2562048h" } });
    fireEvent.click(save);
    expect(form.getByRole("alert").textContent).toMatch(/too long: the longest the server takes is 2562047h47m16.854775807s/);
    expect(sent).toHaveLength(0);
  });

  it("ties an error to the field it is about", async () => {
    const { form } = await edit([() => json(full)]);
    const max = form.getByLabelText(/^Max concurrent runs/);
    const after = form.getByLabelText(/^Archive after/);
    fireEvent.change(after, { target: { value: "0s" } });
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    const alert = form.getByRole("alert");
    expect(after.getAttribute("aria-invalid")).toBe("true");
    expect(after.getAttribute("aria-describedby")?.split(" ")).toContain(alert.id);
    expect(max.getAttribute("aria-invalid")).toBeNull();

    fireEvent.change(after, { target: { value: "1h" } });
    fireEvent.change(max, { target: { value: "-1" } });
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    expect(max.getAttribute("aria-invalid")).toBe("true");
    expect(max.getAttribute("aria-describedby")?.split(" ")).toContain(form.getByRole("alert").id);
    expect(after.getAttribute("aria-invalid")).toBeNull();
    expect(after.getAttribute("aria-describedby")).toBe("archive-after-help");

    fireEvent.change(max, { target: { value: "2" } });
    fireEvent.click(within(form.getByRole("group", { name: "Role 1" })).getByRole("button", { name: "Add subject" }));
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    const name = within(form.getByRole("group", { name: "Role 1" })).getByLabelText("Name of subject 2");
    expect(form.getByRole("alert").textContent).toBe("Role 1, subject 2 needs a name.");
    expect(name.getAttribute("aria-invalid")).toBe("true");
    expect(name.getAttribute("aria-describedby")).toBe(form.getByRole("alert").id);
    expect(max.getAttribute("aria-invalid")).toBeNull();
  });

  it("leaves archive-after out when it is cleared", async () => {
    const { form, sent } = await edit([() => json(full)], [() => json(full)]);
    fireEvent.change(form.getByLabelText(/^Archive after/), { target: { value: "" } });
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    await screen.findByText("Saved");
    expect("archiveAfter" in sent[0]).toBe(false);
  });

  it("does not save a cap the browser could not read", async () => {
    const { form, sent } = await edit([() => json(full)]);
    const max = form.getByLabelText(/^Max concurrent runs/) as HTMLInputElement;
    // What a browser reports for 1e: an empty value, and badInput.
    Object.defineProperty(max, "validity", { value: { badInput: true } });
    fireEvent.change(max, { target: { value: "" } });
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    expect(form.getByRole("alert").textContent).toMatch(/Max concurrent runs is a whole number/);
    expect(sent).toHaveLength(0);
  });

  it("checks the roles before sending them, numbered as the form shows them", async () => {
    const { form, sent } = await edit([() => json(full)]);
    fireEvent.click(form.getByRole("button", { name: "Add role" }));
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    expect(form.getByRole("alert").textContent).toBe("Role 3, subject 1 needs a name.");

    fireEvent.click(within(form.getByRole("group", { name: "Role 3" })).getByRole("button", { name: "Remove subject 1" }));
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    expect(form.getByRole("alert").textContent).toBe("Role 3 needs at least one subject.");
    expect(sent).toHaveLength(0);
  });

  it("numbers the roles of a server refusal as the form shows them", async () => {
    const refused = () => json({ error: { code: "invalid", message: "roles[1].subjects[0]: a group needs a name: invalid" } }, 400);
    const { form } = await edit([() => json(full)], [refused]);
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    const alert = await form.findByRole("alert");
    expect(alert.textContent).toBe("Role 2, subject 1: a group needs a name: invalid");
    const name = within(form.getByRole("group", { name: "Role 2" })).getByLabelText("Name of subject 1");
    expect(name.getAttribute("aria-invalid")).toBe("true");
    expect(name.getAttribute("aria-describedby")).toBe(alert.id);
  });

  it("moves the focus to the next role, else the one before, else Add role, once a role is removed", async () => {
    const { form } = await edit([() => json(full)]);
    const select = (n: number) => within(form.getByRole("group", { name: `Role ${n}` })).getByLabelText("Role");
    fireEvent.click(within(form.getByRole("group", { name: "Role 1" })).getByRole("button", { name: "Remove role" }));
    await waitFor(() => expect(document.activeElement).toBe(select(1))); // the viewer role, now first
    expect((select(1) as HTMLSelectElement).value).toBe("viewer");
    fireEvent.click(form.getByRole("button", { name: "Add role" }));
    fireEvent.click(within(form.getByRole("group", { name: "Role 2" })).getByRole("button", { name: "Remove role" }));
    await waitFor(() => expect(document.activeElement).toBe(select(1)));
    fireEvent.click(form.getByRole("button", { name: "Remove role" }));
    await waitFor(() => expect(document.activeElement).toBe(form.getByRole("button", { name: "Add role" })));
  });

  // A role given by the Team goes with it: the server answers a regular
  // admin with a 403, not a 409.
  it("stops editing when a save is refused for want of a role, and reads /me again", async () => {
    const nobody: Me = { mode: "oidc", name: "dana", roles: {} };
    const { form, sent, reads } = await edit([() => json(full), noRole], [noRole], [admin, nobody]);
    fireEvent.change(form.getByLabelText("Display name"), { target: { value: "Team B" } });
    fireEvent.click(form.getByRole("button", { name: "Save" }));

    const banner = await screen.findByText("Your role on a is gone or has changed: its Team was deleted, or someone changed its roles.");
    await waitFor(() => expect(document.activeElement).toBe(banner.closest("[role=alert]")));
    expect(screen.queryByRole("form", { name: "Edit the team" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    expect(await screen.findByText("You have no role on a.")).toBeTruthy();
    await waitFor(() => expect(reads()).toBe(2)); // read again at once
    expect(screen.queryByText(/Could not load the team/)).toBeNull();
    expect(screen.queryByText("Team A")).toBeNull();
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(sent).toHaveLength(1);
  });

  it("stops editing when a read is refused for want of a role", async () => {
    const nobody: Me = { mode: "oidc", name: "dana", roles: {} };
    const { form, reads } = await edit([() => json(full), noRole], [], [admin, nobody]);
    fireEvent.change(form.getByLabelText("Display name"), { target: { value: "Team B" } });
    document.dispatchEvent(new Event("visibilitychange")); // the page reads the Team again
    await waitFor(() => expect(reads()).toBe(2));

    const banner = await screen.findByText(/Your role on a is gone or has changed/);
    await waitFor(() => expect(document.activeElement).toBe(banner.closest("[role=alert]")));
    expect(screen.queryByRole("form", { name: "Edit the team" })).toBeNull();
    expect(screen.queryByText(/Could not load the team/)).toBeNull();
    expect(await screen.findByText("You have no role on a.")).toBeTruthy();
  });

  it("moves the focus to the empty state when a read finds the Team deleted", async () => {
    const { reads } = await edit([() => json(full), noTeam]);
    document.dispatchEvent(new Event("visibilitychange"));
    await waitFor(() => expect(reads()).toBe(2));
    const empty = await screen.findByText(/This namespace has no Team/);
    await waitFor(() => expect(document.activeElement).toBe(empty));
    expect(screen.queryByRole("form", { name: "Edit the team" })).toBeNull();
  });

  it("hides what only admins see once a save took the caller's own admin role", async () => {
    let me = 0;
    const sent: Record<string, unknown>[] = [];
    vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
      if (url.endsWith("/me")) return json({ mode: "oidc", name: "dana", roles: { a: me++ === 0 ? "admin" : "viewer" } });
      if (url.endsWith("/namespaces/a/team") && init?.method === "PUT") {
        sent.push(JSON.parse(String(init.body)));
        return json(stripped); // project(): the Team as a viewer sees it
      }
      if (url.endsWith("/namespaces/a/team")) return json(full);
      return json({ items: [] });
    });
    render(
      <MemoryRouter initialEntries={["/t/a/settings"]}>
        <App />
      </MemoryRouter>,
    );
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    const form = within(screen.getByRole("form", { name: "Edit the team" }));
    fireEvent.change(within(form.getByRole("group", { name: "Role 1" })).getByLabelText("Role"), { target: { value: "viewer" } });
    fireEvent.click(form.getByRole("button", { name: "Save" }));

    const hint = await screen.findByText("You no longer have the admin role on a, so its limits, roles and run slots are hidden.");
    await waitFor(() => expect(document.activeElement).toBe(hint));
    expect(sent).toHaveLength(1);
    expect(screen.queryByText("No roles are given on this team.")).toBeNull();
    expect(screen.queryByRole("heading", { name: "Roles" })).toBeNull();
    expect(screen.queryByRole("heading", { name: "Run slots" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(await screen.findByText("You are a viewer of a.")).toBeTruthy(); // /me was read again
  });

  // A bootstrap admin keeps the admin role without being named in one.
  it("keeps showing an admin the settings after they removed every role", async () => {
    const none = team({ spec: { ...full.spec, roles: undefined }, status: full.status });
    const { form, sent } = await edit([() => json(full)], [() => json(none)]);
    fireEvent.click(form.getAllByRole("button", { name: "Remove role" })[1]);
    fireEvent.click(form.getByRole("button", { name: "Remove role" }));
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    await screen.findByText("Saved");
    expect(sent[0].roles ?? []).toEqual([]);
    expect(screen.getByText("No roles are given on this team.")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Run slots" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy();
  });

  it("warns about a Team managed by GitOps, and saves only once confirmed", async () => {
    const applied = team({ ...full, metadata: { ...full.metadata, annotations: { "kubectl.kubernetes.io/last-applied-configuration": "{}" } } });
    const { form, sent } = await edit([() => json(applied)], [() => json(applied)]);
    expect(form.getByText(/This Team looks managed by GitOps \(it was last written by kubectl apply\)/)).toBeTruthy();
    expect(screen.getByRole("form", { name: "Edit the team" }).getAttribute("aria-describedby")).toBe("team-gitops");
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
