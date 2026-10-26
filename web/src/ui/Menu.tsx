import { useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { buttonClass } from "./Button";
import { Icon } from "./Icon";

// MenuNote is a line of the menu that is not a choice, a loading or an
// error note: a disabled item, so that assistive technology reads it.
export function MenuNote({ children }: { children: ReactNode }) {
  return (
    <li role="none">
      <span role="menuitem" aria-disabled="true" tabIndex={-1} className="menu-item menu-note">
        {children}
      </span>
    </li>
  );
}

// Menu is a button that opens a list of links: the menu-button pattern.
// Arrow keys, Home and End move between the items, Escape closes it and
// gives the button its focus back, and a click outside or Tab away closes
// it, as choosing an item does.
export function Menu({
  label,
  buttonLabel,
  items,
}: {
  // label is the button's content; buttonLabel its accessible name.
  label: ReactNode;
  buttonLabel: string;
  items: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  const list = useRef<HTMLUListElement>(null);
  const id = useId();

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    // The first item takes focus once the list is there.
    list.current?.querySelector<HTMLElement>('[role="menuitem"]')?.focus();
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  const close = (refocus = true) => {
    setOpen(false);
    if (refocus) button.current?.focus();
  };

  const onKey = (e: KeyboardEvent) => {
    const all = [...(list.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? [])];
    const at = all.indexOf(document.activeElement as HTMLElement);
    const go = (i: number) => all[(i + all.length) % all.length]?.focus();
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        go(at + 1);
        break;
      case "ArrowUp":
        e.preventDefault();
        go(at - 1);
        break;
      case "Home":
        e.preventDefault();
        go(0);
        break;
      case "End":
        e.preventDefault();
        go(all.length - 1);
        break;
      case "Escape":
        e.preventDefault();
        close();
        break;
      case "Tab":
        close(false);
        break;
    }
  };

  return (
    <div className="menu" ref={root}>
      <button
        ref={button}
        type="button"
        className={buttonClass("ghost", false, false, "switcher")}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? id : undefined}
        aria-label={buttonLabel}
        onClick={() => setOpen((o) => !o)}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown" && !open) {
            e.preventDefault();
            setOpen(true);
          }
        }}
      >
        {label}
        <Icon name="chevron-down" />
      </button>
      {open && (
        <ul
          ref={list}
          className="menu-list"
          role="menu"
          id={id}
          aria-label={buttonLabel}
          onKeyDown={onKey}
          onClick={(e) => {
            if ((e.target as Element).closest('[role="menuitem"]:not([aria-disabled="true"])')) setOpen(false);
          }}
        >
          {items}
        </ul>
      )}
    </div>
  );
}
