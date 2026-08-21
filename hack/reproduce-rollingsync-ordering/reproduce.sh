#!/usr/bin/env bash
#
# Reproduce the RollingSync out-of-order rollout against a live cluster.
#
# The ApplicationSet controller releases progressive-sync step N+1 as soon as every Application in
# step N reads Healthy in the ApplicationSet status, without checking which revision that Healthy
# belongs to. An Application the Application controller has not refreshed yet still reports Synced
# against the previous commit, so its progressive-sync status stays the Healthy it earned in the
# previous rollout and the gate releases the next step.
#
# In production that is a sub-second race between two refreshes. This script does not fake the bug:
# it removes the need to win the race, by disabling periodic refresh and then refreshing the step 2
# Application only, gating on the revision that Application reports rather than on a sleep.
#
# See README.md for prerequisites, the mechanism, and how to read the output.
#
# shellcheck disable=SC2329  # the cleanup trap and the poll predicates are all called indirectly
set -euo pipefail

NAMESPACE="${NAMESPACE:-argocd}"
APPSET_NAME="${APPSET_NAME:-rollingsync-ordering-repro}"
APPSET_DEPLOY="${APPSET_DEPLOY:-argocd-applicationset-controller}"
STEP1_APP="${STEP1_APP:-rollingsync-repro-beta}"
STEP2_APP="${STEP2_APP:-rollingsync-repro-web}"
REPO_URL="${REPO_URL:-}"
REPO_BRANCH="${REPO_BRANCH:-main}"
BASE_TAG="${BASE_TAG:-rollingsync-repro-base}"
TIMEOUT="${TIMEOUT:-300}"
KEEP="${KEEP:-0}"

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
WORKDIR=""
APP_CONTROLLER=""
PREVIOUS_RECONCILIATION_TIMEOUT=""
RECONCILIATION_TIMEOUT_PATCHED=0

log() { printf '\n== %s\n' "$*"; }
info() { printf '   %s\n' "$*"; }
die() {
	printf '\nERROR: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Usage: REPO_URL=<git url you can push to> hack/reproduce-rollingsync-ordering/reproduce.sh

Environment:
  REPO_URL     (required) git repository the reproduction commits to. Any repository you can push to
               will do; the script only adds base/ and manifests/ and moves one branch.
  REPO_BRANCH  branch the moving source tracks (default: main)
  BASE_TAG     tag the pinned source is nailed to (default: rollingsync-repro-base)
  NAMESPACE    namespace Argo CD runs in (default: argocd)
  TIMEOUT      seconds to wait for each observation (default: 300)
  KEEP         1 to leave the ApplicationSet and its Applications behind (default: 0)

The script restores argocd-cm's timeout.reconciliation and restarts the Application controller on
exit, including on failure.
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
	usage
	exit 0
fi

kc() { kubectl -n "$NAMESPACE" "$@"; }

# app_field <application> <jsonpath body> -- the Application's own status
app_field() {
	kc get application "$1" -o jsonpath="{$2}" 2>/dev/null || true
}

# appset_field <application> <status field> -- the ApplicationSet's view of one Application
appset_field() {
	kc get applicationset "$APPSET_NAME" \
		-o jsonpath="{.status.applicationStatus[?(@.application==\"$1\")].$2}" 2>/dev/null || true
}

app_revisions() { app_field "$1" '.status.sync.revisions[*]'; }
app_operation() { app_field "$1" '.operation.initiatedBy.username'; }

or_none() { printf '%s' "${1:-<none>}"; }

# poll <timeout seconds> <description> <predicate...>
poll() {
	local timeout="$1" desc="$2" deadline
	shift 2
	deadline=$((SECONDS + timeout))
	while true; do
		if "$@"; then
			return 0
		fi
		if ((SECONDS >= deadline)); then
			die "timed out after ${timeout}s waiting for ${desc}"
		fi
		sleep 2
	done
}

cleanup() {
	local status=$?
	set +e
	if [[ "$KEEP" != "1" ]]; then
		log "cleanup: deleting the ApplicationSet (KEEP=1 to keep it)"
		kc delete applicationset "$APPSET_NAME" --ignore-not-found --wait=false
	fi
	if [[ "$RECONCILIATION_TIMEOUT_PATCHED" == "1" ]]; then
		log "cleanup: restoring timeout.reconciliation and restarting the Application controller"
		if [[ -n "$PREVIOUS_RECONCILIATION_TIMEOUT" ]]; then
			kc patch configmap argocd-cm --type merge \
				-p "{\"data\":{\"timeout.reconciliation\":\"${PREVIOUS_RECONCILIATION_TIMEOUT}\"}}"
		else
			kc patch configmap argocd-cm --type json \
				-p '[{"op":"remove","path":"/data/timeout.reconciliation"}]'
		fi
		if [[ -n "$APP_CONTROLLER" ]]; then
			kc rollout restart "$APP_CONTROLLER"
		fi
	fi
	if [[ -n "$WORKDIR" ]]; then
		rm -rf "$WORKDIR"
	fi
	exit "$status"
}

# --------------------------------------------------------------------------------------------------
# 0. preflight
# --------------------------------------------------------------------------------------------------
log "0. preflight"
if [[ -z "$REPO_URL" ]]; then
	usage >&2
	die "REPO_URL is required"
fi
command -v kubectl >/dev/null || die "kubectl is not on PATH"
command -v git >/dev/null || die "git is not on PATH"
kubectl get namespace "$NAMESPACE" >/dev/null || die "namespace ${NAMESPACE} does not exist"
kc get deployment "$APPSET_DEPLOY" >/dev/null || die "no ${APPSET_DEPLOY} deployment in ${NAMESPACE}"

if kc get statefulset argocd-application-controller >/dev/null 2>&1; then
	APP_CONTROLLER="statefulset/argocd-application-controller"
elif kc get deployment argocd-application-controller >/dev/null 2>&1; then
	APP_CONTROLLER="deployment/argocd-application-controller"
else
	die "no argocd-application-controller StatefulSet or Deployment in ${NAMESPACE}"
fi
info "Application controller: ${APP_CONTROLLER}"
info "if a webhook is configured for ${REPO_URL}, remove it before running this: a webhook refreshes"
info "both Applications at once, which puts the race back."

WORKDIR="$(mktemp -d)"
trap cleanup EXIT

# --------------------------------------------------------------------------------------------------
# 1. seed the repository
# --------------------------------------------------------------------------------------------------
log "1. seeding ${REPO_URL} (branch ${REPO_BRANCH}, tag ${BASE_TAG})"
git clone --quiet "$REPO_URL" "$WORKDIR/repo"
git -C "$WORKDIR/repo" config user.email "rollingsync-repro@example.com"
git -C "$WORKDIR/repo" config user.name "rollingsync ordering reproduction"
if git -C "$WORKDIR/repo" rev-parse --verify --quiet "origin/$REPO_BRANCH" >/dev/null; then
	git -C "$WORKDIR/repo" checkout --quiet -B "$REPO_BRANCH" "origin/$REPO_BRANCH"
else
	git -C "$WORKDIR/repo" checkout --quiet -B "$REPO_BRANCH"
fi

mkdir -p "$WORKDIR/repo/base" "$WORKDIR/repo/manifests"
cat >"$WORKDIR/repo/base/configmap.yaml" <<'EOF'
# The pinned source: nailed to a tag, so its resolved revision never moves.
apiVersion: v1
kind: ConfigMap
metadata:
  name: rollingsync-repro-base
data:
  role: pinned-source
EOF
cat >"$WORKDIR/repo/manifests/configmap.yaml" <<'EOF'
# The moving source: this file is what the second commit changes.
apiVersion: v1
kind: ConfigMap
metadata:
  name: rollingsync-repro-rollout
data:
  rollout: "1"
EOF
git -C "$WORKDIR/repo" add base manifests
if ! git -C "$WORKDIR/repo" diff --cached --quiet; then
	git -C "$WORKDIR/repo" -c commit.gpgSign=false commit --quiet -m "rollingsync ordering reproduction: rollout 1"
fi
git -C "$WORKDIR/repo" push --quiet origin "$REPO_BRANCH"
if ! git -C "$WORKDIR/repo" rev-parse --verify --quiet "refs/tags/$BASE_TAG" >/dev/null; then
	# -a with an explicit message and signing off: a repository or user config that turns on
	# tag.gpgSign or tag.forceSignAnnotated otherwise breaks a bare "git tag".
	git -C "$WORKDIR/repo" -c tag.gpgSign=false tag -a -m "rollingsync ordering reproduction base" "$BASE_TAG"
	git -C "$WORKDIR/repo" push --quiet origin "refs/tags/$BASE_TAG"
fi
REVISION_A="$(git -C "$WORKDIR/repo" rev-parse HEAD)"
info "branch ${REPO_BRANCH} is at ${REVISION_A}"

# --------------------------------------------------------------------------------------------------
# 2. first rollout
# --------------------------------------------------------------------------------------------------
log "2. applying the ApplicationSet and waiting for the first rollout to finish"
sed -e "s|__REPO_URL__|${REPO_URL}|g" -e "s|__REPO_BRANCH__|${REPO_BRANCH}|g" \
	"$HERE/applicationset.yaml" | kc apply -f -

appset_status_populated() { [[ -n "$(appset_field "$STEP1_APP" status)" ]]; }
poll 120 "the ApplicationSet to report a progressive-sync status (is --enable-progressive-syncs set?)" \
	appset_status_populated

both_healthy() {
	[[ "$(appset_field "$STEP1_APP" status)" == "Healthy" ]] &&
		[[ "$(appset_field "$STEP2_APP" status)" == "Healthy" ]]
}
poll "$TIMEOUT" "both Applications to reach Healthy" both_healthy
info "${STEP1_APP}: $(app_revisions "$STEP1_APP")"
info "${STEP2_APP}: $(app_revisions "$STEP2_APP")"

# --------------------------------------------------------------------------------------------------
# 3. take the clock out of the picture
# --------------------------------------------------------------------------------------------------
# Done after the first rollout and before the commit that matters: the restart re-enqueues every
# Application for a refresh, which is harmless while the branch has not moved yet and fatal to the
# reproduction if it happened after.
log "3. disabling periodic Application refresh"
PREVIOUS_RECONCILIATION_TIMEOUT="$(kc get configmap argocd-cm -o jsonpath='{.data.timeout\.reconciliation}' 2>/dev/null || true)"
info "current timeout.reconciliation: ${PREVIOUS_RECONCILIATION_TIMEOUT:-<unset, upstream default 180s>}"
kc patch configmap argocd-cm --type merge -p '{"data":{"timeout.reconciliation":"0s"}}' >/dev/null
RECONCILIATION_TIMEOUT_PATCHED=1
kc rollout restart "$APP_CONTROLLER" >/dev/null
kc rollout status "$APP_CONTROLLER" --timeout="${TIMEOUT}s"
info "0s disables automatic polling: from here an Application refreshes only when told to."

# --------------------------------------------------------------------------------------------------
# 4. move the branch
# --------------------------------------------------------------------------------------------------
log "4. pushing a new commit to ${REPO_BRANCH}"
cat >"$WORKDIR/repo/manifests/configmap.yaml" <<EOF
# The moving source: this file is what the second commit changes.
apiVersion: v1
kind: ConfigMap
metadata:
  name: rollingsync-repro-rollout
data:
  rollout: "$(date -u +%Y%m%d%H%M%S)"
EOF
git -C "$WORKDIR/repo" -c commit.gpgSign=false commit --quiet -am "rollingsync ordering reproduction: rollout 2"
git -C "$WORKDIR/repo" push --quiet origin "$REPO_BRANCH"
REVISION_B="$(git -C "$WORKDIR/repo" rev-parse HEAD)"
info "branch ${REPO_BRANCH} moved ${REVISION_A} -> ${REVISION_B}"

for app in "$STEP1_APP" "$STEP2_APP"; do
	observed="$(app_revisions "$app")"
	case "$observed" in
	*"$REVISION_B"*)
		die "${app} already observed ${REVISION_B}: periodic refresh or a webhook is still active"
		;;
	*)
		info "${app} has not noticed the commit: ${observed}"
		;;
	esac
done

# --------------------------------------------------------------------------------------------------
# 5. inject the skew: refresh step 2, and only step 2
# --------------------------------------------------------------------------------------------------
log "5. refreshing the step 2 Application only"
step2_observed_new_revision() {
	case "$(app_revisions "$STEP2_APP")" in
	*"$REVISION_B"*) return 0 ;;
	esac
	# A resolved revision can be served from the repo-server's revision cache (default 3m), so one
	# refresh is not always enough. The Application controller deletes the annotation when it
	# consumes it, so its absence is the signal to ask again. "hard" also skips the manifest cache.
	if [[ -z "$(app_field "$STEP2_APP" '.metadata.annotations.argocd\.argoproj\.io/refresh')" ]]; then
		kc annotate application "$STEP2_APP" argocd.argoproj.io/refresh=hard --overwrite >/dev/null
	fi
	return 1
}
poll "$TIMEOUT" "the step 2 Application to observe ${REVISION_B}" step2_observed_new_revision

STEP1_REVISIONS="$(app_revisions "$STEP1_APP")"
case "$STEP1_REVISIONS" in
*"$REVISION_B"*)
	die "step 1 observed ${REVISION_B} as well: the skew was not injected, there is nothing to reproduce"
	;;
esac
info "skew is in place:"
info "  ${STEP1_APP}: $(app_field "$STEP1_APP" '.status.sync.status') ${STEP1_REVISIONS}"
info "  ${STEP2_APP}: $(app_field "$STEP2_APP" '.status.sync.status') $(app_revisions "$STEP2_APP")"

# --------------------------------------------------------------------------------------------------
# 6. verdict
# --------------------------------------------------------------------------------------------------
# The first rollout left an operationState on both Applications, so only a CHANGE to it is evidence
# that a new sync started. Take the baseline before anything can move.
STEP2_OPERATION_BASELINE="$(app_field "$STEP2_APP" '.status.operationState.startedAt')"

# The step 2 Application's sync-status change already queues an ApplicationSet reconcile, but a list
# generator has no periodic requeue, so nudge the ApplicationSet as well rather than depend on an
# event. This changes nothing about the decision under test: the controller re-runs the same gate
# over the same objects.
kc annotate applicationset "$APPSET_NAME" \
	"rollingsync-repro/nudge=$(date -u +%Y%m%d%H%M%S)" --overwrite >/dev/null

log "6. watching whether step 2 is released while step 1 is still stale"
step2_started_rollout() {
	local started
	if [[ -n "$(app_operation "$STEP2_APP")" ]]; then
		return 0
	fi
	started="$(app_field "$STEP2_APP" '.status.operationState.startedAt')"
	if [[ -n "$started" && "$started" != "$STEP2_OPERATION_BASELINE" ]]; then
		return 0
	fi
	case "$(appset_field "$STEP2_APP" status)" in
	Pending | Progressing) return 0 ;;
	*) return 1 ;;
	esac
}

REPRODUCED=0
DEADLINE=$((SECONDS + TIMEOUT))
while ((SECONDS < DEADLINE)); do
	if step2_started_rollout; then
		REPRODUCED=1
		break
	fi
	sleep 2
done

STEP1_APPSET_STATUS="$(appset_field "$STEP1_APP" status)"
STEP1_APPSET_REVISIONS="$(appset_field "$STEP1_APP" 'targetRevisions[*]')"
STEP2_APPSET_STATUS="$(appset_field "$STEP2_APP" status)"
STEP2_APPSET_REVISIONS="$(appset_field "$STEP2_APP" 'targetRevisions[*]')"

printf '\nApplicationSet status\n'
printf '  %-26s %-12s %s\n' "$STEP1_APP" "$STEP1_APPSET_STATUS" "$STEP1_APPSET_REVISIONS"
printf '  %-26s %-12s %s\n' "$STEP2_APP" "$STEP2_APPSET_STATUS" "$STEP2_APPSET_REVISIONS"
printf 'Applications\n'
printf '  %-26s operation=%-26s revisions=%s\n' \
	"$STEP1_APP" "$(or_none "$(app_operation "$STEP1_APP")")" "$(app_revisions "$STEP1_APP")"
printf '  %-26s operation=%-26s revisions=%s\n' \
	"$STEP2_APP" "$(or_none "$(app_operation "$STEP2_APP")")" "$(app_revisions "$STEP2_APP")"

log "ApplicationSet controller log"
kc logs "deployment/$APPSET_DEPLOY" --tail=4000 2>/dev/null |
	grep -E 'allowed to sync before maxUpdate|triggering sync for application|Progressive sync application changed status|Holding next progressive sync wave' |
	tail -40 || info "no matching log lines found"

if [[ "$REPRODUCED" == "1" ]]; then
	cat <<EOF

BUG REPRODUCED.

  ${STEP2_APP} (step 2) has been released and its rollout of ${REVISION_B} has started, while
  ${STEP1_APP} (step 1) still reports ${STEP1_REVISIONS} and the ApplicationSet still reads
  ${STEP1_APPSET_STATUS} for it at ${STEP1_APPSET_REVISIONS}.

  In the log above, "Application allowed to sync before maxUpdate?" names BOTH Applications, and
  "triggering sync for application" names the step 2 Application only. The step 1 Application
  produces no status line at all: an Application whose progressive-sync status is left untouched is
  silent, so its absence from the log is part of the signature.
EOF
	exit 0
fi

cat <<EOF

NOT REPRODUCED: step 2 was withheld for ${TIMEOUT}s while step 1 was stale.

  That is the correct behaviour, and it is what a revision-aware gate produces. Look for
  "Holding next progressive sync wave" in the log above to confirm the gate is what withheld it,
  rather than the reproduction failing to set itself up: step 1 must read Healthy at
  ${STEP1_APPSET_REVISIONS} and step 2 must read Waiting for this run to have proved anything.
EOF
exit 0
