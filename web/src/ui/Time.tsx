// ago says how long before now a time was, in a short form for the last
// day: "just now", "12 s ago", "5 min ago", "3 h ago"; undefined after.
export function ago(then: Date, now = Date.now()): string | undefined {
  const s = Math.max(0, Math.round((now - then.getTime()) / 1000));
  if (s < 10) return "just now";
  if (s < 60) return `${s} s ago`;
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return undefined;
}

// Time is a moment: relative within a day, the full date after, and the
// full date always in its title.
export function Time({ at }: { at: string }) {
  const d = new Date(at);
  if (Number.isNaN(d.getTime())) return null;
  const full = d.toLocaleString();
  return (
    <time dateTime={at} title={full}>
      {ago(d) ?? full}
    </time>
  );
}
