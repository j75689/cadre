import { useState } from "react";
import { Link, useParams } from "react-router";
import { APIError, getTeam, listArchive, type SnapshotSummary } from "./api";
import { RoleLine } from "./Identity";
import { archiveLink } from "./links";
import { useTeamName } from "./teamList";
import { PhaseBadge } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { Button } from "./ui/Button";
import { useFocusWhenGone } from "./ui/focus";
import { Heading, useTitle } from "./ui/Page";
import { Loading, Skeleton } from "./ui/Skeleton";
import { Time } from "./ui/Time";
import { usePolling } from "./usePolling";

// archivePage is one "load more" of the archive.
export const archivePage = 50;

// pollMs is how often the archive is read again: no hint names an archive.
const pollMs = 15000;

// Archive is the team's archived tasks, newest first, a page at a time:
// the read-only snapshots the server keeps of the finished tasks it took
// off the board after the Team's archiveAfter. An empty archive says
// whether archiving is on.
export function Archive() {
  const { ns = "" } = useParams();
  const name = useTeamName(ns);
  const [items, setItems] = useState<SnapshotSummary[] | null>(null);
  const [more, setMore] = useState(false);
  const [shown, setShown] = useState(1);
  // off is whether the Team sets no archiveAfter, read when the archive is
  // empty.
  const [off, setOff] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  // retry reads the archive again at once, after a failed read.
  const [retry, setRetry] = useState(0);
  useTitle("Archive", name);
  // The last page read takes Load more away: the focus goes to the table.
  const press = useFocusWhenGone();
  usePolling(
    async (signal) => {
      try {
        const all: SnapshotSummary[] = [];
        // A task archived between two page reads moves the rest down, so a
        // later page may repeat one: it is shown once, by its uid.
        const seen = new Set<string>();
        let full = false;
        for (let p = 0; p < shown; p++) {
          const res = await listArchive(ns, { offset: p * archivePage, limit: archivePage }, signal);
          for (const s of res.items) {
            if (seen.has(s.uid)) continue;
            seen.add(s.uid);
            all.push(s);
          }
          full = res.offset + res.items.length < res.total;
          if (!full) break;
        }
        if (all.length === 0) {
          // The Team is only for the empty state's hint: one not read
          // leaves it out.
          const team = await getTeam(ns, signal).catch(() => undefined);
          setOff(team !== undefined && !team.spec?.archiveAfter);
        }
        setItems(all);
        setMore(full);
        setError(null);
      } catch (e) {
        if (!signal.aborted) setError(e as Error);
      }
    },
    pollMs,
    [ns, shown, retry],
  );
  // A 403 is the caller's role: reading again cannot help.
  const forbidden = error instanceof APIError && error.status === 403;
  return (
    <main id="main" className="page">
      <div className="page-head">
        <Heading>Archive</Heading>
        <RoleLine ns={ns} />
      </div>
      <p className="hint">Finished tasks taken off the board, kept read-only for a while.</p>
      {error && (
        <Banner onRetry={forbidden ? undefined : () => setRetry((n) => n + 1)} className="page-banner">
          Could not load the archive: {error.message}
        </Banner>
      )}
      {items === null && !error && (
        <Loading what="the archive" className="card table-loading">
          {[0, 1, 2].map((i) => (
            <Skeleton key={i} width="60%" height={16} />
          ))}
        </Loading>
      )}
      {items?.length === 0 && (
        <div className="card empty">
          <p>No archived tasks.</p>
          {off && <p className="hint">Archiving is off for this team; an admin sets Archive after in Settings.</p>}
        </div>
      )}
      {items && items.length > 0 && (
        <div className="card table-wrap" tabIndex={0} role="region" aria-label="Archived tasks">
          <table className="table">
            <thead>
              <tr>
                <th scope="col">#</th>
                <th scope="col">Title</th>
                <th scope="col">Phase</th>
                <th scope="col">Assignee</th>
                <th scope="col">Finished</th>
                <th scope="col">Archived</th>
              </tr>
            </thead>
            <tbody>
              {items.map((s) => (
                <tr key={s.uid}>
                  <td className="mono">#{s.number}</td>
                  <td className="table-text">
                    <Link to={archiveLink(ns, s.uid)}>{s.title}</Link>
                  </td>
                  <td>
                    <PhaseBadge phase={s.phase} />
                  </td>
                  <td>{s.assignee?.name}</td>
                  <td>{s.finishedAt && <Time at={s.finishedAt} />}</td>
                  <td>
                    <Time at={s.archivedAt} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {more && !error && (
            <div className="card-foot">
              <Button
                variant="ghost"
                small
                className="btn-block"
                onClick={(e) => {
                  const table = e.currentTarget.closest<HTMLElement>(".table-wrap");
                  press(e.currentTarget, () => table);
                  setShown((p) => p + 1);
                }}
              >
                Load more
              </Button>
            </div>
          )}
        </div>
      )}
    </main>
  );
}
