package progressivesync

import (
	"reflect"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/argoproj/argo-cd/v3/applicationset/utils"
	argov1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

const (
	// maxRevisionSkewHold bounds how long a wave is withheld. A commit that does not touch an earlier
	// step's manifest-generate-paths never refreshes that step's Application, and from here that is
	// indistinguishable from a refresh that is merely late, so the gate must not wait forever. The
	// bound also covers the reverse disagreement, which this gate cannot tell apart from the one it
	// exists for; see the Revision-aware step gating section of Progressive-Syncs.md.
	maxRevisionSkewHold = 2 * time.Minute

	// revisionSkewRequeueInterval polls while a wave is withheld, because the event that would clear
	// the hold is not guaranteed to exist: shouldRequeueForApplication ignores Status.Sync.Revision,
	// and a cluster-generated ApplicationSet has no periodic requeue (ClusterGenerator.GetRequeueAfter
	// returns NoRequeueAfter).
	revisionSkewRequeueInterval = 10 * time.Second
)

// applicationSourceKeys identifies the Git coordinates an Application resolves its revisions from,
// in spec source order. Two Applications sharing these coordinates must converge on the same
// revisions; Applications with different coordinates legitimately differ forever, so callers skip
// those pairs rather than demand a consensus that cannot arrive.
//
// The key holds only the fields that decide which commit a source resolves to. Chart and TagPrefix
// are in because they change the resolution (repoURL plus version is not a Helm source, and TagPrefix
// filters which tags a semver constraint may match). Path, Ref, Name and the per-tool blocks are out
// because they change what is rendered from a commit, not which commit is picked -- and per-cluster
// paths are the normal ApplicationSet shape.
func applicationSourceKeys(app *argov1alpha1.Application) []string {
	sources := app.Spec.GetSources()
	keys := make([]string, 0, len(sources))
	for _, source := range sources {
		// NUL separated: unlike "@" or "#" it cannot appear inside a repo URL, a git revision, a
		// chart name or a tag prefix, so no pair of distinct sources can collide on one key.
		keys = append(keys, strings.Join([]string{source.RepoURL, source.TargetRevision, source.Chart, source.TagPrefix}, "\x00"))
	}
	return keys
}

// withholdRevisionSkewedSteps removes from appsToSync every Application whose only step is later than
// the first step that disagrees with a later step about the revisions they have observed, and returns
// how long to wait before deciding again. appsToSync and a zero duration mean nothing is withheld:
// either no step disagrees, or every disagreement has outlasted maxRevisionSkewHold.
//
// getAppsToSync gates a step on ProgressiveSyncHealthy alone, and Healthy does not say which revision
// it belongs to. Filtering its result is equivalent to stopping its loop earlier: it adds steps 0..k
// for the first incomplete step k, so dropping every step after the skewed one leaves exactly what
// that loop would have produced had it stopped there.
func withholdRevisionSkewedSteps(
	logCtx *log.Entry,
	applicationSet argov1alpha1.ApplicationSet,
	appDependencyList [][]string,
	currentApplications []argov1alpha1.Application,
	appsToSync map[string]bool,
	now time.Time,
) (map[string]bool, time.Duration) {
	if len(appDependencyList) < 2 {
		return appsToSync, 0
	}

	appMap := make(map[string]*argov1alpha1.Application, len(currentApplications))
	for i := range currentApplications {
		appMap[currentApplications[i].Name] = &currentApplications[i]
	}

	for stepIndex := range len(appDependencyList) - 1 {
		stepApp, laterApp := findStepRevisionSkew(&applicationSet, appDependencyList, appMap, stepIndex)
		if stepApp == "" {
			continue
		}

		withheld := withholdStepsAfter(appDependencyList, appsToSync, stepIndex)
		if len(withheld) == len(appsToSync) {
			// getAppsToSync already stopped at or before this step, so every later step is absent
			// from the map and there is nothing left to withhold. Reporting a hold here would be a
			// misleading log line and a requeue for a wave nobody released.
			continue
		}

		// Named for their position in the rollout, not for which one is newer: see
		// maxRevisionSkewHold on why the direction of the disagreement is not knowable here.
		gateLogCtx := logCtx.WithFields(log.Fields{
			"step":                stepIndex + 1,
			"app.step":            stepApp,
			"app.step.revisions":  strings.Join(appMap[stepApp].Status.GetRevisions(), ","),
			"app.later":           laterApp,
			"app.later.revisions": strings.Join(appMap[laterApp].Status.GetRevisions(), ","),
		})

		remaining, bounded := remainingRevisionSkewHold(&applicationSet, laterApp, now)
		switch {
		case !bounded:
			// No timestamp to measure the bound against, so this hold cannot be shown to end. Release
			// it: a hold whose bound cannot be evaluated is not bounded, and re-deriving a fresh
			// window on every requeue would withhold the step forever.
			gateLogCtx.Warn("Releasing the next progressive sync wave with a revision skew still present: the ApplicationSet status carries no Waiting transition time to bound the hold with")
		case remaining <= 0:
			// The Application that is behind may not be late at all: a commit that does not touch its
			// manifest-generate-paths never refreshes it, and that is indistinguishable from here.
			// Release the wave rather than stall the rollout, but make the decision visible.
			gateLogCtx.WithField("maxHold", maxRevisionSkewHold).
				Warn("Releasing the next progressive sync wave with a revision skew still present: the two steps did not converge on the same revisions within the hold window")
		default:
			gateLogCtx.Info("Holding the next progressive sync wave: an Application in a later step has observed different revisions than this step, so this step's Healthy may belong to the previous rollout")
			return withheld, min(remaining, revisionSkewRequeueInterval)
		}

		// This step's skew is released, but a later pair may still have a hold left to run: keep
		// looking rather than releasing the whole rollout on the strength of one elapsed bound.
	}

	return appsToSync, 0
}

// withholdStepsAfter copies appsToSync without the Applications whose only membership is in a step
// after stepIndex.
//
// "Only" is load-bearing: matchExpressions may select one Application into two steps, and
// buildAppDependencyList records that under ValidationIssues.DuplicateAppSelections without excluding
// it. Dropping such an Application would also drop it from an already-released step, leaving it
// Waiting with nothing to advance it and the rollout stuck until the bound expired.
func withholdStepsAfter(appDependencyList [][]string, appsToSync map[string]bool, stepIndex int) map[string]bool {
	released := make(map[string]bool)
	for step := 0; step <= stepIndex; step++ {
		for _, appName := range appDependencyList[step] {
			released[appName] = true
		}
	}

	withheld := make(map[string]bool, len(appsToSync))
	for appName := range appsToSync {
		withheld[appName] = true
	}
	for laterStep := stepIndex + 1; laterStep < len(appDependencyList); laterStep++ {
		for _, appName := range appDependencyList[laterStep] {
			if released[appName] {
				continue
			}
			delete(withheld, appName)
		}
	}

	return withheld
}

// findStepRevisionSkew reports an Application pair showing that the step at stepIndex and a later step
// disagree about the revisions they have observed, or two empty strings when they agree. It proves the
// disagreement, not its direction -- nothing reachable from here orders two commits.
//
// Three choices are deliberate. Only a later-step Application in Waiting counts, because Waiting is
// the one status that proves the change has reached it, which also leaves the common "not refreshed
// yet" case alone. Every later step is scanned, not just the next one, since an intervening step may
// hold nothing Waiting. And of several disagreeing pairs it returns the one whose later-step
// Application entered Waiting earliest, which is what makes remainingRevisionSkewHold a bound.
func findStepRevisionSkew(
	applicationSet *argov1alpha1.ApplicationSet,
	appDependencyList [][]string,
	appMap map[string]*argov1alpha1.Application,
	stepIndex int,
) (stepAppName string, laterAppName string) {
	var anchor *metav1.Time

	for _, stepName := range appDependencyList[stepIndex] {
		stepApp, ok := appMap[stepName]
		if !ok {
			continue
		}

		for laterStep := stepIndex + 1; laterStep < len(appDependencyList); laterStep++ {
			for _, laterName := range appDependencyList[laterStep] {
				laterApp, ok := appMap[laterName]
				if !ok {
					continue
				}

				laterStatus := waitingApplicationStatus(applicationSet, laterName)
				if laterStatus == nil || !revisionsDisagree(stepApp, laterApp) {
					continue
				}

				if laterAppName == "" || waitingTransitionBefore(laterStatus.LastTransitionTime, anchor) {
					stepAppName, laterAppName, anchor = stepName, laterName, laterStatus.LastTransitionTime
				}
			}
		}
	}

	return stepAppName, laterAppName
}

// revisionsDisagree reports whether two Applications resolve the same Git coordinates but have
// observed different revisions. Applications on different coordinates may differ forever, and one
// that has never been compared against a revision has nothing to disagree with, so neither counts.
func revisionsDisagree(stepApp *argov1alpha1.Application, laterApp *argov1alpha1.Application) bool {
	stepRevisions, laterRevisions := stepApp.Status.GetRevisions(), laterApp.Status.GetRevisions()
	if len(stepRevisions) == 0 || len(laterRevisions) == 0 {
		return false
	}
	if !reflect.DeepEqual(applicationSourceKeys(stepApp), applicationSourceKeys(laterApp)) {
		return false
	}
	return !reflect.DeepEqual(stepRevisions, laterRevisions)
}

// waitingApplicationStatus returns the ApplicationSet's status entry for appName when that entry
// reads Waiting, and nil when there is no entry or it reads anything else.
func waitingApplicationStatus(applicationSet *argov1alpha1.ApplicationSet, appName string) *argov1alpha1.ApplicationSetApplicationStatus {
	idx := utils.FindApplicationStatusIndex(applicationSet.Status.ApplicationStatus, appName)
	if idx == -1 || applicationSet.Status.ApplicationStatus[idx].Status != argov1alpha1.ProgressiveSyncWaiting {
		return nil
	}
	return &applicationSet.Status.ApplicationStatus[idx]
}

// waitingTransitionBefore orders two Waiting transitions, treating a missing timestamp as the
// earliest of all. A hold anchored on a status with no timestamp cannot be shown to end, and the
// caller releases on the earliest anchor, so an entry with no timestamp must not be able to hide
// behind a sibling that has one.
func waitingTransitionBefore(a *metav1.Time, b *metav1.Time) bool {
	if a == nil {
		return b != nil
	}
	if b == nil {
		return false
	}
	return a.Before(b)
}

// remainingRevisionSkewHold reports how much of maxRevisionSkewHold is left for the skew involving
// laterAppName, measured from that Application's own Waiting transition, and whether the bound could
// be evaluated at all. Anchoring on the earliest such transition -- which findStepRevisionSkew
// selects -- is what stops the deadline drifting as other Applications enter Waiting mid-hold.
//
// The bound is per skew, not per rollout. A new commit re-stamps the Waiting transition and starts a
// new window, deliberately, since the old disagreement no longer exists; so commits landing on a
// later step faster than maxRevisionSkewHold, while the earlier step is never refreshed, can keep
// that step withheld. Every individual hold is still bounded and logged.
func remainingRevisionSkewHold(applicationSet *argov1alpha1.ApplicationSet, laterAppName string, now time.Time) (time.Duration, bool) {
	appStatus := waitingApplicationStatus(applicationSet, laterAppName)
	if appStatus == nil || appStatus.LastTransitionTime == nil {
		// Nothing to measure against. Every Waiting transition on the write path stamps
		// LastTransitionTime (progressive_sync.go:438 and :495-496), so this is unreachable through
		// normal operation and only a status written by something else can produce it. The caller
		// releases: this gate blocks on what it can prove and on nothing else.
		return 0, false
	}

	deadline := appStatus.LastTransitionTime.Add(maxRevisionSkewHold)
	if !now.Before(deadline) {
		return 0, true
	}
	return deadline.Sub(now), true
}
