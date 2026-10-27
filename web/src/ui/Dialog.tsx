import { useEffect, useId, useRef, type ReactNode } from "react";
import { Button } from "./Button";

// Dialog is a modal: a native <dialog> opened with showModal(), so Escape
// closes it, the page behind is inert and it sits in the top layer. It is
// named by its h2. When it closes, the focus goes back to what had it when
// it opened.
export function Dialog({ open, onClose, title, children }: { open: boolean; onClose: () => void; title: string; children: ReactNode }) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  useEffect(() => {
    const dialog = ref.current;
    if (!open || !dialog) return;
    const opener = document.activeElement;
    dialog.showModal();
    return () => {
      if (dialog.open) dialog.close();
      if (opener instanceof HTMLElement) opener.focus();
    };
  }, [open]);
  return (
    <dialog ref={ref} className="dialog" aria-labelledby={titleId} onClose={onClose}>
      <div className="dialog-head">
        <h2 id={titleId}>{title}</h2>
        <Button variant="ghost" icon="x" aria-label="Close" onClick={onClose} />
      </div>
      {open && children}
    </dialog>
  );
}
