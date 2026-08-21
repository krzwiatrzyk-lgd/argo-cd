package progressivesync

import (
	"time"

	argov1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

// MaxSettleWindow is the largest quiet period --progressive-sync-settle-window will honour. It is
// enforced here rather than only at the flag, because env.ParseDurationFromEnv bounds only the
// environment-derived default: a value passed on the command line reaches the Manager unchecked, and
// the Manager's SettleWindow field is exported. Enforcing the bound at the point of use makes the
// documented maximum true however the value arrived.
const MaxSettleWindow = 5 * time.Minute

// NormalizeSettleWindow clamps a configured settle window into [0, MaxSettleWindow].
//
// A negative window means disabled, which is the same as zero: there is no sensible reading of "wait
// for minus one second". A window past the maximum is clamped rather than rejected, so an operator
// who asks for too long a quiet period gets the longest supported one instead of a controller that
// refuses to start. Callers that can report the difference to a human should do so -- silently
// honouring something other than what was configured is its own kind of bug.
func NormalizeSettleWindow(window time.Duration) time.Duration {
	switch {
	case window < 0:
		return 0
	case window > MaxSettleWindow:
		return MaxSettleWindow
	default:
		return window
	}
}

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
	window = NormalizeSettleWindow(window)
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
