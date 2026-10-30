import { useEffect, useState } from "react";
import { Link, Outlet, Route, Routes, useParams } from "react-router";
import { AccessDetail, AccessList } from "./Access";
import { AgentDetail } from "./AgentDetail";
import { Agents } from "./Agents";
import { Board } from "./Board";
import { IdentityBox, IdentityProvider, roleOf, useIdentityDetails } from "./Identity";
import { NewTask } from "./NewTask";
import { Settings } from "./Settings";
import { HelpDialog, useShortcuts } from "./shortcuts";
import { TaskPage } from "./TaskPage";
import { TeamLayout } from "./TeamNav";
import { Teams } from "./Teams";
import { teamName, TeamsProvider, useTeams } from "./teamList";
import { applyTheme, loadTheme, nextTheme, saveTheme, systemDark, type Theme } from "./theme";
import { RoleBadge } from "./ui/Badge";
import { Button, ButtonLink } from "./ui/Button";
import { Menu, MenuNote } from "./ui/Menu";
import { Heading, useRouteChanges, useTitle } from "./ui/Page";

// A board belongs to one team: another team's is a fresh one, not this one
// with old columns until the first poll.
function TeamBoard() {
  const { ns = "" } = useParams();
  return <Board key={ns} />;
}

// Likewise a create form belongs to one team,
function TeamNewTask() {
  const { ns = "" } = useParams();
  return <NewTask key={ns} />;
}

// the agents page,
function TeamAgents() {
  const { ns = "" } = useParams();
  return <Agents key={ns} />;
}

// an Agent's page to one Agent,
function OneAgent() {
  const { ns = "", name = "" } = useParams();
  return <AgentDetail key={`${ns}/${name}`} />;
}

// the settings,
function TeamSettings() {
  const { ns = "" } = useParams();
  return <Settings key={ns} />;
}

// the agent accesses,
function TeamAccesses() {
  const { ns = "" } = useParams();
  return <AccessList key={ns} />;
}

// an AgentAccess's page to one AgentAccess,
function OneAccess() {
  const { ns = "", name = "" } = useParams();
  return <AccessDetail key={`${ns}/${name}`} />;
}

// and a task page to one task.
function OneTask() {
  const { ns = "", name = "" } = useParams();
  return <TaskPage key={`${ns}/${name}`} />;
}

// TeamSwitcher names the current team and opens a menu of them all, each
// with the caller's role when there are logins.
function TeamSwitcher() {
  // The layout's params are its matched child's: the team on a team's
  // pages, none on the others.
  const { ns } = useParams();
  const { teams, error } = useTeams();
  const { mode, me } = useIdentityDetails();
  const current = ns === undefined ? undefined : teams?.find((t) => t.metadata.namespace === ns);
  const label =
    ns === undefined ? (
      <span className="switcher-label">Teams</span>
    ) : (
      <span className="switcher-label">
        <span>{current ? teamName(current) : ns}</span>
        {current?.spec?.displayName && <span className="mono muted switcher-ns">{ns}</span>}
      </span>
    );
  return (
    <Menu
      label={label}
      buttonLabel={ns === undefined ? "Switch team" : `Team ${current ? teamName(current) : ns}, switch team`}
      items={
        <>
          <li role="none">
            <Link role="menuitem" className="menu-item" to="/">
              All teams
            </Link>
          </li>
          {teams === null && !error && <MenuNote>Reading the teams</MenuNote>}
          {error && <MenuNote>Could not load the teams: {error}</MenuNote>}
          {teams?.map((t) => {
            const n = t.metadata.namespace;
            const role = mode === "oidc" ? roleOf(me, n) : undefined;
            return (
              <li role="none" key={n}>
                <Link
                  role="menuitem"
                  className="menu-item"
                  to={`/t/${encodeURIComponent(n)}`}
                  aria-current={n === ns ? "page" : undefined}
                 
                >
                  <span>{teamName(t)}</span>
                  {t.spec?.displayName && <span className="mono muted">{n}</span>}
                  {role && <RoleBadge role={role} />}
                </Link>
              </li>
            );
          })}
        </>
      }
    />
  );
}

const themeIcons = { system: "monitor", light: "sun", dark: "moon" } as const;

// ThemeControl cycles the theme: system, light, dark.
export function ThemeControl() {
  const [theme, setTheme] = useState<Theme>(loadTheme);
  useEffect(() => {
    applyTheme(theme);
    if (theme !== "system") return;
    // Following the system: a change of the system's follows.
    const query = systemDark();
    const follow = () => applyTheme("system");
    query?.addEventListener("change", follow);
    return () => query?.removeEventListener("change", follow);
  }, [theme]);
  return (
    <Button
      variant="ghost"
      icon={themeIcons[theme]}
      aria-label={`Theme: ${theme}. Switch to ${nextTheme[theme]}`}
      title={`Theme: ${theme}`}
      onClick={() => {
        const next = nextTheme[theme];
        saveTheme(next);
        setTheme(next);
      }}
    />
  );
}

function Layout() {
  useRouteChanges();
  const { ns } = useParams();
  const [help, setHelp] = useShortcuts(ns);
  return (
    <>
      <a className="skip-link" href="#main">
        Skip to content
      </a>
      <header className="topbar">
        <Link to="/" className="brand" aria-label="Cadre, all teams">
          <span className="brand-mark" aria-hidden="true">
            C
          </span>
          <span className="brand-name">Cadre</span>
        </Link>
        <span className="topbar-sep" aria-hidden="true" />
        <TeamSwitcher />
        <div className="topbar-end">
          <Button variant="ghost" icon="help-circle" aria-label="Keyboard shortcuts" title="Keyboard shortcuts" onClick={() => setHelp(true)} />
          <ThemeControl />
          <IdentityBox />
        </div>
      </header>
      <HelpDialog ns={ns} open={help} onClose={() => setHelp(false)} />
      <Outlet />
    </>
  );
}

function NotFound() {
  useTitle("Not found");
  return (
    <main id="main">
      <div className="card card-pad not-found">
        <Heading>Not found</Heading>
        <p>There is no page here.</p>
        <ButtonLink to="/" icon="arrow-left">
          Teams
        </ButtonLink>
      </div>
    </main>
  );
}

// The team is in the URL, /t/<namespace>, so a board can be linked to.
export function App() {
  return (
    <IdentityProvider>
      <TeamsProvider>
        <Routes>
          <Route element={<Layout />}>
            <Route index element={<Teams />} />
            <Route path="t/:ns" element={<TeamLayout />}>
              <Route index element={<TeamBoard />} />
              <Route path="agents" element={<TeamAgents />} />
              <Route path="agents/:name" element={<OneAgent />} />
              <Route path="new" element={<TeamNewTask />} />
              <Route path="tasks/:name" element={<OneTask />} />
              <Route path="settings" element={<TeamSettings />} />
              <Route path="settings/access" element={<TeamAccesses />} />
              <Route path="settings/access/:name" element={<OneAccess />} />
              <Route path="*" element={<NotFound />} />
            </Route>
            <Route path="*" element={<NotFound />} />
          </Route>
        </Routes>
      </TeamsProvider>
    </IdentityProvider>
  );
}
