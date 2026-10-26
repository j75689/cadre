import { describe, expect, it } from "vitest";
import { durationOf, formIndexes, refusal, validate } from "./TeamForm";

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
    expect(durationOf("2562047h")).toBe(2562047 * 3600e9); // the most whole hours an int64 holds
    expect(durationOf("9223372036854775807ns")).toBe(2 ** 63); // 2^63 - 1, rounded by the number
  });

  it("refuses what it refuses, and negative durations", () => {
    for (const v of ["", "0", "1", "h", ".h", "1d", "1 h", "1hm", "-1h", "1h-1m", "a week"]) expect(durationOf(v), v).toBeUndefined();
    for (const v of ["2562048h", "9999999h", "2562047h1h", "9223372036854775808ns", "2562047h47m16.854775808s"]) expect(durationOf(v), v).toBeUndefined(); // over Go's int64
  });
});

describe("validate", () => {
  const role = { role: "admin" as const, subjects: [{ kind: "user" as const, name: "dana" }] };

  it("takes durations longer than 0 only, as the server does", () => {
    expect(validate({ archiveAfter: "30m" })).toBeUndefined();
    expect(validate({ archiveAfter: " .5h " })).toBeUndefined();
    for (const v of ["0s", "0h0m", "0.5ns", "-1h"]) expect(validate({ archiveAfter: v }), v).toEqual({ message: expect.stringMatching(/longer than 0/), at: "archiveAfter" });
    expect(validate({ archiveAfter: "" })).toBeUndefined();
  });

  it("says when a duration is over the longest the server takes", () => {
    expect(validate({ archiveAfter: "2562047h47m16.854775807s" })).toBeUndefined();
    for (const v of ["2562048h", "9223372036854775808ns"])
      expect(validate({ archiveAfter: v }), v).toEqual({ message: "Archive after is too long: the longest the server takes is 2562047h47m16.854775807s.", at: "archiveAfter" });
  });

  it("takes a cap the server's int32 holds", () => {
    expect(validate({ maxConcurrentRuns: 0 })).toBeUndefined();
    expect(validate({ maxConcurrentRuns: 2147483647 })).toBeUndefined();
    for (const v of [2147483648, -1, 1.5, NaN])
      expect(validate({ maxConcurrentRuns: v }), String(v)).toEqual({ message: expect.stringMatching(/whole number, from 0 to 2147483647/), at: "maxConcurrentRuns" });
  });

  it("checks the roles, numbered from 1", () => {
    expect(validate({ roles: [role] })).toBeUndefined();
    expect(validate({ roles: [role, { role: "viewer", subjects: [] }] })).toEqual({ message: "Role 2 needs at least one subject.", at: "role-1" });
    expect(validate({ roles: [role, { role: "viewer", subjects: [{ kind: "user", name: "lee" }, { kind: "group", name: "  " }] }] })).toEqual({
      message: "Role 2, subject 2 needs a name.",
      at: "subject-1-1",
    });
  });
});

describe("formIndexes", () => {
  it("numbers the roles and subjects of a server message from 1", () => {
    expect(formIndexes("roles[1].subjects[0]: a user needs a name: invalid")).toBe("Role 2, subject 1: a user needs a name: invalid");
    expect(formIndexes("roles[0]: a viewer role needs at least one subject: invalid")).toBe("Role 1: a viewer role needs at least one subject: invalid");
    expect(formIndexes("keep an admin role")).toBe("keep an admin role");
  });
});

describe("refusal", () => {
  it("names the field a server message is about", () => {
    expect(refusal("roles[1].subjects[0]: a user needs a name: invalid")).toEqual({ message: "Role 2, subject 1: a user needs a name: invalid", at: "subject-1-0" });
    expect(refusal("roles[0]: a viewer role needs at least one subject: invalid").at).toBe("role-0");
    expect(refusal("archiveAfter -1h0m0s must be a positive duration such as 720h: invalid").at).toBe("archiveAfter");
    expect(refusal("maxConcurrentRuns -1: invalid").at).toBe("maxConcurrentRuns");
    expect(refusal("the Team would have no admin: keep an admin role")).toEqual({ message: "the Team would have no admin: keep an admin role", at: undefined });
  });
});
