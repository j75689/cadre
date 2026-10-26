import type { ReactNode } from "react";
import type { Phase } from "../api";
import { Icon, type IconName } from "./Icon";

// The tone class of a phase: its hue, from the status tokens.
export const phaseTone = (phase: Phase) => `tone-${phase.toLowerCase()}`;

// Badge is a tinted pill: text on its tone's soft tint.
export function Badge({ tone, children, className = "" }: { tone?: string; children: ReactNode; className?: string }) {
  return <span className={`badge ${tone ?? ""} ${className}`.trim()}>{children}</span>;
}

// PhaseBadge is a phase: a dot in its hue and its name, never the colour
// alone. A Running one's dot pulses.
export function PhaseBadge({ phase }: { phase: Phase }) {
  return (
    <Badge tone={phaseTone(phase)} className="phase">
      <span className={phase === "Running" ? "dot pulse" : "dot"} aria-hidden="true" />
      {phase}
    </Badge>
  );
}

// PriorityChip is a priority as an icon and its word: urgent and high in
// their hue, low as muted text, medium (the default) as nothing unless
// always is set.
export function PriorityChip({ priority, always = false }: { priority?: string; always?: boolean }) {
  const p = priority ?? "medium";
  if (p === "urgent" || p === "high") {
    return (
      <span className={`chip tone-${p}`}>
        <Icon name="alert-triangle" size={12} />
        {p}
      </span>
    );
  }
  if (p === "low" || always) return <span className={`chip chip-${p}`}>{p}</span>;
  return null;
}

// RoleBadge is the caller's role on a team.
export function RoleBadge({ role }: { role: string }) {
  return <Badge tone={role === "admin" ? "tone-accent" : undefined}>{role}</Badge>;
}

// Pills are labels.
export function Pills({ labels }: { labels: string[] }) {
  return (
    <span className="pills">
      {labels.map((l) => (
        <span className="pill" key={l}>
          {l}
        </span>
      ))}
    </span>
  );
}

// Meta is a row of an icon and text.
export function Meta({ icon, children, className = "" }: { icon: IconName; children: ReactNode; className?: string }) {
  return (
    <div className={`meta ${className}`.trim()}>
      <Icon name={icon} size={14} />
      <span>{children}</span>
    </div>
  );
}

// AssigneeIcon is the glyph of who a task is for, or who wrote an entry.
export const assigneeIcon = (kind: string | undefined): IconName => (kind === "Agent" ? "bot" : "user");
