import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import type { ConfigResource } from "../api";
import { YAMLCard } from "./Config";

afterEach(cleanup);

const tool = (spec: Record<string, unknown>): ConfigResource => ({
  kind: "ToolServer",
  name: "mcp",
  ready: true,
  generation: 1,
  observedGeneration: 1,
  conditions: [],
  metadata: { uid: "u-1", resourceVersion: "3", creationTimestamp: "2026-10-25T08:00:00Z" },
  spec: { transport: "streamable-http", ...spec },
});

const managed = { image: "registry.example.test/mcp:1", port: 8080 };

describe("YAMLCard", () => {
  it.each([
    ["its URL", { url: "https://(set)@api.example.test/mcp/?k=(set)#(set)" }],
    ["a managed server's path", { managed: { ...managed, path: "/mcp?k=(set)" } }],
    ["a managed server's probe path", { managed: { ...managed, readinessProbe: { httpGet: { path: "/healthz#(set)", port: 8080 } } } }],
    ["a managed server's scheme-less path", { managed: { ...managed, path: "//(set)@mcp.internal/mcp" } }],
    ["a managed server's scheme-less probe path", { managed: { ...managed, readinessProbe: { httpGet: { path: "//(set)@h/healthz", port: 8080 } } } }],
  ])("notes a ToolServer's URL masked in %s", (_, spec) => {
    render(<YAMLCard resource={tool(spec)} namespace="a" />);
    const copy = screen.getByRole("button", { name: "Copy" });
    expect(copy.getAttribute("aria-describedby")).toBe("yaml-note");
    expect(screen.getByText("Contains hidden values; not applyable as is.")).toBeTruthy();
    expect(screen.getByText(/are set but hidden from members; admins see them/)).toBeTruthy();
  });

  it("adds no note for a URL-like (set) outside a ToolServer's masked URLs", () => {
    render(<YAMLCard resource={tool({ managed: { ...managed, image: "//(set)@registry.example.test/mcp?x=(set)", path: "/mcp" } })} namespace="a" />);
    expect(screen.getByRole("button", { name: "Copy manifest" })).toBeTruthy();
    expect(screen.queryByText(/hidden/)).toBeNull();
  });
});
