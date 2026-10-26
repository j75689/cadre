import { NavLink, Outlet, useLocation, useParams } from "react-router";
import { roleOf, useIdentityDetails } from "./Identity";
import { teamPath } from "./links";
import type { Role } from "./roles";
import { Icon, type IconName } from "./ui/Icon";

type Mode = ReturnType<typeof useIdentityDetails>["mode"];

// The team's tabs, in order. to is relative to the team's path; end matches
// it alone, not the pages under it. show leaves out a tab the role cannot use.
const items: { to: string; label: string; icon: IconName; end?: boolean; show: (role: Role | undefined, mode: Mode) => boolean }[] = [
  { to: "", label: "Board", icon: "kanban", end: true, show: () => true },
  { to: "agents", label: "Agents", icon: "bot", show: () => true },
];

// TeamNav is the team's tabs, under the top bar on every team page.
export function TeamNav() {
  const { ns = "" } = useParams();
  const { pathname } = useLocation();
  const { mode, me } = useIdentityDetails();
  const role = roleOf(me, ns);
  return (
    <nav className="teamnav" aria-label="Team">
      {items
        .filter((i) => i.show(role, mode))
        .map((i) => {
          const to = teamPath(ns) + (i.to && `/${i.to}`);
          // NavLink's end does not match the path with a trailing slash.
          return (
            <NavLink key={i.label} className="teamnav-tab" to={to} end={i.end && pathname !== `${to}/`}>
              <Icon name={i.icon} />
              {i.label}
            </NavLink>
          );
        })}
    </nav>
  );
}

// TeamLayout is a team page: the team's tabs, then the page.
export function TeamLayout() {
  return (
    <>
      <TeamNav />
      <Outlet />
    </>
  );
}
