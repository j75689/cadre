import { describe, expect, it } from "vitest";
import { toYAML } from "./yaml";

describe("toYAML", () => {
  it("prints nested maps indented, and lists at their key's indent", () => {
    const v = { metadata: { name: "bot", labels: { "app.kubernetes.io/name": "cadre" } }, spec: { toolRefs: ["git", "web"], modelRef: "claude" } };
    expect(toYAML(v)).toBe(
      ["metadata:", "  name: bot", "  labels:", "    app.kubernetes.io/name: cadre", "spec:", "  toolRefs:", "  - git", "  - web", "  modelRef: claude", ""].join("\n"),
    );
  });

  it("starts a map or a list in a list on the dash's line", () => {
    const v = { env: [{ name: "A", value: "(set)" }, { name: "B", valueFrom: { secretKeyRef: { name: "s", key: "k" } } }], m: [[1, 2], []] };
    expect(toYAML(v)).toBe(
      [
        "env:",
        "- name: A",
        "  value: (set)",
        "- name: B",
        "  valueFrom:",
        "    secretKeyRef:",
        "      name: s",
        "      key: k",
        "m:",
        "- - 1",
        "  - 2",
        "- []",
        "",
      ].join("\n"),
    );
  });

  it("prints empty maps and lists inline", () => {
    expect(toYAML({})).toBe("{}\n");
    expect(toYAML([])).toBe("[]\n");
    expect(toYAML({ a: {}, b: [], c: [{}] })).toBe("a: {}\nb: []\nc:\n- {}\n");
  });

  it("prints numbers, booleans and null as they are", () => {
    expect(toYAML({ i: 3, f: 0.5, neg: -2, t: true, off: false, z: null })).toBe('i: 3\nf: 0.5\nneg: -2\nt: true\n"off": false\nz: null\n');
  });

  it("quotes strings YAML would read as something else", () => {
    const quoted = ["", "true", "No", "null", "~", "y", "3", "1.5", "-1", ".5", "-.5", "-", "? x", ": x", "0x1f", ".inf", " lead", "trail ", "a: b", "a #b", "end:", "- x", "#c", "*ref", "&a", "!tag", "{x}", "[x]", "'q'", '"q"', "@at", "%p", "|", ">", "<<", "=", "tab\there"];
    for (const s of quoted) expect(toYAML({ k: s })).toBe(`k: ${JSON.stringify(s)}\n`);
    for (const s of ["(set)", "bot-1", "claude-opus", "http://x:8080/p", "a:b", "a#b", "v1", "x y", "--fast", "-x", "?x", "::1"]) expect(toYAML({ k: s })).toBe(`k: ${s}\n`);
    expect(toYAML({ "a: b": 1, "": 2 })).toBe('"a: b": 1\n"": 2\n');
  });

  it("prints multi-line strings as literal blocks, keeping their last line breaks", () => {
    expect(toYAML({ k: "one\ntwo" })).toBe("k: |-\n  one\n  two\n");
    expect(toYAML({ k: "one\n\ntwo\n" })).toBe("k: |\n  one\n\n  two\n");
    expect(toYAML({ k: "one\n\n" })).toBe("k: |+\n  one\n\n");
    expect(toYAML({ l: ["a\nb"] })).toBe("l:\n- |-\n  a\n  b\n");
    // A first line with a leading space, or a control character, is quoted.
    expect(toYAML({ k: " one\ntwo" })).toBe('k: " one\\ntwo"\n');
    expect(toYAML({ k: "a\r\nb" })).toBe('k: "a\\r\\nb"\n');
  });
});
