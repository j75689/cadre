import { createContext, useContext, useState, type ReactNode } from "react";
import { listTeams, type Team } from "./api";
import { usePolling } from "./usePolling";

// The teams the caller may see, from GET /teams, read for the whole app:
// the top bar's switcher, the Teams page and each page's team name.
interface TeamsState {
  // teams is null until the first read.
  teams: Team[] | null;
  error: string;
  retry: () => void;
}

const TeamsContext = createContext<TeamsState>({ teams: null, error: "", retry: () => {} });

export function TeamsProvider({ children }: { children: ReactNode }) {
  const [teams, setTeams] = useState<Team[] | null>(null);
  const [error, setError] = useState("");
  const [reads, setReads] = useState(0);
  usePolling(
    async (signal) => {
      try {
        const res = await listTeams(signal);
        setTeams(res.items ?? []);
        setError("");
      } catch (e) {
        if (!signal.aborted) setError(String((e as Error).message));
      }
    },
    15000,
    [reads],
  );
  return <TeamsContext.Provider value={{ teams, error, retry: () => setReads((n) => n + 1) }}>{children}</TeamsContext.Provider>;
}

export const useTeams = () => useContext(TeamsContext);

// teamName is what a team is called: its display name, else its namespace.
export const teamName = (t: Team) => t.spec?.displayName || t.metadata.namespace;

// useTeamName is the name of the team in namespace ns, its namespace until
// the teams are read.
export function useTeamName(ns: string): string {
  const { teams } = useTeams();
  const t = teams?.find((t) => t.metadata.namespace === ns);
  return t ? teamName(t) : ns;
}
