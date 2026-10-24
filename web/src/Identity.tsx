import { useState } from "react";
import { loadIdentity, saveIdentity } from "./identityStore";

// IdentityBox asks once for the name to act as and keeps it in this
// browser. Pre-OIDC placeholder: replaced by the signed-in user in S4.
export function IdentityBox() {
  const [name, setName] = useState(loadIdentity);
  const [draft, setDraft] = useState("");
  const [editing, setEditing] = useState(name === "");
  if (!editing) {
    return (
      <div className="identity">
        acting as <strong>{name}</strong>{" "}
        <button type="button" onClick={() => { setDraft(name); setEditing(true); }}>
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
        saveIdentity(next);
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
