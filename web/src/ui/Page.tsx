import { useEffect, useRef, type ReactNode } from "react";
import { useLocation } from "react-router";

// Pages set the document's title and, after a route change, move focus to
// their h1, so a screen reader hears where it landed. The first page of a
// visit keeps the browser's focus.

// useTitle sets document.title: the parts, then "Cadre".
export function useTitle(...parts: (string | undefined)[]) {
  const title = [...parts.filter((p): p is string => !!p), "Cadre"].join(" · ");
  useEffect(() => {
    document.title = title;
  }, [title]);
}

// focusedFor is the navigation whose h1 took the focus: one h1 per route
// change, even when it mounts only once the page has loaded.
let focusedFor = "";

// Heading is a page's h1, the target of the focus after a route change.
// The router's first location has the key "default"; every navigation
// after has its own.
export function Heading({ children, className }: { children: ReactNode; className?: string }) {
  const { key } = useLocation();
  const ref = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    if (key === "default" || key === focusedFor) return;
    focusedFor = key;
    ref.current?.focus();
  }, [key]);
  return (
    <h1 ref={ref} tabIndex={-1} className={className}>
      {children}
    </h1>
  );
}
