package progressivesync

import (
	"bytes"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	argov1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

// A third config revision, so a three-step fixture can put every step on a different commit.
const gateThirdCfgRevision = "b1c2d3e4f5a6978869504132a2b3c4d5e6f7a8b9"

// gateNow is the instant every case in this file is evaluated at.
var gateNow = time.Date(2026, 8, 20, 11, 12, 2, 0, time.UTC)

func gateAt(d time.Duration) *metav1.Time {
	return new(metav1.NewTime(gateNow.Add(d)))
}

// TestRevisionSkewHoldIsOrderIndependent pins the property that makes maxRevisionSkewHold a bound
// rather than a deadline that drifts: it is measured from the EARLIEST Waiting transition among the
// Applications that prove the skew, so neither the order buildAppDependencyList happened to walk the
// Applications in nor a sibling refreshing mid-hold can move it.
//
// appDependencyList order is informer List order, which has nothing to do with which Application has
// been waiting longest. Anchoring on whichever pair was reached first made the same state hold or
// release depending only on that order, and let a late-refreshing, early-sorting sibling restart the
// clock: the deadline moved out every time another Application entered Waiting.
func TestRevisionSkewHoldIsOrderIndependent(t *testing.T) {
	t.Parallel()

	oldRevisions := []string{gateChartRevision, gateOldCfgRevision}
	newRevisions := []string{gateChartRevision, gateNewCfgRevision}

	// app-web has been Waiting past the bound; app-web2 entered Waiting five seconds ago. Both
	// disagree with step 1, so both are evidence of the same hold, and the hold started with the
	// older of them.
	appSet := gateAppSet(2,
		gateStatus("app-beta", "1", argov1alpha1.ProgressiveSyncHealthy, oldRevisions, gateAt(-30*time.Minute)),
		gateStatus("app-web", "2", argov1alpha1.ProgressiveSyncWaiting, newRevisions, gateAt(-maxRevisionSkewHold-time.Second)),
		gateStatus("app-web2", "2", argov1alpha1.ProgressiveSyncWaiting, newRevisions, gateAt(-5*time.Second)),
	)
	currentApps := []argov1alpha1.Application{
		gateApp("app-beta", "1", oldRevisions, argov1alpha1.SyncStatusCodeSynced),
		gateApp("app-web", "2", newRevisions, argov1alpha1.SyncStatusCodeOutOfSync),
		gateApp("app-web2", "2", newRevisions, argov1alpha1.SyncStatusCodeOutOfSync),
	}
	appsToSync := map[string]bool{"app-beta": true, "app-web": true, "app-web2": true}

	for _, cc := range []struct {
		name  string
		step2 []string
	}{
		{name: "the elapsed application first", step2: []string{"app-web", "app-web2"}},
		{name: "the fresh application first", step2: []string{"app-web2", "app-web"}},
	} {
		t.Run(cc.name, func(t *testing.T) {
			t.Parallel()

			gotMap, gotRequeue := withholdRevisionSkewedSteps(log.NewEntry(log.New()), appSet,
				[][]string{{"app-beta"}, cc.step2}, currentApps, appsToSync, gateNow)

			assert.Equal(t, appsToSync, gotMap,
				"the skew is older than the bound in both orders, so the wave must be released in both")
			assert.Zero(t, gotRequeue, "a released wave asks for no requeue of its own")
		})
	}
}

// TestRevisionSkewHoldAnchorsOnEarliestWaitingTransition is the same property stated positively: the
// hold that is still running is measured from the earliest transition, not from whichever pair the
// scan reached first. Listing the freshly-Waiting Application first is what made the old code report
// a nearly full window here.
func TestRevisionSkewHoldAnchorsOnEarliestWaitingTransition(t *testing.T) {
	t.Parallel()

	oldRevisions := []string{gateChartRevision, gateOldCfgRevision}
	newRevisions := []string{gateChartRevision, gateNewCfgRevision}

	appSet := gateAppSet(2,
		gateStatus("app-beta", "1", argov1alpha1.ProgressiveSyncHealthy, oldRevisions, gateAt(-30*time.Minute)),
		gateStatus("app-web2", "2", argov1alpha1.ProgressiveSyncWaiting, newRevisions, gateAt(-time.Second)),
		gateStatus("app-web", "2", argov1alpha1.ProgressiveSyncWaiting, newRevisions, gateAt(-maxRevisionSkewHold+4*time.Second)),
	)
	currentApps := []argov1alpha1.Application{
		gateApp("app-beta", "1", oldRevisions, argov1alpha1.SyncStatusCodeSynced),
		gateApp("app-web", "2", newRevisions, argov1alpha1.SyncStatusCodeOutOfSync),
		gateApp("app-web2", "2", newRevisions, argov1alpha1.SyncStatusCodeOutOfSync),
	}

	gotMap, gotRequeue := withholdRevisionSkewedSteps(log.NewEntry(log.New()), appSet,
		// app-web2 refreshed one second ago and sorts first; app-web has four seconds of bound left.
		[][]string{{"app-beta"}, {"app-web2", "app-web"}},
		currentApps,
		map[string]bool{"app-beta": true, "app-web": true, "app-web2": true},
		gateNow)

	assert.Equal(t, map[string]bool{"app-beta": true}, gotMap, "step 2 is still held")
	assert.Equal(t, 4*time.Second, gotRequeue,
		"the requeue is clamped to what is left of the EARLIEST anchor's bound, not the newest one's")
}

// TestRevisionSkewHoldKeepsLookingPastAnElapsedStep pins that an elapsed bound releases only the step
// it was measured for. Abandoning the whole scan on the first elapsed skew released a later step whose
// own skew was fresh and provable -- the exact torn rollout this gate exists to prevent.
//
// Reachable: step 2 has been waiting out step 1 past the bound (the manifest-generate-paths case) when
// a new commit refreshes step 3's Application.
func TestRevisionSkewHoldKeepsLookingPastAnElapsedStep(t *testing.T) {
	t.Parallel()

	step1Revisions := []string{gateChartRevision, gateOldCfgRevision}
	step2Revisions := []string{gateChartRevision, gateNewCfgRevision}
	step3Revisions := []string{gateChartRevision, gateThirdCfgRevision}

	appSet := gateAppSet(3,
		gateStatus("app-beta", "1", argov1alpha1.ProgressiveSyncHealthy, step1Revisions, gateAt(-30*time.Minute)),
		gateStatus("app-web", "2", argov1alpha1.ProgressiveSyncWaiting, step2Revisions, gateAt(-maxRevisionSkewHold-time.Second)),
		gateStatus("app-web2", "3", argov1alpha1.ProgressiveSyncWaiting, step3Revisions, gateAt(-5*time.Second)),
	)
	currentApps := []argov1alpha1.Application{
		gateApp("app-beta", "1", step1Revisions, argov1alpha1.SyncStatusCodeSynced),
		gateApp("app-web", "2", step2Revisions, argov1alpha1.SyncStatusCodeOutOfSync),
		gateApp("app-web2", "3", step3Revisions, argov1alpha1.SyncStatusCodeOutOfSync),
	}

	gotMap, gotRequeue := withholdRevisionSkewedSteps(log.NewEntry(log.New()), appSet,
		[][]string{{"app-beta"}, {"app-web"}, {"app-web2"}}, currentApps,
		map[string]bool{"app-beta": true, "app-web": true, "app-web2": true}, gateNow)

	assert.Equal(t, map[string]bool{"app-beta": true, "app-web": true}, gotMap,
		"step 2's elapsed bound releases step 2, but step 3's own skew against step 2 is fresh and still holds it")
	assert.Equal(t, revisionSkewRequeueInterval, gotRequeue,
		"a step is still withheld, so the hold must still ask to be reconsidered")
}

// TestRevisionSkewHoldKeepsApplicationsSelectedIntoAnEarlierStep pins that withholding "every later
// step" does not drop an Application that is ALSO a member of a step being released.
//
// buildAppDependencyList permits an Application to land in two steps: it warns and records
// ValidationIssues.DuplicateAppSelections, but it does not exclude it. Deleting such an Application
// left it Waiting with nothing to advance it, so the earlier step it belongs to could never reach
// all-Healthy and the rollout was stuck until the bound expired.
func TestRevisionSkewHoldKeepsApplicationsSelectedIntoAnEarlierStep(t *testing.T) {
	t.Parallel()

	oldRevisions := []string{gateChartRevision, gateOldCfgRevision}
	newRevisions := []string{gateChartRevision, gateNewCfgRevision}

	appSet := gateAppSet(2,
		gateStatus("app-beta", "1", argov1alpha1.ProgressiveSyncHealthy, oldRevisions, gateAt(-30*time.Minute)),
		gateStatus("app-dual", "1", argov1alpha1.ProgressiveSyncHealthy, oldRevisions, gateAt(-30*time.Minute)),
		gateStatus("app-web", "2", argov1alpha1.ProgressiveSyncWaiting, newRevisions, gateAt(-5*time.Second)),
	)
	currentApps := []argov1alpha1.Application{
		gateApp("app-beta", "1", oldRevisions, argov1alpha1.SyncStatusCodeSynced),
		gateApp("app-dual", "1", oldRevisions, argov1alpha1.SyncStatusCodeSynced),
		gateApp("app-web", "2", newRevisions, argov1alpha1.SyncStatusCodeOutOfSync),
	}

	gotMap, gotRequeue := withholdRevisionSkewedSteps(log.NewEntry(log.New()), appSet,
		// app-dual is selected into both steps, so step 1 -- which is not withheld -- needs it.
		[][]string{{"app-beta", "app-dual"}, {"app-web", "app-dual"}},
		currentApps,
		map[string]bool{"app-beta": true, "app-dual": true, "app-web": true},
		gateNow)

	assert.Equal(t, map[string]bool{"app-beta": true, "app-dual": true}, gotMap,
		"app-dual belongs to the released step too, so withholding step 2 must not drop it")
	assert.Equal(t, revisionSkewRequeueInterval, gotRequeue)
}

// TestRevisionSkewReleaseReasonsAreDistinct pins that the two ways a hold ends up released are not
// reported as the same thing. "Did not converge within the hold window" is a timeout; an anchor with
// no timestamp timed out on nothing, and saying it did sends an operator looking for a skew that
// outlasted two minutes when what actually happened is that the status carried no clock at all.
func TestRevisionSkewReleaseReasonsAreDistinct(t *testing.T) {
	t.Parallel()

	oldRevisions := []string{gateChartRevision, gateOldCfgRevision}
	newRevisions := []string{gateChartRevision, gateNewCfgRevision}
	currentApps := []argov1alpha1.Application{
		gateApp("app-beta", "1", oldRevisions, argov1alpha1.SyncStatusCodeSynced),
		gateApp("app-web", "2", newRevisions, argov1alpha1.SyncStatusCodeOutOfSync),
	}
	appsToSync := map[string]bool{"app-beta": true, "app-web": true}

	for _, cc := range []struct {
		name       string
		transition *metav1.Time
		expected   string
		rejected   string
	}{
		{
			name:       "the bound elapsed",
			transition: gateAt(-maxRevisionSkewHold - time.Second),
			expected:   "did not converge on the same revisions within the hold window",
			rejected:   "carries no Waiting transition time",
		},
		{
			name:       "the anchor carries no timestamp",
			transition: nil,
			expected:   "carries no Waiting transition time",
			rejected:   "did not converge on the same revisions within the hold window",
		},
	} {
		t.Run(cc.name, func(t *testing.T) {
			t.Parallel()

			logger := log.New()
			var out bytes.Buffer
			logger.Out = &out
			logger.Level = log.InfoLevel

			appSet := gateAppSet(2,
				gateStatus("app-beta", "1", argov1alpha1.ProgressiveSyncHealthy, oldRevisions, gateAt(-30*time.Minute)),
				gateStatus("app-web", "2", argov1alpha1.ProgressiveSyncWaiting, newRevisions, cc.transition),
			)

			gotMap, gotRequeue := withholdRevisionSkewedSteps(log.NewEntry(logger), appSet,
				[][]string{{"app-beta"}, {"app-web"}}, currentApps, appsToSync, gateNow)

			assert.Equal(t, appsToSync, gotMap, "both reasons release the wave")
			assert.Zero(t, gotRequeue)
			assert.Contains(t, out.String(), cc.expected)
			assert.NotContains(t, out.String(), cc.rejected,
				"the two release reasons must not be reported as each other")
		})
	}
}
