package controllers

import (
	"testing"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/v3/pkg/health"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/argoproj/argo-cd/v3/applicationset/progressivesync"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

/*
Reproduction of the RollingSync out-of-order rollout.

getAppsToSync (applicationset/progressivesync/progressive_sync.go:561) releases step N+1 as soon as
every Application in step N reads ProgressiveSyncHealthy in the ApplicationSet status. It never asks
WHICH revision that Healthy belongs to. An Application the Application controller has not refreshed
yet still reports Synced against the previous commit, so revisionsChanged
(progressive_sync.go:447) is false, its progressive-sync status stays the Healthy it earned in the
PREVIOUS rollout, and the gate releases the next step -- which HAS refreshed and therefore sits in
Waiting.

The incident this reproduces logged, in one reconcile:

	Application allowed to sync before maxUpdate?: map[grafana-beta2-sta:true grafana-web1-sta:true]
	triggering sync for application: grafana-web1-sta, prune enabled: true
	Initialized new operation: {&SyncOperation{Revision:,Prune:true,...,Revisions:[],...}

while grafana-beta2-sta (step 1, the database migration) had not started its rollout at all. The
step-1 Application logs nothing on the way through: the status line at progressive_sync.go:541 is
guarded on the pointer identity newAppStatus.LastTransitionTime == &now, so an Application whose
status is left untouched is silent. Its silence is part of the signature.

These tests document CURRENT behaviour. They are a reproduction, not an aspiration: the assertions
below describe the bug, so the fix that lands the revision-aware gate must change them. Of the four
proposed fixes only fix-1 (revision-aware gate, on by default) does; fixes 2, 3 and 4 are opt-in and
leave this file passing as written, which is worth asserting in those PRs.

Placement note: this lives in package controllers rather than in progressivesync because
PerformProgressiveSyncs takes the ApplicationSet BY VALUE
(applicationset/controllers/applicationset_controller.go:277) and setAppSetApplicationStatus
reassigns the slice on that copy (:1184), so the caller's object never sees the new statuses.
The only way to observe what the next reconcile would observe is to read the ApplicationSet back
through the client, and that needs a Dependencies implementation that actually persists. The
package-local double in progressivesync (regressionDeps, specchanged_regression_test.go:83) is a
no-op, so the production implementation -- ApplicationSetReconciler, wired as in
progressive_sync_dependencies_test.go:890-905 -- is used instead.
*/

const (
	// Coordinates from the incident. Both Applications are two-source and share every coordinate;
	// only the config source's resolved revision differs, which is exactly what production showed.
	chartRepo    = "https://github.com/AirHelp/charts.git"
	configRepo   = "https://github.com/AirHelp/ah-config.git"
	chartRev     = "6a23cff31f9ee99d7276dd35b286b6b267a00dfd" // identical for both Applications
	oldConfigRev = "540d762f792763ced1c71d97d79b0020a6dbc099"
	newConfigRev = "02d86d2a072b33ac9177c8de3c37649ed4804dbd"

	orderingStep1App = "grafana-beta2-sta" // step 1: the migration
	orderingStep2App = "grafana-web1-sta"  // step 2: the web pods
)

// orderingPreviousRollout is when the previous rollout completed. Absolute rather than
// time.Now()-relative so that nothing in these tests depends on the wall clock.
var orderingPreviousRollout = metav1.NewTime(time.Date(2025, 11, 4, 9, 15, 0, 0, time.UTC))

// orderingLiveApp builds one of the two Applications as the Application controller left it: the spec
// the ApplicationSet template renders, plus the revisions that Application has actually observed.
//
// syncPolicy is deliberately nil on both sides, and the desired Applications are deep copies of the
// live ones, so that specChanged -- computed via utils.SpecsEquivalent
// (applicationset/utils/createOrUpdate.go:76) after disableAutomatedSync on the desired copy -- is
// false. A spec difference is not required for the bug, but it is a different route to it: it moves
// step 1 to Waiting and then straight back to Healthy, because step 1 is Synced and Healthy
// (progressive_sync.go:485-494). The reproduction asserts that step 1's status entry is untouched,
// which pins it to the revision-skew route rather than to an incidental spec drift.
func orderingLiveApp(name, env string, observedRevisions []string, syncStatus v1alpha1.SyncStatusCode) v1alpha1.Application {
	return v1alpha1.Application{
		TypeMeta: metav1.TypeMeta{APIVersion: "argoproj.io/v1alpha1", Kind: "Application"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "argocd",
			Labels:    map[string]string{"env": env},
		},
		Spec: v1alpha1.ApplicationSpec{
			Project: "default",
			// Both sources track master, as they did in the incident. Only ah-config's master
			// moved, which is why the first revision slot is identical on both Applications and the
			// second is not.
			Sources: []v1alpha1.ApplicationSource{
				{RepoURL: chartRepo, Path: "charts/grafana", TargetRevision: "master"},
				{RepoURL: configRepo, Path: "grafana", TargetRevision: "master", Ref: "values"},
			},
			Destination: v1alpha1.ApplicationDestination{
				Server:    "https://kubernetes.default.svc",
				Namespace: name,
			},
		},
		Status: v1alpha1.ApplicationStatus{
			Sync: v1alpha1.SyncStatus{
				Status:    syncStatus,
				Revisions: observedRevisions,
			},
			Health:       v1alpha1.AppHealthStatus{Status: health.HealthStatusHealthy},
			ReconciledAt: &orderingPreviousRollout,
		},
	}
}

// orderingAppSet is the ApplicationSet as the previous rollout left it: both Applications Healthy at
// the old config revision, one per RollingSync step.
//
// TargetRevisions must be non-nil. revisionsChanged compares it against
// Application.Status.GetRevisions() with reflect.DeepEqual, and GetRevisions never returns nil
// (pkg/apis/application/v1alpha1/types.go), so a nil here makes every Application look changed and
// destroys the skew the test is about.
func orderingAppSet() v1alpha1.ApplicationSet {
	healthy := func(name, step string) v1alpha1.ApplicationSetApplicationStatus {
		return v1alpha1.ApplicationSetApplicationStatus{
			Application:        name,
			Status:             v1alpha1.ProgressiveSyncHealthy,
			Message:            "Application resource became Healthy, updating status from Progressing to Healthy",
			Step:               step,
			TargetRevisions:    []string{chartRev, oldConfigRev},
			LastTransitionTime: &orderingPreviousRollout,
		}
	}
	step := func(env string) v1alpha1.ApplicationSetRolloutStep {
		return v1alpha1.ApplicationSetRolloutStep{
			MatchExpressions: []v1alpha1.ApplicationMatchExpression{
				{Key: "env", Operator: "In", Values: []string{env}},
			},
		}
	}

	return v1alpha1.ApplicationSet{
		ObjectMeta: metav1.ObjectMeta{Name: "grafana", Namespace: "argocd"},
		Spec: v1alpha1.ApplicationSetSpec{
			Strategy: &v1alpha1.ApplicationSetStrategy{
				Type: "RollingSync",
				RollingSync: &v1alpha1.ApplicationSetRolloutStrategy{
					Steps: []v1alpha1.ApplicationSetRolloutStep{step("beta"), step("web")},
				},
			},
		},
		Status: v1alpha1.ApplicationSetStatus{
			ApplicationStatus: []v1alpha1.ApplicationSetApplicationStatus{
				healthy(orderingStep1App, "1"),
				healthy(orderingStep2App, "2"),
			},
		},
	}
}

// orderingManager wires the progressive sync Manager against the production Dependencies
// implementation and a fake client holding the ApplicationSet, so status writes are persisted and
// can be read back the way the next reconcile would read them.
func orderingManager(t *testing.T, appSet *v1alpha1.ApplicationSet, apps []v1alpha1.Application) (*progressivesync.Manager, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	objects := []client.Object{appSet}
	for i := range apps {
		objects = append(objects, apps[i].DeepCopy())
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(appSet, &v1alpha1.Application{}).Build()

	r := &ApplicationSetReconciler{Client: c, Scheme: scheme}
	r.ProgressiveSyncManager = progressivesync.NewManager(r.Client, r.Client, r)

	return r.ProgressiveSyncManager, c
}

func orderingStatus(t *testing.T, appSet *v1alpha1.ApplicationSet, name string) v1alpha1.ApplicationSetApplicationStatus {
	t.Helper()
	for _, status := range appSet.Status.ApplicationStatus {
		if status.Application == name {
			return status
		}
	}
	require.FailNowf(t, "missing application status", "no status for Application %q", name)
	return v1alpha1.ApplicationSetApplicationStatus{}
}

// orderingAssertSameStatus compares two status entries, treating LastTransitionTime as an instant.
// A metav1.Time that has round-tripped through the client comes back in the local zone, so its
// *time.Location differs from the seeded one and a plain struct comparison would trip on that alone.
func orderingAssertSameStatus(t *testing.T, expected, actual v1alpha1.ApplicationSetApplicationStatus, msg string) {
	t.Helper()
	assert.True(t, expected.LastTransitionTime.Equal(actual.LastTransitionTime),
		"%s (transition time moved from %v to %v)", msg, expected.LastTransitionTime, actual.LastTransitionTime)
	expected.LastTransitionTime = nil
	actual.LastTransitionTime = nil
	assert.Equal(t, expected, actual, msg)
}

func orderingRolloutApp(t *testing.T, apps []v1alpha1.Application, name string) v1alpha1.Application {
	t.Helper()
	for _, app := range apps {
		if app.Name == name {
			return app
		}
	}
	require.FailNowf(t, "missing rollout Application", "no rollout Application %q", name)
	return v1alpha1.Application{}
}

// TestRollingSyncReleasesLaterStepOnStaleHealthy reproduces the bug: step 2 is released, and gets a
// sync operation stamped, while step 1 has not started its rollout.
//
// This test documents current behaviour. It is a reproduction, not an aspiration.
func TestRollingSyncReleasesLaterStepOnStaleHealthy(t *testing.T) {
	t.Parallel()

	appSet := orderingAppSet()

	// Step 1 has not been refreshed yet: it is still Synced against the previous config revision,
	// and still Healthy from the previous rollout.
	step1 := orderingLiveApp(orderingStep1App, "beta", []string{chartRev, oldConfigRev}, v1alpha1.SyncStatusCodeSynced)
	// Step 2 has been refreshed: it has observed the new config revision and has work to do. It must
	// be OutOfSync -- a Synced and Healthy Application is promoted straight back to Healthy
	// (progressive_sync.go:485-494) and there would be nothing to race.
	step2 := orderingLiveApp(orderingStep2App, "web", []string{chartRev, newConfigRev}, v1alpha1.SyncStatusCodeOutOfSync)

	liveApps := []v1alpha1.Application{step1, step2}
	desiredApps := []v1alpha1.Application{*step1.DeepCopy(), *step2.DeepCopy()}

	manager, c := orderingManager(t, &appSet, liveApps)
	logCtx := log.NewEntry(log.StandardLogger())

	appsToSync, err := manager.PerformProgressiveSyncs(t.Context(), logCtx, appSet, liveApps, desiredApps)
	require.NoError(t, err)

	// PerformProgressiveSyncs received the ApplicationSet by value, so read the persisted object.
	persisted := &v1alpha1.ApplicationSet{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(&appSet), persisted))

	// The stale reading the gate trusts: step 1 is left exactly as the previous rollout left it.
	// These assertions are what prove the fixture is exercising the bug rather than an unrelated
	// path -- the whole entry, message and transition time included, is byte-identical to the seed,
	// so nothing in UpdateApplicationSetApplicationStatus looked at this Application at all.
	step1Status := orderingStatus(t, persisted, orderingStep1App)
	assert.Equal(t, v1alpha1.ProgressiveSyncHealthy, step1Status.Status,
		"step 1 must still read Healthy; if it is Waiting the fixture is no longer reproducing the revision skew")
	assert.Equal(t, []string{chartRev, oldConfigRev}, step1Status.TargetRevisions,
		"step 1's Healthy belongs to the previous rollout's revisions")
	seeded := orderingAppSet()
	orderingAssertSameStatus(t, orderingStatus(t, &seeded, orderingStep1App), step1Status,
		"step 1's status entry must be untouched by this pass; a difference means the fixture has drifted onto another path")

	// Step 2 observed the new revision, so it was moved to Waiting and then promoted this pass.
	step2Status := orderingStatus(t, persisted, orderingStep2App)
	assert.Equal(t, v1alpha1.ProgressiveSyncPending, step2Status.Status)
	assert.Equal(t, []string{chartRev, newConfigRev}, step2Status.TargetRevisions)

	// The bug, byte for byte the incident's log line:
	// Application allowed to sync before maxUpdate?: map[grafana-beta2-sta:true grafana-web1-sta:true]
	assert.Equal(t, map[string]bool{orderingStep1App: true, orderingStep2App: true}, appsToSync,
		"BUG: step 2 is released while step 1 still reports Healthy for %v and step 2 has already observed %v",
		[]string{chartRev, oldConfigRev}, []string{chartRev, newConfigRev})

	// The next reconcile stamps the operation, because SyncDesiredApplications requires the
	// persisted status to be Pending and the promotion above landed on a copy the caller never saw.
	rolloutApps := manager.SyncDesiredApplications(logCtx, persisted, appsToSync, desiredApps)

	step2Rollout := orderingRolloutApp(t, rolloutApps, orderingStep2App)
	require.NotNil(t, step2Rollout.Operation, "BUG: step 2 was given a sync operation before step 1 rolled out")
	require.Len(t, step2Rollout.Operation.Info, 1)
	assert.Equal(t, "ApplicationSet RollingSync triggered a sync of this Application resource", step2Rollout.Operation.Info[0].Value)

	// No revision is pinned on the operation (progressive_sync.go:878), matching the incident's
	// "Revision:,Revisions:[]". The sync re-resolves its target at execution time, which is why
	// ordering is the only protection against a torn rollout.
	require.NotNil(t, step2Rollout.Operation.Sync)
	assert.Empty(t, step2Rollout.Operation.Sync.Revision)
	assert.Empty(t, step2Rollout.Operation.Sync.Revisions)

	step1Rollout := orderingRolloutApp(t, rolloutApps, orderingStep1App)
	assert.Nil(t, step1Rollout.Operation,
		"step 1 has no operation: the migration has not started while the web pods are being rolled")
}

// TestRollingSyncOrderingReproductionFixtureIsHonest is the negative control for the test above.
// With the revision skew removed -- step 1 has observed the same new config revision as step 2 --
// the gate behaves correctly: step 1 moves to Waiting, only step 1 is released, and only step 1 gets
// a sync operation. That is what pins the reproduction to the revision skew and nothing else.
func TestRollingSyncOrderingReproductionFixtureIsHonest(t *testing.T) {
	t.Parallel()

	appSet := orderingAppSet()

	// The only difference from the reproduction: step 1 has been refreshed too. It is OutOfSync at
	// the new revision, the same state step 2 is in -- Synced here would be promoted straight back
	// to Healthy (progressive_sync.go:485-494) and would release step 2 for a different and
	// legitimate reason, which is not what this control is testing.
	step1 := orderingLiveApp(orderingStep1App, "beta", []string{chartRev, newConfigRev}, v1alpha1.SyncStatusCodeOutOfSync)
	step2 := orderingLiveApp(orderingStep2App, "web", []string{chartRev, newConfigRev}, v1alpha1.SyncStatusCodeOutOfSync)

	liveApps := []v1alpha1.Application{step1, step2}
	desiredApps := []v1alpha1.Application{*step1.DeepCopy(), *step2.DeepCopy()}

	manager, c := orderingManager(t, &appSet, liveApps)
	logCtx := log.NewEntry(log.StandardLogger())

	appsToSync, err := manager.PerformProgressiveSyncs(t.Context(), logCtx, appSet, liveApps, desiredApps)
	require.NoError(t, err)

	assert.Equal(t, map[string]bool{orderingStep1App: true}, appsToSync,
		"with no revision skew the gate holds step 2 until step 1 is Healthy again")

	persisted := &v1alpha1.ApplicationSet{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(&appSet), persisted))

	// Step 1 observed the new revision, so it left Healthy behind; that is what closes the gate.
	assert.Equal(t, v1alpha1.ProgressiveSyncPending, orderingStatus(t, persisted, orderingStep1App).Status)
	assert.Equal(t, v1alpha1.ProgressiveSyncWaiting, orderingStatus(t, persisted, orderingStep2App).Status)

	rolloutApps := manager.SyncDesiredApplications(logCtx, persisted, appsToSync, desiredApps)
	assert.NotNil(t, orderingRolloutApp(t, rolloutApps, orderingStep1App).Operation,
		"step 1 syncs first, as RollingSync promises")
	assert.Nil(t, orderingRolloutApp(t, rolloutApps, orderingStep2App).Operation,
		"step 2 is not touched until step 1 is Healthy at the new revision")
}
