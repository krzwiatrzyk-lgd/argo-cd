package progressivesync

import (
	"context"
	"testing"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/v3/pkg/health"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

func TestRemainingSettleWindow(t *testing.T) {
	t.Parallel()

	// Fixed so every expectation below is an exact duration rather than a tolerance.
	now := time.Date(2025, time.June, 1, 12, 0, 0, 0, time.UTC)

	appStatus := func(name string, status v1alpha1.ProgressiveSyncStatusCode, transition *time.Time) v1alpha1.ApplicationSetApplicationStatus {
		appStatus := v1alpha1.ApplicationSetApplicationStatus{
			Application: name,
			Status:      status,
		}
		if transition != nil {
			appStatus.LastTransitionTime = &metav1.Time{Time: *transition}
		}
		return appStatus
	}
	at := func(offset time.Duration) *time.Time {
		transition := now.Add(offset)
		return &transition
	}

	for _, cc := range []struct {
		name             string
		statuses         []v1alpha1.ApplicationSetApplicationStatus
		window           time.Duration
		expectedDuration time.Duration
	}{
		{
			name: "window disabled",
			statuses: []v1alpha1.ApplicationSetApplicationStatus{
				appStatus("app1", v1alpha1.ProgressiveSyncWaiting, at(-time.Second)),
			},
			window:           0,
			expectedDuration: 0,
		},
		{
			name: "negative window is treated as disabled",
			statuses: []v1alpha1.ApplicationSetApplicationStatus{
				appStatus("app1", v1alpha1.ProgressiveSyncWaiting, at(-time.Second)),
			},
			window:           -5 * time.Second,
			expectedDuration: 0,
		},
		{
			name:             "no application statuses at all",
			statuses:         nil,
			window:           10 * time.Second,
			expectedDuration: 0,
		},
		{
			name: "no Waiting applications",
			statuses: []v1alpha1.ApplicationSetApplicationStatus{
				appStatus("app1", v1alpha1.ProgressiveSyncHealthy, at(-time.Second)),
				appStatus("app2", v1alpha1.ProgressiveSyncProgressing, at(-time.Second)),
			},
			window:           10 * time.Second,
			expectedDuration: 0,
		},
		{
			name: "Waiting application inside the window",
			statuses: []v1alpha1.ApplicationSetApplicationStatus{
				appStatus("app1", v1alpha1.ProgressiveSyncWaiting, at(-4*time.Second)),
			},
			window:           10 * time.Second,
			expectedDuration: 6 * time.Second,
		},
		{
			name: "Waiting application older than the window",
			statuses: []v1alpha1.ApplicationSetApplicationStatus{
				appStatus("app1", v1alpha1.ProgressiveSyncWaiting, at(-30*time.Second)),
			},
			window:           10 * time.Second,
			expectedDuration: 0,
		},
		{
			name: "window elapsing exactly now",
			statuses: []v1alpha1.ApplicationSetApplicationStatus{
				appStatus("app1", v1alpha1.ProgressiveSyncWaiting, at(-10*time.Second)),
			},
			window:           10 * time.Second,
			expectedDuration: 0,
		},
		{
			name: "latest Waiting transition wins when it comes first in the slice",
			statuses: []v1alpha1.ApplicationSetApplicationStatus{
				appStatus("app1", v1alpha1.ProgressiveSyncWaiting, at(-1*time.Second)),
				appStatus("app2", v1alpha1.ProgressiveSyncWaiting, at(-5*time.Second)),
				appStatus("app3", v1alpha1.ProgressiveSyncWaiting, at(-8*time.Second)),
			},
			window:           10 * time.Second,
			expectedDuration: 9 * time.Second,
		},
		{
			name: "latest Waiting transition wins when it comes last in the slice",
			statuses: []v1alpha1.ApplicationSetApplicationStatus{
				appStatus("app1", v1alpha1.ProgressiveSyncWaiting, at(-8*time.Second)),
				appStatus("app2", v1alpha1.ProgressiveSyncWaiting, at(-5*time.Second)),
				appStatus("app3", v1alpha1.ProgressiveSyncWaiting, at(-1*time.Second)),
			},
			window:           10 * time.Second,
			expectedDuration: 9 * time.Second,
		},
		{
			name: "latest Waiting transition is already outside the window",
			statuses: []v1alpha1.ApplicationSetApplicationStatus{
				appStatus("app1", v1alpha1.ProgressiveSyncWaiting, at(-30*time.Second)),
				appStatus("app2", v1alpha1.ProgressiveSyncWaiting, at(-20*time.Second)),
			},
			window:           10 * time.Second,
			expectedDuration: 0,
		},
		{
			name: "Waiting application with no transition time is skipped",
			statuses: []v1alpha1.ApplicationSetApplicationStatus{
				appStatus("app1", v1alpha1.ProgressiveSyncWaiting, nil),
				appStatus("app2", v1alpha1.ProgressiveSyncWaiting, at(-3*time.Second)),
			},
			window:           10 * time.Second,
			expectedDuration: 7 * time.Second,
		},
		{
			name: "only Waiting applications have no transition time",
			statuses: []v1alpha1.ApplicationSetApplicationStatus{
				appStatus("app1", v1alpha1.ProgressiveSyncWaiting, nil),
			},
			window:           10 * time.Second,
			expectedDuration: 0,
		},
		{
			name: "recent non-Waiting transitions do not extend the window",
			statuses: []v1alpha1.ApplicationSetApplicationStatus{
				appStatus("app1", v1alpha1.ProgressiveSyncWaiting, at(-9*time.Second)),
				appStatus("app2", v1alpha1.ProgressiveSyncHealthy, at(-1*time.Second)),
				appStatus("app3", v1alpha1.ProgressiveSyncPending, at(-1*time.Second)),
				appStatus("app4", v1alpha1.ProgressiveSyncProgressing, at(-1*time.Second)),
			},
			window:           10 * time.Second,
			expectedDuration: 1 * time.Second,
		},
		{
			name: "recent non-Waiting transitions do not hide an elapsed window",
			statuses: []v1alpha1.ApplicationSetApplicationStatus{
				appStatus("app1", v1alpha1.ProgressiveSyncWaiting, at(-30*time.Second)),
				appStatus("app2", v1alpha1.ProgressiveSyncHealthy, at(-1*time.Second)),
			},
			window:           10 * time.Second,
			expectedDuration: 0,
		},
	} {
		t.Run(cc.name, func(t *testing.T) {
			t.Parallel()
			appSet := &v1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "argocd",
				},
				Status: v1alpha1.ApplicationSetStatus{
					ApplicationStatus: cc.statuses,
				},
			}
			assert.Equal(t, cc.expectedDuration, remainingSettleWindow(appSet, cc.window, now), "expected remaining settle window did not match actual")
		})
	}
}

// settleDeps persists statuses onto the ApplicationSet, the way the controller's own Dependencies
// implementation does, and keeps the last set it was handed so the promotion decision taken by
// PerformProgressiveSyncs is observable from outside (the ApplicationSet is passed to it by value).
type settleDeps struct {
	lastStatuses []v1alpha1.ApplicationSetApplicationStatus
}

func (d *settleDeps) SetAppSetApplicationStatus(_ context.Context, _ *log.Entry, applicationSet *v1alpha1.ApplicationSet, applicationStatuses []v1alpha1.ApplicationSetApplicationStatus) error {
	applicationSet.Status.ApplicationStatus = applicationStatuses
	d.lastStatuses = applicationStatuses
	return nil
}

func (*settleDeps) SetApplicationSetStatusCondition(_ context.Context, _ *v1alpha1.ApplicationSet, _ []v1alpha1.ApplicationSetCondition, _ bool) error {
	return nil
}

// settleApp builds an Application already in its final state for the current pass, so
// UpdateApplicationSetApplicationStatus leaves the seeded progressive sync status alone and the test
// isolates the settle window gate.
func settleApp(name, step, revision string, sync v1alpha1.SyncStatusCode) v1alpha1.Application {
	return v1alpha1.Application{
		TypeMeta:   metav1.TypeMeta{APIVersion: "argoproj.io/v1alpha1", Kind: "Application"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "argocd", Labels: map[string]string{"step": step}},
		Spec: v1alpha1.ApplicationSpec{
			Project:     "default",
			Source:      &v1alpha1.ApplicationSource{RepoURL: "git://example/repo", Path: name, TargetRevision: "HEAD"},
			Destination: v1alpha1.ApplicationDestination{Server: "https://kubernetes.default.svc", Namespace: name},
		},
		Status: v1alpha1.ApplicationStatus{
			Sync:   v1alpha1.SyncStatus{Status: sync, Revision: revision},
			Health: v1alpha1.AppHealthStatus{Status: health.HealthStatusHealthy},
		},
	}
}

// The gate is the whole point of the flag: while the window is open PerformProgressiveSyncs must
// promote nothing and must ask to be woken again, because expiry of a quiet period is not an event
// any watch reports.
//
// The state modelled here is the one from the incident: step 1's Application has not been refreshed
// yet, so it still reads Synced and Healthy against the previous commit, while step 2's Application
// has registered the new commit and is Waiting. With no window getAppsToSync releases step 2 on that
// stale Healthy.
func TestPerformProgressiveSyncsHoldsWhileApplicationsSettle(t *testing.T) {
	t.Parallel()

	for _, cc := range []struct {
		name              string
		window            time.Duration
		waitingAge        time.Duration
		expectedToSync    map[string]bool
		expectedRequeue   bool
		expectedStep2Code v1alpha1.ProgressiveSyncStatusCode
	}{
		{
			name:              "window disabled releases the next step on the stale Healthy",
			window:            0,
			waitingAge:        time.Second,
			expectedToSync:    map[string]bool{"step1-db": true, "step2-web": true},
			expectedRequeue:   false,
			expectedStep2Code: v1alpha1.ProgressiveSyncPending,
		},
		{
			name:              "window open promotes nothing and asks for a requeue",
			window:            30 * time.Second,
			waitingAge:        time.Second,
			expectedToSync:    map[string]bool{},
			expectedRequeue:   true,
			expectedStep2Code: v1alpha1.ProgressiveSyncWaiting,
		},
		{
			name:              "window elapsed decides exactly as before",
			window:            30 * time.Second,
			waitingAge:        5 * time.Minute,
			expectedToSync:    map[string]bool{"step1-db": true, "step2-web": true},
			expectedRequeue:   false,
			expectedStep2Code: v1alpha1.ProgressiveSyncPending,
		},
	} {
		t.Run(cc.name, func(t *testing.T) {
			t.Parallel()

			waitingSince := metav1.NewTime(time.Now().Add(-cc.waitingAge))
			appSet := v1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{Name: "settle", Namespace: "argocd"},
				Spec: v1alpha1.ApplicationSetSpec{
					Strategy: &v1alpha1.ApplicationSetStrategy{
						Type: "RollingSync",
						RollingSync: &v1alpha1.ApplicationSetRolloutStrategy{
							Steps: []v1alpha1.ApplicationSetRolloutStep{
								{MatchExpressions: []v1alpha1.ApplicationMatchExpression{{Key: "step", Operator: "In", Values: []string{"1"}}}},
								{MatchExpressions: []v1alpha1.ApplicationMatchExpression{{Key: "step", Operator: "In", Values: []string{"2"}}}},
							},
						},
					},
				},
				Status: v1alpha1.ApplicationSetStatus{
					ApplicationStatus: []v1alpha1.ApplicationSetApplicationStatus{
						{
							// Not refreshed yet: still Healthy against the previous commit.
							Application:        "step1-db",
							Status:             v1alpha1.ProgressiveSyncHealthy,
							Message:            "Application resource has synced, updating status to Healthy",
							Step:               "1",
							TargetRevisions:    []string{"old"},
							LastTransitionTime: &metav1.Time{Time: time.Now().Add(-time.Hour)},
						},
						{
							// Refreshed: registered the new commit and is waiting for its turn.
							Application:        "step2-web",
							Status:             v1alpha1.ProgressiveSyncWaiting,
							Message:            revisionChangedMsg,
							Step:               "2",
							TargetRevisions:    []string{"new"},
							LastTransitionTime: &waitingSince,
						},
					},
				},
			}

			live := []v1alpha1.Application{
				settleApp("step1-db", "1", "old", v1alpha1.SyncStatusCodeSynced),
				settleApp("step2-web", "2", "new", v1alpha1.SyncStatusCodeOutOfSync),
			}
			// Identical to the live Applications so specChanged cannot fire and the only signal the
			// gate reacts to is the settle window.
			desired := []v1alpha1.Application{*live[0].DeepCopy(), *live[1].DeepCopy()}

			scheme := runtime.NewScheme()
			require.NoError(t, v1alpha1.AddToScheme(scheme))
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&appSet).WithStatusSubresource(&appSet).Build()

			deps := &settleDeps{}
			m := NewManager(c, c, deps)
			m.SettleWindow = cc.window

			appsToSync, requeue, err := m.PerformProgressiveSyncs(t.Context(), log.NewEntry(log.New()), appSet, live, desired)
			require.NoError(t, err)

			assert.Equal(t, cc.expectedToSync, appsToSync, "settle window did not gate the promotion decision as expected")

			if cc.expectedRequeue {
				assert.Positive(t, requeue, "an open window must ask to be woken again: no watch reports its expiry")
				assert.LessOrEqual(t, requeue, cc.window, "the requeue can never exceed the configured window")
			} else {
				assert.Zero(t, requeue, "nothing is deferred, so nothing extra needs waking")
			}

			require.Len(t, deps.lastStatuses, 2)
			for _, status := range deps.lastStatuses {
				if status.Application == "step2-web" {
					assert.Equal(t, cc.expectedStep2Code, status.Status,
						"a held decision must leave the later step in Waiting, so SyncDesiredApplications stamps no operation on it")
				}
			}
		})
	}
}
