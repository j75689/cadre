import type { ReactNode } from "react";

// Segmented is a radio group drawn as one row of buttons. hideLegend keeps
// the legend for screen readers only, for a group its row already explains.
export function Segmented<T extends string>(props: {
  legend: string;
  hideLegend?: boolean;
  name: string;
  value: T;
  options: { value: T; label: string }[];
  onChange: (v: T) => void;
  children?: ReactNode;
}) {
  return (
    <fieldset>
      <legend className={props.hideLegend ? "sr-only" : undefined}>{props.legend}</legend>
      <div className="segmented">
        {props.options.map((o) => (
          <label key={o.value}>
            <input type="radio" name={props.name} value={o.value} checked={props.value === o.value} onChange={() => props.onChange(o.value)} />
            {o.label}
          </label>
        ))}
      </div>
      {props.children}
    </fieldset>
  );
}
