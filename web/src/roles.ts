// What each role on a team allows, as the server's internal/authz matrix
// has it. The server decides; the UI only leaves out what would be refused.

export type Role = "viewer" | "member" | "admin";

const rank: Record<Role, number> = { viewer: 1, member: 2, admin: 3 };

export type Action = "read" | "work" | "manage";

// needs is the weakest role allowed each action: work is creating tasks,
// commenting, replying, approving and cancelling.
const needs: Record<Action, Role> = { read: "viewer", work: "member", manage: "admin" };

export function can(role: Role | undefined, action: Action): boolean {
  return role !== undefined && rank[role] >= rank[needs[action]];
}

export function isRole(r: string | undefined): r is Role {
  return r === "viewer" || r === "member" || r === "admin";
}
