import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { getMe, logout, type Me } from "./api";
import { isRole, type Role } from "./roles";
import { loadIdentity, saveIdentity } from "./identityStore";
import { Button } from "./ui/Button";
import { Icon } from "./ui/Icon";

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
        ? { mode, name: typed, author: typed, setName, me, refresh }
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

// roleOf is the caller's role on the team in namespace ns, as /me says:
// an admin everywhere for a bootstrap admin, and without logins for
// everyone. While /me is not read, or for a role it does not know,
// undefined.
export function roleOf(me: Me | undefined, ns: string): Role | undefined {
  if (!me) return undefined;
  const r = me.roles?.[ns];
  if (isRole(r)) return r;
  return me.admin ? "admin" : undefined;
}

// useRole is roleOf for the identity the UI has.
export function useRole(ns: string): Role | undefined {
  const { me } = useContext(IdentityContext);
  return roleOf(me, ns);
}

// RoleLine says what the caller is on a team, with logins only.
export function RoleLine({ ns }: { ns: string }) {
  const { mode } = useContext(IdentityContext);
  const role = useRole(ns);
  if (mode !== "oidc") return null;
  return <p className="role hint">{role ? `You are ${role === "admin" ? "an" : "a"} ${role} of ${ns}.` : `You have no role on ${ns}.`}</p>;
}

// IdentityBox is the top bar's: the user and a logout with a login, the
// acting name and "change" without one, and the name form while none is
// set.
export function IdentityBox() {
  const { mode, name, problem } = useContext(IdentityContext);
  if (mode === "loading") return null;
  if (mode === "error") {
    // Truncated in a narrow bar; the whole of it in the title.
    const text = `Could not tell who you are${problem ? `: ${problem}` : "."}`;
    return (
      <div className="identity error" title={text}>
        {text}
      </div>
    );
  }
  if (mode === "oidc") {
    return (
      <div className="identity">
        <Icon name="user" />
        <strong>{name}</strong>
        <Button
          variant="ghost"
          small
          icon="log-out"
          aria-label="Log out"
          onClick={async () => {
            await logout();
            location.assign("/");
          }}
        >
          <span className="logout-label" aria-hidden="true">
            Log out
          </span>
        </Button>
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
        <span className="acting muted">acting as</span>
        <strong>{name}</strong>
        <Button
          variant="ghost"
          small
          onClick={() => {
            setDraft(name);
            setEditing(true);
          }}
        >
          change
        </Button>
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
        <span className="sr-only">Your name</span>
        <input
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          placeholder="Your name"
          title="This server has no logins (--auth=none): the name is kept in this browser."
          aria-describedby="name-hint"
          autoFocus
        />
      </label>
      <span id="name-hint" className="sr-only">
        This server has no logins (--auth=none): the name is kept in this browser.
      </span>
      <Button type="submit" small variant="primary">
        Save
      </Button>
    </form>
  );
}
