import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { Actions, allowed } from "./Actions";
import { IdentityProvider } from "./Identity";
import { task } from "./testing";

beforeEach(() => localStorage.clear());
afterEach(cleanup);

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
  it("offers reply only to a question, approval only to a tool call", () => {
    renderActions(task("Waiting", { waiting: { reason: "Question" } }));
    expect(screen.getByRole("button", { name: "Reply" })).toBeTruthy();
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

  it("disables every action until a name is given", () => {
    renderActions(task("Waiting", { waiting: { reason: "ToolApproval" } }), "");
    expect(screen.getByText(/give your name/i)).toBeTruthy();
    for (const name of ["Approve", "Reject", "Comment", "Cancel task"]) {
      expect((screen.getByRole("button", { name }) as HTMLButtonElement).disabled, name).toBe(true);
    }
  });
});
