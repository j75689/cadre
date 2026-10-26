import type { ReactNode } from "react";
import { Button } from "./Button";
import { Icon, type IconName } from "./Icon";

// Banner is an error inline where it happened: an icon, the message and,
// where one exists, Retry.
export function Banner({
  children,
  onRetry,
  className = "",
  icon = "alert-triangle",
  id,
}: {
  children: ReactNode;
  onRetry?: () => void;
  className?: string;
  icon?: IconName;
  id?: string;
}) {
  return (
    <div className={`banner ${className}`.trim()} id={id}>
      <Icon name={icon} />
      <p className="banner-text">{children}</p>
      {onRetry && (
        <Button small icon="refresh" onClick={onRetry}>
          Retry
        </Button>
      )}
    </div>
  );
}
