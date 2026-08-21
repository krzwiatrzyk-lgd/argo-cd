#!/usr/bin/env bash
# One run of the RollingSync ordering experiment against a live cluster.
#
#   reproduce.sh <master|fix1> [hold-seconds]
#
# Sequence (identical for both binaries, that is the point):
#   1. reset git to the baseline commit, refresh both Applications, wait for both Healthy on it
#   2. run the applicationset-controller from the chosen binary
#   3. publish a commit that changes step 1's AND step 2's config
#   4. refresh ONLY the step-2 Application, so step 1 still reports Synced against the old commit
#   5. watch for hold-seconds: does step 2 sync while step 1 is still on the old commit?
#   6. refresh the step-1 Application and watch the rest of the rollout
#
# The verdict is computed from the deployed ConfigMaps, not from anything this script asserts.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source-path=SCRIPTDIR
# shellcheck source=lib.sh
source "$HERE/lib.sh"

VARIANT="${1:?usage: reproduce.sh <master|fix1> [hold-seconds]}"
HOLD_SECONDS="${2:-75}"
case "$VARIANT" in
  master|fix1) ;;
  *) echo "variant must be master or fix1" >&2; exit 2 ;;
esac

BINARY="$BIN/argocd-$VARIANT"
if [ ! -x "$BINARY" ]; then
  echo "missing binary $BINARY" >&2
  if [ "$VARIANT" = fix1 ]; then
    cat >&2 <<'MSG'
The fix binary is built from FIX_TREE, which was unset when up.sh ran. The fix does not live on this
branch, so there is nothing to default to:
    git worktree add /tmp/argocd-fix fix/appset-rollingsync-revision-aware-gate
    FIX_TREE=/tmp/argocd-fix ./up.sh
MSG
  else
    echo "Run ./up.sh first." >&2
  fi
  exit 2
fi

STAMP="$(date +%Y%m%d-%H%M%S)"
RUNDIR="$RUNS/$VARIANT-$STAMP"
mkdir -p "$RUNDIR"
APPSET_LOG="appset-$VARIANT-$STAMP"

BASE_COMMIT="$(git -C "$GITROOT/work" rev-list --max-parents=0 master)"
NEW_RELEASE="rel-$STAMP"

say() { echo "[$(date +%H:%M:%S)] $*"; }
cm() { kubectl -n "e2e-$1" get cm "$1-config" -o jsonpath='{.data.release}' 2>/dev/null; }

timeline() {
  printf '%s beta{rev=%s sync/health=%s cm=%s} web{rev=%s sync/health=%s cm=%s}\n' \
    "$(date +%H:%M:%S)" \
    "$(kubectl -n argocd get app app-beta -o jsonpath='{.status.sync.revisions[1]}' 2>/dev/null | cut -c1-8)" \
    "$(kubectl -n argocd get app app-beta -o jsonpath='{.status.sync.status}/{.status.health.status}' 2>/dev/null)" \
    "$(cm beta)" \
    "$(kubectl -n argocd get app app-web -o jsonpath='{.status.sync.revisions[1]}' 2>/dev/null | cut -c1-8)" \
    "$(kubectl -n argocd get app app-web -o jsonpath='{.status.sync.status}/{.status.health.status}' 2>/dev/null)" \
    "$(cm web)"
}

########## 1. baseline ##########
say "resetting git to baseline $BASE_COMMIT"
git -C "$GITROOT/work" reset --hard "$BASE_COMMIT" >/dev/null
git_publish
kubectl -n argocd annotate app app-beta app-web argocd.argoproj.io/refresh=normal --overwrite >/dev/null

for _ in $(seq 1 40); do
  [ "$(cm beta)" = r1 ] && [ "$(cm web)" = r1 ] \
    && [ "$(kubectl -n argocd get app app-beta -o jsonpath='{.status.sync.status}')" = Synced ] \
    && [ "$(kubectl -n argocd get app app-web -o jsonpath='{.status.sync.status}')" = Synced ] \
    && break
  sleep 3
done
if [ "$(cm beta)" != r1 ] || [ "$(cm web)" != r1 ]; then
  say "FAILED to establish baseline: beta=$(cm beta) web=$(cm web)"
  exit 1
fi
say "baseline established: both Applications Healthy on $BASE_COMMIT, both ConfigMaps release=r1"
timeline | tee "$RUNDIR/timeline.txt"

########## 2. the controller under test ##########
say "starting applicationset-controller from argocd-$VARIANT"
stop_component applicationset-controller
sleep 2
start_appset_controller "$BINARY" "$APPSET_LOG"
for _ in $(seq 1 30); do
  grep -q "Starting workers" "$LOGS/$APPSET_LOG.log" && break
  sleep 1
done
sleep 5
LOG_OFFSET="$(wc -l < "$LOGS/$APPSET_LOG.log")"
{
  echo "variant=$VARIANT"
  echo "binary=$BINARY"
  echo "appset_log=$LOGS/$APPSET_LOG.log"
  echo "baseline_commit=$BASE_COMMIT"
} > "$RUNDIR/run.env"

########## 3. publish a commit touching BOTH steps ##########
cd "$GITROOT/work" || exit 1
sed -i "s/release: \"r1\"/release: \"$NEW_RELEASE\"/" config/beta/cm.yaml config/web/cm.yaml
git add -A
git commit -q -m "$NEW_RELEASE: bump beta and web config"
NEW_COMMIT="$(git rev-parse HEAD)"
git_publish
say "published $NEW_COMMIT (release=$NEW_RELEASE), touching config/beta AND config/web"
echo "new_commit=$NEW_COMMIT" >> "$RUNDIR/run.env"
echo "new_release=$NEW_RELEASE" >> "$RUNDIR/run.env"

########## 4. refresh ONLY the step-2 Application ##########
say "refreshing ONLY app-web (step 2). app-beta is left believing it is Synced on the old commit."
kubectl -n argocd annotate app app-web argocd.argoproj.io/refresh=normal --overwrite >/dev/null

########## 5. did step 2 run ahead of step 1? ##########
VIOLATION=no
for _ in $(seq 1 "$((HOLD_SECONDS / 3))"); do
  timeline | tee -a "$RUNDIR/timeline.txt"
  if [ "$(cm web)" = "$NEW_RELEASE" ] && [ "$(cm beta)" = r1 ]; then
    VIOLATION=yes
  fi
  sleep 3
done

{
  echo "### phase 1 verdict (step 2 released while step 1 still on the old commit?)"
  echo "ordering_violation=$VIOLATION"
  echo "beta_config_release=$(cm beta)   (r1 = has NOT deployed the new commit)"
  echo "web_config_release=$(cm web)"
} | tee "$RUNDIR/verdict-phase1.txt"

########## 6. release step 1 and watch the rest ##########
say "now refreshing app-beta (step 1), the event the production webhook would have delivered"
echo "--- app-beta refreshed here ---" >> "$RUNDIR/timeline.txt"
kubectl -n argocd annotate app app-beta argocd.argoproj.io/refresh=normal --overwrite >/dev/null
for _ in $(seq 1 20); do
  timeline | tee -a "$RUNDIR/timeline.txt"
  [ "$(cm beta)" = "$NEW_RELEASE" ] && [ "$(cm web)" = "$NEW_RELEASE" ] && break
  sleep 3
done

{
  echo "### phase 2 verdict (rollout completed after step 1 was refreshed?)"
  echo "beta_config_release=$(cm beta)"
  echo "web_config_release=$(cm web)"
  echo "appset applicationStatus:"
  kubectl -n argocd get appset rolling -o jsonpath='{.status.applicationStatus}' | python3 -m json.tool
} | tee "$RUNDIR/verdict-phase2.txt"

########## 7. the controller's own account ##########
tail -n +"$((LOG_OFFSET + 1))" "$LOGS/$APPSET_LOG.log" > "$RUNDIR/appset.log"
{
  echo "### gate decisions"
  grep -h "allowed to sync before maxUpdate" "$RUNDIR/appset.log" | head -4
  echo "### holds / releases (fix only)"
  grep -h -E "Holding the next progressive sync wave|Releasing the next progressive sync wave" "$RUNDIR/appset.log" | head -4
  echo "### progressive sync status transitions"
  grep -h "Progressive sync application changed status" "$RUNDIR/appset.log"
} > "$RUNDIR/appset-key-lines.log"

say "run artifacts in $RUNDIR"
