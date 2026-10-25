#!/usr/bin/env bash
# run.sh installs the chart into a fresh kind cluster, with Dex and SeaweedFS,
# and checks that a logged-in admin can create a Team and a task through the
# API and that the UI is served. `make e2e` builds the images and runs it.
#
#   hack/e2e/run.sh <image tag>
#
# The images $REGISTRY/{cadre-server,cadre-runner}:<tag>, REGISTRY by default
# ghcr.io/j75689, must be in the local Docker. The cluster has its own
# kubeconfig, so the current kubectl context is never touched. KEEP=1 keeps
# the cluster afterwards. kind's and kubectl's versions are the Makefile's
# KIND_VERSION and KUBECTL_VERSION; their checksums are below.
set -euo pipefail

KIND_NODE=kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5
REGISTRY=${REGISTRY:-ghcr.io/j75689}
CLUSTER=cadre-e2e
TIMEOUT=300s

# The SHA-256 of each pinned download, by platform: change them with the
# versions.
kind_sha256() {
  case $1 in
    linux-amd64) echo aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d ;;
    linux-arm64) echo 20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2 ;;
    darwin-arm64) echo 0c8c7dbe5e23594a198b786c4bc13dacc101fa6196b0cb0b23a1ca44e61f4b4f ;;
    darwin-amd64) echo 5a99f26f57246dc9319dd294803313197a0f34d33c525b3ea8b655db5916ece0 ;;
    *) return 1 ;;
  esac
}
kubectl_sha256() {
  case $1 in
    linux-amd64) echo 6129359f4e1f3848a5572ccb0b26cf28b8ca08cef38c95a765b2f64a2c961a2f ;;
    linux-arm64) echo 922df28df248cc00a9e025f947704f1d1482de64ece54cfe57e61f19eaf1eef3 ;;
    darwin-arm64) echo 583beedaebe422e71d3f1a96acef8b1fef86ea2f09a45ad01aa6c9ce287c1380 ;;
    darwin-amd64) echo d5276c0f4fde77fc446070290f345944a7f1fda153df6b960e5fde93b7a9bccd ;;
    *) return 1 ;;
  esac
}

TAG=${1:?usage: hack/e2e/run.sh <image tag>}
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
HERE=$ROOT/hack/e2e
TOOLS=$ROOT/bin/tools
versions=$(make -s -C "$ROOT" print-e2e-tools)
read -r KIND_VERSION KUBECTL_VERSION <<<"$versions"
KIND=$TOOLS/kind-$KIND_VERSION
KUBECTL=$TOOLS/kubectl-$KUBECTL_VERSION

log() { printf '\n== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
k() { "$KUBECTL" "$@"; }

WORK=$(mktemp -d)
export KUBECONFIG=$WORK/kubeconfig
pids=()
passed=
cleanup() {
  local status=$?
  # Only the last line passes: an exit before it, even one bash reports
  # as 0 (an unbound variable, say), fails.
  if [ -z "$passed" ] && [ "$status" -eq 0 ]; then status=1; fi
  for pid in ${pids[@]+"${pids[@]}"}; do
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  if [ "$status" -ne 0 ] && [ -s "$KUBECONFIG" ]; then
    log "failed: the cluster's state"
    k get pods -A -o wide || true
    k -n cadre-system logs deploy/cadre-server --tail=200 || true
    k -n cadre-system logs deploy/cadre-runner --tail=50 || true
  fi
  if [ "${KEEP:-}" = 1 ]; then
    echo "kept: KUBECONFIG=$KUBECONFIG; delete with: $KIND delete cluster --name $CLUSTER"
  else
    if [ -x "$KIND" ]; then "$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
    rm -rf "$WORK"
  fi
  exit "$status"
}
trap cleanup EXIT

for tool in curl jq docker; do
  command -v "$tool" >/dev/null || fail "$tool is needed"
done

# fetch downloads url to dst, unless there, and checks its SHA-256.
fetch() {
  local url=$1 dst=$2 sum=$3
  if [ ! -x "$dst" ]; then
    mkdir -p "$(dirname "$dst")"
    curl -fsSL -o "$dst.tmp" "$url"
    mv "$dst.tmp" "$dst"
  fi
  if command -v sha256sum >/dev/null; then
    echo "$sum  $dst" | sha256sum -c --quiet - || { rm -f "$dst"; fail "checksum of $dst"; }
  else
    echo "$sum  $dst" | shasum -a 256 -c --quiet - || { rm -f "$dst"; fail "checksum of $dst"; }
  fi
  chmod +x "$dst"
}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case $arch in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
platform=$os-$arch
kind_sum=$(kind_sha256 "$platform") || fail "no kind build for $platform"
kubectl_sum=$(kubectl_sha256 "$platform") || fail "no kubectl build for $platform"
fetch "https://github.com/kubernetes-sigs/kind/releases/download/$KIND_VERSION/kind-$platform" "$KIND" "$kind_sum"
fetch "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/$os/$arch/kubectl" "$KUBECTL" "$kubectl_sum"
HELM=$(make -s -C "$ROOT" print-helm)

# forward port-forwards a namespace's service port to a free local port,
# in the background until the script exits, and sets the variable named
# first to the local port. It runs in this shell, not a subshell, so that
# the exit trap can stop the port-forward.
forward() {
  local var=$1 ns=$2 svc=$3 remote=$4 out=$WORK/forward-$2-$3.log
  # The file is there before sed reads it; kubectl itself, not a shell
  # function's subshell, is what $! names and the trap kills.
  : >"$out"
  "$KUBECTL" -n "$ns" port-forward "svc/$svc" ":$remote" >"$out" 2>&1 &
  pids+=($!)
  for _ in $(seq 1 30); do
    local got
    got=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$out" | head -n 1)
    if [ -n "$got" ]; then printf -v "$var" '%s' "$got"; return 0; fi
    sleep 1
  done
  cat "$out" >&2
  fail "port-forward to $ns/$svc"
}

# wait_for retries a command for up to a minute.
wait_for() {
  local what=$1; shift
  for _ in $(seq 1 60); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  fail "$what"
}

log "kind cluster $CLUSTER"
"$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
"$KIND" create cluster --name "$CLUSTER" --image "$KIND_NODE" --config "$HERE/kind.yaml" --wait 120s
# Every image is loaded from the host, the dependencies' too, so that no
# pull from inside the node meets a registry's rate limit.
# A pulled image is a multi-platform index with one platform's content,
# which kind cannot load: it goes in as an archive of the node's platform.
for img in "$REGISTRY/cadre-server:$TAG" "$REGISTRY/cadre-runner:$TAG"; do
  "$KIND" load docker-image --name "$CLUSTER" "$img"
done
for img in $(sed -n 's/^ *image: *//p' "$HERE/seaweedfs.yaml" "$HERE/dex.yaml"); do
  docker pull -q --platform "linux/$arch" "$img" >/dev/null
  docker save --platform "linux/$arch" -o "$WORK/image.tar" "$img"
  "$KIND" load image-archive --name "$CLUSTER" "$WORK/image.tar"
  rm -f "$WORK/image.tar"
done

log "SeaweedFS and Dex"
k apply -f "$HERE/seaweedfs.yaml" -f "$HERE/dex.yaml"
k -n storage rollout status deploy/seaweedfs --timeout="$TIMEOUT"
k -n dex rollout status deploy/dex --timeout="$TIMEOUT"
forward s3 storage seaweedfs 8333
wait_for "creating the bucket" curl -fsS -X PUT "http://localhost:$s3/cadre"

log "helm install"
k create namespace cadre-system
k -n cadre-system create secret generic cadre-oidc --from-literal=client-secret=cadre-e2e-not-a-secret
k -n cadre-system create secret generic cadre-s3 --from-literal=access-key=e2e --from-literal=secret-key=e2e-not-a-secret
"$HELM" install cadre "$ROOT/charts/cadre" -n cadre-system -f "$HERE/values.yaml" --set image.registry="$REGISTRY" --set image.tag="$TAG"
k -n cadre-system rollout status deploy/cadre-server --timeout="$TIMEOUT"
k -n cadre-system rollout status deploy/cadre-runner --timeout="$TIMEOUT"

log "API and UI"
forward port cadre-system cadre-server 8080
server=http://localhost:$port
forward port dex dex 5556
dex=http://localhost:$port
wait_for "cadre-server's /readyz" curl -fsS "$server/readyz"
wait_for "Dex" curl -fsS "$dex/dex/.well-known/openid-configuration"

# An ID token for dev@example.test, by the password grant: no browser.
token=$(curl -fsS "$dex/dex/token" \
  -d grant_type=password -d client_id=cadre-cli \
  --data-urlencode "scope=openid profile email groups" \
  -d username=dev@example.test -d password=password | jq -r .id_token)
[ -n "$token" ] && [ "$token" != null ] || fail "no ID token from Dex"

api() { curl -fsS -H "Authorization: Bearer $token" -H "Content-Type: application/json" "$@"; }

me=$(api $server/api/v1/me)
[ "$(jq -r .username <<<"$me")" = dev@example.test ] || fail "GET /me: $me"
[ "$(jq -r .admin <<<"$me")" = true ] || fail "GET /me is not an admin: $me"

k create namespace team-a
api -X PUT $server/api/v1/namespaces/team-a/team -d '{"displayName":"Team A"}' >/dev/null
created=$(api -X POST $server/api/v1/namespaces/team-a/tasks -d '{"spec":{"title":"Prove the install"}}')
name=$(jq -r .metadata.name <<<"$created")
[ -n "$name" ] && [ "$name" != null ] || fail "POST /tasks: $created"

# Numbered by the Task Manager a moment after it is created.
listed() {
  api $server/api/v1/namespaces/team-a/tasks |
    jq -e --arg name "$name" '.items[] | select(.metadata.name == $name and .status.number > 0)'
}
wait_for "task $name listed with a number" listed

for path in / /t/team-a; do
  type=$(curl -fsS -o /dev/null -w '%{content_type}' "$server$path")
  [[ $type == text/html* ]] || fail "GET $path: $type"
done
status=$(curl -sS -o /dev/null -w '%{http_code}' $server/api/v1/namespaces/team-a/tasks)
[ "$status" = 401 ] || fail "GET /tasks without a token: $status"

passed=1
log "ok: the chart installs, logs in, and serves the API and the UI"
