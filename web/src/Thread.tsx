import { useEffect, useRef } from "react";
import type { ThreadEntry } from "./api";
import { assigneeIcon } from "./ui/Badge";
import { Icon, type IconName } from "./ui/Icon";
import { Time } from "./ui/Time";

// What each entry kind is called on the page, its icon and its tone.
const kinds: Record<string, { label: string; icon: IconName; tone?: string }> = {
  Comment: { label: "comment", icon: "message-square" },
  Question: { label: "question", icon: "help-circle", tone: "tone-waiting" },
  Answer: { label: "answer", icon: "corner-down-right" },
  ToolCall: { label: "tool call", icon: "wrench" },
  Approved: { label: "approved", icon: "check", tone: "tone-done" },
  Denied: { label: "denied", icon: "x", tone: "tone-danger" },
  ToolResult: { label: "tool result", icon: "terminal" },
};

// Thread shows a task's entries in order, as a timeline. The entry the task
// waits on, if any, is marked, and scrolled into view once when it is first
// there: once for each wait, not again on every poll. Bodies are plain
// text: rendering markdown needs a dependency and sanitising, which the UI
// does not have.
export function Thread({ entries, waitingOn }: { entries: ThreadEntry[]; waitingOn?: number }) {
  const marked = useRef<HTMLLIElement>(null);
  const scrolledTo = useRef<number | undefined>(undefined);
  const hasMarked = entries.some((e) => e.seq === waitingOn);
  useEffect(() => {
    if (!hasMarked || scrolledTo.current === waitingOn) return;
    scrolledTo.current = waitingOn;
    marked.current?.scrollIntoView?.({ block: "center" });
  }, [hasMarked, waitingOn]);
  if (entries.length === 0) return <p className="hint">Nothing in the thread yet.</p>;
  return (
    <ol className="thread">
      {entries.map((e) => {
        const kind = kinds[e.kind] ?? { label: e.kind, icon: "message-square" as const };
        const waited = e.seq === waitingOn;
        return (
          <li
            key={e.seq}
            ref={waited ? marked : undefined}
            className={`entry entry-${e.kind.toLowerCase()} author-${e.author.kind.toLowerCase()}${waited ? " waiting-on" : ""}`}
            aria-current={waited ? "true" : undefined}
          >
            <span className="glyph" aria-hidden="true">
              <Icon name={assigneeIcon(e.author.kind)} />
            </span>
            <div className="entry-main">
              <div className="entry-head">
                <span className="author">
                  <span className="sr-only">{e.author.kind === "Agent" ? "agent " : "user "}</span>
                  {e.author.name}
                </span>
                <span className={`kind chip ${kind.tone ?? ""}`.trim()}>
                  <Icon name={kind.icon} size={12} />
                  <span className="kind-label">{kind.label}</span>
                </span>
                <Time at={e.at} />
                {waited && (
                  <span className="badge tone-waiting">
                    <Icon name="hourglass" size={12} />
                    Waiting on this
                  </span>
                )}
              </div>
              {e.call && <Call entry={e} />}
              {e.body && <pre className="entry-body">{e.body}</pre>}
            </div>
          </li>
        );
      })}
    </ol>
  );
}

// Call is the tool an entry is about. A tool call's arguments sit in a
// closed <details>, summarised by the tool's name.
function Call({ entry: e }: { entry: ThreadEntry }) {
  const call = e.call!;
  const name = call.name ?? call.tool ?? call.id;
  const failed = call.isError ? <span className="failed">failed</span> : null;
  if (e.kind === "ToolCall" && call.arguments !== undefined) {
    return (
      <details className="call">
        <summary>
          <Icon name="chevron-right" size={14} className="chev" />
          <code>{name}</code>
          {failed}
          <span className="sr-only">, arguments</span>
        </summary>
        <pre>{JSON.stringify(call.arguments, null, 2)}</pre>
      </details>
    );
  }
  return (
    <div className="call call-name">
      <code>{name}</code>
      {failed}
    </div>
  );
}
