#!/usr/bin/env bash
# Bring up everything the live RollingSync ordering experiment needs: a k3d cluster, a git server, the
# Argo CD components as local processes, and a two-step RollingSync ApplicationSet with both
# Applications Healthy.
#
# Shape follows docs/developer-guide/running-locally.md: the cluster holds only CRDs, RBAC, Secrets
# and ConfigMaps, and the components run as local processes against it. Swapping the unfixed
# controller for the fixed one is then a binary swap rather than an image build.
#
# See README.md for prerequisites and runtime.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source-path=SCRIPTDIR
# shellcheck source=lib.sh
source "$HERE/lib.sh"

say() { echo "[$(date +%H:%M:%S)] $*"; }
die() { echo "FATAL: $*" >&2; exit 1; }

for tool in kubectl k3d go git python3 docker; do
  command -v "$tool" >/dev/null 2>&1 || die "missing required tool: $tool"
done

########## 0. cluster ##########
if ! k3d cluster list "$CLUSTER" >/dev/null 2>&1; then
  say "creating k3d cluster $CLUSTER"
  # KubeletInUserNamespace is required under rootless Docker; harmless otherwise.
  k3d cluster create "$CLUSTER" \
    --k3s-arg '--kubelet-arg=feature-gates=KubeletInUserNamespace=true@server:*' \
    || die "k3d cluster create failed"
else
  say "reusing existing k3d cluster $CLUSTER"
fi
kubectl config use-context "k3d-$CLUSTER" >/dev/null 2>&1
kubectl get nodes >/dev/null 2>&1 || die "cluster $CLUSTER is not reachable"
say "cluster: $(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion}')"

########## 1. cluster-side install, workloads scaled to zero ##########
say "installing Argo CD manifests (CRDs, RBAC, ConfigMaps, Secrets)"
kubectl create namespace argocd >/dev/null 2>&1
kubectl apply -n argocd --server-side --force-conflicts -f "$REPO/manifests/install.yaml" >/dev/null \
  || die "manifest apply failed"
kubectl -n argocd scale deploy --all --replicas=0 >/dev/null
kubectl -n argocd scale statefulset --all --replicas=0 >/dev/null
kubectl config set-context --current --namespace=argocd >/dev/null

# manifests/install.yaml ships no AppProject object -- argocd-server normally creates it, in
# initializeDefaultProject (server/server.go:293), and argocd-server is deliberately not run here.
# This spec is field-for-field what that function creates.
kubectl apply -f - >/dev/null <<'EOF'
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: default
  namespace: argocd
spec:
  sourceRepos: ['*']
  destinations:
    - namespace: '*'
      server: '*'
  clusterResourceWhitelist:
    - group: '*'
      kind: '*'
EOF

# Kept for the record, but do NOT rely on it: run as a local process the Application controller never
# reads this key. Its real knob is --app-resync, set from APP_RESYNC_SECONDS in lib.sh.
kubectl -n argocd patch cm argocd-cm --type merge \
  -p '{"data":{"timeout.reconciliation":"1h"}}' >/dev/null

for ns in e2e-beta e2e-web; do kubectl create namespace "$ns" >/dev/null 2>&1; done

########## 2. the git repo the Applications track ##########
say "creating git repo and starting the smart-HTTP git server on :$GIT_PORT"
rm -rf "$GITROOT"
mkdir -p "$GITROOT"
git init -q -b master "$GITROOT/work"
git -C "$GITROOT/work" config user.email e2e@example.com
git -C "$GITROOT/work" config user.name e2e
mkdir -p "$GITROOT/work/base" "$GITROOT/work/config/beta" "$GITROOT/work/config/web"

cat > "$GITROOT/work/base/cm.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: base-cm
data:
  chart: "v1"
EOF
for name in beta web; do
  cat > "$GITROOT/work/config/$name/cm.yaml" <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: $name-config
data:
  release: "r1"
EOF
done

git -C "$GITROOT/work" add -A
git -C "$GITROOT/work" commit -q -m "initial"
git -C "$GITROOT/work" tag -a v1 -m v1
git clone -q --bare "$GITROOT/work" "$GITROOT/repo.git"
git -C "$GITROOT/work" push -q "$GITROOT/repo.git" v1
git -C "$GITROOT/repo.git" update-server-info

stop_component git-server
start_git_server
sleep 2
git ls-remote "http://127.0.0.1:$GIT_PORT/repo.git" >/dev/null 2>&1 \
  || die "git server not answering. Note go-git needs the SMART protocol; a static file server is not enough"

########## 3. binaries ##########
build_variant argocd-master "$BASELINE_TREE"
if [ -n "$FIX_TREE" ]; then
  build_variant argocd-fix1 "$FIX_TREE"
fi
if [ -z "$FIX_TREE" ]; then
  say "note: FIX_TREE is unset, so only 'reproduce.sh master' can run; see README.md"
fi

# The repo-server shells out to this during manifest generation even with ARGOCD_GPG_ENABLED=false
# (reposerver/repository/repository.go:549 calls VerifyCommitSignature unconditionally). Absent from
# PATH, every GenerateManifest fails with a bare "permission denied".
cp "$REPO/hack/git-verify-wrapper.sh" "$BIN/"
chmod +x "$BIN/git-verify-wrapper.sh"

########## 4. long-running components ##########
say "starting redis, repo-server, application-controller"
docker rm -f argocd-redis >/dev/null 2>&1
mkdir -p /tmp/argocd-local/tls /tmp/argocd-local/ssh /tmp/argocd-local/gpg/keys /tmp/argocd-local/gpg/source "$E2E/cmp"
_launch "$RUN/redis.pid" "$LOGS/redis.log" \
  env ARGOCD_E2E_REDIS_PORT="$REDIS_PORT" bash "$REPO/hack/start-redis-with-password.sh"
for _ in $(seq 1 30); do
  docker ps --format '{{.Names}}' | grep -qx argocd-redis && break
  sleep 2
done
docker ps --format '{{.Names}}' | grep -qx argocd-redis || die "redis did not start"

stop_component repo-server
start_repo_server "$BIN/argocd-master"
for _ in $(seq 1 30); do grep -q "is listening" "$LOGS/repo-server.log" && break; sleep 1; done
grep -q "is listening" "$LOGS/repo-server.log" || die "repo-server did not come up"

stop_component application-controller
start_app_controller "$BIN/argocd-master"
sleep 10
grep -m1 "appResyncPeriod" "$LOGS/application-controller.log" \
  || die "application controller did not log its resync period"

########## 5. sanity gate: one trivial Application must reach Synced/Healthy ##########
say "sanity gate: single-source Application must reach Synced/Healthy"
kubectl apply -f - >/dev/null <<EOF
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: sanity
  namespace: argocd
spec:
  project: default
  source:
    repoURL: http://127.0.0.1:$GIT_PORT/repo.git
    targetRevision: v1
    path: base
  destination:
    server: https://kubernetes.default.svc
    namespace: e2e-beta
  syncPolicy:
    automated: {}
EOF
ok=no
for _ in $(seq 1 30); do
  [ "$(kubectl -n argocd get app sanity -o jsonpath='{.status.sync.status}/{.status.health.status}')" \
     = "Synced/Healthy" ] && { ok=yes; break; }
  sleep 5
done
kubectl -n argocd delete app sanity --wait=false >/dev/null 2>&1
if [ "$ok" != yes ]; then
  kubectl -n argocd get app sanity -o jsonpath='{.status.conditions}' 2>/dev/null
  die "sanity Application never became Synced/Healthy; fix that before running the experiment"
fi
say "sanity gate passed"

########## 6. the ApplicationSet under test ##########
say "applying the two-step RollingSync ApplicationSet"
appset_manifest | kubectl apply -f - >/dev/null

# An ApplicationSet on its own generates nothing: only the applicationset-controller creates the two
# Applications, and reproduce.sh starts its controller AFTER refreshing them, so without this a fresh
# harness would fail its first run at the baseline step with nothing to refresh. The baseline binary
# is used because every run replaces this controller with the one under test anyway.
say "starting the baseline applicationset-controller to generate the Applications"
start_appset_controller "$BIN/argocd-master" applicationset-controller-bootstrap
for _ in $(seq 1 60); do
  kubectl -n argocd get app app-beta app-web >/dev/null 2>&1 && break
  sleep 2
done
kubectl -n argocd get app app-beta app-web >/dev/null 2>&1 \
  || die "the ApplicationSet generated no Applications; see $LOGS/applicationset-controller-bootstrap.log"
say "both Applications exist"

say "up. Now run:  ./reproduce.sh master   and   ./reproduce.sh fix1"
