import { describe, expect, it } from "vitest";
import { loginURL } from "./api";

describe("loginURL", () => {
  it("comes back to the page it left", () => {
    history.replaceState(null, "", "/t/team-a/tasks/x?tab=runs");
    expect(loginURL()).toBe("/auth/login?next=%2Ft%2Fteam-a%2Ftasks%2Fx%3Ftab%3Druns");
  });
});
