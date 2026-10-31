// The namespace's watch stream (GET /api/v1/namespaces/<ns>/watch): hints
// that something a page shows changed, so the page reads it again with the
// route it reads it with anyway. A topic is live while the stream is open
// and its hello says the topic is served; a page polls as before otherwise.
// Hints are lossy: a live page still polls, slowly.
//
// The tabs of a browser profile share the streams, since each holds one of
// the 6 connections a browser keeps to a host over HTTP/1.1. The tab that
// holds the Web Lock "cadre-live" is the leader: it opens at most
// maxStreams streams, for the namespaces the tabs' pages subscribe to, and
// relays what they send over the BroadcastChannel "cadre-live". The other
// tabs wait for the lock, so one takes over when the leader goes. A tab
// asks for streams only while it is visible. A page that is left lets go
// of the lock and the streams, also when the browser keeps it in its
// back/forward cache, and starts again when it is shown from there. Without Web Locks (not a
// secure context) or BroadcastChannel no tab opens a stream: they all poll.

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
  // (resync), or the topic went live after hints may have been missed.
  onHint: () => void;
  // onLive is told whether the topic is live, now and on every change.
  onLive: (live: boolean) => void;
}

type Hello = Partial<Record<Topic, boolean>>;

// maxStreams is how many streams a browser profile holds at most, leaving
// the rest of its 6 connections to the host for reads.
export const maxStreams = 2;
// retryMs is how long after a refused open (401, 403, 404, 503) the
// stream is opened again.
export const retryMs = 60000;

// Want is a stream a tab asks for: its namespace and the ?task= of a run
// it shows, "" for none.
interface Want {
  ns: string;
  task: string;
}

type Message =
  | { type: "demand"; tab: string; at: number; wants: Want[] }
  | { type: "who" }
  | { type: "bye" }
  | { type: "state"; ns: string; task: string; hello: Hello | null }
  | { type: "hint"; ns: string; hint: Hint }
  | { type: "resync"; ns: string };

// Local is what a tab knows of a namespace's stream.
interface Local {
  subs: Set<Subscriber>;
  // task is the ?task= the stream is open with.
  task: string;
  // hello is what the open stream serves; null while it is not open.
  hello: Hello | null;
}

// Feed is a stream the leader holds.
interface Feed {
  source: EventSource;
  task: string;
  hello: Hello | null;
  // dead is set by end no_role or no_team: it is not opened again.
  dead: boolean;
  retry?: ReturnType<typeof setTimeout>;
}

const topicOf: Record<Hint["event"], Topic> = { task: "tasks", config: "config", run: "runs" };

// Tab is one tab's share in the streams.
export interface Tab {
  subscribe(ns: string, sub: Subscriber): () => void;
  // leave does what closing the tab does.
  leave(): void;
}

// tab starts this tab's share, with the browser's locks and channel. page
// is what fires the tab's pagehide and pageshow, for tests.
export function tab(page: EventTarget = window): Tab {
  const id = Math.random().toString(36).slice(2);
  const locals = new Map<string, Local>();
  const shared = typeof navigator !== "undefined" && navigator.locks !== undefined && typeof BroadcastChannel !== "undefined";
  let channel: BroadcastChannel | undefined;
  let release: (() => void) | undefined;
  let abort: AbortController | undefined;
  let leader = false;
  let queued = false;
  // Leader only: each tab's demand, and the streams held for it.
  const demands = new Map<string, { at: number; wants: Want[] }>();
  const feeds = new Map<string, Feed>();

  const live = (l: Local, sub: Subscriber) => l.hello?.[sub.topic] === true && (sub.topic !== "runs" || sub.task === l.task);

  // send gives a message to every tab, this one included.
  const send = (m: Message) => {
    channel?.postMessage(m);
    receive(m);
  };

  const receive = (m: Message) => {
    switch (m.type) {
      case "demand":
        if (!leader) return;
        demands.set(m.tab, { at: m.at, wants: m.wants });
        reconcileFeeds();
        return;
      case "who":
      case "bye":
        // A new leader, or the leader went: the old one's streams are gone.
        locals.forEach((_, ns) => apply(ns, "", null));
        announce();
        return;
      case "state":
        apply(m.ns, m.task, m.hello);
        return;
      case "hint": {
        const l = locals.get(m.ns);
        l?.subs.forEach((sub) => sub.topic === topicOf[m.hint.event] && live(l, sub) && (sub.match?.(m.hint) ?? true) && sub.onHint());
        return;
      }
      case "resync": {
        const l = locals.get(m.ns);
        l?.subs.forEach((sub) => live(l, sub) && sub.onHint());
      }
    }
  };

  // apply sets what ns's stream serves. A subscriber whose topic goes
  // live reads again, as hints sent before were not seen.
  const apply = (ns: string, task: string, hello: Hello | null) => {
    const l = locals.get(ns);
    if (!l) return;
    const was = new Map([...l.subs].map((sub) => [sub, live(l, sub)]));
    l.task = task;
    l.hello = hello;
    l.subs.forEach((sub) => {
      const on = live(l, sub);
      sub.onLive(on);
      if (on && !was.get(sub)) sub.onHint();
    });
  };

  // demand tells the leader which streams this tab wants. A tab that is
  // closed is hidden first, so it says at once that it wants none.
  const demand = () => {
    const wants =
      document.visibilityState === "hidden"
        ? []
        : [...locals].filter(([, l]) => l.subs.size).map(([ns, l]) => ({ ns, task: [...l.subs].find((x) => x.topic === "runs")?.task ?? "" }));
    send({ type: "demand", tab: id, at: Date.now(), wants });
  };

  // announce calls demand once the current changes are done, so a page
  // that unsubscribes and subscribes again in one go keeps the stream it
  // had.
  const announce = () => {
    if (queued) return;
    queued = true;
    setTimeout(() => {
      queued = false;
      locals.forEach((l, ns) => l.subs.size || locals.delete(ns));
      demand();
    }, 0);
  };

  // reconcileFeeds opens, reopens and closes the leader's streams for the
  // tabs' demand: the namespaces of the latest, up to maxStreams, each
  // with the ?task= of the latest that has one.
  const reconcileFeeds = () => {
    const want = new Map<string, string>();
    for (const d of [...demands.values()].sort((a, b) => b.at - a.at)) {
      for (const w of d.wants) {
        if (!want.has(w.ns) && want.size === maxStreams) continue;
        if (!want.get(w.ns)) want.set(w.ns, w.task);
      }
    }
    feeds.forEach((f, ns) => {
      if (want.get(ns) === f.task) return;
      close(f);
      feeds.delete(ns);
      send({ type: "state", ns, task: "", hello: null });
    });
    want.forEach((task, ns) => feeds.has(ns) || open(ns, task));
    // Tell a tab that just asked what the open streams serve.
    feeds.forEach((f, ns) => send({ type: "state", ns, task: f.task, hello: f.hello }));
  };

  const close = (f: Feed) => {
    f.source.close();
    clearTimeout(f.retry);
  };

  const open = (ns: string, task: string) => {
    if (typeof EventSource === "undefined") return;
    const query = task ? `?task=${encodeURIComponent(task)}` : "";
    const src = new EventSource(`/api/v1/namespaces/${encodeURIComponent(ns)}/watch${query}`);
    const f: Feed = { source: src, task, hello: null, dead: false };
    feeds.set(ns, f);
    const state = (hello: Hello | null) => {
      f.hello = hello;
      send({ type: "state", ns, task, hello });
    };
    src.addEventListener("hello", (e) => state(JSON.parse((e as MessageEvent).data) as Hello));
    for (const event of ["task", "config", "run"] as const) {
      src.addEventListener(event, (e) => send({ type: "hint", ns, hint: { event, ...JSON.parse((e as MessageEvent).data) } as Hint }));
    }
    src.addEventListener("resync", () => send({ type: "resync", ns }));
    src.addEventListener("end", (e) => {
      const { reason } = JSON.parse((e as MessageEvent).data) as { reason: string };
      // lifetime and shutdown: EventSource reconnects by itself. Without a
      // role or a Team a reconnect is refused: the pages poll, and their next
      // read shows the 403 or 404.
      if (reason === "no_role" || reason === "no_team") {
        src.close();
        f.dead = true;
        state(null);
      }
    });
    // An error while open is a dropped connection, which EventSource retries;
    // one at open (401, 403, 404, 503) closes it, and it is opened again
    // after retryMs, as a 502 or 503 may be a server being replaced.
    src.addEventListener("error", () => {
      state(null);
      if (src.readyState !== EventSource.CLOSED || f.dead) return;
      f.retry = setTimeout(() => {
        if (feeds.get(ns) !== f) return;
        feeds.delete(ns);
        reconcileFeeds();
      }, retryMs);
    });
  };

  const start = () => {
    channel = new BroadcastChannel("cadre-live");
    channel.onmessage = (e: MessageEvent<Message>) => receive(e.data);
    document.addEventListener("visibilitychange", demand);
    abort = new AbortController();
    navigator.locks
      .request(
        "cadre-live",
        { signal: abort.signal },
        () =>
          new Promise<void>((resolve) => {
            release = resolve;
            leader = true;
            demands.clear();
            send({ type: "who" });
          }),
      )
      .catch(() => {});
  };

  // stop leaves the streams to the other tabs: the leader closes them and
  // says it goes, so their pages poll even if the next tab to get the lock
  // is frozen and late; another tab says it wants none. A tab still
  // waiting for the lock stops waiting, so a page in the back/forward
  // cache never holds it. This tab's pages poll until a hello after start.
  const stop = () => {
    feeds.forEach(close);
    feeds.clear();
    channel?.postMessage(leader ? { type: "bye" } : { type: "demand", tab: id, at: Date.now(), wants: [] });
    leader = false;
    release?.();
    abort?.abort();
    channel?.close();
    channel = undefined;
    document.removeEventListener("visibilitychange", demand);
    locals.forEach((_, ns) => apply(ns, "", null));
  };

  const show = (e: Event) => {
    if (!(e as PageTransitionEvent).persisted) return;
    start();
    announce();
  };

  if (shared) {
    start();
    page.addEventListener("pagehide", stop);
    page.addEventListener("pageshow", show);
  }
  return {
    subscribe(ns, sub) {
      if (!shared) {
        sub.onLive(false);
        return () => {};
      }
      let l = locals.get(ns);
      if (!l) {
        l = { subs: new Set(), task: "", hello: null };
        locals.set(ns, l);
      }
      const local = l;
      local.subs.add(sub);
      sub.onLive(live(local, sub));
      announce();
      return () => {
        local.subs.delete(sub);
        announce();
      };
    },
    leave() {
      stop();
      page.removeEventListener("pagehide", stop);
      page.removeEventListener("pageshow", show);
    },
  };
}

let here = tab();

// subscribe adds sub to ns's stream and returns what removes it again.
export function subscribe(ns: string, sub: Subscriber): () => void {
  return here.subscribe(ns, sub);
}

// restart makes this tab a new one, for tests: they each start from none.
export function restart() {
  here.leave();
  here = tab();
}
