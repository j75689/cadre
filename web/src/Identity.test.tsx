import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { IdentityBox } from "./Identity";
import { loadIdentity } from "./identityStore";

beforeEach(() => localStorage.clear());
afterEach(cleanup);

describe("IdentityBox", () => {
  it("asks once, then keeps the name across reloads", () => {
    render(<IdentityBox />);
    fireEvent.change(screen.getByLabelText(/your name/i), { target: { value: "  dana " } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(screen.getByText("dana")).toBeTruthy();
    expect(loadIdentity()).toBe("dana");

    cleanup();
    render(<IdentityBox />);
    expect(screen.getByText("dana")).toBeTruthy();
    expect(screen.queryByLabelText(/your name/i)).toBeNull();
  });

  it("refuses an empty name", () => {
    render(<IdentityBox />);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(loadIdentity()).toBe("");
    expect(screen.getByLabelText(/your name/i)).toBeTruthy();
  });
});
