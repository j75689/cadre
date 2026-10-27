import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import type { Me } from "./api";
import { access, json, team } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

const admin: Me = { mode: "oidc", name: "dana", roles: { a: "admin" } };

// open renders path with /me as given, or failing for null; the
// AgentAccess requests answer with accesses, the Team with team a, and the
// rest with no items. It returns the URLs fetched.
function open(path: string, me: Me | null, accesses: (url: string, init?: RequestInit) => Promise<Response> = () => json({ items: [] })) {
  const urls: string[] = [];
  vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
    urls.push(url);
    if (url.endsWith("/me")) return me ? json(me) : json({ error: { code: "unavailable", message: "down" } }, 503);
    if (url.includes("/agentaccesses")) return accesses(url, init);
    if (url.endsWith("/namespaces/a/team")) return json(team());
    return json({ items: [] });
  });
  render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
  return urls;
}

const notServed = /Agent access needs logins/;

// The mux's answer for a route it does not have: plain text, no code.
const plainNotFound = () => Promise.resolve(new Response("404 page not found\n", { status: 404, statusText: "Not Found" }));

describe("AccessList", () => {
  it("shows an admin each access's role, credential and readiness", async () => {
    open("/t/a/settings/access", admin, () =>
      json({
        items: [
          access({ name: "sa-bot", spec: { role: "viewer", serviceAccount: { name: "ci" } } }),
          access({ name: "minted", tokenSecret: "minted-token", mintedAt: "2026-10-25T09:00:00Z", ready: false, reason: "SecretMissing", message: "secret minted-token not found" }),
          access({ name: "new one", ready: false, reason: "Pending", message: "its current spec has not been checked yet" }),
        ],
      }),
    );
    const region = within(await screen.findByRole("region", { name: "Agent accesses" }));
    const [, sa, minted, fresh] = region.getAllByRole("row").map((r) => within(r));
    expect(sa.getByRole("link", { name: "sa-bot" }).getAttribute("href")).toBe("/t/a/settings/access/sa-bot");
    expect(sa.getByText("viewer")).toBeTruthy();
    expect(sa.getByText("ci")).toBeTruthy();
    expect(sa.getByText("Ready")).toBeTruthy();

    expect(minted.getByText("member")).toBeTruthy();
    expect(minted.getByText("minted-token")).toBeTruthy();
    expect(minted.getByText(/minted/, { selector: "span" })).toBeTruthy();
    expect(minted.getByText("Not ready")).toBeTruthy();
    expect(minted.getByText("SecretMissing")).toBeTruthy();
    expect(minted.getByText("secret minted-token not found")).toBeTruthy();

    // The name is encoded in the link; the Reason cell leaves Pending to the badge.
    expect(fresh.getByRole("link", { name: "new one" }).getAttribute("href")).toBe("/t/a/settings/access/new%20one");
    expect(fresh.getByText("Token not minted yet")).toBeTruthy();
    expect(fresh.getAllByText("Pending")).toHaveLength(1);
  });

  it("says when there is none", async () => {
    open("/t/a/settings/access", admin);
    expect(await screen.findByText("No AgentAccess yet.")).toBeTruthy();
  });

  it("explains, and requests nothing, without logins", async () => {
    const urls = open("/t/a/settings/access", { mode: "none", admin: true });
    expect(await screen.findByText(notServed)).toBeTruthy();
    expect(urls.some((u) => u.includes("/agentaccesses"))).toBe(false);
  });

  it("explains a plain 404 the same way when /me failed", async () => {
    open("/t/a/settings/access", null, plainNotFound);
    expect(await screen.findByText(notServed)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it("shows a plain 404 as an error with logins, which serve the routes", async () => {
    open("/t/a/settings/access", admin, plainNotFound);
    expect(await screen.findByText("Could not load the agent accesses: Not Found")).toBeTruthy();
    expect(screen.queryByText(notServed)).toBeNull();
  });

  it("shows a 404 with a code as an error, whatever the mode", async () => {
    const notFound = () => json({ error: { code: "not_found", message: "no such namespace" } }, 404);
    open("/t/a/settings/access", null, notFound);
    expect(await screen.findByText("Could not load the agent accesses: no such namespace")).toBeTruthy();
    expect(screen.queryByText(notServed)).toBeNull();
  });

  it.each([["member"], ["viewer"]])("tells a %s it needs the admin role, and requests nothing", async (role) => {
    const urls = open("/t/a/settings/access", { mode: "oidc", name: "lee", roles: { a: role } });
    expect(await screen.findByText("Managing agent access needs the admin role.")).toBeTruthy();
    expect(urls.some((u) => u.includes("/agentaccesses"))).toBe(false);
  });

  it("shows a banner with Retry when the accesses cannot be read, without it for a 403", async () => {
    open("/t/a/settings/access", admin, () => json({ error: { code: "unavailable", message: "not synced" } }, 503));
    expect(await screen.findByText("Could not load the agent accesses: not synced")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    cleanup();

    open("/t/a/settings/access", admin, () => json({ error: { code: "forbidden", message: "not allowed" } }, 403));
    expect(await screen.findByText("Could not load the agent accesses: not allowed")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it("keeps Settings current in the team nav", async () => {
    open("/t/a/settings/access", admin);
    await screen.findByText("No AgentAccess yet.");
    const team = within(screen.getByRole("navigation", { name: "Team" }));
    expect(team.getByRole("link", { name: "Settings" }).getAttribute("aria-current")).toBe("page");
    const tabs = within(screen.getByRole("navigation", { name: "Settings" }));
    expect(tabs.getByRole("link", { name: "Agent access" }).getAttribute("aria-current")).toBe("page");
    expect(tabs.getByRole("link", { name: "Team" }).getAttribute("aria-current")).toBeNull();
  });
});

describe("Settings tabs", () => {
  it("keep Team current with a trailing slash", async () => {
    open("/t/a/settings/", admin);
    const tabs = within(screen.getByRole("navigation", { name: "Settings" }));
    await tabs.findByRole("link", { name: "Agent access" });
    expect(tabs.getByRole("link", { name: "Team" }).getAttribute("aria-current")).toBe("page");
  });

  it("show Agent access to an oidc admin", async () => {
    open("/t/a/settings", admin);
    const tabs = within(screen.getByRole("navigation", { name: "Settings" }));
    const tab = await tabs.findByRole("link", { name: "Agent access" });
    expect(tab.getAttribute("href")).toBe("/t/a/settings/access");
    expect(tabs.getByRole("link", { name: "Team" }).getAttribute("aria-current")).toBe("page");
  });

  it.each<[string, Me]>([
    ["a viewer", { mode: "oidc", name: "lee", roles: { a: "viewer" } }],
    ["a member", { mode: "oidc", name: "lee", roles: { a: "member" } }],
    ["--auth=none", { mode: "none", admin: true }],
  ])("hide Agent access for %s", async (_, me) => {
    open("/t/a/settings", me);
    // The Team card shows once /me and the Team are read.
    await screen.findByRole("region", { name: "Team" });
    const tabs = within(screen.getByRole("navigation", { name: "Settings" }));
    expect(tabs.getByRole("link", { name: "Team" })).toBeTruthy();
    expect(tabs.queryByRole("link", { name: "Agent access" })).toBeNull();
  });
});

describe("AccessDetail", () => {
  it("shows the access, its reason and message, and where the token is", async () => {
    open("/t/a/settings/access/minted", admin, () =>
      json(access({ name: "minted", tokenSecret: "minted-token", mintedAt: "2026-10-25T09:00:00Z", ready: false, reason: "SecretMissing", message: "secret minted-token not found" })),
    );
    const card = within(await screen.findByRole("region", { name: "AgentAccess" }));
    expect(card.getByText("member")).toBeTruthy();
    expect(card.getByText("Not ready")).toBeTruthy();
    expect(card.getByText("SecretMissing")).toBeTruthy();
    expect(card.getByText("secret minted-token not found")).toBeTruthy();
    expect(card.getByText(/never shown again/)).toBeTruthy();
    expect(card.getByText("kubectl -n a get secret minted-token -o jsonpath='{.data.token}' | base64 -d")).toBeTruthy();
    expect(card.getByText("Minted")).toBeTruthy();
    const crumbs = within(screen.getByRole("navigation", { name: "Breadcrumb" }));
    expect(crumbs.getByRole("link", { name: "Agent access" }).getAttribute("href")).toBe("/t/a/settings/access");
  });

  it("does not need the optional fields", async () => {
    open("/t/a/settings/access/bot", admin, () => json(access({ ready: false })));
    const card = within(await screen.findByRole("region", { name: "AgentAccess" }));
    expect(card.getByText("Token not minted yet")).toBeTruthy();
    expect(card.getByText("Not ready")).toBeTruthy();
    expect(card.queryByText("Minted")).toBeNull();
    expect(card.queryByText(/never shown again/)).toBeNull();
  });

  it("leaves Pending to the badge", async () => {
    open("/t/a/settings/access/bot", admin, () => json(access({ ready: false, reason: "Pending" })));
    const card = within(await screen.findByRole("region", { name: "AgentAccess" }));
    expect(card.getAllByText("Pending")).toHaveLength(1);
    expect(card.queryByText("Reason")).toBeNull();
  });

  it("does not point a ServiceAccount's access at a leftover Secret", async () => {
    open("/t/a/settings/access/bot", admin, () => json(access({ spec: { role: "member", serviceAccount: { name: "ci" } }, tokenSecret: "bot.access-token" })));
    const card = within(await screen.findByRole("region", { name: "AgentAccess" }));
    expect(card.getByText("ci")).toBeTruthy();
    expect(card.queryByText(/never shown again/)).toBeNull();
  });

  it("requests no name that is not a DNS label", async () => {
    const urls = open("/t/a/settings/access/Foo_Bar", admin);
    expect(await screen.findByText(/No AgentAccess/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "All agent accesses" })).toBeTruthy();
    expect(urls.some((u) => u.includes("/agentaccesses"))).toBe(false);
    cleanup();

    const long = open("/t/a/settings/access/" + "a".repeat(64), admin);
    expect(await screen.findByText(/No AgentAccess/)).toBeTruthy();
    expect(long.some((u) => u.includes("/agentaccesses"))).toBe(false);
  });

  it("requests a name of 63 characters", async () => {
    const name = "a".repeat(63);
    const urls = open(`/t/a/settings/access/${name}`, admin, () => json(access({ name })));
    await waitFor(() => expect(urls.some((u) => u.endsWith(`/agentaccesses/${name}`))).toBe(true));
  });

  it("links back to the list when there is no such access", async () => {
    open("/t/a/settings/access/gone", admin, () => json({ error: { code: "not_found", message: "not found" } }, 404));
    expect(await screen.findByText(/No AgentAccess/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "All agent accesses" }).getAttribute("href")).toBe("/t/a/settings/access");
  });

  it("explains a plain 404 when /me failed, and requests nothing without logins", async () => {
    open("/t/a/settings/access/bot", null, plainNotFound);
    expect(await screen.findByText(notServed)).toBeTruthy();
    cleanup();

    open("/t/a/settings/access/bot", admin, plainNotFound);
    expect(await screen.findByText("Could not load the agent access: Not Found")).toBeTruthy();
    expect(screen.queryByText(notServed)).toBeNull();
    cleanup();

    const urls = open("/t/a/settings/access/bot", { mode: "none", admin: true });
    expect(await screen.findByText(notServed)).toBeTruthy();
    await waitFor(() => expect(urls.some((u) => u.endsWith("/me"))).toBe(true));
    expect(urls.some((u) => u.includes("/agentaccesses"))).toBe(false);
  });
});

// A Call is a request the AgentAccess routes were sent other than a read.
interface Call {
  method: string;
  url: string;
  body?: unknown;
}

// server answers the AgentAccess routes: reads with the access current
// holds, and each other request with answer. calls are those others.
function server(current: { access: ReturnType<typeof access> | null }, answer: (c: Call) => Promise<Response>) {
  const calls: Call[] = [];
  const handle = (url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET";
    if (method === "GET") {
      if (url.endsWith("/agentaccesses")) return json({ items: current.access ? [current.access] : [] });
      return current.access ? json(current.access) : json({ error: { code: "not_found", message: "not found" } }, 404);
    }
    const c = { method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined };
    calls.push(c);
    return answer(c);
  };
  return { calls, handle };
}

const secret = "cdr_s3cr3t-t0ken-value";

describe("Creating an agent access", () => {
  it("sends a minted token's spec without a version, then opens the access", async () => {
    const current: { access: ReturnType<typeof access> | null } = { access: null };
    const s = server(current, (c) => {
      current.access = access({ name: "bot", spec: (c.body as { spec: never }).spec });
      return json(current.access, 201);
    });
    open("/t/a/settings/access", admin, s.handle);
    fireEvent.click(await screen.findByRole("button", { name: "New agent access" }));
    const form = within(screen.getByRole("form", { name: "New agent access" }));
    fireEvent.change(form.getByRole("textbox", { name: /^Name/ }), { target: { value: "bot" } });
    fireEvent.click(form.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(s.calls).toHaveLength(1));
    expect(s.calls[0]).toEqual({ method: "PUT", url: "/api/v1/namespaces/a/agentaccesses/bot", body: { spec: { role: "member" } } });
    expect(await screen.findByRole("region", { name: "AgentAccess" })).toBeTruthy();
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe("bot");
  });

  it("sends a ServiceAccount and the role picked", async () => {
    const s = server({ access: null }, () => json(access({ name: "ci-bot" }), 201));
    open("/t/a/settings/access", admin, s.handle);
    fireEvent.click(await screen.findByRole("button", { name: "New agent access" }));
    const form = within(screen.getByRole("form", { name: "New agent access" }));
    fireEvent.change(form.getByRole("textbox", { name: /^Name/ }), { target: { value: "ci-bot" } });
    fireEvent.click(form.getByRole("radio", { name: "viewer" }));
    fireEvent.click(form.getByRole("radio", { name: "ServiceAccount" }));
    fireEvent.change(form.getByRole("textbox", { name: "ServiceAccount name" }), { target: { value: " ci " } });
    fireEvent.click(form.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(s.calls).toHaveLength(1));
    expect(s.calls[0].body).toEqual({ spec: { role: "viewer", serviceAccount: { name: "ci" } } });
  });

  it("refuses a name that is not a DNS label, and sends nothing", async () => {
    const s = server({ access: null }, () => json(access(), 201));
    open("/t/a/settings/access", admin, s.handle);
    fireEvent.click(await screen.findByRole("button", { name: "New agent access" }));
    const form = within(screen.getByRole("form", { name: "New agent access" }));
    fireEvent.change(form.getByRole("textbox", { name: /^Name/ }), { target: { value: "Bot_1" } });
    fireEvent.click(form.getByRole("button", { name: "Create" }));
    expect((await form.findByRole("alert")).textContent).toMatch(/at most 63 lower-case letters/);
    expect(form.getByRole("textbox", { name: /^Name/ }).getAttribute("aria-invalid")).toBe("true");
    expect(s.calls).toHaveLength(0);
  });

  it.each([
    [409, "already_exists", "agent access bot: already exists"],
    [400, "invalid", "spec.role: Unsupported value: \"admin\""],
  ])("shows a %d's message as it is", async (status, code, message) => {
    const s = server({ access: null }, () => json({ error: { code, message } }, status));
    open("/t/a/settings/access", admin, s.handle);
    fireEvent.click(await screen.findByRole("button", { name: "New agent access" }));
    const form = within(screen.getByRole("form", { name: "New agent access" }));
    fireEvent.change(form.getByRole("textbox", { name: /^Name/ }), { target: { value: "bot" } });
    fireEvent.click(form.getByRole("button", { name: "Create" }));
    expect((await form.findByRole("alert")).textContent).toBe(message);
    expect((form.getByRole("textbox", { name: /^Name/ }) as HTMLInputElement).value).toBe("bot");
  });
});

describe("Editing an agent access", () => {
  it("sends the version read, and shows what was saved", async () => {
    const current = { access: access({ resourceVersion: "3" }) };
    const s = server(current, (c) => json(access({ resourceVersion: "4", spec: (c.body as { spec: never }).spec })));
    open("/t/a/settings/access/bot", admin, s.handle);
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    const form = within(screen.getByRole("form", { name: "Edit bot" }));
    expect(form.getByText(/an edit here drifts from Git/)).toBeTruthy();
    expect(form.getByRole("textbox", { name: /^Name/ }).hasAttribute("readonly")).toBe(true);
    fireEvent.click(form.getByRole("radio", { name: "viewer" }));
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(s.calls).toHaveLength(1));
    expect(s.calls[0]).toEqual({ method: "PUT", url: "/api/v1/namespaces/a/agentaccesses/bot", body: { spec: { role: "viewer" }, resourceVersion: "3" } });
    const card = within(screen.getByRole("region", { name: "AgentAccess" }));
    expect(await card.findByText("Saved")).toBeTruthy();
    expect(card.getByText("viewer")).toBeTruthy();
  });

  it("on a 409 shows the server's words, keeps the draft, and saves with the version read again", async () => {
    const current = { access: access({ resourceVersion: "3" }) };
    const conflict = "AgentAccess bot changed since it was read: read it again, and edit that";
    const s = server(current, (c) =>
      (c.body as { resourceVersion: string }).resourceVersion === "3"
        ? json({ error: { code: "conflict", message: conflict } }, 409)
        : json(access({ resourceVersion: "6", spec: { role: "viewer" } })),
    );
    open("/t/a/settings/access/bot", admin, s.handle);
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    const form = within(screen.getByRole("form", { name: "Edit bot" }));
    fireEvent.click(form.getByRole("radio", { name: "viewer" }));
    current.access = access({ resourceVersion: "5", spec: { role: "member", serviceAccount: { name: "ops" } } });
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    expect((await form.findByRole("alert")).textContent).toBe(conflict);
    expect((form.getByRole("radio", { name: "viewer" }) as HTMLInputElement).checked).toBe(true);

    fireEvent.click(form.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(s.calls).toHaveLength(2));
    expect(s.calls[1].body).toEqual({ spec: { role: "viewer" }, resourceVersion: "5" });
  });

  it("on a 409 from another admin's change to the role, shows it and does not save over it", async () => {
    const current = { access: access({ resourceVersion: "3" }) };
    const conflict = "AgentAccess bot changed since it was read: read it again, and edit that";
    const s = server(current, (c) =>
      (c.body as { resourceVersion: string }).resourceVersion === "3"
        ? json({ error: { code: "conflict", message: conflict } }, 409)
        : json(access({ resourceVersion: "5", spec: { role: "member", serviceAccount: { name: "ops" } } })),
    );
    open("/t/a/settings/access/bot", admin, s.handle);
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    const form = within(screen.getByRole("form", { name: "Edit bot" }));
    fireEvent.click(form.getByRole("radio", { name: "ServiceAccount" }));
    fireEvent.change(form.getByRole("textbox", { name: "ServiceAccount name" }), { target: { value: "ops" } });
    current.access = access({ resourceVersion: "4", spec: { role: "viewer" } });
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    expect((await form.findByRole("alert")).textContent).toBe(conflict);
    expect(s.calls).toHaveLength(1);
    expect((form.getByRole("radio", { name: "member" }) as HTMLInputElement).checked).toBe(true);
    expect((form.getByRole("textbox", { name: "ServiceAccount name" }) as HTMLInputElement).value).toBe("ops");

    fireEvent.click(form.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(s.calls).toHaveLength(2));
    expect(s.calls[1].body).toEqual({ spec: { role: "member", serviceAccount: { name: "ops" } }, resourceVersion: "4" });
  });

  // reconciled is an API server whose reconciler writes the status after
  // each save and rotation, which moves the version on with the spec as it
  // is.
  function reconciled() {
    const current = { access: access({ resourceVersion: "3", tokenSecret: "bot.access-token" }) };
    const settle = () => (current.access = { ...current.access, resourceVersion: String(Number(current.access.resourceVersion) + 1) });
    const s = server(current, (c) => {
      if (c.method === "POST") {
        settle();
        return json({ token: secret, tokenSecret: "bot.access-token" });
      }
      const { spec, resourceVersion } = c.body as { spec: never; resourceVersion: string };
      if (resourceVersion !== current.access.resourceVersion) return json({ error: { code: "conflict", message: "AgentAccess bot changed since it was read" } }, 409);
      settle();
      current.access = { ...current.access, spec };
      const saved = current.access;
      settle();
      return json(saved);
    });
    return s;
  }

  async function edit(role: "viewer" | "member") {
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    const form = within(screen.getByRole("form", { name: "Edit bot" }));
    fireEvent.click(form.getByRole("radio", { name: role }));
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    const card = within(screen.getByRole("region", { name: "AgentAccess" }));
    expect(await card.findByText("Saved")).toBeTruthy();
    expect(card.queryByRole("alert")).toBeNull();
  }

  it("saves again after a save, though the reconciler wrote the status since", async () => {
    const s = reconciled();
    open("/t/a/settings/access/bot", admin, s.handle);
    await edit("viewer");
    await edit("member");
    expect(s.calls.map((c) => (c.body as { spec: unknown }).spec)).toEqual([{ role: "viewer" }, { role: "member" }, { role: "member" }]);
  });

  it("saves after a rotation, though the reconciler wrote the status since", async () => {
    const s = reconciled();
    open("/t/a/settings/access/bot", admin, s.handle);
    fireEvent.click(await screen.findByRole("button", { name: "Rotate token" }));
    fireEvent.click(screen.getByRole("button", { name: "Yes, rotate it" }));
    fireEvent.click(await screen.findByRole("button", { name: "Done" }));
    await edit("viewer");
    expect(s.calls.at(-1)?.body).toEqual({ spec: { role: "viewer" }, resourceVersion: "4" });
  });

  it("after a 409 that was shown, saves though the reconciler wrote the status since", async () => {
    const current = { access: access({ resourceVersion: "3" }) };
    const s = server(current, (c) =>
      (c.body as { resourceVersion: string }).resourceVersion === current.access.resourceVersion
        ? json(access({ resourceVersion: "9", spec: { role: "viewer" } }))
        : json({ error: { code: "conflict", message: "AgentAccess bot changed since it was read" } }, 409),
    );
    open("/t/a/settings/access/bot", admin, s.handle);
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    const form = within(screen.getByRole("form", { name: "Edit bot" }));
    fireEvent.click(form.getByRole("radio", { name: "viewer" }));
    current.access = access({ resourceVersion: "5", spec: { role: "member", serviceAccount: { name: "ops" } } });
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    expect(await form.findByRole("alert")).toBeTruthy();

    current.access = { ...current.access, resourceVersion: "6" };
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    const card = within(screen.getByRole("region", { name: "AgentAccess" }));
    expect(await card.findByText("Saved")).toBeTruthy();
    expect(card.queryByRole("alert")).toBeNull();
    expect(s.calls.map((c) => (c.body as { resourceVersion: string }).resourceVersion)).toEqual(["3", "5", "6"]);
  });

  it("saves again only once when every save is a 409 with the spec as it was", async () => {
    const current = { access: access({ resourceVersion: "3" }) };
    const conflict = "AgentAccess bot changed since it was read";
    const s = server(current, () => {
      current.access = { ...current.access, resourceVersion: String(Number(current.access.resourceVersion) + 1) };
      return json({ error: { code: "conflict", message: conflict } }, 409);
    });
    open("/t/a/settings/access/bot", admin, s.handle);
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    const form = within(screen.getByRole("form", { name: "Edit bot" }));
    fireEvent.click(form.getByRole("radio", { name: "viewer" }));
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    expect((await form.findByRole("alert")).textContent).toBe(conflict);
    expect(s.calls.map((c) => (c.body as { resourceVersion: string }).resourceVersion)).toEqual(["3", "4"]);
  });

  it("puts the focus on the role, and back on Edit after Cancel", async () => {
    open("/t/a/settings/access/bot", admin, server({ access: access() }, () => json(access())).handle);
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    const form = within(screen.getByRole("form", { name: "Edit bot" }));
    await waitFor(() => expect(document.activeElement).toBe(form.getByRole("radio", { name: "member" })));
    fireEvent.click(form.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Edit" })));
  });

  it("puts the focus back on Edit after a save", async () => {
    open("/t/a/settings/access/bot", admin, server({ access: access() }, () => json(access({ resourceVersion: "4" }))).handle);
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    fireEvent.click(within(screen.getByRole("form", { name: "Edit bot" })).getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Saved")).toBeTruthy();
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Edit" })));
  });

  it.each([
    [400, "invalid", "spec.role must be viewer or member"],
    [500, "internal", "the AgentAccess could not be written"],
  ])("on a %i shows the server's words, and saves again with the version opened", async (status, code, message) => {
    const current = { access: access({ resourceVersion: "3" }) };
    let saves = 0;
    const s = server(current, () => (++saves === 1 ? json({ error: { code, message } }, status) : json(access({ resourceVersion: "4" }))));
    open("/t/a/settings/access/bot", admin, s.handle);
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    const form = within(screen.getByRole("form", { name: "Edit bot" }));
    // Only a 409 reads the version again; this one would be another admin's.
    current.access = access({ resourceVersion: "5" });
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    expect((await form.findByRole("alert")).textContent).toBe(message);

    fireEvent.click(form.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(s.calls).toHaveLength(2));
    expect(s.calls[1].body).toEqual({ spec: { role: "member" }, resourceVersion: "3" });
  });

  it("does not let the 15 s read overwrite the draft", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const current = { access: access({ resourceVersion: "3" }) };
    const s = server(current, () => json(access()));
    open("/t/a/settings/access/bot", admin, s.handle);
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    const form = within(screen.getByRole("form", { name: "Edit bot" }));
    fireEvent.click(form.getByRole("radio", { name: "ServiceAccount" }));
    fireEvent.change(form.getByRole("textbox", { name: "ServiceAccount name" }), { target: { value: "half-typed" } });

    current.access = access({ resourceVersion: "9", spec: { role: "viewer" } });
    await act(() => vi.advanceTimersByTimeAsync(15000));
    await act(() => vi.advanceTimersByTimeAsync(15000));
    expect((form.getByRole("textbox", { name: "ServiceAccount name" }) as HTMLInputElement).value).toBe("half-typed");
    expect((form.getByRole("radio", { name: "member" }) as HTMLInputElement).checked).toBe(true);
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(s.calls).toHaveLength(1));
    expect(s.calls[0].body).toEqual({ spec: { role: "member", serviceAccount: { name: "half-typed" } }, resourceVersion: "3" });
  });
});

describe("Rotating a token", () => {
  // rotate opens bot, confirms a rotation answered with answer, and
  // returns the requests made.
  async function rotate(answer: () => Promise<Response>) {
    const s = server({ access: access({ tokenSecret: "bot.access-token" }) }, answer);
    open("/t/a/settings/access/bot", admin, s.handle);
    fireEvent.click(await screen.findByRole("button", { name: "Rotate token" }));
    const confirm = within(screen.getByRole("group", { name: "Rotate the token?" }));
    expect(confirm.getByText("Rotate the token? The old one stops working at once.")).toBeTruthy();
    fireEvent.click(confirm.getByRole("button", { name: "Yes, rotate it" }));
    return s.calls;
  }
  const minted = () => json({ token: secret, tokenSecret: "bot.access-token" });

  it("shows the token once, with Copy, and never in a live region", async () => {
    const writeText = vi.fn(() => Promise.resolve());
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    const calls = await rotate(minted);
    const field = (await screen.findByRole("textbox", { name: "New token" })) as HTMLInputElement;
    expect(calls).toEqual([{ method: "POST", url: "/api/v1/namespaces/a/agentaccesses/bot/token", body: {} }]);
    expect(field.value).toBe(secret);
    expect(field.readOnly).toBe(true);
    expect(field.getAttribute("autocomplete")).toBe("off");
    expect(field.getAttribute("spellcheck")).toBe("false");
    expect(screen.getByText(/This is the only time this token is shown. It is also in Secret/)).toBeTruthy();
    for (const live of document.querySelectorAll("[role=status], [role=alert], [aria-live]")) expect(live.textContent).not.toContain(secret);
    expect(screen.getByText("New token shown below")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Copy" }));
    expect(await screen.findByText("Copied")).toBeTruthy();
    expect(writeText).toHaveBeenCalledWith(secret);
    for (const live of document.querySelectorAll("[role=status], [role=alert], [aria-live]")) expect(live.textContent).not.toContain(secret);
    expect(document.title).not.toContain(secret);

    fireEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByRole("textbox", { name: "New token" })).toBeNull();
    expect(document.body.innerHTML).not.toContain(secret);
  });

  it("selects the token to copy by hand without a clipboard", async () => {
    vi.stubGlobal("navigator", { ...navigator, clipboard: undefined });
    await rotate(minted);
    const field = (await screen.findByRole("textbox", { name: "New token" })) as HTMLInputElement;
    fireEvent.click(screen.getByRole("button", { name: "Copy" }));
    expect(await screen.findByText("Press Ctrl+C to copy it.")).toBeTruthy();
    expect(document.activeElement).toBe(field);
    expect(field.value.slice(field.selectionStart ?? 0, field.selectionEnd ?? 0)).toBe(secret);
  });

  it("keeps the token nowhere: not after leaving, nor in storage, the URL or the console", async () => {
    const logged: unknown[] = [];
    const spies = (["log", "info", "warn", "error", "debug"] as const).map((m) => vi.spyOn(console, m).mockImplementation((...a) => void logged.push(...a)));
    await rotate(minted);
    expect(((await screen.findByRole("textbox", { name: "New token" })) as HTMLInputElement).value).toBe(secret);
    const stored = (st: Storage) => Array.from({ length: st.length }, (_, i) => `${st.key(i)}=${st.getItem(st.key(i)!)}`).join("\n");
    expect(stored(localStorage)).not.toContain(secret);
    expect(stored(sessionStorage)).not.toContain(secret);
    expect(location.search + location.hash + location.href).not.toContain(secret);
    expect(JSON.stringify(history.state) ?? "").not.toContain(secret);

    cleanup();
    open("/t/a/settings/access/bot", admin, server({ access: access({ tokenSecret: "bot.access-token" }) }, minted).handle);
    expect(await screen.findByRole("button", { name: "Rotate token" })).toBeTruthy();
    expect(screen.queryByRole("textbox", { name: "New token" })).toBeNull();
    expect(document.body.innerHTML).not.toContain(secret);
    expect(logged.map((x) => String(x instanceof Error ? x.stack : typeof x === "object" ? JSON.stringify(x) : x)).join("\n")).not.toContain(secret);
    spies.forEach((s) => s.mockRestore());
  });

  it("clears the token as the page is left for the back/forward cache", async () => {
    await rotate(minted);
    expect(((await screen.findByRole("textbox", { name: "New token" })) as HTMLInputElement).value).toBe(secret);
    // At once: the page is frozen as it is right after pagehide, and Back
    // shows it again.
    window.dispatchEvent(new Event("pagehide"));
    expect(screen.queryByRole("textbox", { name: "New token" })).toBeNull();
    expect(screen.queryByText("New token shown below")).toBeNull();
    expect(document.body.innerHTML).not.toContain(secret);
    expect(screen.getByRole("button", { name: "Rotate token" })).toBeTruthy();
  });

  it("shows no token from a rotation answered after the page was left", async () => {
    let answer: (r: Response) => void = () => {};
    await rotate(() => new Promise<Response>((resolve) => (answer = resolve)));
    // Left while the rotation is in flight: Back from the back/forward
    // cache then lets the answer in.
    window.dispatchEvent(new Event("pagehide"));
    answer(new Response(JSON.stringify({ token: secret, tokenSecret: "bot.access-token" })));
    expect(
      await screen.findByText("Rotated while the page was left, so the token is not shown: it is in Secret bot.access-token; rotate again to see one here"),
    ).toBeTruthy();
    expect(screen.queryByRole("textbox", { name: "New token" })).toBeNull();
    expect(document.body.innerHTML).not.toContain(secret);
    expect(screen.getByRole("button", { name: "Rotate token" })).toBeTruthy();
  });

  it("drops the token and the form when the access is gone, and a new one of its name brings neither back", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const current = { access: access({ tokenSecret: "bot.access-token" }) as ReturnType<typeof access> | null };
    open("/t/a/settings/access/bot", admin, server(current, minted).handle);
    fireEvent.click(await screen.findByRole("button", { name: "Rotate token" }));
    fireEvent.click(screen.getByRole("button", { name: "Yes, rotate it" }));
    expect(await screen.findByRole("textbox", { name: "New token" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Rotate token" }));
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    expect(screen.getByRole("form", { name: "Edit bot" })).toBeTruthy();
    expect(screen.getByRole("group", { name: "Rotate the token?" })).toBeTruthy();

    current.access = null;
    await act(() => vi.advanceTimersByTimeAsync(15000));
    expect(screen.getByText(/No AgentAccess/)).toBeTruthy();
    current.access = access({ tokenSecret: "bot.access-token" });
    await act(() => vi.advanceTimersByTimeAsync(15000));
    expect(await screen.findByRole("button", { name: "Edit" })).toBeTruthy();
    expect(screen.queryByRole("form", { name: "Edit bot" })).toBeNull();
    expect(screen.queryByRole("textbox", { name: "New token" })).toBeNull();
    expect(screen.queryByRole("group", { name: "Rotate the token?" })).toBeNull();
    expect(document.body.innerHTML).not.toContain(secret);
  });

  it("shows a 409's message as it is, and no token", async () => {
    const message = "AgentAccess bot's token Secret changed while rotating; its token is in Secret bot.access-token";
    await rotate(() => json({ error: { code: "conflict", message } }, 409));
    expect((await screen.findByRole("alert")).textContent).toBe(message);
    expect(screen.queryByRole("textbox", { name: "New token" })).toBeNull();
    expect(screen.queryByText("New token shown below")).toBeNull();
  });

  it("shows a 400's message as it is", async () => {
    const message = "namespace a has no Team, and no token is minted outside one";
    await rotate(() => json({ error: { code: "invalid", message } }, 400));
    expect((await screen.findByRole("alert")).textContent).toBe(message);
  });

  it("is not offered for a ServiceAccount", async () => {
    open("/t/a/settings/access/bot", admin, () => json(access({ spec: { role: "member", serviceAccount: { name: "ci" } } })));
    expect(await screen.findByRole("button", { name: "Edit" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Rotate token" })).toBeNull();
    expect(screen.queryByText(/rotate it/)).toBeNull();
  });

  it("says where a token not yet minted will be", async () => {
    open("/t/a/settings/access/bot", admin, () => json(access()));
    expect(await screen.findByText("The reconciler mints the token into a Secret; rotate it to see one here.")).toBeTruthy();
  });
});

describe("Deleting an agent access", () => {
  it("asks, deletes, and goes back to the list without it", async () => {
    const current = { access: access() as ReturnType<typeof access> | null };
    const s = server(current, () => {
      current.access = null;
      return Promise.resolve(new Response(null, { status: 204 }));
    });
    open("/t/a/settings/access/bot", admin, s.handle);
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    const confirm = within(screen.getByRole("group", { name: "Delete bot?" }));
    expect(confirm.getByText("Delete bot? Its agent loses access at once.")).toBeTruthy();
    // The status is there, empty, before it says so: a live region that
    // comes with its text is not announced.
    const said: Node[] = [];
    const seen = new MutationObserver((records) => {
      for (const r of records) if (r.target instanceof Element && r.target.getAttribute("role") === "status") said.push(...r.addedNodes);
    });
    seen.observe(document.body, { childList: true, subtree: true });
    fireEvent.click(confirm.getByRole("button", { name: "Yes, delete it" }));
    expect(await screen.findByText("Deleted bot.")).toBeTruthy();
    await waitFor(() => expect(said.map((n) => n.textContent)).toContain("Deleted bot."));
    seen.disconnect();
    expect(s.calls).toEqual([{ method: "DELETE", url: "/api/v1/namespaces/a/agentaccesses/bot", body: undefined }]);
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe("Agent access");
    expect(await screen.findByText("No AgentAccess yet.")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "bot" })).toBeNull();
  });

  it("takes a 404 as already gone", async () => {
    open("/t/a/settings/access/bot", admin, server({ access: access() }, () => json({ error: { code: "not_found", message: "not found" } }, 404)).handle);
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    fireEvent.click(screen.getByRole("button", { name: "Yes, delete it" }));
    expect(await screen.findByText("Deleted bot.")).toBeTruthy();
  });

  it("keeps the access, and shows why, when refused", async () => {
    const message = "cadre-server may not do this: its ClusterRole lacks the verb; see its log";
    open("/t/a/settings/access/bot", admin, server({ access: access() }, () => json({ error: { code: "server_forbidden", message } }, 500)).handle);
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    fireEvent.click(screen.getByRole("button", { name: "Yes, delete it" }));
    expect((await screen.findByRole("alert")).textContent).toBe(message);
    expect(screen.getByRole("region", { name: "AgentAccess" })).toBeTruthy();
  });
});
