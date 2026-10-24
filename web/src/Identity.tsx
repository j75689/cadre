import { createContext, useContext, useState, type ReactNode } from "react";
import { loadIdentity, saveIdentity } from "./identityStore";

// The name the UI acts as: the author of every action. Pre-OIDC
// placeholder: replaced by the signed-in user in S4.
const IdentityContext = createContext<{ name: string; setName: (name: string) => void }>({ name: "", setName: () => {} });

export function IdentityProvider({ children }: { children: ReactNode }) {
  const [name, setStored] = useState(loadIdentity);
  const setName = (next: string) => {
    saveIdentity(next);
    setStored(next);
  };
  return <IdentityContext.Provider value={{ name, setName }}>{children}</IdentityContext.Provider>;
}

// useIdentity is the name to act as, "" until one is given.
export const useIdentity = () => useContext(IdentityContext).name;

// IdentityBox asks once for the name to act as and keeps it in this
// browser.
export function IdentityBox() {
  const { name, setName } = useContext(IdentityContext);
  const [draft, setDraft] = useState("");
  const [editing, setEditing] = useState(name === "");
  if (!editing) {
    return (
      <div className="identity">
        acting as <strong>{name}</strong>{" "}
        <button
          type="button"
          onClick={() => {
            setDraft(name);
            setEditing(true);
          }}
        >
          change
        </button>
      </div>
    );
  }
  return (
    <form
      className="identity"
      onSubmit={(e) => {
        e.preventDefault();
        const next = draft.trim();
        if (next === "") return;
        setName(next);
        setEditing(false);
      }}
    >
      <label>
        Your name <input value={draft} onChange={(e) => setDraft(e.target.value)} placeholder="shown on what you do" autoFocus />
      </label>
      <button type="submit">Save</button>
      <span className="hint">Kept in this browser until sign-in arrives.</span>
    </form>
  );
}
