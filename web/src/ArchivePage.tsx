import { useEffect, useState } from "react";
import { Link, useParams } from "react-router";
import { APIError, getSnapshot, getTeam, ulidTime, type RunEvent, type RunRecord, type Snapshot } from "./api";
import { RoleLine } from "./Identity";
import { teamPath } from "./links";
import { EventList, outcomeTone, totals } from "./Runs";
import { useTeamName } from "./teamList";
import { Thread } from "./Thread";
import { assigneeIcon, Badge, PhaseBadge, Pills, PriorityChip } from "./ui/Badge";
import { Banner } from "./ui/Banner";
import { Icon } from "./ui/Icon";
import { Heading, useTitle } from "./ui/Page";
import { Loading, Skeleton } from "./ui/Skeleton";
import { Time } from "./ui/Time";

// defaultRetention is how long the server keeps a snapshot when the Team
// sets no archiveRetention.
const defaultRetention = "90 days";

// ArchivePage shows one archived task, read-only: the snapshot the server
// kept of it, read once, as it never changes. It has no actions: the task
// is gone.
export function ArchivePage() {
  const { ns = "", uid = "" } = useParams();
  const team = useTeamName(ns);
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [missing, setMissing] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  // retention is the Team's archiveRetention, the default when it sets
  // none or cannot be read.
  const [retention, setRetention] = useState(defaultRetention);
  // retry reads the snapshot again at once, after a failed read.
  const [retry, setRetry] = useState(0);
  useEffect(() => {
    const controller = new AbortController();
    getSnapshot(ns, uid, controller.signal).then(
      (s) => {
        setSnapshot(s);
        setError(null);
      },
      (e) => {
        if (controller.signal.aborted) return;
        // A 400 is a uid no snapshot can have: it never existed either.
        if (e instanceof APIError && (e.status === 404 || e.status === 400)) {
          setMissing(true);
          setError(null);
        } else setError(e as Error);
      },
    );
    getTeam(ns, controller.signal).then(
      (t) => setRetention(t.spec?.archiveRetention || defaultRetention),
      () => {},
    );
    return () => controller.abort();
  }, [ns, uid, retry]);
  const list = `${teamPath(ns)}/archive`;
  useTitle(snapshot ? `#${snapshot.number} ${snapshot.title}` : undefined, "Archive", team);
  // A 403 is the caller's role: reading again cannot help.
  const forbidden = error instanceof APIError && error.status === 403;
  const spec = snapshot?.task.spec;
  return (
    <main id="main" className="task-page">
      <nav className="breadcrumb" aria-label="Breadcrumb">
        <ol>
          <li>
            <Link to={teamPath(ns)}>{team}</Link>
          </li>
          <li>
            <Link to={list}>Archive</Link>
          </li>
          <li aria-current="page" className="mono">
            {snapshot ? `#${snapshot.number}` : uid}
          </li>
        </ol>
      </nav>
      {error && (
        <Banner onRetry={forbidden ? undefined : () => setRetry((n) => n + 1)} className="page-banner">
          Could not load the archived task: {error.message}
        </Banner>
      )}
      {missing && (
        <div className="card empty">
          <p>This archived task is gone or never existed.</p>
          <p>
            <Link to={list}>All archived tasks</Link>
          </p>
        </div>
      )}
      {!snapshot && !missing && !error && (
        <Loading what="the archived task" className="card card-pad stack">
          <Skeleton width="50%" height={24} />
          <Skeleton width="80%" />
          <Skeleton width="60%" />
        </Loading>
      )}
      {snapshot && spec && (
        <>
          <Banner icon="archive" className="banner-warning page-banner archive-banner">
            Archived <Time at={snapshot.archivedAt} />; read-only, removed {retention} after the archive.
          </Banner>
          <div className="task-head">
            <div className="title-block">
              <Heading>
                <span className="mono num">#{snapshot.number}</span> <span>{snapshot.title}</span>
              </Heading>
              <div className="badges">
                <PhaseBadge phase={snapshot.phase} />
                <PriorityChip priority={spec.priority} always />
                {spec.labels && spec.labels.length > 0 && <Pills labels={spec.labels} />}
                <span className="meta">
                  {spec.assignee ? (
                    <>
                      <Icon name={assigneeIcon(spec.assignee.kind)} size={14} />
                      <span>
                        <span className="sr-only">{spec.assignee.kind === "Agent" ? "agent " : "user "}</span>
                        {spec.assignee.name}
                      </span>
                    </>
                  ) : (
                    <span className="muted">unassigned</span>
                  )}
                </span>
              </div>
              <RoleLine ns={ns} />
            </div>
          </div>
          <div className="task-layout">
            <div className="stack">
              {spec.description && (
                <section className="card" aria-labelledby="description">
                  <div className="card-head">
                    <h2 id="description">Description</h2>
                  </div>
                  {/* Plain text: markdown needs a renderer and sanitising. */}
                  <pre className="card-body prose">{spec.description}</pre>
                </section>
              )}
              <section className="card" aria-labelledby="thread">
                <div className="card-head">
                  <h2 id="thread">Thread</h2>
                  <span className="count-pill">{snapshot.thread.length}</span>
                </div>
                <div className="card-body">
                  <Thread entries={snapshot.thread} />
                </div>
              </section>
              <Files files={snapshot.files} />
            </div>
            <aside className="stack" aria-label="About the task">
              <ArchiveRuns snapshot={snapshot} />
            </aside>
          </div>
        </>
      )}
    </main>
  );
}

// ArchiveRuns lists an archived task's runs, each with its events and
// tokens, and a step-mode run's transcript. All of it is in the snapshot:
// nothing is read, and a run has no link, as the task has no page.
function ArchiveRuns({ snapshot }: { snapshot: Snapshot }) {
  return (
    <section className="card runs" aria-labelledby="runs">
      <div className="card-head">
        <h2 id="runs">Runs</h2>
        <span className="count-pill">{snapshot.runs.length}</span>
      </div>
      {snapshot.runs.length === 0 && <p className="card-body hint">No run ended.</p>}
      {snapshot.runs.length > 0 && (
        <ol className="rows">
          {snapshot.runs.map((r) => (
            <ArchiveRun key={r.id} run={r} events={snapshot.events[r.id] ?? []} transcript={snapshot.transcripts[r.id]} />
          ))}
        </ol>
      )}
    </section>
  );
}

function ArchiveRun({ run, events, transcript }: { run: RunRecord; events: RunEvent[]; transcript?: unknown[] }) {
  const started = ulidTime(run.id);
  const t = totals(events);
  const n = (v: number) => v.toLocaleString();
  return (
    <li className="archive-run">
      <code title={run.id}>{run.id}</code>
      <span className="line">
        <span>
          {run.mode} on {run.runnerName || run.runner || "a runner"}: {run.reason}
        </span>
        {run.outcome && <Badge tone={outcomeTone(run.outcome)}>{run.outcome}</Badge>}
      </span>
      <span className="run-times">
        {started && <>started {started.toLocaleString()}, </>}ended {new Date(run.endedAt).toLocaleString()}
      </span>
      {events.length > 0 && (
        <span className="run-times">
          {n(t.inputTokens)} in · {n(t.outputTokens)} out
          {t.contextTokens > 0 && <> · context {n(t.contextTokens)}</>}
          {Boolean(t.cacheReadInputTokens || t.cacheCreationInputTokens) && (
            <>
              {" "}
              · cache {n(t.cacheReadInputTokens)} read, {n(t.cacheCreationInputTokens)} written
            </>
          )}
        </span>
      )}
      <div className="events">
        {events.length === 0 ? <p className="hint">No events.</p> : <EventList events={events} />}
        {transcript && <Transcript steps={transcript} />}
      </div>
    </li>
  );
}

// A Step is a transcript's committed step, as the server's TranscriptEntry
// stores it; only what the page shows is named.
interface Step {
  step?: number;
  kind?: string;
  at?: string;
  model?: string;
  text?: string;
  calls?: { name?: string }[];
}

// Transcript is a step-mode run's steps, collapsed: they are rendered
// only once opened, as a long run's are large.
function Transcript({ steps }: { steps: unknown[] }) {
  const [open, setOpen] = useState(false);
  return (
    <details className="call" onToggle={(e) => setOpen(e.currentTarget.open)}>
      <summary>
        <Icon name="chevron-right" size={14} className="chev" />
        Transcript ({steps.length} {steps.length === 1 ? "step" : "steps"})
      </summary>
      {open && (
        <ol className="transcript">
          {steps.map((raw, i) => {
            const s = (raw ?? {}) as Step;
            const calls = (s.calls ?? []).map((c) => c.name).filter(Boolean);
            return (
              <li key={i}>
                <span className="line">
                  <span className="mono">#{s.step ?? i}</span> <span>{s.kind}</span> {s.at && <Time at={s.at} />}
                </span>
                {s.model && <span className="sub">model {s.model}</span>}
                {s.text && <p className="transcript-text">{s.text}</p>}
                {calls.length > 0 && <span className="sub">calls {calls.join(", ")}</span>}
                <details className="call">
                  <summary>
                    <Icon name="chevron-right" size={14} className="chev" />
                    JSON of step {s.step ?? i}
                  </summary>
                  <pre>{JSON.stringify(raw, null, 2)}</pre>
                </details>
              </li>
            );
          })}
        </ol>
      )}
    </details>
  );
}

// Files are the archived task's files, the latest version of each, each
// collapsed.
function Files({ files }: { files: Snapshot["files"] }) {
  return (
    <section className="card" aria-labelledby="files">
      <div className="card-head">
        <h2 id="files">Files</h2>
        <span className="count-pill">{files.length}</span>
      </div>
      <div className="card-body">
        {files.length === 0 && <p className="hint">No files.</p>}
        {files.map((f) => (
          <details key={f.path} className="call">
            <summary>
              <Icon name="chevron-right" size={14} className="chev" />
              <span className="mono">{f.path}</span>
              <span className="muted">
                version {f.version}, {f.size.toLocaleString()} bytes
              </span>
            </summary>
            <pre>{f.content}</pre>
          </details>
        ))}
      </div>
    </section>
  );
}
