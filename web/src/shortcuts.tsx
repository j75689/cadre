import { useEffect, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router";
import { useRole } from "./Identity";
import { teamPath } from "./links";
import { can } from "./roles";
import { Dialog } from "./ui/Dialog";

// The console's keyboard shortcuts are single keys, never with Ctrl, Cmd
// or Alt, which belong to the browser and the system. They do not fire
// while typing: in a text field, during an input method's composition, or
// in an open dialog, where only ? (and the dialog's own Escape) works. The
// ones that leave the page do not fire from a form or a button either,
// or while a form on the page holds a draft: such a form is marked
// data-dirty.

// chordMs is how long after g a b still goes to the board.
const chordMs = 1000;

function typing(e: KeyboardEvent) {
  // An input method's keys arrive as keyCode 229 in some browsers, also
  // just after the composition has ended.
  if (e.isComposing || e.keyCode === 229) return true;
  return e.target instanceof Element && e.target.closest("input, textarea, select, [contenteditable]:not([contenteditable=false])") !== null;
}

function inFormOrButton(e: KeyboardEvent) {
  return e.target instanceof Element && e.target.closest("form, button") !== null;
}

// useShortcuts listens for the shortcuts on the team in namespace ns, if
// any; the always-mounted layout calls it once. It holds whether the help
// is open.
export function useShortcuts(ns: string | undefined): [boolean, (open: boolean) => void] {
  const [help, setHelp] = useState(false);
  const navigate = useNavigate();
  const { pathname } = useLocation();
  const role = useRole(ns ?? "");
  const gAt = useRef(0);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      // AltGr, which types / or ? on some layouts, arrives with Ctrl and
      // Alt on Windows.
      const altGr = e.getModifierState("AltGraph");
      if (e.defaultPrevented || e.metaKey || (!altGr && (e.ctrlKey || e.altKey)) || typing(e)) return;
      if (e.key === "?") {
        setHelp(true);
        return;
      }
      if (document.querySelector("dialog[open]")) return;
      const afterG = Date.now() - gAt.current <= chordMs;
      gAt.current = 0;
      if (e.key === "/") {
        const search = document.getElementById("board-search");
        if (!search) return;
        e.preventDefault();
        search.focus();
      } else if (ns === undefined || inFormOrButton(e) || document.querySelector("[data-dirty]")) {
        return;
      } else if (e.key === "c" && can(role, "work")) {
        void navigate(`${teamPath(ns)}/new`);
      } else if (e.key === "g") {
        gAt.current = Date.now();
      } else if (e.key === "b" && afterG && pathname !== teamPath(ns)) {
        // On the board already, it keeps the board's search and filters.
        void navigate(teamPath(ns));
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [ns, role, navigate, pathname]);
  return [help, setHelp];
}

// HelpDialog lists the shortcuts the caller can use on this page's team.
export function HelpDialog({ ns, open, onClose }: { ns: string | undefined; open: boolean; onClose: () => void }) {
  const role = useRole(ns ?? "");
  return (
    <Dialog open={open} onClose={onClose} title="Keyboard shortcuts">
      <dl className="shortcuts">
        <dt>
          <kbd>?</kbd>
        </dt>
        <dd>Show the keyboard shortcuts</dd>
        <dt>
          <kbd>/</kbd>
        </dt>
        <dd>Search the board</dd>
        {ns !== undefined && can(role, "work") && (
          <>
            <dt>
              <kbd>c</kbd>
            </dt>
            <dd>New task</dd>
          </>
        )}
        <dt>
          <kbd>g</kbd> then <kbd>b</kbd>
        </dt>
        <dd>Go to the board</dd>
      </dl>
      <p className="hint">Search works on the board, the others on a team&apos;s pages. None works while typing in a field.</p>
    </Dialog>
  );
}
