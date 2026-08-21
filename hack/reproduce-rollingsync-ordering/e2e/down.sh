#!/usr/bin/env bash
# Stop everything up.sh started and remove what it created in the cluster. Leaves the k3d cluster
# and the built binaries alone; pass --purge to drop those too.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source-path=SCRIPTDIR
# shellcheck source=lib.sh
source "$HERE/lib.sh"

for c in applicationset-controller application-controller repo-server git-server redis; do
  echo "stopping $c"
  stop_component "$c"
done
docker rm -f argocd-redis >/dev/null 2>&1

appset_manifest | kubectl delete -f - --ignore-not-found >/dev/null 2>&1
kubectl -n argocd delete app --all --ignore-not-found >/dev/null 2>&1
for ns in e2e-beta e2e-web argocd; do
  kubectl delete namespace "$ns" --ignore-not-found --wait=false >/dev/null 2>&1
done

if [ "${1:-}" = --purge ]; then
  echo "purging run output and build artifacts"
  rm -rf "$BIN" "$GITROOT" "$LOGS" "$RUN" "$RUNS" "$E2E/cmp" "$E2E/state"
  # The cluster is left alone by default: creating it is the slowest part of up.sh, and a stale
  # cluster is harmless. DELETE_CLUSTER=1 removes it too.
  if [ "${DELETE_CLUSTER:-0}" = 1 ]; then
    echo "deleting k3d cluster $CLUSTER"
    k3d cluster delete "$CLUSTER" >/dev/null 2>&1
  fi
fi
echo "down."
