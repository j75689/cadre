import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { allowed, CancelTask, carryOver, Composer } from "./Actions";
import { IdentityProvider } from "./Identity";
import { json, task } from "./testing";

beforeEach(() => {
  localStorage.clear();
  vi.stubGlobal("fetch", () => json({ mode: "none", admin: true }));
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderActions(t: ReturnType<typeof task>, name = "dana") {
  if (name) localStorage.setItem("cadre.identity", name);
  render(
    <IdentityProvider>
      <CancelTask task={t} onTask={() => {}} onRefresh={() => {}} />
      <Composer task={t} onTask={() => {}} onRefresh={() => {}} waitingTool="search__web" />
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
      if (url.endsWith("/me")) return json({ mode: "oidc", username: "dana@example.test", name: "Dana", roles: { a: "member" } });
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
    expect(await screen.findByRole("button", { name: "Approve" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reject" })).toBeTruthy();
  });

  it("asks for no name while logging in is still being checked", () => {
    vi.stubGlobal("fetch", () => new Promise(() => {}));
    renderActions(task("Todo"), "");
    expect(screen.queryByText(/give your name/i)).toBeNull();
  });

  it("hides cancel and comment once the task has ended", () => {
    renderActions(task("Done"));
    expect(screen.queryByRole("region", { name: "Actions" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Cancel task" })).toBeNull();
  });

  it("disables every action until a name is given", async () => {
    renderActions(task("Waiting", { waiting: { reason: "ToolApproval" } }), "");
    expect(await screen.findByText(/give your name/i)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Comment instead" }));
    for (const name of ["Comment", "Cancel task"]) {
      expect((screen.getByRole("button", { name }) as HTMLButtonElement).disabled, name).toBe(true);
    }
    fireEvent.click(screen.getByRole("button", { name: "Comment instead" }));
    for (const name of ["Approve", "Reject"]) {
      expect((screen.getByRole("button", { name }) as HTMLButtonElement).disabled, name).toBe(true);
    }
  });
});

describe("Tool call note", () => {
  it("is data-dirty only once it holds text", async () => {
    renderActions(task("Waiting", { waiting: { reason: "ToolApproval" } }));
    const note = await screen.findByLabelText("Tool call note");
    const area = document.querySelector(".approval")!;
    expect(area.hasAttribute("data-dirty")).toBe(false);
    expect(document.querySelector("[data-dirty]")).toBeNull();
    fireEvent.change(note, { target: { value: "draft" } });
    expect(area.hasAttribute("data-dirty")).toBe(true);
  });

  it("stays a draft while Comment instead hides it", async () => {
    renderActions(task("Waiting", { waiting: { reason: "ToolApproval" } }));
    fireEvent.change(await screen.findByLabelText("Tool call note"), { target: { value: "draft" } });
    fireEvent.click(screen.getByRole("button", { name: "Comment instead" }));
    expect(screen.queryByLabelText("Tool call note")).toBeNull();
    expect(document.querySelector("[data-dirty]")).not.toBeNull();
  });
});

describe("Composer", () => {
  it("picks its mode from what the task waits for", async () => {
    const modes: [ReturnType<typeof task>, string, string[]][] = [
      [task("Todo"), "Comment", ["Reply", "Approve"]],
      [task("Running"), "Comment", ["Reply", "Approve"]],
      [task("Waiting", { waiting: { reason: "Question" } }), "Reply", ["Comment", "Approve"]],
      [task("Waiting", { waiting: { reason: "Review" } }), "Reply", ["Comment", "Approve"]],
      [task("Waiting", { waiting: { reason: "ToolApproval" } }), "Approve", ["Comment", "Reply"]],
    ];
    for (const [t, shown, hidden] of modes) {
      renderActions(t);
      const what = `${t.status?.phase} ${t.status?.waiting?.reason ?? ""}`;
      expect(await screen.findByRole("button", { name: shown }), what).toBeTruthy();
      for (const name of hidden) expect(screen.queryByRole("button", { name }), `${what}: ${name}`).toBeNull();
      // Only a reply or an approval offers to comment instead.
      expect(screen.queryByRole("button", { name: "Comment instead" }) !== null, what).toBe(shown !== "Comment");
      cleanup();
    }
  });

  it("says what a reply is for, and names the tool to approve", async () => {
    renderActions(task("Waiting", { waiting: { reason: "Review" } }));
    expect(await screen.findByText("The task waits for your review.")).toBeTruthy();
    expect(screen.getByPlaceholderText("Your review")).toBeTruthy();
    cleanup();
    renderActions(task("Waiting", { waiting: { reason: "ToolApproval" } }));
    expect((await screen.findByText("search__web")).tagName).toBe("CODE");
  });

  it("switches a waiting task's composer to a comment and back", async () => {
    renderActions(task("Waiting", { waiting: { reason: "Question" } }));
    const toggle = await screen.findByRole("button", { name: "Comment instead" });
    expect(toggle.getAttribute("aria-pressed")).toBe("false");
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByRole("button", { name: "Comment" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Reply" })).toBeNull();
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-pressed")).toBe("false");
    expect(screen.getByRole("button", { name: "Reply" })).toBeTruthy();
  });

  it("posts a comment from a task waiting on an approval", async () => {
    const posts: string[] = [];
    vi.stubGlobal("fetch", (url: string) => {
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      posts.push(url);
      return json({ seq: 2, kind: "Comment", author: { kind: "User", name: "dana" }, body: "hold on", at: "" }, 201);
    });
    renderActions(task("Waiting", { waiting: { reason: "ToolApproval" } }));
    fireEvent.click(await screen.findByRole("button", { name: "Comment instead" }));
    const box = screen.getByLabelText("Comment");
    await vi.waitFor(() => expect((box as HTMLTextAreaElement).disabled).toBe(false));
    fireEvent.change(box, { target: { value: "hold on" } });
    fireEvent.click(screen.getByRole("button", { name: "Comment" }));
    await vi.waitFor(() => expect(posts).toEqual(["/api/v1/namespaces/a/tasks/x/comments"]));
  });

  it("keeps the draft and the note across the toggle, both ways", async () => {
    renderActions(task("Waiting", { waiting: { reason: "Question" } }));
    const reply = (await screen.findByLabelText("Reply")) as HTMLTextAreaElement;
    await vi.waitFor(() => expect(reply.disabled).toBe(false));
    fireEvent.change(reply, { target: { value: "half an answer" } });
    fireEvent.click(screen.getByRole("button", { name: "Comment instead" }));
    expect((screen.getByLabelText("Comment") as HTMLTextAreaElement).value).toBe("half an answer");
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "half an answer, and more" } });
    fireEvent.click(screen.getByRole("button", { name: "Comment instead" }));
    expect((screen.getByLabelText("Reply") as HTMLTextAreaElement).value).toBe("half an answer, and more");
    cleanup();

    renderActions(task("Waiting", { waiting: { reason: "ToolApproval" } }));
    const note = (await screen.findByLabelText("Tool call note")) as HTMLInputElement;
    await vi.waitFor(() => expect(note.disabled).toBe(false));
    fireEvent.change(note, { target: { value: "only staging" } });
    fireEvent.click(screen.getByRole("button", { name: "Comment instead" }));
    fireEvent.click(screen.getByRole("button", { name: "Comment instead" }));
    expect((screen.getByLabelText("Tool call note") as HTMLInputElement).value).toBe("only staging");
  });

  // composer renders the composer for a task that changes as the test says.
  function composer(t: ReturnType<typeof task>) {
    localStorage.setItem("cadre.identity", "dana");
    const view = (t: ReturnType<typeof task>) => (
      <IdentityProvider>
        <Composer task={t} onTask={() => {}} onRefresh={() => {}} />
      </IdentityProvider>
    );
    const { rerender } = render(view(t));
    return (next: ReturnType<typeof task>) => rerender(view(next));
  }
  const question = (since: string, threadSeq?: number) => task("Waiting", { waiting: { reason: "Question", since, threadSeq } });
  const approval = (since: string, threadSeq?: number) => task("Waiting", { waiting: { reason: "ToolApproval", since, threadSeq } });
  const box = (label: string) => screen.getByLabelText(label) as HTMLTextAreaElement;
  const pressed = () => screen.getByRole("button", { name: "Comment instead" }).getAttribute("aria-pressed");
  const ready = () => vi.waitFor(() => expect(screen.getAllByRole("textbox").every((b) => !(b as HTMLInputElement).disabled)).toBe(true));

  it("keeps a reply through the wait it answers, even as its entry is filled in", async () => {
    const to = composer(question("2026-10-26T01:00:00Z"));
    await ready();
    fireEvent.change(box("Reply"), { target: { value: "half an answer" } });
    // The manager writes the question's entry a poll later; the wait is the same.
    to(question("2026-10-26T01:00:00Z", 5));
    expect(box("Reply").value).toBe("half an answer");
  });

  it("drops a reply and a note with their wait, but keeps a comment", async () => {
    const to = composer(question("2026-10-26T01:00:00Z", 3));
    await ready();
    fireEvent.change(box("Reply"), { target: { value: "for the first question" } });
    // Another question: the reply was for the last one.
    to(question("2026-10-26T01:05:00Z", 5));
    expect(box("Reply").value).toBe("");
    expect(pressed()).toBe("false");

    // A comment typed under "Comment instead" stays one.
    fireEvent.click(screen.getByRole("button", { name: "Comment instead" }));
    fireEvent.change(box("Comment"), { target: { value: "a remark" } });
    to(question("2026-10-26T01:10:00Z", 7));
    expect(pressed()).toBe("true");
    expect(box("Comment").value).toBe("a remark");

    // A note belongs to its tool call, whatever else is kept.
    to(approval("2026-10-26T01:15:00Z", 9));
    fireEvent.click(screen.getByRole("button", { name: "Comment instead" }));
    fireEvent.change(screen.getByLabelText("Tool call note"), { target: { value: "only for search" } });
    to(approval("2026-10-26T01:15:00Z", 9));
    expect((screen.getByLabelText("Tool call note") as HTMLInputElement).value).toBe("only for search");
    to(approval("2026-10-26T01:20:00Z", 11));
    expect((screen.getByLabelText("Tool call note") as HTMLInputElement).value).toBe("");
  });

  it("keeps a comment being typed when a question arrives, as a comment", async () => {
    const to = composer(task("Running"));
    await ready();
    fireEvent.change(box("Comment"), { target: { value: "while it runs" } });
    to(question("2026-10-26T01:00:00Z"));
    expect(pressed()).toBe("true");
    expect(box("Comment").value).toBe("while it runs");
    // With nothing typed, the question is what the composer offers.
    cleanup();
    const again = composer(task("Running"));
    await ready();
    again(question("2026-10-26T01:00:00Z"));
    expect(pressed()).toBe("false");
    expect(box("Reply").value).toBe("");
  });

  it("keeps the text when the wait ends", async () => {
    const to = composer(question("2026-10-26T01:00:00Z", 3));
    await ready();
    fireEvent.change(box("Reply"), { target: { value: "never sent" } });
    // Someone else answered: the task runs again.
    to(task("Running"));
    expect(box("Comment").value).toBe("never sent");
  });

  it("drops a form's error with its wait", async () => {
    vi.stubGlobal("fetch", (url: string) => {
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      return json({ error: { code: "Conflict", message: "the task moved on" } }, 409);
    });
    const to = composer(question("2026-10-26T01:00:00Z", 3));
    await ready();
    fireEvent.change(box("Reply"), { target: { value: "too late" } });
    fireEvent.click(screen.getByRole("button", { name: "Reply" }));
    expect((await screen.findByRole("alert")).textContent).toBe("the task moved on");
    to(question("2026-10-26T01:05:00Z", 5));
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("carries nothing of a sent reply into the comment box", async () => {
    // As the task page does: the reply's answer is the task, running again.
    vi.stubGlobal("fetch", (url: string) => {
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      return json({ task: task("Running"), entry: { seq: 4, kind: "Answer", author: { kind: "User", name: "dana" }, body: "the answer", at: "" } });
    });
    localStorage.setItem("cadre.identity", "dana");
    function Page() {
      const [t, setT] = useState(question("2026-10-26T01:00:00Z", 3));
      return (
        <IdentityProvider>
          <Composer task={t} onTask={setT} onRefresh={() => {}} />
        </IdentityProvider>
      );
    }
    render(<Page />);
    await ready();
    fireEvent.change(box("Reply"), { target: { value: "the answer" } });
    fireEvent.click(screen.getByRole("button", { name: "Reply" }));
    await vi.waitFor(() => expect(screen.queryByLabelText("Reply")).toBeNull());
    expect(box("Comment").value).toBe("");
  });

  it("keeps an unsent reply, become a comment, through to the next question", async () => {
    const to = composer(question("2026-10-26T01:00:00Z", 3));
    await ready();
    fireEvent.change(box("Reply"), { target: { value: "never sent" } });
    // Someone else answered; the text is a comment now.
    to(task("Running"));
    expect(box("Comment").value).toBe("never sent");
    // Another question: a comment typed before it stays a comment.
    to(question("2026-10-26T01:10:00Z", 5));
    expect(pressed()).toBe("true");
    expect(box("Comment").value).toBe("never sent");
  });

  it("carries the toggle as it is, when it moved while a comment was being sent", async () => {
    let answer: (r: Response) => void = () => {};
    vi.stubGlobal("fetch", (url: string) => {
      if (url.endsWith("/me")) return json({ mode: "none", admin: true });
      return new Promise<Response>((r) => (answer = r));
    });
    const to = composer(question("2026-10-26T01:00:00Z", 3));
    await ready();
    fireEvent.click(screen.getByRole("button", { name: "Comment instead" }));
    fireEvent.change(box("Comment"), { target: { value: "a remark" } });
    fireEvent.click(screen.getByRole("button", { name: "Comment" }));
    // Back to the reply while the comment is on its way.
    fireEvent.click(screen.getByRole("button", { name: "Comment instead" }));
    expect(pressed()).toBe("false");
    await act(async () => answer(new Response(JSON.stringify({ seq: 4, kind: "Comment", author: { kind: "User", name: "dana" }, body: "a remark", at: "" }), { status: 201 })));
    // The next question starts from the toggle as it was left: released.
    to(question("2026-10-26T01:10:00Z", 5));
    expect(pressed()).toBe("false");
    expect(box("Reply").value).toBe("");
  });

  it("keeps everything for the same wait, closed and opened again", () => {
    const last = { body: "half an answer", commenting: false };
    expect(carryOver("Question/a", "Question/a", last)).toBe(last);
    expect(carryOver("Question/a", "Question/b", last)).toEqual({ body: "", commenting: false });
  });

  it("names the reply box Reply, and describes it with what the task waits for", async () => {
    renderActions(task("Waiting", { waiting: { reason: "Review" } }));
    const box = await screen.findByLabelText("Reply");
    expect(box.tagName).toBe("TEXTAREA");
    const described = document.getElementById(box.getAttribute("aria-describedby") ?? "");
    expect(described?.textContent).toBe("The task waits for your review.");
  });
});
