package progressivesync

import (
	"context"
	"slices"
	"testing"

	"github.com/argoproj/argo-cd/gitops-engine/v3/pkg/health"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

const (
	chartsRepoURL   = "https://github.com/AirHelp/charts.git"
	ahConfigRepoURL = "https://github.com/AirHelp/ah-config.git"
	helmRepoURL     = "https://charts.example.com"
)

// rolloutSources mirrors a typical multi-source Application: a chart repository plus a repository
// holding the values, both tracked on master.
func rolloutSources() []v1alpha1.ApplicationSource {
	return []v1alpha1.ApplicationSource{
		{RepoURL: chartsRepoURL, TargetRevision: "master", Chart: "generic-service"},
		{RepoURL: ahConfigRepoURL, TargetRevision: "master", Path: "apps/payments"},
	}
}

func newRolloutApp(name string, sources []v1alpha1.ApplicationSource, revisions []string, annotations map[string]string) v1alpha1.Application {
	return v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "argocd",
			Annotations: annotations,
		},
		Spec: v1alpha1.ApplicationSpec{
			Sources: sources,
		},
		Status: v1alpha1.ApplicationStatus{
			Sync: v1alpha1.SyncStatus{
				Status:    v1alpha1.SyncStatusCodeSynced,
				Revisions: revisions,
			},
			Health: v1alpha1.AppHealthStatus{Status: health.HealthStatusHealthy},
		},
	}
}

// helmChartSource is a Helm-repository source: the chart name, not the path, is what picks which
// artifact the semver targetRevision resolves against.
func helmChartSource(chart string) []v1alpha1.ApplicationSource {
	return []v1alpha1.ApplicationSource{
		{RepoURL: helmRepoURL, TargetRevision: "18.*", Chart: chart},
	}
}

// withError gives an Application a condition that stops it reconciling, so its resolved revisions
// will never advance no matter how many refreshes it is sent.
func withError(app v1alpha1.Application) v1alpha1.Application {
	app.Status.Conditions = []v1alpha1.ApplicationCondition{{
		Type:    v1alpha1.ApplicationConditionInvalidSpecError,
		Message: "Unable to generate manifests: repository not accessible",
	}}
	return app
}

func newRolloutAppStatus(name string, status v1alpha1.ProgressiveSyncStatusCode, step string, targetRevisions []string) v1alpha1.ApplicationSetApplicationStatus {
	return v1alpha1.ApplicationSetApplicationStatus{
		Application:     name,
		Status:          status,
		Step:            step,
		TargetRevisions: targetRevisions,
	}
}

// TestRefreshApplicationsBehindRollout covers the core of --progressive-sync-refresh-all: an
// Application that still reports the previous revision while another Application in the same
// ApplicationSet has already registered the new one has not been refreshed yet, and its Healthy is
// about the wrong revision.
func TestRefreshApplicationsBehindRollout(t *testing.T) {
	t.Parallel()

	oldRevisions := []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "1111111111111111111111111111111111111111"}
	newRevisions := []string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "2222222222222222222222222222222222222222"}

	otherSources := []v1alpha1.ApplicationSource{
		{RepoURL: "https://github.com/AirHelp/other-charts.git", TargetRevision: "main", Chart: "generic-service"},
		{RepoURL: ahConfigRepoURL, TargetRevision: "production", Path: "apps/payments"},
	}

	for _, cc := range []struct {
		name            string
		apps            []v1alpha1.Application
		appStatuses     []v1alpha1.ApplicationSetApplicationStatus
		expectedBehind  int
		expectedPatched []string
	}{
		{
			name: "refreshes a healthy application still sitting on the previous revision",
			apps: []v1alpha1.Application{
				newRolloutApp("app-step1", rolloutSources(), oldRevisions, nil),
				newRolloutApp("app-step2", rolloutSources(), newRevisions, nil),
			},
			appStatuses: []v1alpha1.ApplicationSetApplicationStatus{
				newRolloutAppStatus("app-step1", v1alpha1.ProgressiveSyncHealthy, "1", oldRevisions),
				newRolloutAppStatus("app-step2", v1alpha1.ProgressiveSyncWaiting, "2", newRevisions),
			},
			expectedBehind:  1,
			expectedPatched: []string{"app-step1"},
		},
		{
			name: "leaves an application that already observed the revision alone",
			apps: []v1alpha1.Application{
				newRolloutApp("app-step1", rolloutSources(), newRevisions, nil),
				newRolloutApp("app-step2", rolloutSources(), newRevisions, nil),
			},
			appStatuses: []v1alpha1.ApplicationSetApplicationStatus{
				newRolloutAppStatus("app-step1", v1alpha1.ProgressiveSyncHealthy, "1", newRevisions),
				newRolloutAppStatus("app-step2", v1alpha1.ProgressiveSyncWaiting, "2", newRevisions),
			},
			expectedBehind: 0,
		},
		{
			name: "does not re-request a refresh that is already pending",
			apps: []v1alpha1.Application{
				newRolloutApp("app-step1", rolloutSources(), oldRevisions, map[string]string{
					v1alpha1.AnnotationKeyRefresh: string(v1alpha1.RefreshTypeNormal),
				}),
				newRolloutApp("app-step2", rolloutSources(), newRevisions, nil),
			},
			appStatuses: []v1alpha1.ApplicationSetApplicationStatus{
				newRolloutAppStatus("app-step1", v1alpha1.ProgressiveSyncHealthy, "1", oldRevisions),
				newRolloutAppStatus("app-step2", v1alpha1.ProgressiveSyncWaiting, "2", newRevisions),
			},
			// Still behind: the Application controller has not consumed the annotation yet, so the
			// caller must keep deferring. Patching it a second time would only churn the object.
			expectedBehind: 1,
		},
		{
			name: "does nothing when no application has registered a change",
			apps: []v1alpha1.Application{
				newRolloutApp("app-step1", rolloutSources(), oldRevisions, nil),
				newRolloutApp("app-step2", rolloutSources(), newRevisions, nil),
			},
			appStatuses: []v1alpha1.ApplicationSetApplicationStatus{
				newRolloutAppStatus("app-step1", v1alpha1.ProgressiveSyncHealthy, "1", oldRevisions),
				newRolloutAppStatus("app-step2", v1alpha1.ProgressiveSyncHealthy, "2", newRevisions),
			},
			expectedBehind: 0,
		},
		{
			name: "leaves applications tracking different sources alone",
			apps: []v1alpha1.Application{
				newRolloutApp("app-step1", otherSources, oldRevisions, nil),
				newRolloutApp("app-step2", rolloutSources(), newRevisions, nil),
			},
			appStatuses: []v1alpha1.ApplicationSetApplicationStatus{
				newRolloutAppStatus("app-step1", v1alpha1.ProgressiveSyncHealthy, "1", oldRevisions),
				newRolloutAppStatus("app-step2", v1alpha1.ProgressiveSyncWaiting, "2", newRevisions),
			},
			expectedBehind: 0,
		},
		{
			name: "skips applications that are already moving on the change",
			apps: []v1alpha1.Application{
				newRolloutApp("app-step1", rolloutSources(), oldRevisions, nil),
				newRolloutApp("app-step2", rolloutSources(), newRevisions, nil),
			},
			appStatuses: []v1alpha1.ApplicationSetApplicationStatus{
				newRolloutAppStatus("app-step1", v1alpha1.ProgressiveSyncProgressing, "1", oldRevisions),
				newRolloutAppStatus("app-step2", v1alpha1.ProgressiveSyncWaiting, "2", newRevisions),
			},
			expectedBehind: 0,
		},
		{
			name: "skips an application that has never been compared",
			apps: []v1alpha1.Application{
				newRolloutApp("app-step1", rolloutSources(), nil, nil),
				newRolloutApp("app-step2", rolloutSources(), newRevisions, nil),
			},
			appStatuses: []v1alpha1.ApplicationSetApplicationStatus{
				newRolloutAppStatus("app-step1", v1alpha1.ProgressiveSyncHealthy, "1", nil),
				newRolloutAppStatus("app-step2", v1alpha1.ProgressiveSyncWaiting, "2", newRevisions),
			},
			expectedBehind: 0,
		},
		{
			// Two charts from the same Helm repository on the same semver constraint resolve to
			// different chart versions. Keying on repoURL@targetRevision alone would group them and
			// demand a consensus they can never reach, which with no deadline on the hold is a
			// permanent stall of the rollout rather than a delay.
			name: "leaves helm applications that differ only by chart alone",
			apps: []v1alpha1.Application{
				newRolloutApp("app-step1", helmChartSource("redis"), []string{"18.1.0"}, nil),
				newRolloutApp("app-step2", helmChartSource("postgres"), []string{"18.4.2"}, nil),
			},
			appStatuses: []v1alpha1.ApplicationSetApplicationStatus{
				newRolloutAppStatus("app-step1", v1alpha1.ProgressiveSyncHealthy, "1", []string{"18.1.0"}),
				newRolloutAppStatus("app-step2", v1alpha1.ProgressiveSyncWaiting, "2", []string{"18.4.2"}),
			},
			expectedBehind: 0,
		},
		{
			// An Application that cannot reconcile keeps its old revisions for as long as it is
			// broken, so counting it would hold the rollout indefinitely. Same fail-open rule
			// progressive sync already applies via isApplicationWithError.
			name: "does not wait for an application that cannot reconcile",
			apps: []v1alpha1.Application{
				withError(newRolloutApp("app-step1", rolloutSources(), oldRevisions, nil)),
				newRolloutApp("app-step2", rolloutSources(), newRevisions, nil),
			},
			appStatuses: []v1alpha1.ApplicationSetApplicationStatus{
				newRolloutAppStatus("app-step1", v1alpha1.ProgressiveSyncHealthy, "1", oldRevisions),
				newRolloutAppStatus("app-step2", v1alpha1.ProgressiveSyncWaiting, "2", newRevisions),
			},
			expectedBehind: 0,
		},
	} {
		t.Run(cc.name, func(t *testing.T) {
			t.Parallel()

			scheme := runtime.NewScheme()
			require.NoError(t, v1alpha1.AddToScheme(scheme))

			appSet := v1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{Name: "appset", Namespace: "argocd"},
				Spec: v1alpha1.ApplicationSetSpec{
					Strategy: &v1alpha1.ApplicationSetStrategy{
						Type:        "RollingSync",
						RollingSync: &v1alpha1.ApplicationSetRolloutStrategy{},
					},
				},
				Status: v1alpha1.ApplicationSetStatus{ApplicationStatus: cc.appStatuses},
			}

			initObjs := make([]client.Object, 0, len(cc.apps))
			for i := range cc.apps {
				initObjs = append(initObjs, cc.apps[i].DeepCopy())
			}

			var patched []string
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(initObjs...).
				WithInterceptorFuncs(interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						patched = append(patched, obj.GetName())
						return c.Patch(ctx, obj, patch, opts...)
					},
				}).
				Build()

			resourceVersionsBefore := liveResourceVersions(t, fakeClient, cc.apps)

			m := NewManager(fakeClient, fakeClient, nil)
			logCtx := log.NewEntry(log.New())

			appsBehind, err := m.refreshApplicationsBehindRollout(t.Context(), logCtx, &appSet, cc.apps)
			require.NoError(t, err)
			assert.Equal(t, cc.expectedBehind, appsBehind, "the count is of applications behind the rollout, not of refreshes issued")
			assert.ElementsMatch(t, cc.expectedPatched, patched, "only the applications behind the rollout and without a pending refresh may be patched")

			// An Application nobody patched must come back byte for byte as it went in, which the
			// resourceVersion proves independently of the Patch interceptor above.
			resourceVersionsAfter := liveResourceVersions(t, fakeClient, cc.apps)
			for name, before := range resourceVersionsBefore {
				if slices.Contains(cc.expectedPatched, name) {
					assert.NotEqual(t, before, resourceVersionsAfter[name], "application %s should have been written to", name)
				} else {
					assert.Equal(t, before, resourceVersionsAfter[name], "application %s should not have been written to", name)
				}
			}

			// The annotation must survive on the API server, that is what makes the Application
			// controller re-compare and what requeues the ApplicationSet.
			for i := range cc.apps {
				var live v1alpha1.Application
				require.NoError(t, fakeClient.Get(t.Context(), types.NamespacedName{Name: cc.apps[i].Name, Namespace: "argocd"}, &live))

				_, wasAlreadyAnnotated := cc.apps[i].Annotations[v1alpha1.AnnotationKeyRefresh]
				expectAnnotation := wasAlreadyAnnotated || slices.Contains(cc.expectedPatched, cc.apps[i].Name)

				if expectAnnotation {
					assert.Equal(t, string(v1alpha1.RefreshTypeNormal), live.Annotations[v1alpha1.AnnotationKeyRefresh], "application %s should carry the refresh annotation", cc.apps[i].Name)
				} else {
					assert.NotContains(t, live.Annotations, v1alpha1.AnnotationKeyRefresh, "application %s should not carry the refresh annotation", cc.apps[i].Name)
				}
			}
		})
	}
}

// TestApplicationSourceKeys pins the comparison key down to the source coordinates, in spec order:
// the resolved revisions of two Applications are only comparable when they come from the same
// repositories on the same target revisions.
func TestApplicationSourceKeys(t *testing.T) {
	t.Parallel()

	multiSource := newRolloutApp("multi", rolloutSources(), nil, nil)
	assert.Equal(t, []string{
		chartsRepoURL + "\x00master\x00generic-service\x00",
		ahConfigRepoURL + "\x00master\x00\x00",
	}, applicationSourceKeys(&multiSource))

	singleSource := v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "single", Namespace: "argocd"},
		Spec: v1alpha1.ApplicationSpec{
			Source: &v1alpha1.ApplicationSource{RepoURL: chartsRepoURL, TargetRevision: "master"},
		},
	}
	assert.Equal(t, []string{chartsRepoURL + "\x00master\x00\x00"}, applicationSourceKeys(&singleSource))

	// Source order is part of the identity: the same repositories in a different order resolve
	// revisions into different positions, so the lists are not comparable.
	reversed := newRolloutApp("reversed", []v1alpha1.ApplicationSource{rolloutSources()[1], rolloutSources()[0]}, nil, nil)
	assert.NotEqual(t, applicationSourceKeys(&multiSource), applicationSourceKeys(&reversed))

	// Chart and tagPrefix change which revision the source resolves to, so they are part of the
	// identity. Path does not, and per-Application paths are the normal ApplicationSet shape, so
	// including it would stop the comparison seeing the Applications it exists to compare.
	redis := newRolloutApp("redis", helmChartSource("redis"), nil, nil)
	postgres := newRolloutApp("postgres", helmChartSource("postgres"), nil, nil)
	assert.NotEqual(t, applicationSourceKeys(&redis), applicationSourceKeys(&postgres), "two charts from one Helm repository are two different sources")

	tagged := newRolloutApp("tagged", []v1alpha1.ApplicationSource{{RepoURL: chartsRepoURL, TargetRevision: "1.0.*", TagPrefix: "component-a/"}}, nil, nil)
	untagged := newRolloutApp("untagged", []v1alpha1.ApplicationSource{{RepoURL: chartsRepoURL, TargetRevision: "1.0.*"}}, nil, nil)
	assert.NotEqual(t, applicationSourceKeys(&tagged), applicationSourceKeys(&untagged), "tagPrefix filters which tags the constraint may resolve to")

	otherPath := newRolloutApp("other-path", []v1alpha1.ApplicationSource{{RepoURL: ahConfigRepoURL, TargetRevision: "master", Path: "apps/billing"}}, nil, nil)
	samePath := newRolloutApp("same-path", []v1alpha1.ApplicationSource{{RepoURL: ahConfigRepoURL, TargetRevision: "master", Path: "apps/payments"}}, nil, nil)
	assert.Equal(t, applicationSourceKeys(&otherPath), applicationSourceKeys(&samePath), "path changes what is rendered from a revision, not which revision is resolved")
}

// liveResourceVersions reads the resourceVersion of every given Application from the client, so a
// test can tell an object that was written to from one that was left alone.
func liveResourceVersions(t *testing.T, c client.Client, apps []v1alpha1.Application) map[string]string {
	t.Helper()

	resourceVersions := make(map[string]string, len(apps))
	for i := range apps {
		var live v1alpha1.Application
		require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: apps[i].Name, Namespace: "argocd"}, &live))
		resourceVersions[apps[i].Name] = live.ResourceVersion
	}
	return resourceVersions
}

// TestRefreshApplicationsBehindRolloutAcrossRequeue covers the reconcile that follows the refresh.
// The annotation write is itself an Application mutation, so shouldRequeueForApplication requeues
// the ApplicationSet straight away, normally long before the Application controller has consumed the
// annotation and re-compared. On that second pass the Application is still behind and still
// annotated: it must keep counting, or the step it is holding is released against a revision it has
// never seen.
func TestRefreshApplicationsBehindRolloutAcrossRequeue(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	oldRevisions := []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "1111111111111111111111111111111111111111"}
	newRevisions := []string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "2222222222222222222222222222222222222222"}

	appSet := v1alpha1.ApplicationSet{
		ObjectMeta: metav1.ObjectMeta{Name: "appset", Namespace: "argocd"},
		Status: v1alpha1.ApplicationSetStatus{ApplicationStatus: []v1alpha1.ApplicationSetApplicationStatus{
			newRolloutAppStatus("app-step1", v1alpha1.ProgressiveSyncHealthy, "1", oldRevisions),
			newRolloutAppStatus("app-step2", v1alpha1.ProgressiveSyncWaiting, "2", newRevisions),
		}},
	}

	apps := []v1alpha1.Application{
		newRolloutApp("app-step1", rolloutSources(), oldRevisions, nil),
		newRolloutApp("app-step2", rolloutSources(), newRevisions, nil),
	}

	patchCalls := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(apps[0].DeepCopy(), apps[1].DeepCopy()).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patchCalls++
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	m := NewManager(fakeClient, fakeClient, nil)
	logCtx := log.NewEntry(log.New())

	appsBehind, err := m.refreshApplicationsBehindRollout(t.Context(), logCtx, &appSet, apps)
	require.NoError(t, err)
	assert.Equal(t, 1, appsBehind)
	assert.Equal(t, 1, patchCalls, "the first pass requests the refresh")

	// The requeue arrives before the Application controller has re-compared: the informer cache now
	// carries the annotation, but the Application still reports the previous revision.
	refreshed := make([]v1alpha1.Application, 0, len(apps))
	for i := range apps {
		var live v1alpha1.Application
		require.NoError(t, fakeClient.Get(t.Context(), types.NamespacedName{Name: apps[i].Name, Namespace: "argocd"}, &live))
		refreshed = append(refreshed, live)
	}
	require.Equal(t, string(v1alpha1.RefreshTypeNormal), refreshed[0].Annotations[v1alpha1.AnnotationKeyRefresh])
	resourceVersionsBefore := liveResourceVersions(t, fakeClient, refreshed)

	appsBehind, err = m.refreshApplicationsBehindRollout(t.Context(), logCtx, &appSet, refreshed)
	require.NoError(t, err)
	assert.Equal(t, 1, appsBehind, "an application whose refresh is still pending is still behind the rollout")
	assert.Equal(t, 1, patchCalls, "the pending refresh must not be re-requested")
	assert.Equal(t, resourceVersionsBefore, liveResourceVersions(t, fakeClient, refreshed), "no application may be written to on the second pass")
}

// TestPerformProgressiveSyncsDefersWhileRefreshPending is the end of the same story, through the
// caller: step 1 reports Healthy against the previous revision, so step 2 must stay closed on the
// pass that requests the refresh and on the requeue that the request itself triggers.
func TestPerformProgressiveSyncsDefersWhileRefreshPending(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	oldRevisions := []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "1111111111111111111111111111111111111111"}
	newRevisions := []string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "2222222222222222222222222222222222222222"}

	step := func(stage string) v1alpha1.ApplicationSetRolloutStep {
		return v1alpha1.ApplicationSetRolloutStep{
			MatchExpressions: []v1alpha1.ApplicationMatchExpression{{Key: "stage", Operator: "In", Values: []string{stage}}},
		}
	}

	newStepApp := func(name, stage string, revisions []string) v1alpha1.Application {
		app := newRolloutApp(name, rolloutSources(), revisions, nil)
		app.Labels = map[string]string{"stage": stage}
		return app
	}

	// Step 1 is Healthy, but against the revision from before the commit that started this rollout,
	// which is precisely the state getAppsToSync cannot tell apart from a completed step.
	appSet := v1alpha1.ApplicationSet{
		ObjectMeta: metav1.ObjectMeta{Name: "appset", Namespace: "argocd"},
		Spec: v1alpha1.ApplicationSetSpec{
			Strategy: &v1alpha1.ApplicationSetStrategy{
				Type: "RollingSync",
				RollingSync: &v1alpha1.ApplicationSetRolloutStrategy{
					Steps: []v1alpha1.ApplicationSetRolloutStep{step("1"), step("2")},
				},
			},
		},
		Status: v1alpha1.ApplicationSetStatus{ApplicationStatus: []v1alpha1.ApplicationSetApplicationStatus{
			newRolloutAppStatus("app-step1", v1alpha1.ProgressiveSyncHealthy, "1", oldRevisions),
			newRolloutAppStatus("app-step2", v1alpha1.ProgressiveSyncWaiting, "2", newRevisions),
		}},
	}

	apps := []v1alpha1.Application{
		newStepApp("app-step1", "1", oldRevisions),
		newStepApp("app-step2", "2", newRevisions),
	}

	newManager := func(t *testing.T, apps []v1alpha1.Application, refreshBeforeSync bool) (*Manager, client.Client) {
		t.Helper()
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(apps[0].DeepCopy(), apps[1].DeepCopy()).Build()
		m := NewManager(c, c, regressionDeps{})
		m.RefreshApplicationsBeforeSync = refreshBeforeSync
		return m, c
	}

	logCtx := log.NewEntry(log.New())

	t.Run("the flag off releases step 2 against the stale Healthy", func(t *testing.T) {
		t.Parallel()

		m, _ := newManager(t, apps, false)
		appsToSync, err := m.PerformProgressiveSyncs(t.Context(), logCtx, *appSet.DeepCopy(), apps, apps)
		require.NoError(t, err)
		assert.True(t, appsToSync["app-step2"], "this is the behaviour the flag exists to change")
	})

	t.Run("the flag on holds step 2 across the requeue", func(t *testing.T) {
		t.Parallel()

		m, c := newManager(t, apps, true)

		appsToSync, err := m.PerformProgressiveSyncs(t.Context(), logCtx, *appSet.DeepCopy(), apps, apps)
		require.NoError(t, err)
		assert.Empty(t, appsToSync, "step 2 must not be released while step 1 reports a stale revision")

		// The refresh annotation requeues the ApplicationSet immediately, before the Application
		// controller has had a chance to re-compare.
		requeued := make([]v1alpha1.Application, 0, len(apps))
		for i := range apps {
			var live v1alpha1.Application
			require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: apps[i].Name, Namespace: "argocd"}, &live))
			requeued = append(requeued, live)
		}
		require.Equal(t, string(v1alpha1.RefreshTypeNormal), requeued[0].Annotations[v1alpha1.AnnotationKeyRefresh])

		appsToSync, err = m.PerformProgressiveSyncs(t.Context(), logCtx, *appSet.DeepCopy(), requeued, requeued)
		require.NoError(t, err)
		assert.Empty(t, appsToSync, "the pending refresh must keep step 2 closed, not reopen it")
	})
}
