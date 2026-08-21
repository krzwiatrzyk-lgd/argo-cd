package progressivesync

import (
	"reflect"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/argoproj/argo-cd/v3/applicationset/utils"
	argov1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

const (
	// maxRevisionSkewHold bounds how long the revision-aware gate withholds a wave.
	//
	// A commit that does not touch an earlier step's manifest-generate-paths never refreshes that
	// step's Application, so its resolved revision stays behind until the Application controller's
	// own reconciliation timeout fires. From the ApplicationSet controller that is indistinguishable
	// from a refresh that is merely late, so the gate cannot wait indefinitely: past this bound it
	// releases the wave and says so, rather than stalling the rollout for as long as that timeout.
	maxRevisionSkewHold = 2 * time.Minute

	// revisionSkewRequeueInterval is how often the ApplicationSet is re-examined while a wave is
	// withheld. A poll is needed because the event that clears the hold is not guaranteed to exist:
	// shouldRequeueForApplication ignores Status.Sync.Revision, so an Application that refreshes and
	// turns out to be Synced at the new revision anyway produces no watch event, and an
	// ApplicationSet generated only by the cluster generator has no periodic requeue either
	// (ClusterGenerator.GetRequeueAfter returns NoRequeueAfter).
	revisionSkewRequeueInterval = 10 * time.Second
)

// applicationSourceKeys identifies the Git coordinates an Application resolves its revisions from,
// in spec source order. Two Applications sharing these coordinates must converge on the same
// resolved revisions, so a difference between them means one of the two has not been refreshed yet.
// Applications with different coordinates legitimately sit at different revisions, and comparing
// them would hold the rollout forever, so they are skipped.
//
// The key is exactly the fields that decide which commit a source resolves to: RepoURL,
// TargetRevision, Chart (for a Helm repo, repoURL and version alone are not a source: redis at 18.*
// and postgres at 18.* are different resolutions) and TagPrefix (it filters which git tags a semver
// TargetRevision may resolve to, so the same repo and the same 1.0.* constraint under two prefixes
// resolve to permanently different revisions). Path, Ref, Name and the per-tool blocks are
// deliberately excluded: they change what is rendered from a commit, not which commit is resolved,
// and per-cluster paths are the normal ApplicationSet shape. Neither RepoURL nor TargetRevision is
// normalized: within one ApplicationSet every Application comes from one template, so any difference
// is a deliberate per-Application difference and is the signal worth keeping.
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

// withholdRevisionSkewedSteps removes from appsToSync every Application in a step later than the
// first step that is demonstrably evaluating an older revision than a later step has already
// observed, and returns how long to wait before deciding again. It returns appsToSync unchanged and
// a zero duration when no such skew exists, or when the hold has already lasted longer than
// maxRevisionSkewHold.
//
// getAppsToSync gates a step purely on ProgressiveSyncHealthy, and Healthy does not say which
// revision it is healthy for. An Application the Application controller has not refreshed yet still
// reports Synced against the previous commit, so revisionsChanged is false for it in this pass and
// its status stays the Healthy earned in the previous rollout. It does reach Waiting, but only once a
// later reconcile sees the refreshed Application -- by which point the next wave has already been
// released and its sync operation stamped. That is how RollingSync ends up syncing two steps at once.
//
// Filtering the returned map is equivalent to breaking out of getAppsToSync's own loop earlier:
// getAppsToSync adds steps 0..k for the first incomplete step k, so dropping every step after the
// skewed one leaves exactly the steps that loop would have added had it stopped there.
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
		behindApp, aheadApp := findStepRevisionSkew(applicationSet, appDependencyList, appMap, stepIndex)
		if behindApp == "" {
			continue
		}

		withheld := make(map[string]bool, len(appsToSync))
		for appName := range appsToSync {
			withheld[appName] = true
		}
		for laterStep := stepIndex + 1; laterStep < len(appDependencyList); laterStep++ {
			for _, appName := range appDependencyList[laterStep] {
				delete(withheld, appName)
			}
		}
		if len(withheld) == len(appsToSync) {
			// getAppsToSync already stopped at or before this step, so every later step is absent
			// from the map and there is nothing left to withhold. Reporting a hold here would be a
			// misleading log line and a requeue for a wave nobody released.
			continue
		}

		gateLogCtx := logCtx.WithFields(log.Fields{
			"step":                 stepIndex + 1,
			"app.behind":           behindApp,
			"app.behind.revisions": strings.Join(appMap[behindApp].Status.GetRevisions(), ","),
			"app.ahead":            aheadApp,
			"app.ahead.revisions":  strings.Join(appMap[aheadApp].Status.GetRevisions(), ","),
		})

		remaining := remainingRevisionSkewHold(&applicationSet, now)
		if remaining <= 0 {
			// The Application that is behind may not be late at all: a commit that does not touch its
			// manifest-generate-paths never refreshes it, and that is indistinguishable from here.
			// Release the wave rather than stall the rollout, but make the decision visible.
			gateLogCtx.WithField("maxHold", maxRevisionSkewHold).
				Warn("Releasing the next progressive sync wave with a revision skew still present: the earlier step has not observed the revision within the hold window")
			return appsToSync, 0
		}

		gateLogCtx.Info("Holding the next progressive sync wave: this step reports Healthy for a revision an Application in a later step has already moved past")

		requeue := min(remaining, revisionSkewRequeueInterval)
		return withheld, requeue
	}

	return appsToSync, 0
}

// findStepRevisionSkew reports an Application pair proving the step at stepIndex is evaluating a
// revision older than one a LATER step has already observed: the name of the Application that is
// behind and the one that is ahead, or two empty strings when the steps agree.
//
// Only an Application in Waiting counts as evidence of being ahead. Waiting is the state the
// ApplicationSet controller assigns the moment it observes a revision or spec change, so it is the
// one status that proves the change has reached that Application. Healthy or Progressing means the
// Application is finished or already moving and says nothing about revision skew.
//
// Every later step is considered, not just the immediately following one: a step in between may hold
// no Waiting Application while a further one does, and releasing everything after this step would be
// just as wrong.
func findStepRevisionSkew(
	applicationSet argov1alpha1.ApplicationSet,
	appDependencyList [][]string,
	appMap map[string]*argov1alpha1.Application,
	stepIndex int,
) (string, string) {
	for _, stepAppName := range appDependencyList[stepIndex] {
		stepApp, ok := appMap[stepAppName]
		if !ok {
			continue
		}
		stepRevisions := stepApp.Status.GetRevisions()
		if len(stepRevisions) == 0 {
			// Never compared against a revision, so there is nothing for it to be behind.
			continue
		}
		stepKeys := applicationSourceKeys(stepApp)

		for laterStep := stepIndex + 1; laterStep < len(appDependencyList); laterStep++ {
			for _, laterAppName := range appDependencyList[laterStep] {
				laterApp, ok := appMap[laterAppName]
				if !ok {
					continue
				}

				idx := utils.FindApplicationStatusIndex(applicationSet.Status.ApplicationStatus, laterAppName)
				if idx == -1 || applicationSet.Status.ApplicationStatus[idx].Status != argov1alpha1.ProgressiveSyncWaiting {
					continue
				}

				laterRevisions := laterApp.Status.GetRevisions()
				if len(laterRevisions) == 0 || !reflect.DeepEqual(stepKeys, applicationSourceKeys(laterApp)) {
					continue
				}

				if !reflect.DeepEqual(stepRevisions, laterRevisions) {
					return stepAppName, laterAppName
				}
			}
		}
	}

	return "", ""
}

// remainingRevisionSkewHold reports how much of maxRevisionSkewHold is left, measured from the most
// recent Waiting transition in the ApplicationSet status. That transition is when the ApplicationSet
// last learned about a change, so it is the start of the skew the gate is waiting out; a burst of
// commits pushes it forward, which is the intended behaviour. It is already persisted in the
// ApplicationSet status, so the bound survives a controller restart and needs no new API field.
func remainingRevisionSkewHold(applicationSet *argov1alpha1.ApplicationSet, now time.Time) time.Duration {
	var latest time.Time
	for _, appStatus := range applicationSet.Status.ApplicationStatus {
		if appStatus.Status != argov1alpha1.ProgressiveSyncWaiting || appStatus.LastTransitionTime == nil {
			continue
		}
		if appStatus.LastTransitionTime.After(latest) {
			latest = appStatus.LastTransitionTime.Time
		}
	}

	if latest.IsZero() {
		// A skew was found, so some Application is Waiting; if none of them carries a transition time
		// there is nothing to measure against and the hold is treated as fresh.
		return maxRevisionSkewHold
	}

	deadline := latest.Add(maxRevisionSkewHold)
	if !now.Before(deadline) {
		return 0
	}
	return deadline.Sub(now)
}
