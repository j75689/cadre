import type { ReactNode } from "react";
import { Button } from "./Button";
import { Icon } from "./Icon";

// Banner is an error inline where it happened: an icon, the message and,
// where one exists, Retry.
export function Banner({ children, onRetry, className = "" }: { children: ReactNode; onRetry?: () => void; className?: string }) {
  return (
    <div className={`banner ${className}`.trim()}>
      <Icon name="alert-triangle" />
      <p className="banner-text">{children}</p>
      {onRetry && (
        <Button small icon="refresh" onClick={onRetry}>
          Retry
        </Button>
      )}
    </div>
  );
}
