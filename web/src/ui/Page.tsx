import { useEffect, useRef, type ReactNode } from "react";
import { useLocation } from "react-router";

// Pages set the document's title and, after a route change, move focus to
// their h1, so a screen reader hears where it landed. The first page of a
// visit keeps the browser's focus, and so does a change of the query only
// (a run linked): the page is the same.

// useTitle sets document.title: the parts, then "Cadre".
export function useTitle(...parts: (string | undefined)[]) {
  const title = [...parts.filter((p): p is string => !!p), "Cadre"].join(" · ");
  useEffect(() => {
    document.title = title;
  }, [title]);
}

// focusedFor is the navigation whose h1 took the focus: one h1 per route
// change, even when it mounts only once the page has loaded. focusedPath is
// the path of the last navigation an h1 saw.
let focusedFor = "";
let focusedPath = "";

// Heading is a page's h1, the target of the focus after a route change.
// The router's first location has the key "default"; every navigation
// after has its own.
export function Heading({ children, className }: { children: ReactNode; className?: string }) {
  const { key, pathname } = useLocation();
  const ref = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    if (key === focusedFor) return;
    const samePage = pathname === focusedPath;
    focusedFor = key;
    focusedPath = pathname;
    if (key !== "default" && !samePage) ref.current?.focus();
  }, [key, pathname]);
  return (
    <h1 ref={ref} tabIndex={-1} className={className}>
      {children}
    </h1>
  );
}
