package progressivesync

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/v3/pkg/health"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	argov1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

const (
	chartRepo  = "https://charts.example.com"
	configRepo = "git@github.com:acme/config.git"
	oldConfig  = "540d762ff0a0b1c2d3e4f5061728394a5b6c7d8e"
	newConfig  = "6a23cff1b2c3d4e5f60718293a4b5c6d7e8f9012"
	chartRev   = "18.2.1"
)

// source builds a spec-shaped source, the shape ComparedTo records.
func source(repoURL, targetRevision string) argov1alpha1.ApplicationSource {
	return argov1alpha1.ApplicationSource{RepoURL: repoURL, TargetRevision: targetRevision}
}

// multiSourceApp is an Application whose status reports revisions for several compared sources,
// index-aligned with them the way CompareAppState writes them.
func multiSourceApp(name string, sources []argov1alpha1.ApplicationSource, revisions ...string) argov1alpha1.Application {
	return argov1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "argocd"},
		Status: argov1alpha1.ApplicationStatus{
			Sync: argov1alpha1.SyncStatus{
				ComparedTo: argov1alpha1.ComparedTo{Sources: sources},
				Revisions:  revisions,
			},
		},
	}
}

// singleSourceApp is the single-source form: ComparedTo.Source plus Sync.Revision.
func singleSourceApp(name string, src argov1alpha1.ApplicationSource, revision string) argov1alpha1.Application {
	return argov1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "argocd"},
		Status: argov1alpha1.ApplicationStatus{
			Sync: argov1alpha1.SyncStatus{
				ComparedTo: argov1alpha1.ComparedTo{Source: src},
				Revision:   revision,
			},
		},
	}
}

// TestObservedRevisions pins how a slot key is built and how revisions are attributed to slots.
// Every case here is a pairing rule the consensus gate depends on: a misattributed slot either
// invents a disagreement that cannot be resolved, or hides a real one.
func TestObservedRevisions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		app      argov1alpha1.Application
		expected map[sourceSlot]string
	}{
		{
			name:     "single source pairs comparedTo source with sync revision",
			app:      singleSourceApp("app", source(configRepo, "master"), newConfig),
			expected: map[sourceSlot]string{{repoURL: configRepo, targetRevision: "master"}: newConfig},
		},
		{
			name: "multi source pairs comparedTo sources with sync revisions by index",
			app: multiSourceApp("app",
				[]argov1alpha1.ApplicationSource{source(chartRepo, chartRev), source(configRepo, "master")},
				chartRev, newConfig),
			expected: map[sourceSlot]string{
				{repoURL: chartRepo, targetRevision: chartRev}:  chartRev,
				{repoURL: configRepo, targetRevision: "master"}: newConfig,
			},
		},
		{
			name: "revisions wins over revision when both are populated",
			app: func() argov1alpha1.Application {
				app := multiSourceApp("app", []argov1alpha1.ApplicationSource{source(configRepo, "master")}, newConfig)
				// Never written together by the Application controller, but GetRevisions gives
				// Revisions precedence and this must agree with it.
				app.Status.Sync.Revision = oldConfig
				app.Status.Sync.ComparedTo.Source = source(chartRepo, chartRev)
				return app
			}(),
			expected: map[sourceSlot]string{{repoURL: configRepo, targetRevision: "master"}: newConfig},
		},
		{
			name:     "never compared application yields no slots",
			app:      argov1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "app"}},
			expected: nil,
		},
		{
			name: "empty revision entry is dropped",
			app: multiSourceApp("app",
				[]argov1alpha1.ApplicationSource{source(chartRepo, chartRev), source(configRepo, "master")},
				chartRev, ""),
			expected: map[sourceSlot]string{{repoURL: chartRepo, targetRevision: chartRev}: chartRev},
		},
		{
			name: "slot without a repoURL is dropped",
			app: multiSourceApp("app",
				[]argov1alpha1.ApplicationSource{{TargetRevision: "master"}, source(configRepo, "master")},
				oldConfig, newConfig),
			expected: map[sourceSlot]string{{repoURL: configRepo, targetRevision: "master"}: newConfig},
		},
		{
			name: "more revisions than compared sources truncates",
			app: multiSourceApp("app",
				[]argov1alpha1.ApplicationSource{source(configRepo, "master")},
				newConfig, chartRev),
			expected: map[sourceSlot]string{{repoURL: configRepo, targetRevision: "master"}: newConfig},
		},
		{
			name: "duplicate identical slots take the first revision",
			app: multiSourceApp("app",
				[]argov1alpha1.ApplicationSource{source(configRepo, "master"), source(configRepo, "master")},
				newConfig, oldConfig),
			expected: map[sourceSlot]string{{repoURL: configRepo, targetRevision: "master"}: newConfig},
		},
		{
			name: "chart is part of the slot identity",
			app: multiSourceApp("app",
				[]argov1alpha1.ApplicationSource{
					{RepoURL: chartRepo, TargetRevision: "18.*", Chart: "redis"},
					{RepoURL: chartRepo, TargetRevision: "18.*", Chart: "postgres"},
				},
				"18.2.1", "18.5.0"),
			expected: map[sourceSlot]string{
				{repoURL: chartRepo, targetRevision: "18.*", chart: "redis"}:    "18.2.1",
				{repoURL: chartRepo, targetRevision: "18.*", chart: "postgres"}: "18.5.0",
			},
		},
		{
			name: "tagPrefix is part of the slot identity",
			app: multiSourceApp("app",
				[]argov1alpha1.ApplicationSource{
					{RepoURL: configRepo, TargetRevision: "1.0.*", TagPrefix: "component-a/"},
					{RepoURL: configRepo, TargetRevision: "1.0.*", TagPrefix: "component-b/"},
				},
				"component-a/1.0.7", "component-b/1.0.2"),
			expected: map[sourceSlot]string{
				{repoURL: configRepo, targetRevision: "1.0.*", tagPrefix: "component-a/"}: "component-a/1.0.7",
				{repoURL: configRepo, targetRevision: "1.0.*", tagPrefix: "component-b/"}: "component-b/1.0.2",
			},
		},
		{
			name: "path and ref are not part of the slot identity",
			app: multiSourceApp("app",
				[]argov1alpha1.ApplicationSource{
					{RepoURL: configRepo, TargetRevision: "master", Path: "overlays/cluster-a", Ref: "config"},
				},
				newConfig),
			expected: map[sourceSlot]string{{repoURL: configRepo, targetRevision: "master"}: newConfig},
		},
		{
			name: "a spec that has moved ahead of the status does not shift slot attribution",
			// The Application has gained a source that has not been compared yet. Pairing the
			// recorded revision with spec.GetSources()[0] would attribute the config repo's commit
			// to the chart repo, which is a fabricated coordinate no other Application can match -
			// or worse, one that another Application does match, producing a disagreement that does
			// not exist. ComparedTo records what was actually compared, so the slot stays correct.
			app: func() argov1alpha1.Application {
				app := multiSourceApp("app", []argov1alpha1.ApplicationSource{source(configRepo, "master")}, newConfig)
				app.Spec.Sources = argov1alpha1.ApplicationSources{source(chartRepo, chartRev), source(configRepo, "master")}
				return app
			}(),
			expected: map[sourceSlot]string{{repoURL: configRepo, targetRevision: "master"}: newConfig},
		},
		{
			name: "source hydrator application reports its sync source slot",
			app: func() argov1alpha1.Application {
				// A hydrator Application is the single-source form; its compared source is the
				// synthesized sync source, and its revision is the hydrated commit.
				app := singleSourceApp("app", argov1alpha1.ApplicationSource{
					RepoURL:        configRepo,
					Path:           "cluster-a",
					TargetRevision: "environments/prod",
				}, newConfig)
				app.Spec.SourceHydrator = &argov1alpha1.SourceHydrator{
					DrySource:  argov1alpha1.DrySource{RepoURL: configRepo, TargetRevision: "main", Path: "cluster-a"},
					SyncSource: argov1alpha1.SyncSource{TargetBranch: "environments/prod", Path: "cluster-a"},
				}
				return app
			}(),
			expected: map[sourceSlot]string{{repoURL: configRepo, targetRevision: "environments/prod"}: newConfig},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.expected, observedRevisions(&tc.app))
		})
	}
}

// waitingStatus is a progressive sync status entry that arms the gate.
func waitingStatus(app string, transition time.Time) argov1alpha1.ApplicationSetApplicationStatus {
	return progressiveStatus(app, argov1alpha1.ProgressiveSyncWaiting, transition)
}

func progressiveStatus(app string, status argov1alpha1.ProgressiveSyncStatusCode, transition time.Time) argov1alpha1.ApplicationSetApplicationStatus {
	stamped := metav1.NewTime(transition)
	return argov1alpha1.ApplicationSetApplicationStatus{
		Application:        app,
		Status:             status,
		LastTransitionTime: &stamped,
	}
}

func appSetWithStatuses(statuses ...argov1alpha1.ApplicationSetApplicationStatus) *argov1alpha1.ApplicationSet {
	return &argov1alpha1.ApplicationSet{
		ObjectMeta: metav1.ObjectMeta{Name: "appset", Namespace: "argocd"},
		Status:     argov1alpha1.ApplicationSetStatus{ApplicationStatus: statuses},
	}
}

// TestEvaluateRevisionConsensus pins the gate and, more importantly, every way out of it. A gate
// that withholds a rollout must never withhold one it does not understand, so each subtest below
// names the escape it exercises.
func TestEvaluateRevisionConsensus(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	timeout := 2 * time.Minute

	// The production shape: two sources, the chart pinned to a tag and agreeing, the config repo
	// tracking master and disagreeing because only one of the two Applications has been refreshed.
	incidentSources := []argov1alpha1.ApplicationSource{source(chartRepo, chartRev), source(configRepo, "master")}
	behind := multiSourceApp("migration", incidentSources, chartRev, oldConfig)
	ahead := multiSourceApp("web", incidentSources, chartRev, newConfig)

	bothSteps := map[string]int{"migration": 0, "web": 1}

	for _, tc := range []struct {
		name              string
		appset            *argov1alpha1.ApplicationSet
		applications      []argov1alpha1.Application
		appStepMap        map[string]int
		timeout           time.Duration
		expectedHold      bool
		expectedExpired   bool
		expectedRemaining time.Duration
		expectedSlot      string
		expectedDetail    string
	}{
		{
			name:         "one of two sources diverging holds the rollout",
			appset:       appSetWithStatuses(progressiveStatus("migration", argov1alpha1.ProgressiveSyncHealthy, now.Add(-time.Hour)), waitingStatus("web", now.Add(-30*time.Second))),
			applications: []argov1alpha1.Application{behind, ahead},
			appStepMap:   bothSteps,
			timeout:      timeout,
			expectedHold: true,
			// Only the config slot diverges; the chart slot agrees and is not reported.
			expectedSlot:      configRepo + "@master",
			expectedDetail:    oldConfig + "=migration " + newConfig + "=web",
			expectedRemaining: 90 * time.Second,
		},
		{
			name:         "converged multi source appset does not hold",
			appset:       appSetWithStatuses(waitingStatus("migration", now), waitingStatus("web", now)),
			applications: []argov1alpha1.Application{ahead, multiSourceApp("migration", incidentSources, chartRev, newConfig)},
			appStepMap:   bothSteps,
			timeout:      timeout,
		},
		{
			name: "nothing waiting means the gate is off",
			// The only promotion RollingSync makes is Waiting -> Pending, so with nothing waiting
			// there is nothing to withhold. This is what keeps an ApplicationSet whose
			// Applications legitimately never agree from being gated for its whole life.
			appset: appSetWithStatuses(
				progressiveStatus("migration", argov1alpha1.ProgressiveSyncHealthy, now.Add(-time.Hour)),
				progressiveStatus("web", argov1alpha1.ProgressiveSyncHealthy, now),
			),
			applications: []argov1alpha1.Application{behind, ahead},
			appStepMap:   bothSteps,
			timeout:      timeout,
		},
		{
			name: "only a step -1 application is waiting so the gate is off",
			// Nothing ever promotes an Application no step selects out of Waiting, so counting it
			// as evidence would arm the gate for the lifetime of the ApplicationSet.
			appset: appSetWithStatuses(
				progressiveStatus("migration", argov1alpha1.ProgressiveSyncHealthy, now.Add(-time.Hour)),
				waitingStatus("orphan", now),
			),
			applications: []argov1alpha1.Application{behind, ahead, multiSourceApp("orphan", incidentSources, chartRev, newConfig)},
			appStepMap:   map[string]int{"migration": 0, "web": 1},
			timeout:      timeout,
			// web is not Waiting either, so no step-selected Application is.
		},
		{
			name: "a diverging step -1 application is not a participant",
			appset: appSetWithStatuses(
				progressiveStatus("migration", argov1alpha1.ProgressiveSyncHealthy, now.Add(-time.Hour)),
				waitingStatus("web", now),
			),
			applications: []argov1alpha1.Application{
				multiSourceApp("migration", incidentSources, chartRev, newConfig),
				ahead,
				// Left behind on the old commit forever, and never synced by this rollout.
				multiSourceApp("orphan", incidentSources, chartRev, oldConfig),
			},
			appStepMap: bothSteps,
			timeout:    timeout,
		},
		{
			name:   "different target revisions are never compared",
			appset: appSetWithStatuses(waitingStatus("migration", now), waitingStatus("web", now)),
			applications: []argov1alpha1.Application{
				singleSourceApp("migration", source(configRepo, "release-1.2"), oldConfig),
				singleSourceApp("web", source(configRepo, "master"), newConfig),
			},
			appStepMap: bothSteps,
			timeout:    timeout,
		},
		{
			name:   "different charts on one repo are never compared",
			appset: appSetWithStatuses(waitingStatus("migration", now), waitingStatus("web", now)),
			applications: []argov1alpha1.Application{
				singleSourceApp("migration", argov1alpha1.ApplicationSource{RepoURL: chartRepo, TargetRevision: "18.*", Chart: "postgres"}, "18.5.0"),
				singleSourceApp("web", argov1alpha1.ApplicationSource{RepoURL: chartRepo, TargetRevision: "18.*", Chart: "redis"}, "18.2.1"),
			},
			appStepMap: bothSteps,
			timeout:    timeout,
		},
		{
			name:   "never synced application does not hold",
			appset: appSetWithStatuses(waitingStatus("migration", now), waitingStatus("web", now)),
			applications: []argov1alpha1.Application{
				{ObjectMeta: metav1.ObjectMeta{Name: "migration", Namespace: "argocd"}},
				ahead,
			},
			appStepMap: bothSteps,
			timeout:    timeout,
		},
		{
			name:   "terminating application does not hold",
			appset: appSetWithStatuses(waitingStatus("migration", now), waitingStatus("web", now)),
			applications: []argov1alpha1.Application{
				func() argov1alpha1.Application {
					app := behind.DeepCopy()
					deleted := metav1.NewTime(now.Add(-time.Minute))
					app.DeletionTimestamp = &deleted
					return *app
				}(),
				ahead,
			},
			appStepMap: bothSteps,
			timeout:    timeout,
		},
		{
			name:   "application with a comparison error does not hold",
			appset: appSetWithStatuses(waitingStatus("migration", now), waitingStatus("web", now)),
			applications: []argov1alpha1.Application{
				withCondition(behind, argov1alpha1.ApplicationConditionComparisonError),
				ahead,
			},
			appStepMap: bothSteps,
			timeout:    timeout,
		},
		{
			name:   "application with an invalid spec error does not hold",
			appset: appSetWithStatuses(waitingStatus("migration", now), waitingStatus("web", now)),
			applications: []argov1alpha1.Application{
				withCondition(behind, argov1alpha1.ApplicationConditionInvalidSpecError),
				ahead,
			},
			appStepMap: bothSteps,
			timeout:    timeout,
		},
		{
			name:   "a status entry without a live application does not hold",
			appset: appSetWithStatuses(waitingStatus("migration", now), waitingStatus("web", now)),
			// migration has already been deleted, so only web is left and nothing disagrees.
			applications: []argov1alpha1.Application{ahead},
			appStepMap:   bothSteps,
			timeout:      timeout,
		},
		{
			name:   "applications with different source counts are compared on the slots they share",
			appset: appSetWithStatuses(waitingStatus("migration", now), waitingStatus("web", now)),
			applications: []argov1alpha1.Application{
				// The shared config repo sits at slot 1 here and at slot 2 there. A whole-list
				// comparison classifies these as unrelated and misses the divergence entirely.
				multiSourceApp("migration",
					[]argov1alpha1.ApplicationSource{source(chartRepo, chartRev), source(configRepo, "master")},
					chartRev, oldConfig),
				multiSourceApp("web",
					[]argov1alpha1.ApplicationSource{source(chartRepo, chartRev), source("https://charts.example.com/extra", "1.0.0"), source(configRepo, "master")},
					chartRev, "1.0.0", newConfig),
			},
			appStepMap:        bothSteps,
			timeout:           timeout,
			expectedHold:      true,
			expectedSlot:      configRepo + "@master",
			expectedDetail:    oldConfig + "=migration " + newConfig + "=web",
			expectedRemaining: timeout,
		},
		{
			name:              "zero timeout holds indefinitely",
			appset:            appSetWithStatuses(progressiveStatus("migration", argov1alpha1.ProgressiveSyncHealthy, now.Add(-24*time.Hour)), waitingStatus("web", now.Add(-24*time.Hour))),
			applications:      []argov1alpha1.Application{behind, ahead},
			appStepMap:        bothSteps,
			timeout:           0,
			expectedHold:      true,
			expectedSlot:      configRepo + "@master",
			expectedDetail:    oldConfig + "=migration " + newConfig + "=web",
			expectedRemaining: 0,
		},
		{
			name:            "elapsed timeout fails open and reports expired",
			appset:          appSetWithStatuses(progressiveStatus("migration", argov1alpha1.ProgressiveSyncHealthy, now.Add(-time.Hour)), waitingStatus("web", now.Add(-3*time.Minute))),
			applications:    []argov1alpha1.Application{behind, ahead},
			appStepMap:      bothSteps,
			timeout:         timeout,
			expectedExpired: true,
			expectedSlot:    configRepo + "@master",
			expectedDetail:  oldConfig + "=migration " + newConfig + "=web",
		},
		{
			name: "the deadline anchors on the newest step-selected transition of any status",
			// This is the anti-deadlock choice, and it is not a Waiting-only anchor: a Waiting
			// transition is stamped once and never restamped, so on a rollout whose earlier steps
			// run longer than the timeout a Waiting-only anchor is already expired by the time the
			// gate is first consulted, and the protection is silently gone. Here web has been
			// Waiting for an hour while migration reached Healthy a moment ago; the gate must still
			// hold.
			appset: appSetWithStatuses(
				waitingStatus("web", now.Add(-time.Hour)),
				progressiveStatus("migration", argov1alpha1.ProgressiveSyncHealthy, now.Add(-30*time.Second)),
			),
			applications:      []argov1alpha1.Application{behind, ahead},
			appStepMap:        bothSteps,
			timeout:           timeout,
			expectedHold:      true,
			expectedSlot:      configRepo + "@master",
			expectedDetail:    oldConfig + "=migration " + newConfig + "=web",
			expectedRemaining: 90 * time.Second,
		},
		{
			name: "a step -1 transition does not extend the deadline",
			// Otherwise an Application no step selects could keep re-arming the bound forever and
			// the hold would not be bounded at all.
			appset: appSetWithStatuses(
				waitingStatus("web", now.Add(-3*time.Minute)),
				progressiveStatus("orphan", argov1alpha1.ProgressiveSyncHealthy, now),
			),
			applications:    []argov1alpha1.Application{behind, ahead},
			appStepMap:      bothSteps,
			timeout:         timeout,
			expectedExpired: true,
			expectedSlot:    configRepo + "@master",
			expectedDetail:  oldConfig + "=migration " + newConfig + "=web",
		},
		{
			name: "a transition in the future cannot extend the bound past the timeout",
			appset: appSetWithStatuses(
				waitingStatus("web", now.Add(time.Hour)),
				progressiveStatus("migration", argov1alpha1.ProgressiveSyncHealthy, now),
			),
			applications:      []argov1alpha1.Application{behind, ahead},
			appStepMap:        bothSteps,
			timeout:           timeout,
			expectedHold:      true,
			expectedSlot:      configRepo + "@master",
			expectedDetail:    oldConfig + "=migration " + newConfig + "=web",
			expectedRemaining: timeout,
		},
		{
			name: "no transition time on any step-selected status fails open",
			appset: appSetWithStatuses(
				argov1alpha1.ApplicationSetApplicationStatus{Application: "migration", Status: argov1alpha1.ProgressiveSyncHealthy},
				argov1alpha1.ApplicationSetApplicationStatus{Application: "web", Status: argov1alpha1.ProgressiveSyncWaiting},
			),
			applications:    []argov1alpha1.Application{behind, ahead},
			appStepMap:      bothSteps,
			timeout:         timeout,
			expectedExpired: true,
			expectedSlot:    configRepo + "@master",
			expectedDetail:  oldConfig + "=migration " + newConfig + "=web",
		},
		{
			name: "a spec that has moved ahead of the status does not fabricate a disagreement",
			// Both Applications have been compared against the same config commit, so nothing
			// diverges. web's spec has since gained a chart source that has not been compared yet:
			// reading coordinates from the spec would pair web's config commit with the chart repo
			// and report a disagreement against migration's chart revision that does not exist.
			appset: appSetWithStatuses(waitingStatus("migration", now), waitingStatus("web", now)),
			applications: []argov1alpha1.Application{
				func() argov1alpha1.Application {
					app := multiSourceApp("migration", []argov1alpha1.ApplicationSource{source(chartRepo, chartRev), source(configRepo, "master")}, chartRev, newConfig)
					app.Spec.Sources = argov1alpha1.ApplicationSources{source(chartRepo, chartRev), source(configRepo, "master")}
					return app
				}(),
				func() argov1alpha1.Application {
					// The chart source is in the spec but has not been compared yet, so the status
					// still holds one revision for one source.
					app := multiSourceApp("web", []argov1alpha1.ApplicationSource{source(configRepo, "master")}, newConfig)
					app.Spec.Sources = argov1alpha1.ApplicationSources{source(chartRepo, chartRev), source(configRepo, "master")}
					return app
				}(),
			},
			appStepMap: bothSteps,
			timeout:    timeout,
		},
		{
			name:   "the reported group is the lexicographically lowest diverging slot",
			appset: appSetWithStatuses(waitingStatus("migration", now), waitingStatus("web", now)),
			applications: []argov1alpha1.Application{
				multiSourceApp("migration", incidentSources, "18.0.0", oldConfig),
				multiSourceApp("web", incidentSources, chartRev, newConfig),
			},
			appStepMap:        bothSteps,
			timeout:           timeout,
			expectedHold:      true,
			expectedSlot:      configRepo + "@master",
			expectedDetail:    oldConfig + "=migration " + newConfig + "=web",
			expectedRemaining: timeout,
		},
		{
			name:   "three applications report every side of the disagreement",
			appset: appSetWithStatuses(waitingStatus("migration", now), waitingStatus("web", now), waitingStatus("worker", now)),
			applications: []argov1alpha1.Application{
				singleSourceApp("migration", source(configRepo, "master"), oldConfig),
				singleSourceApp("web", source(configRepo, "master"), newConfig),
				singleSourceApp("worker", source(configRepo, "master"), newConfig),
			},
			appStepMap:        map[string]int{"migration": 0, "web": 1, "worker": 1},
			timeout:           timeout,
			expectedHold:      true,
			expectedSlot:      configRepo + "@master",
			expectedDetail:    oldConfig + "=migration " + newConfig + "=web,worker",
			expectedRemaining: timeout,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			consensus := evaluateRevisionConsensus(tc.appset, tc.applications, tc.appStepMap, tc.timeout, now)

			assert.Equal(t, tc.expectedHold, consensus.Hold, "hold")
			assert.Equal(t, tc.expectedExpired, consensus.Expired, "expired")
			assert.Equal(t, tc.expectedRemaining, consensus.Remaining, "remaining")
			if tc.expectedSlot == "" {
				assert.Empty(t, consensus.Revisions)
				return
			}
			assert.Equal(t, tc.expectedSlot, consensus.Slot.String())
			assert.Equal(t, tc.expectedDetail, consensus.LogDetail())
		})
	}
}

func withCondition(app argov1alpha1.Application, conditionType argov1alpha1.ApplicationConditionType) argov1alpha1.Application {
	out := app.DeepCopy()
	out.Status.Conditions = append(out.Status.Conditions, argov1alpha1.ApplicationCondition{
		Type:    conditionType,
		Message: "manifest generation error",
	})
	return *out
}

// TestRevisionConsensusRequeueAfter pins the poll interval. A hold that returns no requeue can
// outlive its own condition: an Application refreshing into agreement changes neither its sync nor
// its health status, so shouldRequeueForApplication produces no event, and a cluster generator
// ApplicationSet has no periodic requeue either.
func TestRevisionConsensusRequeueAfter(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		consensus revisionConsensus
		expected  time.Duration
	}{
		{
			name:      "not holding needs no requeue",
			consensus: revisionConsensus{},
		},
		{
			name:      "expired needs no requeue",
			consensus: revisionConsensus{Expired: true},
		},
		{
			name:      "a long bound is polled at the poll interval",
			consensus: revisionConsensus{Hold: true, Remaining: 2 * time.Minute},
			expected:  revisionConsensusRequeueInterval,
		},
		{
			name:      "an unbounded hold is still polled",
			consensus: revisionConsensus{Hold: true},
			expected:  revisionConsensusRequeueInterval,
		},
		{
			name:      "a bound shorter than the poll interval wins so the bound is evaluated on time",
			consensus: revisionConsensus{Hold: true, Remaining: 3 * time.Second},
			expected:  3 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.expected, tc.consensus.RequeueAfter())
		})
	}
}

func TestRevisionConsensusLogDetail(t *testing.T) {
	t.Parallel()

	consensus := revisionConsensus{
		Revisions: map[string][]string{
			newConfig: {"web2", "web1"},
			oldConfig: {"migration"},
		},
	}
	// Revisions sorted, then Applications within each revision, so the field is stable across
	// reconciliations and Go map iteration order.
	assert.Equal(t, oldConfig+"=migration "+newConfig+"=web1,web2", consensus.LogDetail())
}

// consensusDeps records the statuses progressive sync persists and applies them to the
// ApplicationSet the way the real dependency does, so a later phase of the same pass reads what an
// earlier one wrote.
type consensusDeps struct {
	writes [][]argov1alpha1.ApplicationSetApplicationStatus
}

func (d *consensusDeps) SetAppSetApplicationStatus(_ context.Context, _ *log.Entry, applicationSet *argov1alpha1.ApplicationSet, statuses []argov1alpha1.ApplicationSetApplicationStatus) error {
	d.writes = append(d.writes, statuses)
	applicationSet.Status.ApplicationStatus = make([]argov1alpha1.ApplicationSetApplicationStatus, len(statuses))
	copy(applicationSet.Status.ApplicationStatus, statuses)
	sort.Slice(applicationSet.Status.ApplicationStatus, func(i, j int) bool {
		return applicationSet.Status.ApplicationStatus[i].Application < applicationSet.Status.ApplicationStatus[j].Application
	})
	return nil
}

func (d *consensusDeps) SetApplicationSetStatusCondition(_ context.Context, _ *argov1alpha1.ApplicationSet, _ []argov1alpha1.ApplicationSetCondition, _ bool) error {
	return nil
}

// last returns the statuses of the most recent write, which is the one
// UpdateApplicationSetApplicationStatusProgress made.
func (d *consensusDeps) last() map[string]argov1alpha1.ProgressiveSyncStatusCode {
	out := map[string]argov1alpha1.ProgressiveSyncStatusCode{}
	if len(d.writes) == 0 {
		return out
	}
	for _, status := range d.writes[len(d.writes)-1] {
		out[status.Application] = status.Status
	}
	return out
}

// TestPerformProgressiveSyncsRevisionConsensus drives the whole pass on the shape production went
// wrong in: a two-step rollout whose step 1 Application (the database migration) has not been
// refreshed against the new commit and therefore still reports the Healthy it earned in the
// previous rollout, while step 2 (the web tier) has been refreshed and sits in Waiting.
func TestPerformProgressiveSyncsRevisionConsensus(t *testing.T) {
	t.Parallel()

	incidentSources := []argov1alpha1.ApplicationSource{source(chartRepo, chartRev), source(configRepo, "master")}

	// app builds the live Application: index-aligned compared sources and revisions, plus the
	// health and sync status the progressive sync status machine reads.
	app := func(name, step, configRevision string, syncStatus argov1alpha1.SyncStatusCode) argov1alpha1.Application {
		out := multiSourceApp(name, incidentSources, chartRev, configRevision)
		out.Labels = map[string]string{"stage": step}
		out.Spec.Sources = incidentSources
		out.Spec.Destination = argov1alpha1.ApplicationDestination{Server: "https://kubernetes.default.svc", Namespace: name}
		out.Status.Sync.Status = syncStatus
		out.Status.Health = argov1alpha1.AppHealthStatus{Status: health.HealthStatusHealthy}
		return out
	}

	for _, tc := range []struct {
		name                  string
		requireConsensus      bool
		timeout               time.Duration
		migrationConfigRev    string
		transitionAge         time.Duration
		expectedAppsToSync    map[string]bool
		expectedRequeue       time.Duration
		expectedFinalStatuses map[string]argov1alpha1.ProgressiveSyncStatusCode
	}{
		{
			// The bug, reproduced: the gate is off by default, so step 2 is released off a Healthy
			// that belongs to the previous commit and the web tier is promoted ahead of the
			// migration.
			name:               "flag off releases the later step on a stale Healthy",
			requireConsensus:   false,
			migrationConfigRev: oldConfig,
			transitionAge:      30 * time.Second,
			expectedAppsToSync: map[string]bool{"migration": true, "web": true},
			expectedFinalStatuses: map[string]argov1alpha1.ProgressiveSyncStatusCode{
				"migration": argov1alpha1.ProgressiveSyncHealthy,
				"web":       argov1alpha1.ProgressiveSyncPending,
			},
		},
		{
			name:               "flag on promotes nothing while the revisions disagree",
			requireConsensus:   true,
			timeout:            2 * time.Minute,
			migrationConfigRev: oldConfig,
			transitionAge:      30 * time.Second,
			expectedAppsToSync: map[string]bool{},
			expectedRequeue:    revisionConsensusRequeueInterval,
			expectedFinalStatuses: map[string]argov1alpha1.ProgressiveSyncStatusCode{
				"migration": argov1alpha1.ProgressiveSyncHealthy,
				// Still Waiting: no sync operation is stamped on an Application that is not Pending.
				"web": argov1alpha1.ProgressiveSyncWaiting,
			},
		},
		{
			name:               "flag on adds nothing once the revisions agree",
			requireConsensus:   true,
			timeout:            2 * time.Minute,
			migrationConfigRev: newConfig,
			transitionAge:      30 * time.Second,
			expectedAppsToSync: map[string]bool{"migration": true, "web": true},
			expectedFinalStatuses: map[string]argov1alpha1.ProgressiveSyncStatusCode{
				"migration": argov1alpha1.ProgressiveSyncHealthy,
				"web":       argov1alpha1.ProgressiveSyncPending,
			},
		},
		{
			// The bound has run out, so the gate fails open rather than wedging the rollout. This
			// is the documented trade: past the bound the protection is gone, by design.
			name:               "flag on releases the wave once the bound has elapsed",
			requireConsensus:   true,
			timeout:            2 * time.Minute,
			migrationConfigRev: oldConfig,
			transitionAge:      10 * time.Minute,
			expectedAppsToSync: map[string]bool{"migration": true, "web": true},
			expectedFinalStatuses: map[string]argov1alpha1.ProgressiveSyncStatusCode{
				"migration": argov1alpha1.ProgressiveSyncHealthy,
				"web":       argov1alpha1.ProgressiveSyncPending,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transition := metav1.NewTime(time.Now().Add(-tc.transitionAge))
			appset := argov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{Name: "appset", Namespace: "argocd"},
				Spec: argov1alpha1.ApplicationSetSpec{
					Strategy: &argov1alpha1.ApplicationSetStrategy{
						Type: "RollingSync",
						RollingSync: &argov1alpha1.ApplicationSetRolloutStrategy{
							Steps: []argov1alpha1.ApplicationSetRolloutStep{
								{MatchExpressions: []argov1alpha1.ApplicationMatchExpression{{Key: "stage", Operator: "In", Values: []string{"migration"}}}},
								{MatchExpressions: []argov1alpha1.ApplicationMatchExpression{{Key: "stage", Operator: "In", Values: []string{"web"}}}},
							},
						},
					},
				},
				Status: argov1alpha1.ApplicationSetStatus{
					ApplicationStatus: []argov1alpha1.ApplicationSetApplicationStatus{
						{
							Application: "migration",
							Status:      argov1alpha1.ProgressiveSyncHealthy,
							Step:        "1",
							// The revisions this Application reports, which is why
							// revisionsChanged is false and its Healthy is not revisited: the
							// commit is invisible to it until the Application controller refreshes.
							TargetRevisions:    []string{chartRev, tc.migrationConfigRev},
							LastTransitionTime: &transition,
						},
						{
							Application:        "web",
							Status:             argov1alpha1.ProgressiveSyncWaiting,
							Step:               "2",
							TargetRevisions:    []string{chartRev, newConfig},
							LastTransitionTime: &transition,
						},
					},
				},
			}

			migration := app("migration", "migration", tc.migrationConfigRev, argov1alpha1.SyncStatusCodeSynced)
			web := app("web", "web", newConfig, argov1alpha1.SyncStatusCodeOutOfSync)
			applications := []argov1alpha1.Application{migration, web}

			deps := &consensusDeps{}
			m := &Manager{
				dependencies:             deps,
				RequireRevisionConsensus: tc.requireConsensus,
				RevisionConsensusTimeout: tc.timeout,
			}

			// desiredApplications are the Applications as generated, so specChanged stays false and
			// these cases isolate the revision skew.
			appsToSync, requeue, err := m.PerformProgressiveSyncs(context.Background(), log.NewEntry(log.New()), appset, applications, applications)
			require.NoError(t, err)

			assert.Equal(t, tc.expectedAppsToSync, appsToSync)
			assert.Equal(t, tc.expectedRequeue, requeue)
			assert.Equal(t, tc.expectedFinalStatuses, deps.last())
		})
	}
}
