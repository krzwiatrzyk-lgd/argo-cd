package progressivesync

import (
	"sort"
	"strings"
	"time"

	argov1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

const (
	// DefaultRevisionConsensusTimeout bounds how long the revision consensus gate withholds the
	// rollout decision before deciding on the state it has. Two minutes is roughly three orders of
	// magnitude more than the refresh skew the gate exists to absorb, and short enough that an
	// Application which is never going to converge is noticed rather than silently waited on.
	DefaultRevisionConsensusTimeout = 2 * time.Minute

	// MaxRevisionConsensusTimeout is the largest value accepted from the environment. A bound longer
	// than this is indistinguishable from a wedged rollout to whoever is on call.
	MaxRevisionConsensusTimeout = 10 * time.Minute

	// revisionConsensusRequeueInterval is how often the ApplicationSet is re-examined while the
	// decision is withheld. Polling is required rather than optional: shouldRequeueForApplication
	// ignores Status.Sync.Revision(s), so an Application that refreshes into agreement without
	// changing its sync or health status produces no watch event, and an ApplicationSet generated
	// only by the cluster generator has no periodic requeue either. It also caps the wait once the
	// Applications do agree, which is why it is much shorter than the timeout.
	revisionConsensusRequeueInterval = 10 * time.Second
)

// sourceSlot identifies the coordinates one source slot of an Application resolves its revision
// from. Two slots that agree on every field ask the same repository for the same requested
// revision, so the revisions they resolve to have to agree as well. Anything that does not take
// part in resolving a revision is deliberately absent: path, helm values, kustomize options and
// plugin config change what is rendered, not which commit is rendered, and including them would
// stop the gate comparing the per-cluster Applications it exists to compare. ref is absent for the
// same reason - it is a local alias for a source, not part of its coordinates.
type sourceSlot struct {
	repoURL        string
	targetRevision string
	chart          string
	tagPrefix      string
}

// String renders the slot for log lines and gives the diverging-group selection a stable order, so
// the reported group does not depend on Go map iteration order.
func (s sourceSlot) String() string {
	out := s.repoURL + "@" + s.targetRevision
	if s.chart != "" {
		out += " chart=" + s.chart
	}
	if s.tagPrefix != "" {
		out += " tagPrefix=" + s.tagPrefix
	}
	return out
}

// observedRevisions reports the revision an Application last resolved for each of its source slots.
//
// Both halves are read from status.sync and never from the spec. status.sync.comparedTo and
// status.sync.revisions are written by the same comparison in CompareAppState (controller/state.go:
// one manifest response per source, appended in source order, then Revisions = manifestRevisions
// and ComparedTo.Sources = sources), so they are index-aligned with each other. Pairing
// spec.GetSources() against status.GetRevisions() instead would mis-pair coordinates and revisions
// in precisely the window this gate exists to detect: the one where the spec has already moved and
// the status has not.
func observedRevisions(app *argov1alpha1.Application) map[sourceSlot]string {
	var sources argov1alpha1.ApplicationSources
	var revisions []string

	// Mirrors ApplicationStatus.GetRevisions: Revisions is the multi-source form and wins when
	// populated, Revision is the single-source form. A source hydrator Application is the
	// single-source form and reports its sync source, which is the revision it actually deploys.
	switch {
	case len(app.Status.Sync.Revisions) > 0:
		sources = app.Status.Sync.ComparedTo.Sources
		revisions = app.Status.Sync.Revisions
	case app.Status.Sync.Revision != "":
		sources = argov1alpha1.ApplicationSources{app.Status.Sync.ComparedTo.Source}
		revisions = []string{app.Status.Sync.Revision}
	default:
		return nil
	}

	observed := make(map[sourceSlot]string, len(revisions))
	for i := range revisions {
		if i >= len(sources) {
			// More revisions than coordinates to attribute them to. Nothing identifies the extra
			// entries, so there is nothing they could be compared against.
			break
		}
		if revisions[i] == "" || sources[i].RepoURL == "" {
			// Never resolved, or resolved against coordinates the status does not record. Either
			// way this slot is not evidence of anything.
			continue
		}
		slot := sourceSlot{
			repoURL:        sources[i].RepoURL,
			targetRevision: sources[i].TargetRevision,
			chart:          sources[i].Chart,
			tagPrefix:      sources[i].TagPrefix,
		}
		// An Application may list the same source twice; the controller does not deduplicate them.
		// First slot wins, which is deterministic and identical across Applications rendered from
		// the same template.
		if _, seen := observed[slot]; !seen {
			observed[slot] = revisions[i]
		}
	}
	return observed
}

// revisionUnreliable reports whether an Application's status.sync cannot be trusted to advance. An
// Application that cannot be compared against its source keeps reporting whatever revision it last
// managed to resolve - or, for a single-source Application, the unresolved targetRevision it was
// asked for - so waiting for it to agree with the others is waiting forever.
//
// Deliberately not isApplicationWithError: that predicate drives a Pending -> Progressing
// transition, and adding ComparisonError to it would change behaviour unrelated to this gate.
func revisionUnreliable(app *argov1alpha1.Application) bool {
	for _, condition := range app.Status.Conditions {
		switch condition.Type {
		case argov1alpha1.ApplicationConditionComparisonError,
			argov1alpha1.ApplicationConditionInvalidSpecError,
			argov1alpha1.ApplicationConditionUnknownError:
			return true
		}
	}
	return false
}

// revisionConsensus is the outcome of one consensus evaluation.
type revisionConsensus struct {
	// Hold is true when the promotion decision must be withheld this pass.
	Hold bool
	// Expired is true when Applications disagree but the bound on how long the gate may hold has
	// already elapsed, so the caller promotes anyway. Reported separately so it can be logged as
	// the safety-net failure it is.
	Expired bool
	// Remaining is how much of that bound is left, or zero when no bound is configured. The caller
	// has to requeue while holding: the bound elapsing is not an event any watch reports, and
	// neither is an Application refreshing into agreement.
	Remaining time.Duration
	// Slot is the diverging source slot, and Revisions maps each revision reported for it to the
	// Applications reporting it. Both are for the log line only.
	Slot      sourceSlot
	Revisions map[string][]string
}

// RequeueAfter is how long the caller should wait before deciding again. It is the poll interval
// rather than the remaining bound: an Application refreshing into agreement produces no watch
// event, so the gate has to look again soon, and a hold with no bound configured still has to be
// re-examined or it outlives its own condition.
func (c revisionConsensus) RequeueAfter() time.Duration {
	if !c.Hold {
		return 0
	}
	if c.Remaining > 0 && c.Remaining < revisionConsensusRequeueInterval {
		return c.Remaining
	}
	return revisionConsensusRequeueInterval
}

// LogDetail renders the diverging group as "revision=app,app revision=app", revisions sorted, for
// a single stable log field.
func (c revisionConsensus) LogDetail() string {
	revisions := make([]string, 0, len(c.Revisions))
	for revision := range c.Revisions {
		revisions = append(revisions, revision)
	}
	sort.Strings(revisions)

	parts := make([]string, 0, len(revisions))
	for _, revision := range revisions {
		apps := append([]string(nil), c.Revisions[revision]...)
		sort.Strings(apps)
		parts = append(parts, revision+"="+strings.Join(apps, ","))
	}
	return strings.Join(parts, " ")
}

// evaluateRevisionConsensus reports whether every Application this ApplicationSet manages that
// draws from the same source coordinates has converged on the same resolved revision, and
// therefore whether the controller is looking at a whole view of the world or a torn one.
//
// The Application controller refreshes Applications independently, milliseconds apart. Until an
// Application has been refreshed it still reports Synced and Healthy against the previous
// revision, which is indistinguishable from having completed the current one. A RollingSync step
// released on that reading starts against a revision an earlier step has never seen.
//
// now is passed in rather than read here: one clock read per reconciliation, none inside the
// loops, and deterministic tests.
func evaluateRevisionConsensus(appset *argov1alpha1.ApplicationSet, applications []argov1alpha1.Application, appStepMap map[string]int, timeout time.Duration, now time.Time) revisionConsensus {
	// The only promotion RollingSync makes is Waiting -> Pending, in
	// UpdateApplicationSetApplicationStatusProgress. With no step-selected Application in Waiting
	// there is nothing to withhold, so the gate stays out of the way entirely. That is not an
	// optimisation: it is what stops an ApplicationSet whose Applications legitimately never agree
	// from being gated for its whole life.
	//
	// Applications no step selects are excluded here, and from the anchor below, because nothing
	// ever promotes them out of Waiting - they are never in appDependencyList, so never in
	// appsToSync - so one of them in Waiting would arm the gate permanently.
	anyWaiting := false
	var latestTransition time.Time
	for _, appStatus := range appset.Status.ApplicationStatus {
		if _, selected := appStepMap[appStatus.Application]; !selected {
			continue
		}
		if appStatus.Status == argov1alpha1.ProgressiveSyncWaiting {
			anyWaiting = true
		}
		// The anchor for the bound is the newest transition of ANY status, not of Waiting only. A
		// Waiting transition is stamped once, when the ApplicationSet first observes the change,
		// and is never restamped while the Application waits its turn - so on a rollout whose
		// earlier steps take longer than the timeout, a Waiting-only anchor is already expired by
		// the time the gate is first consulted, and the protection is silently gone. Every
		// promotion and every health transition advances this anchor instead, so the bound only
		// counts down once the ApplicationSet has stopped moving, which is exactly when holding
		// has stopped being productive.
		if appStatus.LastTransitionTime != nil && appStatus.LastTransitionTime.After(latestTransition) {
			latestTransition = appStatus.LastTransitionTime.Time
		}
	}
	if !anyWaiting {
		return revisionConsensus{}
	}

	// slot -> revision -> Applications reporting that revision for that slot.
	groups := map[sourceSlot]map[string][]string{}
	for i := range applications {
		app := &applications[i]
		if _, selected := appStepMap[app.Name]; !selected {
			// Selected by no step: the rollout never syncs it and never promotes it, so it is
			// allowed to sit on an old revision forever.
			continue
		}
		if app.DeletionTimestamp != nil {
			// Terminating: its status.sync is frozen and will never converge.
			continue
		}
		if revisionUnreliable(app) {
			continue
		}
		for slot, revision := range observedRevisions(app) {
			byRevision := groups[slot]
			if byRevision == nil {
				byRevision = map[string][]string{}
				groups[slot] = byRevision
			}
			byRevision[revision] = append(byRevision[revision], app.Name)
		}
	}

	result := revisionConsensus{}
	for slot, byRevision := range groups {
		if len(byRevision) < 2 {
			continue
		}
		// Lowest slot string wins so the reported group is stable across reconciliations.
		if !result.Hold || slot.String() < result.Slot.String() {
			result = revisionConsensus{Hold: true, Slot: slot, Revisions: byRevision}
		}
	}
	if !result.Hold {
		return revisionConsensus{}
	}

	if timeout <= 0 {
		// No bound configured: hold until the Applications agree.
		return result
	}

	if latestTransition.IsZero() {
		// No step-selected status carries a transition time, so there is nothing to measure the
		// bound from. Holding without a bound is what the bound exists to prevent, so fail open.
		result.Hold = false
		result.Expired = true
		return result
	}

	deadline := latestTransition.Add(timeout)
	if !now.Before(deadline) {
		result.Hold = false
		result.Expired = true
		return result
	}

	// A transition timestamp in the future, from clock skew between writers, must not extend the
	// bound beyond what was configured.
	result.Remaining = min(deadline.Sub(now), timeout)
	return result
}
