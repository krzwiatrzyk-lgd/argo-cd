package progressivesync

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	argov1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// rolloutRevision pairs the revisions an Application has resolved with the source coordinates those
// revisions belong to, so that the two are only ever compared between Applications that track the
// same coordinates. observedAt is the Waiting transition that put it here: the moment the
// ApplicationSet controller learned about this change, which is what the hold below is measured from.
type rolloutRevision struct {
	sourceKeys []string
	revisions  []string
	observedAt time.Time
}

const (
	// maxRefreshHold bounds how long the sync decision is deferred while an Application has not
	// reported back. A refresh normally lands in under a second, so the bound is not for the common
	// case: it is for an Application that never consumes the annotation at all -- controller down,
	// namespace unwatched, shard not running. Holding a whole rollout on one such Application would
	// be a worse failure than the race this flag removes.
	maxRefreshHold = 2 * time.Minute

	// refreshHoldRequeueInterval re-examines the ApplicationSet while the decision is deferred, and is
	// the only thing that makes the hold self-healing. The first annotation patch requeues through
	// Owns(&Application{}), but a later pass does not re-patch an already-annotated Application and so
	// produces no event, and shouldRequeueForApplication ignores
	// Status.Sync.Revision, and an ApplicationSet generated only by the cluster generator has no
	// periodic requeue either (ClusterGenerator.GetRequeueAfter returns NoRequeueAfter).
	refreshHoldRequeueInterval = 10 * time.Second
)

// refreshResult reports what one refresh pass found.
type refreshResult struct {
	// behind is the number of Applications that have not observed the revision the rollout is about.
	// It counts Applications behind, not refreshes issued: see refreshApplicationsBehindRollout.
	behind int
	// remaining is the largest window left across the outstanding skews, so the decision waits while
	// ANY of them is still inside its bound. Each skew's own window runs from the oldest observation
	// the Application behind it disagrees with. Only meaningful when behind is above zero.
	remaining time.Duration
}

// sourceKeySeparator joins the parts of a source key. A NUL byte cannot appear in any of them, so
// distinct coordinates cannot collide into one key.
const sourceKeySeparator = "\x00"

// applicationSourceKeys identifies the coordinates an Application resolves its revisions from, in
// spec source order. Two Applications sharing these coordinates must converge on the same revisions;
// Applications with different coordinates legitimately differ forever and are skipped, since demanding
// a consensus that cannot arrive would wedge the rollout.
//
// A field belongs in the key if and only if it changes which revision the source resolves to. Chart
// qualifies because for a Helm repository repoURL@targetRevision is not a source at all, and TagPrefix
// because it filters which tags a semver constraint may match. Path, Ref, Name and the per-tool option
// blocks do not: they change what is rendered from a revision, and per-Application paths are the
// normal ApplicationSet shape, so keying on them would hide the very Applications this compares.
func applicationSourceKeys(app *argov1alpha1.Application) []string {
	sources := app.Spec.GetSources()
	keys := make([]string, 0, len(sources))
	for _, source := range sources {
		keys = append(keys, strings.Join([]string{
			source.RepoURL,
			source.TargetRevision,
			source.Chart,
			source.TagPrefix,
		}, sourceKeySeparator))
	}
	return keys
}

// refreshApplicationsBehindRollout asks the Application controller to re-compare every Application
// in the ApplicationSet that has not observed the revision the rollout is about, and returns how
// many Applications are behind it.
//
// The count is Applications still behind, not refreshes issued: one whose refresh is already pending
// is counted but not patched again. Counting refreshes instead would drop to zero on the requeue the
// annotation write itself triggers -- usually before the Application controller has consumed it --
// and release the step this exists to hold.
//
// Every skip below is deliberately fail-open, because an Application that can never converge would
// turn a delay into a stall. The returned remaining duration is the backstop for the case no skip can
// recognise: an Application that is simply never processed.
func (m *Manager) refreshApplicationsBehindRollout(ctx context.Context, logCtx *log.Entry, applicationSet *argov1alpha1.ApplicationSet, applications []argov1alpha1.Application, now time.Time) (refreshResult, error) {
	appMap := make(map[string]*argov1alpha1.Application, len(applications))
	for i := range applications {
		appMap[applications[i].Name] = &applications[i]
	}

	// Waiting is the only status that proves the ApplicationSet controller has observed a change for
	// an Application, so the Applications in that state are what defines the revision of the rollout.
	var frontier []rolloutRevision
	for _, appStatus := range applicationSet.Status.ApplicationStatus {
		if appStatus.Status != argov1alpha1.ProgressiveSyncWaiting {
			continue
		}
		app, ok := appMap[appStatus.Application]
		if !ok {
			continue
		}
		revisions := app.Status.GetRevisions()
		if len(revisions) == 0 {
			continue
		}
		observedAt := time.Time{}
		if appStatus.LastTransitionTime != nil {
			observedAt = appStatus.LastTransitionTime.Time
		}
		frontier = append(frontier, rolloutRevision{
			sourceKeys: applicationSourceKeys(app),
			revisions:  revisions,
			observedAt: observedAt,
		})
	}

	if len(frontier) == 0 {
		// No Application has registered a change, so there is no revision for the others to be behind.
		return refreshResult{}, nil
	}

	result := refreshResult{}
	refreshRequests := 0
	for _, appStatus := range applicationSet.Status.ApplicationStatus {
		if appStatus.Status == argov1alpha1.ProgressiveSyncWaiting ||
			appStatus.Status == argov1alpha1.ProgressiveSyncPending ||
			appStatus.Status == argov1alpha1.ProgressiveSyncProgressing {
			// The change is already registered for this Application, or it is already moving on it.
			continue
		}

		app, ok := appMap[appStatus.Application]
		if !ok {
			continue
		}

		revisions := app.Status.GetRevisions()
		if len(revisions) == 0 {
			// The Application has never been compared, so there is nothing for it to be stale against.
			continue
		}

		// The anchor for this Application is the OLDEST observation it disagrees with, not the first
		// one found. As more Applications refresh and join the frontier, an Application's anchor can
		// then only move backwards in time, never forwards -- so Applications refreshing one after
		// another cannot keep pushing the release deadline out. That is the whole point of a bound.
		sourceKeys := applicationSourceKeys(app)
		behind := false
		anchor := time.Time{}
		for _, observed := range frontier {
			if !reflect.DeepEqual(sourceKeys, observed.sourceKeys) || reflect.DeepEqual(revisions, observed.revisions) {
				continue
			}
			behind = true
			if !observed.observedAt.IsZero() && (anchor.IsZero() || observed.observedAt.Before(anchor)) {
				anchor = observed.observedAt
			}
		}
		if !behind {
			continue
		}

		if isApplicationWithError(*app) {
			// The Application cannot be reconciled, so its revisions will not advance however many
			// refreshes it is sent, and counting it would hold the rollout for as long as it stays
			// broken. This is the same predicate progressive sync already uses to stop waiting on a
			// broken Application, see UpdateApplicationSetApplicationStatus.
			logCtx.WithField("app.name", app.Name).Info("Not waiting for an application that is behind the rollout but has an error and cannot reconcile")
			continue
		}

		result.behind++
		// Hold while ANY outstanding skew is still inside its window, so one Application that has
		// given up waiting does not release a wave another is still legitimately waiting on.
		if remaining := remainingRefreshHold(anchor, now); remaining > result.remaining {
			result.remaining = remaining
		}

		if _, alreadyRequested := app.Annotations[argov1alpha1.AnnotationKeyRefresh]; alreadyRequested {
			// A refresh is already pending, patching the same annotation again would only churn the
			// object. The Application still counts as behind: it has not reported back yet.
			continue
		}

		patch := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"`+argov1alpha1.AnnotationKeyRefresh+`":"`+string(argov1alpha1.RefreshTypeNormal)+`"}}}`))
		// Patched through a copy: appMap holds pointers into the applications slice the caller owns,
		// and client.Patch decodes the API response back into the object it is given. Patching app
		// directly would hand the rest of the reconcile a mutated Application -- a new
		// resourceVersion and an annotation it never asked about -- and would make the slice unsafe
		// for any concurrent reader. Nothing here needs the response.
		if err := m.Client.Patch(ctx, app.DeepCopy(), patch); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return result, fmt.Errorf("failed to request refresh of application %s: %w", app.Name, err)
		}

		refreshRequests++
		logCtx.WithField("app.name", app.Name).Info("Requesting a refresh, the Application has not observed the revision the rollout is about")
	}

	if result.behind > 0 {
		logCtx.WithFields(log.Fields{
			"applications.behind":    result.behind,
			"applications.refreshed": refreshRequests,
			"hold.remaining":         result.remaining,
		}).Info("Applications have not observed the revision the rollout is about")
	}

	return result, nil
}

// remainingRefreshHold reports how much of maxRefreshHold is left for a skew first observed at anchor.
//
// A zero anchor is reported as expired, not as a full window: the caller re-evaluates on every
// requeue, so a fresh window each time would defer the decision forever, and a hold whose bound
// cannot be evaluated is not bounded. Unreachable through the write path, where both transitions into
// ProgressiveSyncWaiting stamp LastTransitionTime (progressive_sync.go:440 and :495-496).
func remainingRefreshHold(anchor time.Time, now time.Time) time.Duration {
	if anchor.IsZero() {
		return 0
	}
	deadline := anchor.Add(maxRefreshHold)
	if !now.Before(deadline) {
		return 0
	}
	// An anchor in the future -- clock skew between the API server and this controller -- must not buy
	// the hold more than one window.
	return min(deadline.Sub(now), maxRefreshHold)
}
