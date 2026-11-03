import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { auth } from "./api";
import { IdentityBox, IdentityProvider, RoleLine, useIdentity } from "./Identity";
import { loadIdentity } from "./identityStore";
import { json } from "./testing";

beforeEach(() => localStorage.clear());
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const me = (body: unknown, status = 200) => vi.stubGlobal("fetch", () => json(body, status));

function renderBox() {
  render(
    <IdentityProvider>
      <IdentityBox />
    </IdentityProvider>,
  );
}

describe("IdentityBox without logins", () => {
  beforeEach(() => me({ mode: "none", admin: true }));

  it("asks once, then keeps the name across reloads", async () => {
    renderBox();
    fireEvent.change(await screen.findByLabelText(/your name/i), { target: { value: "  dana " } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(screen.getByText("dana")).toBeTruthy();
    expect(loadIdentity()).toBe("dana");

    cleanup();
    renderBox();
    expect(await screen.findByText("dana")).toBeTruthy();
    expect(screen.queryByLabelText(/your name/i)).toBeNull();
  });

  it("refuses an empty name", async () => {
    renderBox();
    fireEvent.click(await screen.findByRole("button", { name: "Save" }));
    expect(loadIdentity()).toBe("");
    expect(screen.getByLabelText(/your name/i)).toBeTruthy();
  });
});

describe("IdentityBox with a login", () => {
  it("shows the user and a logout, and asks for no name", async () => {
    me({ mode: "oidc", username: "dana@example.test", name: "Dana Example" });
    renderBox();
    expect(await screen.findByText("Dana Example")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Log out" })).toBeTruthy();
    expect(screen.queryByLabelText(/your name/i)).toBeNull();
  });
});

describe("an unauthenticated request", () => {
  it("sends the browser to log in, to come back where it was", async () => {
    const toLogin = vi.spyOn(auth, "toLogin").mockImplementation(() => {});
    me({ error: { code: "unauthenticated", message: "log in at /auth/login" } }, 401);
    render(
      <MemoryRouter initialEntries={["/t/a"]}>
        <App />
      </MemoryRouter>,
    );
    await vi.waitFor(() => expect(toLogin).toHaveBeenCalled());
  });

  it("says the session was dropped, rather than going round the login again", async () => {
    const toLogin = vi.spyOn(auth, "toLogin").mockImplementation(() => {});
    vi.spyOn(auth, "justLoggedIn").mockReturnValue(true);
    me({ error: { code: "unauthenticated", message: "log in at /auth/login" } }, 401);
    renderBox();
    expect(await screen.findByText(/did not keep the session/)).toBeTruthy();
    expect(toLogin).not.toHaveBeenCalled();
  });
});

describe("the shared read of /me", () => {
  // answers are the reads of /me in flight, in the order they were sent,
  // each ended by its signal as the browser's fetch.
  let answers: { answer: (body: unknown, status?: number) => void }[] = [];
  beforeEach(() => {
    answers = [];
    vi.stubGlobal(
      "fetch",
      (_url: string, init?: RequestInit) =>
        new Promise<Response>((resolve, reject) => {
          init?.signal?.addEventListener("abort", () => reject(new DOMException("The read was aborted.", "AbortError")));
          answers.push({ answer: (body, status = 200) => void json(body, status).then(resolve) });
        }),
    );
  });

  function Refresh() {
    const { refresh } = useIdentity();
    return <button onClick={refresh}>Read again</button>;
  }
  function renderRoles() {
    render(
      <IdentityProvider>
        <IdentityBox />
        <RoleLine ns="a" />
        <Refresh />
      </IdentityProvider>,
    );
  }
  const roles = (r: Record<string, string>) => ({ mode: "oidc", username: "dana@example.test", name: "Dana", roles: r });

  it("keeps the newest answer, not the last one to come", async () => {
    renderRoles();
    await act(async () => answers[0].answer(roles({ a: "viewer" })));
    expect(await screen.findByText("You are a viewer of a.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Read again" }));
    fireEvent.click(screen.getByRole("button", { name: "Read again" }));
    await act(async () => answers[2].answer(roles({})));
    expect(await screen.findByText("You have no role on a.")).toBeTruthy();
    await act(async () => answers[1].answer(roles({ a: "admin" })));
    expect(screen.getByText("You have no role on a.")).toBeTruthy();
  });

  it("does not report the read it ended as a failure", async () => {
    renderRoles();
    fireEvent.click(screen.getByRole("button", { name: "Read again" }));
    await act(async () => {});
    expect(screen.queryByText(/Could not tell who you are/)).toBeNull();
    await act(async () => answers[1].answer(roles({ a: "viewer" })));
    expect(await screen.findByText("You are a viewer of a.")).toBeTruthy();
  });
});
