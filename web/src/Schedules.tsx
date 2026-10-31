import { getSchedule, listSchedules, type ConfigResource } from "./api";
import { ConfigDetail } from "./ConfigDetail";
import { ConfigList } from "./ConfigList";
import { scheduleLink } from "./links";

// Schedules is the team's schedules: whether each is ready, and why not.
// A suspended one is Ready, and the list says why only when not, so it
// reads as a running one; its page says Suspended.
export function Schedules() {
  return (
    <ConfigList
      kind="Schedule"
      title="Schedules"
      noun="schedule"
      link={scheduleLink}
      load={listSchedules}
      empty="This team has no schedules. Schedules are Schedule resources in the namespace, applied with kubectl or GitOps."
    />
  );
}

// ScheduleDetail is one Schedule: its health, conditions, metadata and
// YAML, as the caller's role is shown it; its spec (cron, timezone, task
// template) is in the YAML, with nothing masked.
export function ScheduleDetail() {
  return <ConfigDetail kind="Schedule" noun="schedule" title="Schedules" section="schedules" load={loadSchedule} />;
}

// loadSchedule reads one Schedule. The server says why only when it is not
// ready; a ready one's why (Suspended or Scheduled) is its Ready condition's,
// which the server has checked is about the current spec.
async function loadSchedule(ns: string, name: string, signal: AbortSignal): Promise<ConfigResource> {
  const s = await getSchedule(ns, name, signal);
  const c = s.ready ? s.conditions.find((c) => c.type === "Ready") : undefined;
  return c ? { ...s, reason: c.reason, message: c.message } : s;
}
