#!/usr/bin/env bash
# Shared settings and component launchers for the live RollingSync ordering experiment.
# Source this file; do not execute it. See README.md.
#
# Everything resolves from this file's own location, so the harness can live anywhere in a checkout
# and be run from any working directory. Output goes into gitignored directories beside this file
# (see .gitignore); nothing else is written except /tmp/argocd-local.

# Directory holding this script, and the checkout it belongs to.
E2E="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(git -C "$E2E" rev-parse --show-toplevel)"

# Run output and build artifacts. All gitignored; `down.sh --purge` removes them.
LOGS="$E2E/logs"
BIN="$E2E/bin"
RUN="$E2E/run"
GITROOT="$E2E/gitroot"
RUNS="$E2E/runs"

# The two source trees the experiment compares.
#   BASELINE_TREE: the controller WITHOUT the fix, built as `argocd-master`. Defaults to this
#                  checkout, which is correct on a branch that changes no controller code.
#   FIX_TREE:      the controller WITH the fix, built as `argocd-fix1`. No default: the fix does not
#                  live on this branch, so there is nothing honest to default to. `reproduce.sh fix1`
#                  refuses with instructions when it is unset.
# "master" and "fix1" are labels for the two binaries; either tree can be any ref.
BASELINE_TREE="${BASELINE_TREE:-$REPO}"
FIX_TREE="${FIX_TREE:-}"

# Ports. GIT_PORT is substituted into manifests/appset.yaml at apply time, so changing it here is
# enough -- the manifest is not the source of truth for the port.
GIT_PORT="${GIT_PORT:-9080}"
REPO_SERVER_PORT="${REPO_SERVER_PORT:-8081}"
REDIS_PORT="${REDIS_PORT:-6379}"

# The Application controller's own periodic refresh, in seconds. The knob is the --app-resync flag,
# NOT timeout.reconciliation in argocd-cm: run as a local process the controller never reads that
# ConfigMap key (the shipped manifests map it into ARGOCD_RECONCILIATION_TIMEOUT through the
# StatefulSet env, which does not exist here). Left at its 120s default the controller refreshes the
# lagging Application on its own and releases the hold without the experiment asking, which makes the
# result meaningless -- so it is pushed far out of the way. EVIDENCE.md records how that cost a run.
APP_RESYNC_SECONDS="${APP_RESYNC_SECONDS:-3600}"

# The k3d cluster up.sh creates if it is absent.
CLUSTER="${CLUSTER:-argocd-repro}"

export E2E REPO LOGS BIN RUN GITROOT RUNS
export BASELINE_TREE FIX_TREE GIT_PORT REPO_SERVER_PORT REDIS_PORT APP_RESYNC_SECONDS CLUSTER

mkdir -p "$LOGS" "$BIN" "$RUN" "$RUNS"

# Env shared by every locally-run Argo CD component (mirrors the repo Procfile).
common_env=(
  ARGOCD_FAKE_IN_CLUSTER=true
  ARGOCD_TLS_DATA_PATH=/tmp/argocd-local/tls
  ARGOCD_SSH_DATA_PATH=/tmp/argocd-local/ssh
)

# Start a fully detached process, recording its real pid (not setsid's) in $1.
# usage: _launch <pidfile> <logfile> <cmd> [args...]
_launch() {
  local pidfile="$1" logfile="$2"
  shift 2
  # Single quotes are required: $$ and $@ must be evaluated by the inner shell, which records its
  # own pid and then execs, so the pid file names the component and not the setsid wrapper.
  # shellcheck disable=SC2016
  setsid nohup bash -c 'echo $$ >"$1"; shift; exec "$@"' _ "$pidfile" "$@" \
    >"$logfile" 2>&1 &
  disown
}

# usage: stop_component <name>   (name = pid file basename without .pid)
stop_component() {
  local pidfile="$RUN/$1.pid" p
  [ -f "$pidfile" ] || return 0
  p="$(cat "$pidfile")"
  if kill -0 "$p" 2>/dev/null; then
    kill "$p" 2>/dev/null || true
    for _ in $(seq 1 20); do
      kill -0 "$p" 2>/dev/null || break
      sleep 0.5
    done
    kill -9 "$p" 2>/dev/null || true
  fi
  rm -f "$pidfile"
}

start_git_server() {
  _launch "$RUN/git-server.pid" "$LOGS/git-server.log" \
    python3 "$E2E/git-smart-http.py" "$GITROOT" "$GIT_PORT"
}

start_repo_server() {
  local binary="$1"
  _launch "$RUN/repo-server.pid" "$LOGS/repo-server.log" \
    env "${common_env[@]}" \
      PATH="$BIN:$PATH" \
      ARGOCD_GNUPGHOME=/tmp/argocd-local/gpg/keys \
      ARGOCD_GPG_DATA_PATH=/tmp/argocd-local/gpg/source \
      ARGOCD_GPG_ENABLED=false \
      ARGOCD_PLUGINSOCKFILEPATH="$E2E/cmp" \
      ARGOCD_BINARY_NAME=argocd-repo-server \
      "$binary" --loglevel debug --port "$REPO_SERVER_PORT" --redis "localhost:$REDIS_PORT"
}

start_app_controller() {
  local binary="$1"
  _launch "$RUN/application-controller.pid" "$LOGS/application-controller.log" \
    env "${common_env[@]}" \
      HOSTNAME=testappcontroller-1 \
      ARGOCD_BINARY_NAME=argocd-application-controller \
      "$binary" --loglevel debug --redis "localhost:$REDIS_PORT" \
        --repo-server "localhost:$REPO_SERVER_PORT" \
        --server-side-diff-enabled=false --hydrator-enabled=false \
        --app-resync "$APP_RESYNC_SECONDS" --app-resync-jitter 0
}

# $1 = binary path, $2 = log file basename (master and fix1 runs keep separate logs)
start_appset_controller() {
  local binary="$1" logname="${2:-applicationset-controller}"
  _launch "$RUN/applicationset-controller.pid" "$LOGS/$logname.log" \
    env "${common_env[@]}" \
      ARGOCD_BINARY_NAME=argocd-applicationset-controller \
      ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_PROGRESSIVE_SYNCS=true \
      "$binary" --loglevel debug \
        --metrics-addr localhost:12345 --probe-addr localhost:12346 \
        --webhook-addr localhost:7001 \
        --argocd-repo-server "localhost:$REPO_SERVER_PORT"
}

# Publish the current master of the working tree through the bare repo.
git_publish() {
  git -C "$GITROOT/work" push -q "$GITROOT/repo.git" master --force
  git -C "$GITROOT/repo.git" update-server-info
}

# Render the ApplicationSet with the configured git port substituted in.
appset_manifest() {
  sed "s|http://127\.0\.0\.1:9080|http://127.0.0.1:$GIT_PORT|g" "$E2E/manifests/appset.yaml"
}

# Print "<revisions> <sync> <health>" for an Application.
app_state() {
  kubectl -n argocd get app "$1" \
    -o jsonpath='{.status.sync.revisions}{" "}{.status.sync.status}{" "}{.status.health.status}' 2>/dev/null
}
