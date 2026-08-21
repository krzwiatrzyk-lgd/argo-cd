package progressivesync

import (
	"time"

	argov1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

// remainingSettleWindow reports how long is left of the quiet period that began when an Application
// in the ApplicationSet most recently registered a change. It returns zero when the window is
// disabled, when no Application is Waiting, or when the window has already elapsed.
//
// The Application controller refreshes Applications independently, so a new commit does not become
// visible to all of them at the same instant. An Application that has not been refreshed yet still
// reports the previous revision as Synced and Healthy, which is indistinguishable from having
// completed the current one. Waiting is the status assigned the moment a revision or spec change is
// observed, so the latest Waiting transition is the most recent evidence that the ApplicationSet is
// still learning about a change, and the window is measured from there.
func remainingSettleWindow(applicationSet *argov1alpha1.ApplicationSet, window time.Duration, now time.Time) time.Duration {
	if window <= 0 {
		return 0
	}

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
		return 0
	}

	deadline := latest.Add(window)
	if !now.Before(deadline) {
		return 0
	}

	return deadline.Sub(now)
}
