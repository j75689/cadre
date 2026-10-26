import { useId, useRef, useState } from "react";
import { APIError, approve, cancel, comment, reply, terminalPhases, type Task } from "./api";
import { useIdentity, useRole } from "./Identity";
import { can as canDo } from "./roles";
import { Button } from "./ui/Button";
import { Icon } from "./ui/Icon";

// What a task in this state lets a human do, as the API decides it: reply
// while it waits on a human (a question or a review), approve or deny while
// it waits on a tool approval, comment and cancel until it ends.
export function allowed(task: Task, mayWork = true) {
  const phase = task.status?.phase ?? "Todo";
  const waiting = phase === "Waiting" ? task.status?.waiting : undefined;
  const open = mayWork && !terminalPhases.includes(phase);
  return {
    comment: open,
    reply: open && waiting !== undefined && waiting.reason !== "ToolApproval",
    approve: open && waiting?.reason === "ToolApproval",
    cancel: open,
  };
}

interface Props {
  task: Task;
  onTask: (task: Task) => void;
  // onRefresh asks for the task and the thread to be read again at once:
  // after an action, which wrote to the thread, or a conflict, when the
  // task moved on. The thread is only ever read in order, so what others
  // wrote meanwhile is not skipped.
  onRefresh: () => void;
  // waitingTool names the tool call the task waits to have approved, from
  // the thread, once it is read.
  waitingTool?: string;
}

// Outcome is what run says of a call: "" when it was made, the server's
// error when it was not, or undefined when it was refused for the role,
// which Alert shows once, in its own place, and not under the form too.
type Outcome = string | undefined;

// useActions is what the caller may do to the task and how: run calls the
// API, then reads the task again.
function useActions({ task, onRefresh }: Props) {
  const role = useRole(task.metadata.namespace);
  // What the task's state allows, of what the caller's role does.
  const can = allowed(task, canDo(role, "work"));
  const identity = useIdentity();
  // A refusal outlives the actions it hides: the role is read again and
  // they may go, but the reason stays.
  const [refused, setRefused] = useState("");
  const run = async <T,>(call: () => Promise<T>, done: (r: T) => void): Promise<Outcome> => {
    try {
      done(await call());
      onRefresh();
      return "";
    } catch (e) {
      if (e instanceof APIError && e.status === 409) onRefresh();
      // Refused for the role: it changed since the page read it.
      if (e instanceof APIError && e.code === "no_role") {
        setRefused((e as Error).message);
        identity.refresh();
        return undefined;
      }
      return (e as Error).message;
    }
  };
  return { can, run, refused, ...identity };
}

// Alert is an error from the server, next to what caused it. A component
// that shows a refusal keeps its Alert in one place in its tree whatever
// else it shows, so the alert stays the same element, said once.
function Alert({ message }: { message: string }) {
  return message ? (
    <p className="inline-error" role="alert">
      {message}
    </p>
  ) : null;
}

// waitOf is what identifies the wait a task is in: when it started, which
// stays for the wait (its entry is filled in a poll later), or else the
// entry, or the outbox item writing it; "" while it waits on nothing.
function waitOf(task: Task): string {
  const w = task.status?.phase === "Waiting" ? task.status.waiting : undefined;
  if (!w) return "";
  return `${w.reason}/${w.since ?? w.threadSeq ?? w.itemId ?? ""}`;
}

// Carried is what one wait's drafts hand the next: the text typed and
// whether it was a comment. A note never passes on.
interface Carried {
  body: string;
  commenting: boolean;
}

// carryOver is how a new wait starts from the last one's drafts. A reply
// belongs to the question it answers and goes with it; a comment survives:
// typed before a question arrived, it stays a comment, and typed under
// "Comment instead", it stays one for the next wait. The same wait again
// (the composer closed and opened on it) keeps all of it.
export function carryOver(from: string, to: string, last: Carried): Carried {
  if (from === to) return last;
  if (from === "" || to === "") return { body: last.body, commenting: to !== "" && last.body !== "" };
  return last.commenting ? last : { body: "", commenting: false };
}

// Composer is the one card at the end of the thread: a Reply form while the
// task waits on a question or a review, an approval card while it waits on
// a tool approval, a Comment form otherwise. "Comment instead" turns a
// reply or an approval into a comment, which the API takes in every phase
// until the task ends, and back. Drafts holds what is typed for one wait.
export function Composer(props: Props) {
  const actions = useActions(props);
  const { can, refused, mode, ready } = actions;
  const wait = waitOf(props.task);
  // The carry is always the current wait and its drafts: a Drafts writes
  // its start into it as it mounts, and its changes after. A Drafts whose
  // wait is gone (a form clearing after the task moved on) writes nothing.
  const carriedRef = useRef<{ wait: string; last: Carried }>({ wait, last: { body: "", commenting: false } });
  const open = can.comment || can.reply || can.approve;
  // The card stays while it has a refusal to show, its alert in the same
  // place whether the forms are there or not.
  if (!open && !refused) return null;
  return (
    <section className="card card-pad composer" aria-label="Actions">
      {open && mode === "none" && !ready && <p className="hint">Give your name at the top to act on this task.</p>}
      {open && (
        <Drafts
          key={wait}
          {...props}
          actions={actions}
          start={() => {
            const last = carryOver(carriedRef.current.wait, wait, carriedRef.current.last);
            carriedRef.current = { wait, last };
            return last;
          }}
          onDrafts={(last) => {
            if (carriedRef.current.wait === wait) carriedRef.current = { wait, last };
          }}
        />
      )}
      <Alert message={refused} />
    </section>
  );
}

function Drafts(
  props: Props & {
    actions: ReturnType<typeof useActions>;
    // start is what this wait's drafts start from; onDrafts is told what
    // they hold, for the next.
    start: () => Carried;
    onDrafts: (last: Carried) => void;
  },
) {
  const { task, onTask, actions, onDrafts } = props;
  const { can, run, author, ready } = actions;
  const [start] = useState(props.start);
  const [commenting, setCommentingState] = useState(start.commenting);
  const [body, setBodyState] = useState(start.body);
  const [note, setNote] = useState("");
  // The drafts as they are now, whichever render a setter came from (a
  // form clears itself after awaiting its call), each change told at once.
  const latestRef = useRef(start);
  const change = (patch: Partial<Carried>) => {
    latestRef.current = { ...latestRef.current, ...patch };
    onDrafts(latestRef.current);
  };
  const setBody = (next: string) => {
    setBodyState(next);
    change({ body: next });
  };
  const setCommenting = (next: boolean) => {
    setCommentingState(next);
    change({ commenting: next });
  };
  const { namespace: ns, name } = task.metadata;
  const waitingFor = task.status?.phase === "Waiting" ? task.status.waiting?.reason : undefined;
  const primary = can.reply ? "reply" : can.approve ? "approve" : "comment";
  const showing = commenting && can.comment ? "comment" : primary;
  return (
    <>
      {primary !== "comment" && can.comment && (
        <div className="composer-switch">
          <Button variant="ghost" small icon="message-square" aria-pressed={showing === "comment"} onClick={() => setCommenting(!commenting)}>
            Comment instead
          </Button>
        </div>
      )}
      {showing === "reply" && (
        <TextAction
          label="Reply"
          hint={waitingFor === "Review" ? "The task waits for your review." : "The task waits for an answer to its question."}
          placeholder={waitingFor === "Review" ? "Your review" : "Your answer"}
          disabled={!ready}
          body={body}
          onBody={setBody}
          onSubmit={(text) =>
            run(
              () => reply(ns, name, author, text),
              (r) => {
                // Sent: nothing of it passes to what comes next. The form
                // clears only after its call returns, which may be after
                // the next wait's drafts mounted, when it no longer writes.
                onDrafts({ body: "", commenting: false });
                onTask(r.task);
              },
            )
          }
        />
      )}
      {showing === "approve" && (
        <Approval
          tool={props.waitingTool}
          disabled={!ready}
          note={note}
          onNote={setNote}
          onDecide={(ok) =>
            run(
              () => approve(ns, name, author, ok, note),
              (r) => onTask(r.task),
            )
          }
        />
      )}
      {showing === "comment" && (
        <TextAction
          label="Comment"
          placeholder="Add to the thread"
          disabled={!ready}
          body={body}
          onBody={setBody}
          onSubmit={(text) => run(() => comment(ns, name, author, text), () => {})}
        />
      )}
    </>
  );
}

// TextAction is a form of one text box. Its text, and its error, are the
// wait's: they go with it.
function TextAction(props: {
  label: string;
  hint?: string;
  placeholder: string;
  disabled: boolean;
  body: string;
  onBody: (body: string) => void;
  onSubmit: (body: string) => Promise<Outcome>;
}) {
  const { body, onBody } = props;
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const id = useId();
  return (
    <form
      onSubmit={async (e) => {
        e.preventDefault();
        if (body.trim() === "") return;
        setBusy(true);
        const err = await props.onSubmit(body);
        setBusy(false);
        setError(err ?? "");
        if (err === "") onBody("");
      }}
    >
      <div className="field">
        <label htmlFor={`${id}-text`}>{props.label}</label>
        {props.hint && (
          <p className="hint" id={`${id}-hint`}>
            {props.hint}
          </p>
        )}
        <textarea
          id={`${id}-text`}
          aria-describedby={props.hint ? `${id}-hint` : undefined}
          value={body}
          onChange={(e) => onBody(e.target.value)}
          placeholder={props.placeholder}
          rows={3}
          disabled={props.disabled}
        />
      </div>
      <div className="buttons">
        <Button type="submit" variant="primary" disabled={props.disabled || busy || body.trim() === ""}>
          {props.label}
        </Button>
      </div>
      <Alert message={error} />
    </form>
  );
}

function Approval(props: { tool?: string; disabled: boolean; note: string; onNote: (note: string) => void; onDecide: (approve: boolean) => Promise<Outcome> }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const decide = async (ok: boolean) => {
    setBusy(true);
    setError((await props.onDecide(ok)) ?? "");
    setBusy(false);
  };
  return (
    <div className="approval">
      <p className="call-name">
        <Icon name="wrench" />
        <span>
          The task waits for approval of {props.tool ? <code>{props.tool}</code> : "a tool call"}, marked in the thread.
        </span>
      </p>
      <label>
        Tool call note <input value={props.note} onChange={(e) => props.onNote(e.target.value)} placeholder="Optional" disabled={props.disabled} />
      </label>
      <div className="buttons">
        <Button variant="success" icon="check" disabled={props.disabled || busy} onClick={() => decide(true)}>
          Approve
        </Button>
        <Button variant="danger" icon="x" disabled={props.disabled || busy} onClick={() => decide(false)}>
          Reject
        </Button>
      </div>
      <Alert message={error} />
    </div>
  );
}

// CancelTask is the header's "Cancel task": an inline confirmation, then
// the call.
export function CancelTask(props: Props) {
  const { task, onTask } = props;
  const { can, run, refused, ready } = useActions(props);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const { namespace: ns, name } = task.metadata;
  // The wrapper stays when the button goes, a refusal outliving it, as the
  // composer's does, in the same place; with nothing to hold, it goes.
  if (!can.cancel && !error && !refused) return null;
  return (
    <div className="cancel">
      {!can.cancel ? null : !confirming ? (
        <Button variant="danger-ghost" icon="x" disabled={!ready} onClick={() => setConfirming(true)}>
          Cancel task
        </Button>
      ) : (
        <div className="confirm" role="group" aria-label="Cancel this task?">
          <p>Cancel this task? A run in progress is stopped.</p>
          <div className="buttons">
            <Button
              variant="danger-solid"
              small
              disabled={busy}
              onClick={async () => {
                setBusy(true);
                setError((await run(() => cancel(ns, name), onTask)) ?? "");
                setBusy(false);
                setConfirming(false);
              }}
            >
              Yes, cancel it
            </Button>
            <Button small onClick={() => setConfirming(false)}>
              Keep it
            </Button>
          </div>
        </div>
      )}
      <Alert message={error} />
      <Alert message={refused} />
    </div>
  );
}
