import type { ButtonHTMLAttributes } from "react";
import { Link, type LinkProps } from "react-router";
import { Icon, type IconName } from "./Icon";

export type Variant = "primary" | "secondary" | "ghost" | "danger" | "danger-ghost" | "danger-solid" | "success";

// classes is a button's look: the variant, small or not, icon-only or not.
export function buttonClass(variant: Variant = "secondary", small = false, iconOnly = false, extra = "") {
  const v = {
    primary: "btn-primary",
    secondary: "",
    ghost: "btn-ghost",
    danger: "btn-danger",
    "danger-ghost": "btn-ghost btn-danger",
    "danger-solid": "btn-danger-solid",
    success: "btn-success",
  }[variant];
  return ["btn", v, small && "btn-sm", iconOnly && "btn-icon", extra].filter(Boolean).join(" ");
}

interface Look {
  variant?: Variant;
  small?: boolean;
  icon?: IconName;
}

// Button is a button. An icon-only one (no children) must carry an
// aria-label, which the types ask for.
export function Button({
  variant,
  small,
  icon,
  className,
  children,
  type = "button",
  ...rest
}: Look & ButtonHTMLAttributes<HTMLButtonElement>) {
  const iconOnly = children === undefined;
  return (
    <button type={type} className={buttonClass(variant, small, iconOnly, className)} {...rest}>
      {icon && <Icon name={icon} />}
      {children}
    </button>
  );
}

// ButtonLink is a link that looks like a button, for navigation.
export function ButtonLink({ variant, small, icon, className, children, ...rest }: Look & LinkProps) {
  return (
    <Link className={buttonClass(variant, small, false, className)} {...rest}>
      {icon && <Icon name={icon} />}
      {children}
    </Link>
  );
}
