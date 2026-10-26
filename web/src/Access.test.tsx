import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import type { Me } from "./api";
import { access, json, team } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const admin: Me = { mode: "oidc", name: "dana", roles: { a: "admin" } };

// open renders path with /me as given, or failing for null; the
// AgentAccess reads answer with accesses, the Team with team a, and the
// rest with no items. It returns the URLs fetched.
function open(path: string, me: Me | null, accesses: () => Promise<Response> = () => json({ items: [] })) {
  const urls: string[] = [];
  vi.stubGlobal("fetch", (url: string) => {
    urls.push(url);
    if (url.endsWith("/me")) return me ? json(me) : json({ error: { code: "unavailable", message: "down" } }, 503);
    if (url.includes("/agentaccesses")) return accesses();
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
