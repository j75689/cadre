import type { CSSProperties, ReactNode } from "react";

// Skeleton is one grey block of a placeholder. It is decoration: the group
// around it says what is loading.
export function Skeleton({ width = "100%", height = 12 }: { width?: CSSProperties["width"]; height?: CSSProperties["height"] }) {
  return <span className="skeleton" style={{ width, height }} aria-hidden="true" />;
}

// Loading wraps placeholders: busy, and named for screen readers, which
// skip the blocks themselves.
export function Loading({ what, children, className }: { what: string; children: ReactNode; className?: string }) {
  return (
    <div className={className} aria-busy="true">
      <span className="sr-only">Loading {what}</span>
      {children}
    </div>
  );
}

// SkeletonCard is a card's placeholder: a short line, a title and a meta
// line.
export function SkeletonCard({ className = "" }: { className?: string }) {
  return (
    <div className={`card skeleton-card ${className}`.trim()} aria-hidden="true">
      <Skeleton width="30%" height={10} />
      <Skeleton width="85%" height={14} />
      <Skeleton width="55%" height={10} />
    </div>
  );
}
