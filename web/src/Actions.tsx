import { useState, type ReactNode } from "react";
import { APIError, approve, cancel, comment, reply, terminalPhases, type Task, type ThreadEntry } from "./api";
import { useIdentity } from "./Identity";

// What a task in this state lets a human do, as the API decides it: reply
// while it waits on a human (a question or a review), approve or deny while
// it waits on a tool approval, comment and cancel until it ends.
export function allowed(task: Task) {
  const phase = task.status?.phase ?? "Todo";
  const waiting = phase === "Waiting" ? task.status?.waiting : undefined;
  const open = !terminalPhases.includes(phase);
  return {
    comment: open,
    reply: waiting !== undefined && waiting.reason !== "ToolApproval",
    approve: waiting?.reason === "ToolApproval",
    cancel: open,
  };
}

interface Props {
  task: Task;
  onTask: (task: Task) => void;
  onEntries: (entries: ThreadEntry[]) => void;
  // onConflict asks for a fresh read: the task moved on.
  onConflict: () => void;
}

export function Actions({ task, onTask, onEntries, onConflict }: Props) {
  const can = allowed(task);
  const { namespace: ns, name } = task.metadata;
  const author = useIdentity();
  if (!can.comment && !can.reply && !can.approve && !can.cancel) return null;
  const run = async <T,>(call: () => Promise<T>, done: (r: T) => void): Promise<string> => {
    try {
      done(await call());
      return "";
    } catch (e) {
      if (e instanceof APIError && e.status === 409) onConflict();
      return (e as Error).message;
    }
  };
  return (
    <section className="actions" aria-label="Actions">
      {author === "" && <p className="hint">Give your name at the top to act on this task.</p>}
      {can.reply && (
        <TextAction
          label="Reply"
          placeholder={task.status?.waiting?.reason === "Review" ? "Your review" : "Your answer"}
          disabled={author === ""}
          onSubmit={(body) =>
            run(
              () => reply(ns, name, author, body),
              (r) => {
                onTask(r.task);
                onEntries([r.entry]);
              },
            )
          }
        />
      )}
      {can.approve && <Approval disabled={author === ""} onDecide={(ok, note) => run(() => approve(ns, name, author, ok, note), (r) => {
        onTask(r.task);
        onEntries(r.result ? [r.decision, r.result] : [r.decision]);
      })} />}
      {can.comment && (
        <TextAction label="Comment" placeholder="Add to the thread" disabled={author === ""} onSubmit={(body) => run(() => comment(ns, name, author, body), (e) => onEntries([e]))} />
      )}
      {can.cancel && <Cancel disabled={author === ""} onCancel={() => run(() => cancel(ns, name), onTask)} />}
    </section>
  );
}

// ActionError shows the server's message next to what caused it.
function ActionError({ message }: { message: string }) {
  return message ? (
    <p className="error" role="alert">
      {message}
    </p>
  ) : null;
}

function TextAction(props: { label: string; placeholder: string; disabled: boolean; onSubmit: (body: string) => Promise<string> }) {
  const [body, setBody] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  return (
    <form
      className="action"
      onSubmit={async (e) => {
        e.preventDefault();
        if (body.trim() === "") return;
        setBusy(true);
        const err = await props.onSubmit(body);
        setBusy(false);
        setError(err);
        if (err === "") setBody("");
      }}
    >
      <label>
        {props.label}
        <textarea value={body} onChange={(e) => setBody(e.target.value)} placeholder={props.placeholder} rows={3} disabled={props.disabled} />
      </label>
      <button type="submit" disabled={props.disabled || busy || body.trim() === ""}>
        {props.label}
      </button>
      <ActionError message={error} />
    </form>
  );
}

function Approval(props: { disabled: boolean; onDecide: (approve: boolean, note: string) => Promise<string> }) {
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const decide = async (ok: boolean) => {
    setBusy(true);
    setError(await props.onDecide(ok, note));
    setBusy(false);
  };
  return (
    <div className="action approval">
      <label>
        Tool call note <input value={note} onChange={(e) => setNote(e.target.value)} placeholder="optional" disabled={props.disabled} />
      </label>
      <button type="button" disabled={props.disabled || busy} onClick={() => decide(true)}>
        Approve
      </button>
      <button type="button" disabled={props.disabled || busy} onClick={() => decide(false)}>
        Reject
      </button>
      <ActionError message={error} />
    </div>
  );
}

function Cancel(props: { disabled: boolean; onCancel: () => Promise<string> }) {
  const [confirming, setConfirming] = useState(false);
  const [error, setError] = useState("");
  let content: ReactNode;
  if (!confirming) {
    content = (
      <button type="button" disabled={props.disabled} onClick={() => setConfirming(true)}>
        Cancel task
      </button>
    );
  } else {
    content = (
      <>
        <span>Cancel this task? A run in progress is stopped.</span>{" "}
        <button
          type="button"
          onClick={async () => {
            const err = await props.onCancel();
            setError(err);
            setConfirming(false);
          }}
        >
          Yes, cancel it
        </button>{" "}
        <button type="button" onClick={() => setConfirming(false)}>
          Keep it
        </button>
      </>
    );
  }
  return (
    <div className="action cancel">
      {content}
      <ActionError message={error} />
    </div>
  );
}
