// toYAML prints a JSON value as block YAML, for reading and copying: maps
// and lists one entry per line, lists at their key's indent as kubectl
// prints them, a multi-line string as a literal block. It is no YAML
// library: the value is JSON, so it has strings, numbers, booleans, null,
// lists and maps only, and a string YAML would read as something else is
// quoted, as JSON quotes it, which YAML reads the same.
export function toYAML(v: unknown): string {
  return lines(v, 0).join("\n") + "\n";
}

const pad = (n: number) => " ".repeat(n);

const isMap = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);

// nested is a value printed on the lines under its key or dash.
const nested = (v: unknown) => (Array.isArray(v) ? v.length > 0 : isMap(v) && Object.keys(v).length > 0);

const control = (s: string) => [...s].some((c) => c < " " || c === "\x7f");

// plain says whether a string can go unquoted: not empty, no word YAML
// reads as a boolean, null or a merge, nothing that starts like a number, an
// indicator (- ? : only before a space) or a comment, no space at an end.
function plain(s: string): boolean {
  return (
    s !== "" &&
    !control(s) &&
    !/^[\s,[\]{}#&*!|>'"%@`]|^[-?:](\s|$)|\s$|: |\s#|:$/.test(s) &&
    !/^(true|false|yes|no|on|off|y|n|null|~|<<|=)$/i.test(s) &&
    !/^[-+]?\.?\d|^[-+]?\.(inf|nan)$/i.test(s)
  );
}

function scalar(v: unknown): string {
  if (Array.isArray(v)) return "[]";
  if (isMap(v)) return "{}";
  if (typeof v === "string") return plain(v) ? v : JSON.stringify(v);
  return String(v);
}

// literal says whether a string prints as a literal block: it has lines,
// and its first does not start with a space, which would need an
// indentation indicator.
const literal = (v: unknown): v is string => typeof v === "string" && v.includes("\n") && !/^\s/.test(v) && !control(v.replaceAll("\n", "").replaceAll("\t", ""));

function lines(v: unknown, indent: number): string[] {
  if (Array.isArray(v) && v.length > 0) return v.flatMap((item) => dashed(item, indent));
  if (isMap(v) && nested(v)) return Object.entries(v).flatMap(([k, val]) => entry(k, val, indent));
  return withValue("", v, indent);
}

function entry(k: string, v: unknown, indent: number): string[] {
  const head = `${pad(indent)}${scalar(k)}:`;
  if (nested(v)) return [head, ...lines(v, Array.isArray(v) ? indent : indent + 2)];
  return withValue(head, v, indent + 2);
}

// dashed is a list's item: a map or a list starts on the dash's line.
function dashed(item: unknown, indent: number): string[] {
  if (!nested(item)) return withValue(`${pad(indent)}-`, item, indent + 2);
  const [first, ...rest] = lines(item, indent + 2);
  return [`${pad(indent)}- ${first.trimStart()}`, ...rest];
}

// withValue is head and a value that is no non-empty map or list: on the
// same line, or a literal block under it. The block keeps the string's
// last line break (|), drops a missing one (|-) or keeps several (|+).
function withValue(head: string, v: unknown, indent: number): string[] {
  const sep = head ? " " : "";
  if (!literal(v)) return [head + sep + scalar(v)];
  const chomp = v.endsWith("\n\n") ? "+" : v.endsWith("\n") ? "" : "-";
  const body = chomp === "+" ? v.slice(0, -1) : v.replace(/\n$/, "");
  return [`${head}${sep}|${chomp}`, ...body.split("\n").map((l) => (l ? pad(indent) + l : ""))];
}
