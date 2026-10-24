import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Actions, allowed } from "./Actions";
import { IdentityProvider } from "./Identity";
import { json, task } from "./testing";

beforeEach(() => {
  localStorage.clear();
  vi.stubGlobal("fetch", () => json({ mode: "none" }));
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderActions(t: ReturnType<typeof task>, name = "dana") {
  if (name) localStorage.setItem("cadre.identity", name);
  render(
    <IdentityProvider>
      <Actions task={t} onTask={() => {}} onRefresh={() => {}} />
    </IdentityProvider>,
  );
}

describe("allowed", () => {
  it("mirrors what the API accepts in each state", () => {
    const cases: [ReturnType<typeof task>, ReturnType<typeof allowed>][] = [
      [task("Todo"), { comment: true, reply: false, approve: false, cancel: true }],
      [task("Running"), { comment: true, reply: false, approve: false, cancel: true }],
      [task("Waiting", { waiting: { reason: "Question" } }), { comment: true, reply: true, approve: false, cancel: true }],
      [task("Waiting", { waiting: { reason: "Review" } }), { comment: true, reply: true, approve: false, cancel: true }],
      [task("Waiting", { waiting: { reason: "ToolApproval" } }), { comment: true, reply: false, approve: true, cancel: true }],
      [task("Review"), { comment: true, reply: false, approve: false, cancel: true }],
      [task("Done"), { comment: false, reply: false, approve: false, cancel: false }],
      [task("Cancelled"), { comment: false, reply: false, approve: false, cancel: false }],
      [task("Failed"), { comment: false, reply: false, approve: false, cancel: false }],
    ];
    for (const [t, want] of cases) expect(allowed(t), `${t.status?.phase} ${t.status?.waiting?.reason ?? ""}`).toEqual(want);
  });
});

describe("Actions", () => {
  it("are enabled once a name is given", async () => {
    renderActions(task("Todo"));
    await vi.waitFor(() => expect((screen.getByRole("button", { name: "Cancel task" }) as HTMLButtonElement).disabled).toBe(false));
  });

  it("are enabled with a login, and send no author", async () => {
    const bodies: string[] = [];
    vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
      if (url.endsWith("/me")) return json({ mode: "oidc", username: "dana@example.test", name: "Dana" });
      bodies.push(String(init?.body));
      return json({ seq: 1, kind: "Comment", author: { kind: "User", name: "dana@example.test" }, body: "hi", at: "" }, 201);
    });
    renderActions(task("Todo"), "");
    const box = await screen.findByLabelText("Comment");
    await vi.waitFor(() => expect((box as HTMLTextAreaElement).disabled).toBe(false));
    fireEvent.change(box, { target: { value: "hi" } });
    fireEvent.click(screen.getByRole("button", { name: "Comment" }));
    await vi.waitFor(() => expect(bodies).toHaveLength(1));
    expect(JSON.parse(bodies[0])).toEqual({ body: "hi" });
  });

  it("offers reply only to a question, approval only to a tool call", async () => {
    renderActions(task("Waiting", { waiting: { reason: "Question" } }));
    expect(await screen.findByRole("button", { name: "Reply" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    cleanup();
    renderActions(task("Waiting", { waiting: { reason: "ToolApproval" } }));
    expect(screen.queryByRole("button", { name: "Reply" })).toBeNull();
    expect(screen.getByRole("button", { name: "Approve" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reject" })).toBeTruthy();
  });

  it("hides cancel and comment once the task has ended", () => {
    renderActions(task("Done"));
    expect(screen.queryByRole("region", { name: "Actions" })).toBeNull();
  });

  it("disables every action until a name is given", async () => {
    renderActions(task("Waiting", { waiting: { reason: "ToolApproval" } }), "");
    expect(await screen.findByText(/give your name/i)).toBeTruthy();
    for (const name of ["Approve", "Reject", "Comment", "Cancel task"]) {
      expect((screen.getByRole("button", { name }) as HTMLButtonElement).disabled, name).toBe(true);
    }
  });
});
