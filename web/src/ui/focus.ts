import { useEffect, useRef } from "react";

// useFocusWhenGone is for a button that may go away once pressed, such as
// the last "Load more": press(button, then) records it, and once it has gone
// while it held the focus, the focus goes to what then() returns rather
// than to the body. A button that stays keeps the focus.
export function useFocusWhenGone() {
  const pressed = useRef<{ button: HTMLElement; then: () => HTMLElement | null | undefined } | undefined>(undefined);
  useEffect(() => {
    if (!pressed.current) return;
    const { button, then } = pressed.current;
    if (button.isConnected && document.activeElement === button) return;
    pressed.current = undefined;
    if (!button.isConnected && document.activeElement === document.body) then()?.focus();
  });
  return (button: HTMLElement, then: () => HTMLElement | null | undefined) => {
    pressed.current = { button, then };
  };
}
