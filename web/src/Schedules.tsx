import { getSchedule, listSchedules } from "./api";
import { ConfigDetail } from "./ConfigDetail";
import { ConfigList } from "./ConfigList";
import { scheduleLink } from "./links";

// Schedules is the team's schedules: whether each is ready, and why not;
// a suspended one is Ready, with the reason Suspended.
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
  return <ConfigDetail kind="Schedule" noun="schedule" title="Schedules" section="schedules" load={getSchedule} />;
}
