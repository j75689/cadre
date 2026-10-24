import { Link, Outlet, Route, Routes, useParams } from "react-router";
import { Board } from "./Board";
import { IdentityBox, IdentityProvider } from "./Identity";
import { NewTask } from "./NewTask";
import { TaskPage } from "./TaskPage";
import { Teams } from "./Teams";

// A board belongs to one team: another team's is a fresh one, not this one
// with old columns until the first poll.
function TeamBoard() {
  const { ns = "" } = useParams();
  return <Board key={ns} />;
}

// Likewise a task page belongs to one task.
function OneTask() {
  const { ns = "", name = "" } = useParams();
  return <TaskPage key={`${ns}/${name}`} />;
}

function Layout() {
  return (
    <>
      <nav className="top">
        <Link to="/" className="brand">
          Cadre
        </Link>
        <IdentityBox />
      </nav>
      <Outlet />
    </>
  );
}

// The team is in the URL, /t/<namespace>, so a board can be linked to.
export function App() {
  return (
    <IdentityProvider>
      <Routes>
        <Route element={<Layout />}>
          <Route index element={<Teams />} />
          <Route path="t/:ns" element={<TeamBoard />} />
          <Route path="t/:ns/new" element={<NewTask />} />
          <Route path="t/:ns/tasks/:name" element={<OneTask />} />
          <Route path="*" element={<main><p>Not found. <Link to="/">Teams</Link></p></main>} />
        </Route>
      </Routes>
    </IdentityProvider>
  );
}
