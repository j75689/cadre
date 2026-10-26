import type { ObjectMeta } from "./api";

// managedByGitOps says why a resource looks applied from Git, for the
// warning before an edit here, or undefined when nothing says so. The
// server sends the annotations, labels and managedFields to admins only,
// who are the only ones who can edit.
export function managedByGitOps(meta: ObjectMeta): string | undefined {
  const a = meta.annotations ?? {};
  const l = meta.labels ?? {};
  if ("argocd.argoproj.io/tracking-id" in a) return "it has Argo CD's tracking id";
  if ("kustomize.toolkit.fluxcd.io/name" in l) return `Flux Kustomization ${l["kustomize.toolkit.fluxcd.io/name"]} applies it`;
  if ("helm.toolkit.fluxcd.io/name" in l) return `Flux HelmRelease ${l["helm.toolkit.fluxcd.io/name"]} applies it`;
  if ("app.kubernetes.io/instance" in l) return `it is labelled app.kubernetes.io/instance=${l["app.kubernetes.io/instance"]}`;
  if ("kubectl.kubernetes.io/last-applied-configuration" in a) return "it was last written by kubectl apply";
  const apply = meta.managedFields?.find((f) => f.operation === "Apply");
  if (apply) return `${apply.manager || "a tool"} applies it server-side`;
  return undefined;
}
