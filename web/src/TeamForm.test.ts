import { describe, expect, it } from "vitest";
import { durationOf, formIndexes, validate } from "./TeamForm";

describe("durationOf", () => {
  it("reads what Go's time.ParseDuration reads", () => {
    expect(durationOf("168h")).toBe(168 * 3600e9);
    expect(durationOf("1h30m")).toBe(5400e9);
    expect(durationOf(".5h")).toBe(1800e9);
    expect(durationOf("1.h")).toBe(3600e9);
    expect(durationOf("+1h")).toBe(3600e9);
    expect(durationOf("1µs")).toBe(1e3); // U+00B5
    expect(durationOf("1μs")).toBe(1e3); // U+03BC
    expect(durationOf("1us2ms3ns")).toBe(2001003);
    expect(durationOf("0s")).toBe(0);
    expect(durationOf("0.5ns")).toBe(0); // Go drops the part under 1 ns
  });

  it("refuses what it refuses, and negative durations", () => {
    for (const v of ["", "0", "1", "h", ".h", "1d", "1 h", "1hm", "-1h", "1h-1m", "a week"]) expect(durationOf(v), v).toBeUndefined();
  });
});

describe("validate", () => {
  const role = { role: "admin" as const, subjects: [{ kind: "user" as const, name: "dana" }] };

  it("takes durations longer than 0 only, as the server does", () => {
    expect(validate({ archiveAfter: "30m" })).toBe("");
    expect(validate({ archiveAfter: " .5h " })).toBe("");
    for (const v of ["0s", "0h0m", "0.5ns", "-1h"]) expect(validate({ archiveAfter: v }), v).toMatch(/longer than 0/);
    expect(validate({ archiveAfter: "" })).toBe("");
  });

  it("takes a cap the server's int32 holds", () => {
    expect(validate({ maxConcurrentRuns: 0 })).toBe("");
    expect(validate({ maxConcurrentRuns: 2147483647 })).toBe("");
    for (const v of [2147483648, -1, 1.5, NaN]) expect(validate({ maxConcurrentRuns: v }), String(v)).toMatch(/whole number, from 0 to 2147483647/);
  });

  it("checks the roles, numbered from 1", () => {
    expect(validate({ roles: [role] })).toBe("");
    expect(validate({ roles: [role, { role: "viewer", subjects: [] }] })).toBe("Role 2 needs at least one subject.");
    expect(validate({ roles: [role, { role: "viewer", subjects: [{ kind: "user", name: "lee" }, { kind: "group", name: "  " }] }] })).toBe(
      "Role 2, subject 2 needs a name.",
    );
  });
});

describe("formIndexes", () => {
  it("numbers the roles and subjects of a server message from 1", () => {
    expect(formIndexes("roles[1].subjects[0]: a user needs a name: invalid")).toBe("Role 2, subject 1: a user needs a name: invalid");
    expect(formIndexes("roles[0]: a viewer role needs at least one subject: invalid")).toBe("Role 1: a viewer role needs at least one subject: invalid");
    expect(formIndexes("keep an admin role")).toBe("keep an admin role");
  });
});
