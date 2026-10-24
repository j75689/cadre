import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { entry, json, task } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

// server answers as cadre-server with logins: /me with roles, and the rest
// from a task waiting on a tool approval.
function server(me: () => unknown, onPost?: () => Promise<Response>) {
  const reads: string[] = [];
  vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
    if (init?.method === "POST") return onPost ? onPost() : json({});
    reads.push(url);
    if (url.endsWith("/me")) return json(me());
    if (url.endsWith("/teams")) return json({ items: [{ metadata: { name: "team", namespace: "a" } }, { metadata: { name: "team", namespace: "b" } }] });
    if (url.includes("/thread")) return json({ items: [entry(1, "ToolCall")] });
    if (url.endsWith("/runs")) return json({ items: [] });
    if (url.includes("/tasks?")) return json({ items: [], offset: 0, limit: 50 });
    return json(task("Waiting", { waiting: { reason: "ToolApproval", threadSeq: 1 } }));
  });
  return reads;
}

const oidc = (roles: Record<string, string>, admin = false) => ({ mode: "oidc", username: "dana@example.test", name: "Dana", roles, admin });

function at(path: string) {
  render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
}

describe("roles", () => {
  it("leave a viewer no actions, and say so", async () => {
    server(() => oidc({ a: "viewer" }));
    at("/t/a/tasks/x");
    expect(await screen.findByText("You are a viewer of a.")).toBeTruthy();
    expect(await screen.findByText("tool call")).toBeTruthy();
    for (const name of ["Approve", "Reject", "Comment", "Cancel task"]) expect(screen.queryByRole("button", { name })).toBeNull();
  });

  it("give a member the actions", async () => {
    server(() => oidc({ a: "member" }));
    at("/t/a/tasks/x");
    expect(await screen.findByRole("button", { name: "Approve" })).toBeTruthy();
    expect(screen.getByText("You are a member of a.")).toBeTruthy();
  });

  it("leave a viewer no New task on the board, nor the form", async () => {
    server(() => oidc({ a: "viewer" }));
    at("/t/a");
    expect(await screen.findByText("You are a viewer of a.")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "New task" })).toBeNull();
    cleanup();
    at("/t/a/new");
    expect(await screen.findByText("Creating tasks needs the member role.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Create task" })).toBeNull();
  });

  it("show each team's role in the picker", async () => {
    server(() => oidc({ a: "admin", b: "viewer" }));
    at("/");
    expect(await screen.findByText("admin")).toBeTruthy();
    expect(screen.getByText("viewer")).toBeTruthy();
  });

  it("are read again when an action is refused", async () => {
    let role = "member";
    const reads = server(
      () => oidc({ a: role }),
      () => {
        role = "viewer";
        return json({ error: { code: "no_role", message: "to comment on a needs the member role; you are a viewer" } }, 403);
      },
    );
    at("/t/a/tasks/x");
    fireEvent.change(await screen.findByLabelText("Comment"), { target: { value: "hi" } });
    fireEvent.click(screen.getByRole("button", { name: "Comment" }));
    expect(await screen.findByText(/you are a viewer/)).toBeTruthy();
    await vi.waitFor(() => expect(reads.filter((u) => u.endsWith("/me")).length).toBe(2));
    expect(await screen.findByText("You are a viewer of a.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Comment" })).toBeNull();
    // The reason stays when the actions go.
    expect(screen.getByRole("alert").textContent).toMatch(/you are a viewer/);
  });

  it("are all an admin's without logins", async () => {
    server(() => ({ mode: "none", admin: true }));
    localStorage.setItem("cadre.identity", "dana");
    at("/t/a/tasks/x");
    expect(await screen.findByRole("button", { name: "Approve" })).toBeTruthy();
    expect(screen.queryByText(/You are a/)).toBeNull();
  });

  it("are not read again for a 403 that is not about the role", async () => {
    const reads = server(
      () => oidc({ a: "member" }),
      () => json({ error: { code: "forbidden", message: "a change made with a session cookie must come from this site" } }, 403),
    );
    at("/t/a/tasks/x");
    fireEvent.change(await screen.findByLabelText("Comment"), { target: { value: "hi" } });
    fireEvent.click(screen.getByRole("button", { name: "Comment" }));
    expect(await screen.findByText(/must come from this site/)).toBeTruthy();
    expect(reads.filter((u) => u.endsWith("/me")).length).toBe(1);
    expect(screen.getByRole("button", { name: "Comment" })).toBeTruthy();
  });
});
