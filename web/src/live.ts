// The namespace's watch stream (GET /api/v1/namespaces/<ns>/watch): hints
// that something a page shows changed, so the page reads it again with the
// route it reads it with anyway. A tab holds one stream per namespace,
// shared by every page that subscribes, and closes it when the last one
// goes. A topic is live while the stream is open and its hello says the
// topic is served; a page polls as before otherwise. Hints are lossy: a
// live page still polls, slowly.

export type Topic = "tasks" | "config" | "runs";

export type Hint =
  | { event: "task"; name: string; number?: number; phase?: string; parent?: string }
  | { event: "config"; kind: string; name: string }
  | { event: "run"; task: string; run: string; seq: number };

export interface Subscriber {
  topic: Topic;
  // task is the task whose run a "runs" subscriber follows: the stream is
  // opened with ?task=<task> for it.
  task?: string;
  // match picks the hints of the topic that are for this subscriber; all
  // of them without it.
  match?: (hint: Hint) => boolean;
  // onHint is told to read again: a hint matched, the stream fell behind
  // (resync), or it reconnected after hints may have been missed.
  onHint: () => void;
  // onLive is told whether the topic is live, now and on every change.
  onLive: (live: boolean) => void;
}

type Hello = Partial<Record<Topic, boolean>>;

interface Stream {
  subs: Set<Subscriber>;
  source?: EventSource;
  // task is the ?task= the stream was opened with, "" for none.
  task: string;
  // hello is what the open stream serves; null while it is not open.
  hello: Hello | null;
  // greeted is set by the first hello: a later one is a reconnect.
  greeted: boolean;
  queued: boolean;
}

const topicOf: Record<Hint["event"], Topic> = { task: "tasks", config: "config", run: "runs" };

const streams = new Map<string, Stream>();

// subscribe adds sub to ns's stream, opening it if need be, and returns
// what removes it again.
export function subscribe(ns: string, sub: Subscriber): () => void {
  let s = streams.get(ns);
  if (!s) {
    s = { subs: new Set(), task: "", hello: null, greeted: false, queued: false };
    streams.set(ns, s);
  }
  const stream = s;
  stream.subs.add(sub);
  sub.onLive(live(stream, sub));
  reconcile(ns, stream);
  return () => {
    stream.subs.delete(sub);
    reconcile(ns, stream);
  };
}

function live(s: Stream, sub: Subscriber) {
  return s.hello?.[sub.topic] === true && (sub.topic !== "runs" || sub.task === s.task);
}

// reconcile opens, reopens or closes the stream for its subscribers once
// the current changes are done, so a page that unsubscribes and
// subscribes again in one go keeps the stream it had.
function reconcile(ns: string, s: Stream) {
  if (s.queued) return;
  s.queued = true;
  setTimeout(() => {
    s.queued = false;
    if (s.subs.size === 0) {
      s.source?.close();
      streams.delete(ns);
      return;
    }
    const task = [...s.subs].find((x) => x.topic === "runs")?.task ?? "";
    // A stream that was refused or ended for good is not opened again,
    // unless it is for another task.
    if (s.source && task === s.task) return;
    open(ns, s, task);
  }, 0);
}

function open(ns: string, s: Stream, task: string) {
  s.source?.close();
  s.task = task;
  setHello(s, null);
  if (typeof EventSource === "undefined") return;
  const query = task ? `?task=${encodeURIComponent(task)}` : "";
  const src = new EventSource(`/api/v1/namespaces/${encodeURIComponent(ns)}/watch${query}`);
  s.source = src;
  const everyone = () => s.subs.forEach((sub) => live(s, sub) && sub.onHint());
  src.addEventListener("hello", (e) => {
    const again = s.greeted;
    s.greeted = true;
    setHello(s, JSON.parse((e as MessageEvent).data) as Hello);
    // Hints sent while it was down are not replayed.
    if (again) everyone();
  });
  for (const event of ["task", "config", "run"] as const) {
    src.addEventListener(event, (e) => {
      const hint = { event, ...JSON.parse((e as MessageEvent).data) } as Hint;
      s.subs.forEach((sub) => sub.topic === topicOf[event] && live(s, sub) && (sub.match?.(hint) ?? true) && sub.onHint());
    });
  }
  src.addEventListener("resync", everyone);
  src.addEventListener("end", (e) => {
    const { reason } = JSON.parse((e as MessageEvent).data) as { reason: string };
    // lifetime and shutdown: EventSource reconnects by itself. Without a
    // role or a Team a reconnect is refused: the pages poll, and their next
    // read shows the 403 or 404.
    if (reason === "no_role" || reason === "no_team") {
      src.close();
      setHello(s, null);
    }
  });
  // An error while open is a dropped connection, which EventSource retries;
  // one at open (401, 403, 404, 503) closes it for good. Either way the
  // pages poll until a hello.
  src.addEventListener("error", () => setHello(s, null));
}

function setHello(s: Stream, hello: Hello | null) {
  s.hello = hello;
  s.subs.forEach((sub) => sub.onLive(live(s, sub)));
}
