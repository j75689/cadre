import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { validNamespace } from "./Teams";
import { json, team } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function Where() {
  return <output data-testid="where">{useLocation().pathname}</output>;
}

// at renders the Teams page for a caller who is a bootstrap admin or not,
// with puts answering put; it returns the PUTs sent.
function at(admin: boolean, put: () => Promise<Response> = () => json(team({ metadata: { name: "team", namespace: "x" } }), 201)) {
  const puts: { url: string; body: unknown }[] = [];
  vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
    if (url.endsWith("/me")) return json({ mode: "oidc", username: "dana", admin });
    if (init?.method === "PUT") {
      puts.push({ url, body: JSON.parse(String(init.body)) });
      return put();
    }
    if (url.endsWith("/teams")) return json({ items: puts.length > 0 ? [team({ metadata: { name: "team", namespace: "x" } })] : [] });
    return json({ items: [] });
  });
  render(
    <MemoryRouter initialEntries={["/"]}>
      <App />
      <Where />
    </MemoryRouter>,
  );
  return puts;
}

describe("validNamespace", () => {
  it("mirrors the server's DNS-1123 label check", () => {
    expect(validNamespace("")).toMatch(/required/);
    expect(validNamespace("a".repeat(64))).toMatch(/63/);
    expect(validNamespace("Team")).toMatch(/lower-case/);
    expect(validNamespace("-a")).toMatch(/lower-case/);
    expect(validNamespace("a-")).toMatch(/lower-case/);
    expect(validNamespace("a".repeat(63))).toBe("");
    expect(validNamespace("team-a1")).toBe("");
  });
});

describe("Teams, with no team yet", () => {
  it("lets a bootstrap admin create one, and opens its board", async () => {
    const puts = at(true);
    fireEvent.change(await screen.findByLabelText(/^Namespace/), { target: { value: "x" } });
    fireEvent.change(screen.getByLabelText("Display name"), { target: { value: "Team X" } });
    fireEvent.click(screen.getByRole("button", { name: "Create team" }));
    await vi.waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/t/x"));
    expect(puts).toEqual([{ url: "/api/v1/namespaces/x/team", body: { displayName: "Team X" } }]);
  });

  it("catches an invalid namespace before the request", async () => {
    const puts = at(true);
    fireEvent.change(await screen.findByLabelText(/^Namespace/), { target: { value: "Team_X" } });
    fireEvent.click(screen.getByRole("button", { name: "Create team" }));
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toMatch(/lower-case/);
    const field = screen.getByLabelText(/^Namespace/);
    expect(field.getAttribute("aria-invalid")).toBe("true");
    expect(field.getAttribute("aria-describedby")).toBe(`namespace-help ${alert.id}`);
    expect(document.activeElement).toBe(field);
    expect(puts).toHaveLength(0);
    expect(screen.getByTestId("where").textContent).toBe("/");
  });

  it("says when the namespace already has a Team, with a link to it", async () => {
    at(true, () => json({ error: { code: "already_exists", message: "team x: already exists" } }, 409));
    fireEvent.change(await screen.findByLabelText(/^Namespace/), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "Create team" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("x already has a Team. Open it");
    expect(screen.getByRole("link", { name: "Open it" }).getAttribute("href")).toBe("/t/x");
    expect(document.activeElement).toBe(alert);
  });

  it("says when the namespace isn't in the cluster", async () => {
    at(true, () => json({ error: { code: "not_found", message: "not found" } }, 404));
    fireEvent.change(await screen.findByLabelText(/^Namespace/), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "Create team" }));
    expect((await screen.findByRole("alert")).textContent).toBe(
      "There is no namespace x in the cluster. Create it first (kubectl create namespace x), then create the team.",
    );
    const field = screen.getByLabelText(/^Namespace/);
    expect(field.getAttribute("aria-invalid")).toBe("true");
    expect(document.activeElement).toBe(field);
  });

  it("shows the server's other errors under the form", async () => {
    at(true, () => json({ error: { code: "forbidden", message: "only a bootstrap admin can create a Team" } }, 403));
    fireEvent.change(await screen.findByLabelText(/^Namespace/), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "Create team" }));
    expect((await screen.findByRole("alert")).textContent).toBe("only a bootstrap admin can create a Team");
    expect(screen.getByLabelText(/^Namespace/).getAttribute("aria-invalid")).toBe("false");
    expect(screen.getByRole("button", { name: "Create team" }).hasAttribute("disabled")).toBe(false);
    expect(document.activeElement).toBe(screen.getByRole("alert"));
  });

  it("keeps focus on the error after a server failure", async () => {
    at(true, () => json({ error: { code: "internal", message: "etcd is down" } }, 500));
    fireEvent.change(await screen.findByLabelText(/^Namespace/), { target: { value: "x" } });
    const button = screen.getByRole("button", { name: "Create team" });
    button.focus();
    fireEvent.click(button);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("etcd is down");
    expect(document.activeElement).toBe(alert);
  });

  it("shows anyone else the cadre team init hint, and no form", async () => {
    at(false);
    expect(await screen.findByText("cadre team init -n <namespace>")).toBeTruthy();
    expect(screen.queryByRole("form", { name: "Create a team" })).toBeNull();
    expect(screen.queryByLabelText(/^Namespace/)).toBeNull();
  });
});

describe("Teams, with teams", () => {
  it("reveals the form under New team, for a bootstrap admin", async () => {
    vi.stubGlobal("fetch", (url: string) => {
      if (url.endsWith("/me")) return json({ mode: "oidc", username: "dana", admin: true });
      if (url.endsWith("/teams")) return json({ items: [team()] });
      return json({ items: [] });
    });
    render(
      <MemoryRouter initialEntries={["/"]}>
        <App />
      </MemoryRouter>,
    );
    const toggle = await screen.findByRole("button", { name: "New team" });
    expect(screen.queryByRole("form", { name: "Create a team" })).toBeNull();
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByRole("form", { name: "Create a team" }).id).toBe(toggle.getAttribute("aria-controls"));
    expect(document.activeElement).toBe(screen.getByLabelText(/^Namespace/));
  });
});
