# cadre Helm chart

Installs Cadre into one namespace: cadre-server (the API, the board, the
runner protocol and the controllers) and cadre-runner (which works builtin
agents' runs), with the CRDs and the RBAC they need.

```sh
helm install cadre oci://ghcr.io/j75689/charts/cadre -n cadre-system --create-namespace \
  --set auth.oidc.issuer=https://dex.example.test \
  --set auth.oidc.clientId=cadre \
  --set server.externalURL=https://cadre.example.test \
  --set auth.admins.users='{admin@example.test}' \
  --set storage.s3.bucket=cadre
```

`values.yaml` documents every value; `values.schema.json` checks them.

Each release `vX.Y.Z` publishes the chart at version `X.Y.Z` with `appVersion`
`vX.Y.Z`, the image tag it installs by default; pick one with `--version`. The
chart in the repository has `appVersion` `main`, the images built from main.

## What it installs

- **cadre-server**: a Deployment of `server.replicas` (default 2) with
  leader election in the release namespace, a Service, an optional Ingress, a
  ServiceAccount, and the ClusterRole `<release>-server` (from
  `config/rbac/role.yaml`) bound to it cluster-wide.
- **The workloads ClusterRole** `<release>-server-workloads` (from
  `config/rbac/workloads.yaml`): creating runner Jobs and managed ToolServers.
  It is bound cluster-wide only with `workloads.clusterWide: true`; otherwise
  bind it in each Team namespace (below).
- **cadre-runner** (`runner.enabled`, default on): a Deployment, a
  ServiceAccount whose token is projected for the audience `cadre` only, and
  the ClusterRole `<release>-runner` (from `config/runner/rbac.yaml`), which
  grants nothing until bound in a Team namespace (below).
- **The session key** (`auth.mode: oidc`): see below.
- **The CRDs**, from `crds/`.

## The values that matter

| Value | Default | |
|---|---|---|
| `auth.mode` | `oidc` | `none` lets anyone act as anyone: local development only |
| `auth.oidc.issuer`, `auth.oidc.clientId` | | required with `oidc` |
| `auth.oidc.clientSecret.secretName` | | a Secret holding the client's secret, under `auth.oidc.clientSecret.key` |
| `auth.oidc.offlineAccess` | `true` | let `cadre login` ask for `offline_access` and keep a refresh token, where the provider offers it |
| `auth.admins.users`, `auth.admins.groups` | | admin on every team; the only ones who may create teams |
| `storage.s3.bucket` | | required |
| `storage.s3.credentialsSecret` | | a Secret with `access-key` and `secret-key`; without it the AWS default chain |
| `ingress.enabled`, `ingress.host`, `ingress.tlsSecret` | off | the host becomes the external URL logins return to |
| `server.externalURL` | the Ingress host | where browsers reach cadre-server; required with `oidc` and no Ingress, and when TLS terminates before the Ingress (`https://<host>`) |
| `runner.serverURL` | `server.publicURL` | where the chart's runner reaches cadre-server |
| `server.publicURL` | the Service | where runners, the chart's included, reach cadre-server |
| `server.watchMaxStreams` | 2000 | live-update streams each replica serves at once; one more is answered 503, and that board polls |
| `workloads.clusterWide` | `false` | bind the workloads ClusterRole everywhere |

## The session key

With `auth.mode: oidc`, cadre-server encrypts session cookies with a 32-byte
key, the same on every replica. By default the chart creates the Secret
`<release>-session-key` on first install and keeps it: on upgrades, and on
uninstall (it is annotated `helm.sh/resource-policy: keep`), so that sessions
survive both. To sign everyone out, delete the Secret and `helm upgrade`: that
makes a new key, and the key's checksum on the server pods rolls them, since
cadre-server reads it at start. (With `existingSecret`, the checksum follows
that Secret too, at the next `helm upgrade`.)

The chart reads the existing key with `lookup`, which sees no cluster under
`helm template` and in GitOps tools that render with it: those would make a
new key on every render. There, create the Secret yourself and name it in
`auth.sessionKey.existingSecret`:

```sh
kubectl -n cadre-system create secret generic cadre-session-key \
  --from-literal=key="$(openssl rand -hex 32)"
```

## Per-Team RBAC

cadre-server and cadre-runner act in Team namespaces only where they are
bound, so a cluster's other namespaces stay out of their reach. For each Team
namespace (here `team-a`, with the release `cadre` in `cadre-system`):

```yaml
# Runner Jobs and managed ToolServers; not needed with workloads.clusterWide.
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: cadre-server-workloads
  namespace: team-a
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cadre-server-workloads
subjects:
- kind: ServiceAccount
  name: cadre-server
  namespace: cadre-system
---
# The builtin agents' model credentials.
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: cadre-runner
  namespace: team-a
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cadre-runner
subjects:
- kind: ServiceAccount
  name: cadre-runner
  namespace: cadre-system
```

## Checking an install end to end

`make e2e` builds the images at the current commit and runs
`hack/e2e/run.sh`. The script:

1. Creates a kind cluster with its own kubeconfig.
2. Installs Dex and SeaweedFS into it.
3. Installs this chart with `hack/e2e/values.yaml`: logins on, the
   runner on, and Jobs bound cluster-wide.
4. Checks, with an ID token from Dex's password grant:
   - the admin is admin;
   - a Team and a task can be created through the API;
   - the task is listed with a number;
   - the UI is served;
   - a request without a token is refused.

CI runs it on every pull request and on main. `KEEP=1` keeps the cluster for
a look around.

## Upgrading the CRDs

Helm installs `crds/` on first install only and never upgrades or deletes
them. Before upgrading the chart, apply the new version's CRDs:

```sh
kubectl apply -f charts/cadre/crds
```

(from a checkout at the release's tag, or `helm pull` the chart and apply its
`crds/`).
