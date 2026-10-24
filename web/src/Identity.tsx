import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { getMe, logout } from "./api";
import { loadIdentity, saveIdentity } from "./identityStore";

// Who the UI acts as. With a login (--auth=oidc) the server knows: the UI
// shows the user and sends no author. Without one (--auth=none, for local
// development) the UI asks for a name once and sends it as the author: the
// pre-OIDC placeholder, kept for that mode.
interface IdentityState {
  mode: "loading" | "oidc" | "none" | "error";
  // name is shown; author is sent with actions ("" with a login).
  name: string;
  author: string;
  setName: (name: string) => void;
}

const IdentityContext = createContext<IdentityState>({ mode: "loading", name: "", author: "", setName: () => {} });

export function IdentityProvider({ children }: { children: ReactNode }) {
  const [mode, setMode] = useState<IdentityState["mode"]>("loading");
  const [user, setUser] = useState("");
  const [typed, setTyped] = useState(loadIdentity);
  useEffect(() => {
    const controller = new AbortController();
    getMe(controller.signal).then(
      (me) => {
        if (me.mode === "oidc") {
          setMode("oidc");
          setUser(me.name || me.username || "");
        } else {
          setMode("none");
        }
      },
      // A 401 is on its way to the login page already.
      () => {
        if (!controller.signal.aborted) setMode("error");
      },
    );
    return () => controller.abort();
  }, []);
  const setName = (next: string) => {
    saveIdentity(next);
    setTyped(next);
  };
  const value: IdentityState =
    mode === "oidc" ? { mode, name: user, author: "", setName } : mode === "none" ? { mode, name: typed, author: typed, setName } : { mode, name: "", author: "", setName };
  return <IdentityContext.Provider value={value}>{children}</IdentityContext.Provider>;
}

// useIdentity is who the UI acts as. ready says whether actions may be
// taken: logged in, or a name given.
export function useIdentity() {
  const { mode, name, author } = useContext(IdentityContext);
  return { mode, name, author, ready: mode === "oidc" || (mode === "none" && author !== "") };
}

// IdentityBox is the header's: the user and a logout with a login, a name
// box without one.
export function IdentityBox() {
  const { mode, name } = useContext(IdentityContext);
  if (mode === "loading") return null;
  if (mode === "error") return <div className="identity error">Could not tell who you are.</div>;
  if (mode === "oidc") {
    return (
      <div className="identity">
        <strong>{name}</strong>{" "}
        <button
          type="button"
          onClick={async () => {
            await logout();
            location.assign("/");
          }}
        >
          Log out
        </button>
      </div>
    );
  }
  return <NameBox />;
}

function NameBox() {
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
      <span className="hint">This server has no logins (--auth=none): the name is kept in this browser.</span>
    </form>
  );
}
