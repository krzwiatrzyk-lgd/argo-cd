package progressivesync

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/v3/pkg/health"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	argov1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

// The revisions below mirror the production evidence: the chart source is pinned to a tag and did
// not move, only the config source did. A comparison that looked at a single revision would miss it.
const (
	gateChartRevision  = "6a23cff9c1f0a3f9e0c1b2d3e4f5a6b7c8d9e0f1"
	gateOldCfgRevision = "540d762f4a3b2c1d0e9f8a7b6c5d4e3f2a1b0c9d"
	gateNewCfgRevision = "02d86d2a9b8c7d6e5f4a3b2c1d0e9f8a7b6c5d4e"
	gateChartsRepo     = "https://github.com/example/charts.git"
	gateConfigRepo     = "https://github.com/example/config.git"
)

// gateApp is an Application with the two sources of the production case: a chart pinned to a tag and
// a config repo tracking a branch.
func gateApp(name, step string, revisions []string, syncStatus argov1alpha1.SyncStatusCode) argov1alpha1.Application {
	return argov1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "argocd",
			Labels:    map[string]string{"step": step},
		},
		Spec: argov1alpha1.ApplicationSpec{
			Project: "default",
			Sources: argov1alpha1.ApplicationSources{
				{RepoURL: gateChartsRepo, TargetRevision: "1.2.3", Chart: "generic"},
				{RepoURL: gateConfigRepo, TargetRevision: "master", Ref: "config"},
			},
		},
		Status: argov1alpha1.ApplicationStatus{
			Sync:   argov1alpha1.SyncStatus{Status: syncStatus, Revisions: revisions},
			Health: argov1alpha1.AppHealthStatus{Status: health.HealthStatusHealthy},
		},
	}
}

// gateSingleSourceApp resolves a single source, so its Git coordinates differ from gateApp's.
func gateSingleSourceApp(name, targetRevision, revision string) argov1alpha1.Application {
	return argov1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "argocd"},
		Spec: argov1alpha1.ApplicationSpec{
			Source: &argov1alpha1.ApplicationSource{RepoURL: gateChartsRepo, TargetRevision: targetRevision},
		},
		Status: argov1alpha1.ApplicationStatus{
			Sync: argov1alpha1.SyncStatus{Revision: revision},
		},
	}
}

// gateHelmApp resolves a semver constraint against a tag prefix, the one source field beyond
// repoURL/targetRevision/chart that changes which commit a source resolves to.
func gateHelmApp(name, tagPrefix, revision string) argov1alpha1.Application {
	return argov1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "argocd"},
		Spec: argov1alpha1.ApplicationSpec{
			Source: &argov1alpha1.ApplicationSource{
				RepoURL:        gateChartsRepo,
				TargetRevision: "1.0.*",
				TagPrefix:      tagPrefix,
			},
		},
		Status: argov1alpha1.ApplicationStatus{
			Sync: argov1alpha1.SyncStatus{Revision: revision},
		},
	}
}

// gateAppSet is a RollingSync ApplicationSet selecting Applications by their "step" label.
func gateAppSet(steps int, statuses ...argov1alpha1.ApplicationSetApplicationStatus) argov1alpha1.ApplicationSet {
	rolloutSteps := make([]argov1alpha1.ApplicationSetRolloutStep, 0, steps)
	for i := range steps {
		rolloutSteps = append(rolloutSteps, argov1alpha1.ApplicationSetRolloutStep{
			MatchExpressions: []argov1alpha1.ApplicationMatchExpression{
				{Key: "step", Operator: "In", Values: []string{strconv.Itoa(i + 1)}},
			},
		})
	}

	return argov1alpha1.ApplicationSet{
		ObjectMeta: metav1.ObjectMeta{Name: "grafana", Namespace: "argocd"},
		Spec: argov1alpha1.ApplicationSetSpec{
			Strategy: &argov1alpha1.ApplicationSetStrategy{
				Type:        "RollingSync",
				RollingSync: &argov1alpha1.ApplicationSetRolloutStrategy{Steps: rolloutSteps},
			},
		},
		Status: argov1alpha1.ApplicationSetStatus{ApplicationStatus: statuses},
	}
}

func gateStatus(name, step string, status argov1alpha1.ProgressiveSyncStatusCode, targetRevisions []string, transition *metav1.Time) argov1alpha1.ApplicationSetApplicationStatus {
	return argov1alpha1.ApplicationSetApplicationStatus{
		Application:        name,
		Status:             status,
		Step:               step,
		TargetRevisions:    targetRevisions,
		LastTransitionTime: transition,
	}
}

// TestWithholdRevisionSkewedSteps covers the gate that keeps RollingSync from unlocking a step on a
// Healthy earned in the previous rollout, plus every case that must NOT change behaviour: the gate
// fails open on anything it cannot prove, because a gate that holds on an ambiguous reading stalls
// the rollout for as long as the Application controller's reconciliation timeout.
func TestWithholdRevisionSkewedSteps(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 11, 12, 2, 0, time.UTC)
	at := func(d time.Duration) *metav1.Time {
		return new(metav1.NewTime(now.Add(d)))
	}

	twoSteps := [][]string{{"grafana-beta2-sta"}, {"grafana-web1-sta"}}
	bothSteps := map[string]bool{"grafana-beta2-sta": true, "grafana-web1-sta": true}

	for _, cc := range []struct {
		name              string
		appSet            argov1alpha1.ApplicationSet
		appDependencyList [][]string
		currentApps       []argov1alpha1.Application
		appsToSync        map[string]bool
		expectedMap       map[string]bool
		expectedRequeue   time.Duration
	}{
		{
			// The regression. web1 refreshed first, observed the new config revision and is Waiting.
			// beta2 has not been refreshed, so it still reports Synced against the previous commit
			// and its status is the Healthy it earned in the previous rollout. Step 2 must stay shut.
			name: "next step already saw a revision the current step has not",
			appSet: gateAppSet(2,
				gateStatus("grafana-beta2-sta", "1", argov1alpha1.ProgressiveSyncHealthy, []string{gateChartRevision, gateOldCfgRevision}, at(-10*time.Minute)),
				gateStatus("grafana-web1-sta", "2", argov1alpha1.ProgressiveSyncWaiting, []string{gateChartRevision, gateNewCfgRevision}, at(-5*time.Second)),
			),
			appDependencyList: twoSteps,
			currentApps: []argov1alpha1.Application{
				gateApp("grafana-beta2-sta", "1", []string{gateChartRevision, gateOldCfgRevision}, argov1alpha1.SyncStatusCodeSynced),
				gateApp("grafana-web1-sta", "2", []string{gateChartRevision, gateNewCfgRevision}, argov1alpha1.SyncStatusCodeOutOfSync),
			},
			appsToSync:      bothSteps,
			expectedMap:     map[string]bool{"grafana-beta2-sta": true},
			expectedRequeue: revisionSkewRequeueInterval,
		},
		{
			// Both Applications have observed the new commit, so beta2's Healthy belongs to the
			// rollout in flight and web1 may proceed.
			name: "steps agree",
			appSet: gateAppSet(2,
				gateStatus("grafana-beta2-sta", "1", argov1alpha1.ProgressiveSyncHealthy, []string{gateChartRevision, gateNewCfgRevision}, at(-30*time.Second)),
				gateStatus("grafana-web1-sta", "2", argov1alpha1.ProgressiveSyncWaiting, []string{gateChartRevision, gateNewCfgRevision}, at(-5*time.Second)),
			),
			appDependencyList: twoSteps,
			currentApps: []argov1alpha1.Application{
				gateApp("grafana-beta2-sta", "1", []string{gateChartRevision, gateNewCfgRevision}, argov1alpha1.SyncStatusCodeSynced),
				gateApp("grafana-web1-sta", "2", []string{gateChartRevision, gateNewCfgRevision}, argov1alpha1.SyncStatusCodeOutOfSync),
			},
			appsToSync:  bothSteps,
			expectedMap: bothSteps,
		},
		{
			// Applications resolving different Git coordinates are allowed to sit at different
			// revisions forever, so comparing them would hold the rollout permanently. This is the
			// gate's fail-open property: what it cannot prove, it does not block on.
			name: "different git coordinates are not compared",
			appSet: gateAppSet(2,
				gateStatus("grafana-beta2-sta", "1", argov1alpha1.ProgressiveSyncHealthy, []string{gateOldCfgRevision}, at(-10*time.Minute)),
				gateStatus("grafana-web1-sta", "2", argov1alpha1.ProgressiveSyncWaiting, []string{gateNewCfgRevision}, at(-5*time.Second)),
			),
			appDependencyList: twoSteps,
			currentApps: []argov1alpha1.Application{
				gateSingleSourceApp("grafana-beta2-sta", "release-1.2", gateOldCfgRevision),
				gateSingleSourceApp("grafana-web1-sta", "master", gateNewCfgRevision),
			},
			appsToSync:  bothSteps,
			expectedMap: bothSteps,
		},
		{
			// Same repo and same semver constraint, different tag prefix: the two Applications resolve
			// to permanently different revisions, so treating them as sharing coordinates would hold
			// the wave for the whole bound on every single rollout.
			name: "different tag prefixes are not compared",
			appSet: gateAppSet(2,
				gateStatus("grafana-beta2-sta", "1", argov1alpha1.ProgressiveSyncHealthy, []string{gateOldCfgRevision}, at(-10*time.Minute)),
				gateStatus("grafana-web1-sta", "2", argov1alpha1.ProgressiveSyncWaiting, []string{gateNewCfgRevision}, at(-5*time.Second)),
			),
			appDependencyList: twoSteps,
			currentApps: []argov1alpha1.Application{
				gateHelmApp("grafana-beta2-sta", "beta2/", gateOldCfgRevision),
				gateHelmApp("grafana-web1-sta", "web1/", gateNewCfgRevision),
			},
			appsToSync:  bothSteps,
			expectedMap: bothSteps,
		},
		{
			// The benign direction: the later step simply has not refreshed. It is not Waiting, so it
			// has registered no change and holding it back would only add latency.
			name: "later step is not waiting",
			appSet: gateAppSet(2,
				gateStatus("grafana-beta2-sta", "1", argov1alpha1.ProgressiveSyncHealthy, []string{gateChartRevision, gateOldCfgRevision}, at(-10*time.Minute)),
				gateStatus("grafana-web1-sta", "2", argov1alpha1.ProgressiveSyncHealthy, []string{gateChartRevision, gateNewCfgRevision}, at(-5*time.Second)),
			),
			appDependencyList: twoSteps,
			currentApps: []argov1alpha1.Application{
				gateApp("grafana-beta2-sta", "1", []string{gateChartRevision, gateOldCfgRevision}, argov1alpha1.SyncStatusCodeSynced),
				gateApp("grafana-web1-sta", "2", []string{gateChartRevision, gateNewCfgRevision}, argov1alpha1.SyncStatusCodeSynced),
			},
			appsToSync:  bothSteps,
			expectedMap: bothSteps,
		},
		{
			// A freshly created Application has never been compared against a revision, so there is
			// nothing for it to be behind.
			name: "current step has never been compared",
			appSet: gateAppSet(2,
				gateStatus("grafana-beta2-sta", "1", argov1alpha1.ProgressiveSyncHealthy, nil, at(-10*time.Minute)),
				gateStatus("grafana-web1-sta", "2", argov1alpha1.ProgressiveSyncWaiting, []string{gateChartRevision, gateNewCfgRevision}, at(-5*time.Second)),
			),
			appDependencyList: twoSteps,
			currentApps: []argov1alpha1.Application{
				gateApp("grafana-beta2-sta", "1", nil, argov1alpha1.SyncStatusCodeSynced),
				gateApp("grafana-web1-sta", "2", []string{gateChartRevision, gateNewCfgRevision}, argov1alpha1.SyncStatusCodeOutOfSync),
			},
			appsToSync:  bothSteps,
			expectedMap: bothSteps,
		},
		{
			// Step 2 holds no Waiting Application but step 3 does. Releasing only step 3 would be
			// just as wrong as releasing everything, so every step after the skewed one is withheld.
			name: "skew against a step two ahead",
			appSet: gateAppSet(3,
				gateStatus("grafana-beta2-sta", "1", argov1alpha1.ProgressiveSyncHealthy, []string{gateChartRevision, gateOldCfgRevision}, at(-10*time.Minute)),
				gateStatus("grafana-web1-sta", "2", argov1alpha1.ProgressiveSyncHealthy, []string{gateChartRevision, gateOldCfgRevision}, at(-9*time.Minute)),
				gateStatus("grafana-web2-sta", "3", argov1alpha1.ProgressiveSyncWaiting, []string{gateChartRevision, gateNewCfgRevision}, at(-5*time.Second)),
			),
			appDependencyList: [][]string{{"grafana-beta2-sta"}, {"grafana-web1-sta"}, {"grafana-web2-sta"}},
			currentApps: []argov1alpha1.Application{
				gateApp("grafana-beta2-sta", "1", []string{gateChartRevision, gateOldCfgRevision}, argov1alpha1.SyncStatusCodeSynced),
				gateApp("grafana-web1-sta", "2", []string{gateChartRevision, gateOldCfgRevision}, argov1alpha1.SyncStatusCodeSynced),
				gateApp("grafana-web2-sta", "3", []string{gateChartRevision, gateNewCfgRevision}, argov1alpha1.SyncStatusCodeOutOfSync),
			},
			appsToSync:      map[string]bool{"grafana-beta2-sta": true, "grafana-web1-sta": true, "grafana-web2-sta": true},
			expectedMap:     map[string]bool{"grafana-beta2-sta": true},
			expectedRequeue: revisionSkewRequeueInterval,
		},
		{
			// A commit that does not touch step 1's manifest-generate-paths never refreshes step 1,
			// which from here is indistinguishable from a refresh that is merely late. Past the bound
			// the gate releases the wave rather than stall it for a reconciliation timeout.
			name: "hold expired",
			appSet: gateAppSet(2,
				gateStatus("grafana-beta2-sta", "1", argov1alpha1.ProgressiveSyncHealthy, []string{gateChartRevision, gateOldCfgRevision}, at(-10*time.Minute)),
				gateStatus("grafana-web1-sta", "2", argov1alpha1.ProgressiveSyncWaiting, []string{gateChartRevision, gateNewCfgRevision}, at(-maxRevisionSkewHold-time.Second)),
			),
			appDependencyList: twoSteps,
			currentApps: []argov1alpha1.Application{
				gateApp("grafana-beta2-sta", "1", []string{gateChartRevision, gateOldCfgRevision}, argov1alpha1.SyncStatusCodeSynced),
				gateApp("grafana-web1-sta", "2", []string{gateChartRevision, gateNewCfgRevision}, argov1alpha1.SyncStatusCodeOutOfSync),
			},
			appsToSync:  bothSteps,
			expectedMap: bothSteps,
		},
		{
			// A fresh Waiting transition leaves the whole bound to run, but the gate only asks to be
			// woken every revisionSkewRequeueInterval, not once at the end of it.
			name: "requeue is capped at revisionSkewRequeueInterval",
			appSet: gateAppSet(2,
				gateStatus("grafana-beta2-sta", "1", argov1alpha1.ProgressiveSyncHealthy, []string{gateChartRevision, gateOldCfgRevision}, at(-10*time.Minute)),
				gateStatus("grafana-web1-sta", "2", argov1alpha1.ProgressiveSyncWaiting, []string{gateChartRevision, gateNewCfgRevision}, at(0)),
			),
			appDependencyList: twoSteps,
			currentApps: []argov1alpha1.Application{
				gateApp("grafana-beta2-sta", "1", []string{gateChartRevision, gateOldCfgRevision}, argov1alpha1.SyncStatusCodeSynced),
				gateApp("grafana-web1-sta", "2", []string{gateChartRevision, gateNewCfgRevision}, argov1alpha1.SyncStatusCodeOutOfSync),
			},
			appsToSync:      bothSteps,
			expectedMap:     map[string]bool{"grafana-beta2-sta": true},
			expectedRequeue: revisionSkewRequeueInterval,
		},
		{
			// Near the end of the bound the requeue is clamped to what is left, so the release
			// decision is not deferred past the deadline by a full poll interval.
			name: "requeue never outlasts the remaining hold",
			appSet: gateAppSet(2,
				gateStatus("grafana-beta2-sta", "1", argov1alpha1.ProgressiveSyncHealthy, []string{gateChartRevision, gateOldCfgRevision}, at(-10*time.Minute)),
				gateStatus("grafana-web1-sta", "2", argov1alpha1.ProgressiveSyncWaiting, []string{gateChartRevision, gateNewCfgRevision}, at(-maxRevisionSkewHold+4*time.Second)),
			),
			appDependencyList: twoSteps,
			currentApps: []argov1alpha1.Application{
				gateApp("grafana-beta2-sta", "1", []string{gateChartRevision, gateOldCfgRevision}, argov1alpha1.SyncStatusCodeSynced),
				gateApp("grafana-web1-sta", "2", []string{gateChartRevision, gateNewCfgRevision}, argov1alpha1.SyncStatusCodeOutOfSync),
			},
			appsToSync:      bothSteps,
			expectedMap:     map[string]bool{"grafana-beta2-sta": true},
			expectedRequeue: 4 * time.Second,
		},
		{
			// getAppsToSync had already stopped at step 1, so there is no later step in the map to
			// withhold. Reporting a hold here would be a misleading log line and a requeue for a
			// wave nobody released.
			name: "nothing to withhold because the wave was never released",
			appSet: gateAppSet(2,
				gateStatus("grafana-beta2-sta", "1", argov1alpha1.ProgressiveSyncProgressing, []string{gateChartRevision, gateOldCfgRevision}, at(-10*time.Minute)),
				gateStatus("grafana-web1-sta", "2", argov1alpha1.ProgressiveSyncWaiting, []string{gateChartRevision, gateNewCfgRevision}, at(-5*time.Second)),
			),
			appDependencyList: twoSteps,
			currentApps: []argov1alpha1.Application{
				gateApp("grafana-beta2-sta", "1", []string{gateChartRevision, gateOldCfgRevision}, argov1alpha1.SyncStatusCodeSynced),
				gateApp("grafana-web1-sta", "2", []string{gateChartRevision, gateNewCfgRevision}, argov1alpha1.SyncStatusCodeOutOfSync),
			},
			appsToSync:  map[string]bool{"grafana-beta2-sta": true},
			expectedMap: map[string]bool{"grafana-beta2-sta": true},
		},
		{
			name: "single step",
			appSet: gateAppSet(1,
				gateStatus("grafana-beta2-sta", "1", argov1alpha1.ProgressiveSyncHealthy, []string{gateChartRevision, gateOldCfgRevision}, at(-10*time.Minute)),
			),
			appDependencyList: [][]string{{"grafana-beta2-sta"}},
			currentApps: []argov1alpha1.Application{
				gateApp("grafana-beta2-sta", "1", []string{gateChartRevision, gateOldCfgRevision}, argov1alpha1.SyncStatusCodeSynced),
			},
			appsToSync:  map[string]bool{"grafana-beta2-sta": true},
			expectedMap: map[string]bool{"grafana-beta2-sta": true},
		},
		{
			// The disagreement runs the other way: web1 is Waiting because its generated SPEC changed
			// while it still reports the previous config commit, and beta2 has already moved to the
			// new one. Nothing reachable from the ApplicationSet status orders two commits, so the
			// gate cannot tell this apart from the regression above and holds here too. Pinned
			// deliberately: it is a known, bounded latency cost, not an oversight. The next case is
			// the bound paying out.
			name: "a later step waiting on the older revision is held too, because the direction is not knowable",
			appSet: gateAppSet(2,
				gateStatus("grafana-beta2-sta", "1", argov1alpha1.ProgressiveSyncHealthy, []string{gateChartRevision, gateNewCfgRevision}, at(-2*time.Minute)),
				gateStatus("grafana-web1-sta", "2", argov1alpha1.ProgressiveSyncWaiting, []string{gateChartRevision, gateOldCfgRevision}, at(-5*time.Second)),
			),
			appDependencyList: twoSteps,
			currentApps: []argov1alpha1.Application{
				gateApp("grafana-beta2-sta", "1", []string{gateChartRevision, gateNewCfgRevision}, argov1alpha1.SyncStatusCodeSynced),
				gateApp("grafana-web1-sta", "2", []string{gateChartRevision, gateOldCfgRevision}, argov1alpha1.SyncStatusCodeOutOfSync),
			},
			appsToSync:      bothSteps,
			expectedMap:     map[string]bool{"grafana-beta2-sta": true},
			expectedRequeue: revisionSkewRequeueInterval,
		},
		{
			// Same fixture, but the later step has been Waiting for longer than maxRevisionSkewHold.
			// The gate gives up and releases the wave rather than stalling the rollout on a
			// disagreement it cannot resolve. This is the failsafe that makes the case above a delay.
			name: "the wave is released once the hold has elapsed",
			appSet: gateAppSet(2,
				gateStatus("grafana-beta2-sta", "1", argov1alpha1.ProgressiveSyncHealthy, []string{gateChartRevision, gateNewCfgRevision}, at(-30*time.Minute)),
				gateStatus("grafana-web1-sta", "2", argov1alpha1.ProgressiveSyncWaiting, []string{gateChartRevision, gateOldCfgRevision}, at(-maxRevisionSkewHold-time.Second)),
			),
			appDependencyList: twoSteps,
			currentApps: []argov1alpha1.Application{
				gateApp("grafana-beta2-sta", "1", []string{gateChartRevision, gateNewCfgRevision}, argov1alpha1.SyncStatusCodeSynced),
				gateApp("grafana-web1-sta", "2", []string{gateChartRevision, gateOldCfgRevision}, argov1alpha1.SyncStatusCodeOutOfSync),
			},
			appsToSync:  bothSteps,
			expectedMap: bothSteps,
		},
		{
			// The regression fixture again, with a second step-2 Application that enters Waiting
			// later than the one driving the hold. The bound is measured from the Application the
			// skew was found against, so an unrelated refresh landing mid-hold cannot push the
			// failsafe out; without that scoping this case would still be holding.
			name: "another application entering waiting does not extend an elapsed hold",
			appSet: gateAppSet(2,
				gateStatus("grafana-beta2-sta", "1", argov1alpha1.ProgressiveSyncHealthy, []string{gateChartRevision, gateOldCfgRevision}, at(-30*time.Minute)),
				gateStatus("grafana-web1-sta", "2", argov1alpha1.ProgressiveSyncWaiting, []string{gateChartRevision, gateNewCfgRevision}, at(-maxRevisionSkewHold-time.Second)),
				gateStatus("grafana-web2-sta", "2", argov1alpha1.ProgressiveSyncWaiting, []string{gateChartRevision, gateNewCfgRevision}, at(-time.Second)),
			),
			appDependencyList: [][]string{{"grafana-beta2-sta"}, {"grafana-web1-sta", "grafana-web2-sta"}},
			currentApps: []argov1alpha1.Application{
				gateApp("grafana-beta2-sta", "1", []string{gateChartRevision, gateOldCfgRevision}, argov1alpha1.SyncStatusCodeSynced),
				gateApp("grafana-web1-sta", "2", []string{gateChartRevision, gateNewCfgRevision}, argov1alpha1.SyncStatusCodeOutOfSync),
				gateApp("grafana-web2-sta", "2", []string{gateChartRevision, gateNewCfgRevision}, argov1alpha1.SyncStatusCodeOutOfSync),
			},
			appsToSync:  map[string]bool{"grafana-beta2-sta": true, "grafana-web1-sta": true, "grafana-web2-sta": true},
			expectedMap: map[string]bool{"grafana-beta2-sta": true, "grafana-web1-sta": true, "grafana-web2-sta": true},
		},
		{
			name:              "empty dependency list",
			appSet:            gateAppSet(0),
			appDependencyList: [][]string{},
			appsToSync:        map[string]bool{},
			expectedMap:       map[string]bool{},
		},
	} {
		t.Run(cc.name, func(t *testing.T) {
			t.Parallel()

			appsToSync, requeue := withholdRevisionSkewedSteps(log.NewEntry(log.New()), cc.appSet, cc.appDependencyList, cc.currentApps, cc.appsToSync, now)

			assert.Equal(t, cc.expectedMap, appsToSync, "expected map did not match actual")
			assert.Equal(t, cc.expectedRequeue, requeue, "expected requeue hint did not match actual")
		})
	}
}

func TestRemainingRevisionSkewHold(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 11, 12, 2, 0, time.UTC)
	at := func(d time.Duration) *metav1.Time {
		return new(metav1.NewTime(now.Add(d)))
	}

	for _, cc := range []struct {
		name     string
		laterApp string
		statuses []argov1alpha1.ApplicationSetApplicationStatus
		expected time.Duration
	}{
		{
			// The clock is the Waiting transition of the Application the skew was found against, not
			// the newest one in the ApplicationSet. "b" is newer here and must be ignored: in a
			// many-step ApplicationSet, Applications keep entering Waiting as their own refreshes
			// land, and taking the newest would push the release failsafe out every time one did.
			name:     "measured from the named application, not the newest waiting transition",
			laterApp: "a",
			statuses: []argov1alpha1.ApplicationSetApplicationStatus{
				gateStatus("a", "2", argov1alpha1.ProgressiveSyncWaiting, nil, at(-90*time.Second)),
				gateStatus("b", "3", argov1alpha1.ProgressiveSyncWaiting, nil, at(-20*time.Second)),
			},
			expected: maxRevisionSkewHold - 90*time.Second,
		},
		{
			name:     "slice order does not matter",
			laterApp: "a",
			statuses: []argov1alpha1.ApplicationSetApplicationStatus{
				gateStatus("b", "3", argov1alpha1.ProgressiveSyncWaiting, nil, at(-20*time.Second)),
				gateStatus("a", "2", argov1alpha1.ProgressiveSyncWaiting, nil, at(-90*time.Second)),
			},
			expected: maxRevisionSkewHold - 90*time.Second,
		},
		{
			name:     "zero once the bound has elapsed",
			laterApp: "a",
			statuses: []argov1alpha1.ApplicationSetApplicationStatus{
				gateStatus("a", "2", argov1alpha1.ProgressiveSyncWaiting, nil, at(-maxRevisionSkewHold)),
			},
			expected: 0,
		},
		{
			// An elapsed hold stays elapsed however many other Applications are Waiting. This is the
			// unit-level form of the guarantee: the bound is a bound.
			name:     "an elapsed hold is not extended by other applications",
			laterApp: "a",
			statuses: []argov1alpha1.ApplicationSetApplicationStatus{
				gateStatus("a", "2", argov1alpha1.ProgressiveSyncWaiting, nil, at(-maxRevisionSkewHold-time.Hour)),
				gateStatus("b", "3", argov1alpha1.ProgressiveSyncWaiting, nil, at(0)),
				gateStatus("c", "3", argov1alpha1.ProgressiveSyncWaiting, nil, at(0)),
			},
			expected: 0,
		},
		{
			// Without a timestamp the bound cannot be evaluated, and a hold whose bound cannot be
			// evaluated is not a bound: returning a fresh window here would restart the hold on every
			// requeue and withhold the step forever. Release instead. Unreachable through the write
			// path -- every Waiting transition stamps LastTransitionTime -- and the sibling entry with
			// a valid timestamp must not be borrowed to paper over it.
			name:     "a nil transition time releases rather than restarting the hold",
			laterApp: "a",
			statuses: []argov1alpha1.ApplicationSetApplicationStatus{
				gateStatus("a", "2", argov1alpha1.ProgressiveSyncWaiting, nil, nil),
				gateStatus("b", "3", argov1alpha1.ProgressiveSyncWaiting, nil, at(-30*time.Second)),
			},
			expected: 0,
		},
		{
			name:     "no status entry for the named application releases",
			laterApp: "missing",
			statuses: []argov1alpha1.ApplicationSetApplicationStatus{
				gateStatus("a", "2", argov1alpha1.ProgressiveSyncWaiting, nil, at(-90*time.Second)),
			},
			expected: 0,
		},
		{
			// findStepRevisionSkew only ever names a Waiting Application, so this is defensive: a
			// status that has moved on since is not a clock this hold may use.
			name:     "the named application not in waiting releases",
			laterApp: "a",
			statuses: []argov1alpha1.ApplicationSetApplicationStatus{
				gateStatus("a", "2", argov1alpha1.ProgressiveSyncHealthy, nil, at(-90*time.Second)),
			},
			expected: 0,
		},
		{
			name:     "no statuses at all releases",
			laterApp: "a",
			statuses: nil,
			expected: 0,
		},
	} {
		t.Run(cc.name, func(t *testing.T) {
			t.Parallel()

			appSet := argov1alpha1.ApplicationSet{
				Status: argov1alpha1.ApplicationSetStatus{ApplicationStatus: cc.statuses},
			}
			assert.Equal(t, cc.expected, remainingRevisionSkewHold(&appSet, cc.laterApp, now))
		})
	}
}

// gateDeps mirrors what the ApplicationSet controller's SetAppSetApplicationStatus does to the
// in-memory ApplicationSet: it reassigns Status.ApplicationStatus. That matters here, because the
// statuses getAppsToSync reads are the ones UpdateApplicationSetApplicationStatus just recomputed in
// the same pass. It also records the last statuses persisted so the test can assert on them.
type gateDeps struct {
	statuses []argov1alpha1.ApplicationSetApplicationStatus
}

func (d *gateDeps) SetAppSetApplicationStatus(_ context.Context, _ *log.Entry, applicationSet *argov1alpha1.ApplicationSet, applicationStatuses []argov1alpha1.ApplicationSetApplicationStatus) error {
	applicationSet.Status.ApplicationStatus = applicationStatuses
	d.statuses = applicationStatuses
	return nil
}

func (*gateDeps) SetApplicationSetStatusCondition(_ context.Context, _ *argov1alpha1.ApplicationSet, _ []argov1alpha1.ApplicationSetCondition, _ bool) error {
	return nil
}

func gateStatusOf(t *testing.T, statuses []argov1alpha1.ApplicationSetApplicationStatus, name string) argov1alpha1.ApplicationSetApplicationStatus {
	t.Helper()
	for _, status := range statuses {
		if status.Application == name {
			return status
		}
	}
	require.FailNowf(t, "missing status", "no ApplicationStatus entry for %q", name)
	return argov1alpha1.ApplicationSetApplicationStatus{}
}

// TestPerformProgressiveSyncsHoldsNextStepOnStaleHealthy drives the whole progressive sync pass, not
// just the gate, because the promotion to Pending -- which is what makes the next reconcile stamp a
// sync operation -- happens inside it. The mirror case with the gate disabled reproduces the bug and
// pins the kill switch.
func TestPerformProgressiveSyncsHoldsNextStepOnStaleHealthy(t *testing.T) {
	t.Parallel()

	// beta2 (step 1) has not been refreshed: it still reports Synced against the previous config
	// revision, so revisionsChanged is false for it and its status stays the Healthy it earned in
	// the previous rollout. web1 (step 2) has been refreshed, has observed the new revision and is
	// OutOfSync, so it stays Waiting.
	liveApps := []argov1alpha1.Application{
		gateApp("grafana-beta2-sta", "1", []string{gateChartRevision, gateOldCfgRevision}, argov1alpha1.SyncStatusCodeSynced),
		gateApp("grafana-web1-sta", "2", []string{gateChartRevision, gateNewCfgRevision}, argov1alpha1.SyncStatusCodeOutOfSync),
	}
	waitingSince := new(metav1.NewTime(time.Now().Add(-5 * time.Second)))

	newAppSet := func() argov1alpha1.ApplicationSet {
		return gateAppSet(2,
			gateStatus("grafana-beta2-sta", "1", argov1alpha1.ProgressiveSyncHealthy, []string{gateChartRevision, gateOldCfgRevision}, new(metav1.NewTime(time.Now().Add(-10*time.Minute)))),
			gateStatus("grafana-web1-sta", "2", argov1alpha1.ProgressiveSyncWaiting, []string{gateChartRevision, gateNewCfgRevision}, waitingSince),
		)
	}

	t.Run("gate enabled withholds the next step", func(t *testing.T) {
		t.Parallel()

		deps := &gateDeps{}
		// NewManager is the production constructor, so this also pins the default.
		m := NewManager(nil, nil, deps)
		require.True(t, m.RevisionAwareGate, "the revision aware gate must be on by default")

		appSet := newAppSet()
		appsToSync, requeue, err := m.PerformProgressiveSyncs(t.Context(), log.NewEntry(log.New()), appSet, liveApps, liveApps)
		require.NoError(t, err)

		assert.Equal(t, map[string]bool{"grafana-beta2-sta": true}, appsToSync,
			"step 2 must not be released while step 1 is Healthy for a revision step 2 has moved past")
		assert.Positive(t, requeue, "a withheld wave must ask to be reconsidered; nothing else guarantees a requeue")
		assert.Equal(t, argov1alpha1.ProgressiveSyncWaiting, gateStatusOf(t, deps.statuses, "grafana-web1-sta").Status,
			"step 2 must not be promoted to Pending, which is what stamps a sync operation next reconcile")
	})

	t.Run("kill switch restores the previous behavior", func(t *testing.T) {
		t.Parallel()

		deps := &gateDeps{}
		// A bare struct literal leaves RevisionAwareGate false, which is what
		// --progressive-sync-revision-aware-gate=false produces.
		m := &Manager{dependencies: deps}

		appSet := newAppSet()
		appsToSync, requeue, err := m.PerformProgressiveSyncs(t.Context(), log.NewEntry(log.New()), appSet, liveApps, liveApps)
		require.NoError(t, err)

		assert.Equal(t, map[string]bool{"grafana-beta2-sta": true, "grafana-web1-sta": true}, appsToSync,
			"this is the bug: the gate reads step 1 as Healthy without asking which revision it is Healthy for")
		assert.Zero(t, requeue)
		assert.Equal(t, argov1alpha1.ProgressiveSyncPending, gateStatusOf(t, deps.statuses, "grafana-web1-sta").Status,
			"and step 2 is promoted to Pending, so the next reconcile stamps its sync operation")
	})
}
