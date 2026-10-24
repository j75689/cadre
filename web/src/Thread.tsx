import type { ThreadEntry } from "./api";

// What each entry kind is called on the page.
const kindLabels: Record<string, string> = {
  Comment: "comment",
  Question: "question",
  Answer: "answer",
  ToolCall: "tool call",
  Approved: "approved",
  Denied: "denied",
  ToolResult: "tool result",
};

// Thread shows a task's entries in order. The entry the task waits on, if
// any, is marked. Bodies are plain text: rendering markdown needs a
// dependency and sanitising, which the UI does not have yet.
export function Thread({ entries, waitingOn }: { entries: ThreadEntry[]; waitingOn?: number }) {
  if (entries.length === 0) return <p className="hint">Nothing in the thread yet.</p>;
  return (
    <ol className="thread">
      {entries.map((e) => (
        <li
          key={e.seq}
          className={`entry entry-${e.kind.toLowerCase()} author-${e.author.kind.toLowerCase()}${e.seq === waitingOn ? " waiting-on" : ""}`}
          aria-current={e.seq === waitingOn ? "true" : undefined}
        >
          <div className="entry-head">
            <span className="kind">{kindLabels[e.kind] ?? e.kind}</span>{" "}
            <span className="author">
              {e.author.kind === "Agent" ? "agent " : ""}
              {e.author.name}
            </span>{" "}
            <time dateTime={e.at}>{new Date(e.at).toLocaleString()}</time>
            {e.seq === waitingOn && <span className="waiting-badge"> waiting on this</span>}
          </div>
          {e.call && (
            <div className="call">
              <code>{e.call.name ?? e.call.tool}</code>
              {e.call.isError && <span className="error"> failed</span>}
              {e.call.arguments !== undefined && e.kind === "ToolCall" && <pre>{JSON.stringify(e.call.arguments, null, 2)}</pre>}
            </div>
          )}
          {e.body && <pre className="body">{e.body}</pre>}
        </li>
      ))}
    </ol>
  );
}
