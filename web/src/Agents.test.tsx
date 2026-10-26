import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { AgentSummary } from "./api";
import { App } from "./App";
import { json } from "./testing";
import { ReadyBadge } from "./ui/Badge";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

// at opens team a's agents page; the agents answer with agents, the other
// calls never answer.
function at(agents: () => Promise<Response>) {
  vi.stubGlobal("fetch", (url: string) => (url.endsWith("/namespaces/a/agents") ? agents() : new Promise(() => {})));
  render(
    <MemoryRouter initialEntries={["/t/a/agents"]}>
      <App />
    </MemoryRouter>,
  );
}

const agents: AgentSummary[] = [
  { name: "bot-1", ready: true },
  { name: "bot-2", ready: false, reason: "Pending", message: "its current spec has not been checked yet" },
  { name: "bot-3", ready: false, reason: "SecretMissing", message: "secret llm-key not found" },
];

describe("Agents", () => {
  it("shows each agent's readiness, reason and message", async () => {
    at(() => json({ items: agents }));
    const rows = await screen.findAllByRole("row");
    // The header row, then one per agent.
    expect(rows).toHaveLength(4);
    const [, ready, pending, notReady] = rows.map((r) => within(r));
    expect(ready.getByText("bot-1")).toBeTruthy();
    expect(ready.getByText("Ready")).toBeTruthy();
    // The badge says Pending; the Reason cell does not repeat it.
    expect(pending.getAllByText("Pending")).toHaveLength(1);
    expect(pending.getByText("its current spec has not been checked yet")).toBeTruthy();
    expect(notReady.getByText("Not ready")).toBeTruthy();
    expect(notReady.getByText("SecretMissing")).toBeTruthy();
    expect(notReady.getByText("secret llm-key not found")).toBeTruthy();
  });

  it("links each agent to the board filtered to its tasks", async () => {
    at(() => json({ items: agents }));
    const link = await screen.findByRole("link", { name: "Tasks of bot-1" });
    expect(link.getAttribute("href")).toBe("/t/a?assignee=bot-1");
  });

  it("scrolls the table inside a named, focusable region", async () => {
    at(() => json({ items: agents }));
    const region = await screen.findByRole("region", { name: "Agents" });
    expect(region.getAttribute("tabindex")).toBe("0");
    expect(within(region).getByRole("table")).toBeTruthy();
  });

  it("says how agents are added when there are none", async () => {
    at(() => json({ items: [] }));
    expect(await screen.findByText(/This team has no agents/)).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows a banner with Retry when the agents cannot be read", async () => {
    at(() => json({ error: { code: "Unavailable", message: "not synced" } }, 503));
    expect(await screen.findByText("Could not load the agents: not synced")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });

  it("is the current tab of the team nav", async () => {
    at(() => json({ items: [] }));
    const nav = within(screen.getByRole("navigation", { name: "Team" }));
    const tab = nav.getByRole("link", { name: "Agents" });
    expect(tab.getAttribute("href")).toBe("/t/a/agents");
    expect(tab.getAttribute("aria-current")).toBe("page");
    expect(nav.getByRole("link", { name: "Board" }).getAttribute("aria-current")).toBeNull();
    await screen.findByText(/This team has no agents/);
  });
});

describe("ReadyBadge", () => {
  const word = (ready: boolean, reason?: string) => {
    const { container, unmount } = render(<ReadyBadge ready={ready} reason={reason} />);
    const badge = container.querySelector(".badge");
    const out = { text: badge?.textContent, tone: badge?.className, icon: !!badge?.querySelector("svg") };
    unmount();
    return out;
  };

  it("is Ready when ready, whatever the reason", () => {
    expect(word(true)).toEqual({ text: "Ready", tone: "badge tone-done", icon: true });
    expect(word(true, "Pending").text).toBe("Ready");
  });

  it("is Pending while the current spec is unchecked", () => {
    expect(word(false, "Pending")).toEqual({ text: "Pending", tone: "badge tone-waiting", icon: true });
  });

  it("is Not ready for any other reason, or none", () => {
    expect(word(false, "SecretMissing")).toEqual({ text: "Not ready", tone: "badge tone-danger", icon: true });
    expect(word(false).text).toBe("Not ready");
  });
});
