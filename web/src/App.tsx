import { Link, Outlet, Route, Routes } from "react-router";
import { Board } from "./Board";
import { IdentityBox } from "./Identity";
import { TaskPage } from "./TaskPage";
import { Teams } from "./Teams";

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
    <Routes>
      <Route element={<Layout />}>
        <Route index element={<Teams />} />
        <Route path="t/:ns" element={<Board />} />
        <Route path="t/:ns/tasks/:name" element={<TaskPage />} />
        <Route path="*" element={<main><p>Not found. <Link to="/">Teams</Link></p></main>} />
      </Route>
    </Routes>
  );
}
