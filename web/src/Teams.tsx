import { useState } from "react";
import { Link } from "react-router";
import { listTeams, type Team } from "./api";
import { usePolling } from "./usePolling";

export function Teams() {
  const [teams, setTeams] = useState<Team[] | null>(null);
  const [error, setError] = useState("");
  usePolling(
    async (signal) => {
      try {
        const res = await listTeams(signal);
        setTeams(res.items);
        setError("");
      } catch (e) {
        if (!signal.aborted) setError(String((e as Error).message));
      }
    },
    15000,
    [],
  );
  return (
    <main className="teams">
      <h1>Teams</h1>
      {error && <p className="error">Could not load the teams: {error}</p>}
      {teams === null && !error && <p>Loading…</p>}
      {teams?.length === 0 && <p>No Team yet. A namespace becomes a team when it has a Team resource.</p>}
      <ul>
        {teams?.map((t) => (
          <li key={t.metadata.namespace}>
            <Link to={`/t/${encodeURIComponent(t.metadata.namespace)}`}>{t.spec?.displayName || t.metadata.namespace}</Link>
            {t.spec?.displayName && <span className="ns"> {t.metadata.namespace}</span>}
          </li>
        ))}
      </ul>
    </main>
  );
}
