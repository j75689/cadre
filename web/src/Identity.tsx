import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { getMe, logout, type Me } from "./api";
import { isRole, type Role } from "./roles";
import { loadIdentity, saveIdentity } from "./identityStore";

// Who the UI acts as. With a login (--auth=oidc) the server knows: the UI
// shows the user and sends no author. Without one (--auth=none, for local
// development) the UI asks for a name once and sends it as the author: the
// pre-OIDC placeholder, kept for that mode.
interface IdentityState {
  mode: "loading" | "oidc" | "none" | "error";
  problem?: string;
  me?: Me;
  // refresh reads /me again, when a refusal says the roles changed.
  refresh: () => void;
  // name is shown; author is sent with actions ("" with a login).
  name: string;
  author: string;
  setName: (name: string) => void;
}

const IdentityContext = createContext<IdentityState>({ mode: "loading", name: "", author: "", setName: () => {}, refresh: () => {} });

export function IdentityProvider({ children }: { children: ReactNode }) {
  const [mode, setMode] = useState<IdentityState["mode"]>("loading");
  const [problem, setProblem] = useState("");
  const [user, setUser] = useState("");
  const [me, setMe] = useState<Me | undefined>(undefined);
  const [reads, setReads] = useState(0);
  const refresh = () => setReads((n) => n + 1);
  const [typed, setTyped] = useState(loadIdentity);
  useEffect(() => {
    const controller = new AbortController();
    getMe(controller.signal).then(
      (got) => {
        setMe(got);
        if (got.mode === "oidc") {
          setMode("oidc");
          setUser(got.name || got.username || "");
        } else {
          setMode("none");
        }
      },
      // A 401 is on its way to the login page already, unless the
      // session was just dropped.
      (e) => {
        if (controller.signal.aborted) return;
        setMode("error");
        setProblem((e as Error).message);
      },
    );
    return () => controller.abort();
  }, [reads]);
  const setName = (next: string) => {
    saveIdentity(next);
    setTyped(next);
  };
  const value: IdentityState =
    mode === "oidc"
      ? { mode, name: user, author: "", setName, me, refresh }
      : mode === "none"
        ? { mode, name: typed, author: typed, setName, refresh }
        : { mode, name: "", author: "", setName, problem, refresh };
  return <IdentityContext.Provider value={value}>{children}</IdentityContext.Provider>;
}

// useIdentity is who the UI acts as. ready says whether actions may be
// taken: logged in, or a name given.
export function useIdentity() {
  const { mode, name, author, refresh } = useContext(IdentityContext);
  return { mode, name, author, refresh, ready: mode === "oidc" || (mode === "none" && author !== "") };
}

// useIdentityDetails is the mode and /me, for showing roles.
export function useIdentityDetails() {
  const { mode, me } = useContext(IdentityContext);
  return { mode, me };
}

// useRole is the caller's role on the team in namespace ns. Without logins
// everyone is an admin; while it is not known yet, undefined.
export function useRole(ns: string): Role | undefined {
  const { mode, me } = useContext(IdentityContext);
  if (mode === "none") return "admin";
  if (mode !== "oidc" || !me) return undefined;
  const r = me.roles?.[ns];
  if (isRole(r)) return r;
  return me.admin ? "admin" : undefined;
}

// RoleLine says what the caller is on a team, with logins only.
export function RoleLine({ ns }: { ns: string }) {
  const { mode } = useContext(IdentityContext);
  const role = useRole(ns);
  if (mode !== "oidc") return null;
  return <p className="role hint">{role ? `You are ${role === "admin" ? "an" : "a"} ${role} of ${ns}.` : `You have no role on ${ns}.`}</p>;
}

// IdentityBox is the header's: the user and a logout with a login, a name
// box without one.
export function IdentityBox() {
  const { mode, name, problem } = useContext(IdentityContext);
  if (mode === "loading") return null;
  if (mode === "error") return <div className="identity error">Could not tell who you are{problem ? `: ${problem}` : "."}</div>;
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
