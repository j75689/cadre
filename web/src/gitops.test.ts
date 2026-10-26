import { describe, expect, it } from "vitest";
import type { ObjectMeta } from "./api";
import { managedByGitOps } from "./gitops";

const meta = (extra: Partial<ObjectMeta>): ObjectMeta => ({ name: "team", namespace: "a", ...extra });

describe("managedByGitOps", () => {
  it.each<[string, Partial<ObjectMeta>, RegExp]>([
    ["kubectl apply", { annotations: { "kubectl.kubernetes.io/last-applied-configuration": "{}" } }, /kubectl apply/],
    ["Argo CD", { annotations: { "argocd.argoproj.io/tracking-id": "teams:cadre.io/Team:a/team" } }, /Argo CD/],
    ["an app instance label", { labels: { "app.kubernetes.io/instance": "teams" } }, /app.kubernetes.io\/instance=teams/],
    ["a Flux Kustomization", { labels: { "kustomize.toolkit.fluxcd.io/name": "teams" } }, /Flux Kustomization teams/],
    ["a Flux HelmRelease", { labels: { "helm.toolkit.fluxcd.io/name": "teams" } }, /Flux HelmRelease teams/],
    ["a server-side apply", { managedFields: [{ manager: "cadre-server", operation: "Update" }, { manager: "argocd-controller", operation: "Apply" }] }, /argocd-controller applies it/],
  ])("says why for %s", (_, extra, why) => {
    expect(managedByGitOps(meta(extra))).toMatch(why);
  });

  it("says nothing without a sign", () => {
    expect(managedByGitOps(meta({}))).toBeUndefined();
    expect(managedByGitOps(meta({ annotations: { note: "x" }, labels: { team: "a" } }))).toBeUndefined();
  });

  it("says nothing for managedFields that only update", () => {
    expect(managedByGitOps(meta({ managedFields: [{ manager: "cadre-server", operation: "Update" }] }))).toBeUndefined();
  });
});
