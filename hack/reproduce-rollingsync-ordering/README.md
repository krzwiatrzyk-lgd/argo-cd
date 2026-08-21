# Reproducing the RollingSync out-of-order rollout

A progressive-sync (`strategy.type: RollingSync`) ApplicationSet can start step 2 before step 1 has
rolled out at all. This directory reproduces that against a live cluster.

If you only want to see the bug, and not run a cluster, there is a deterministic Go reproduction that
needs nothing but the repository:

```sh
go test -count=1 -v -run TestRollingSyncReleasesLaterStepOnStaleHealthy ./applicationset/controllers/
```

## The mechanism

`getAppsToSync` (`applicationset/progressivesync/progressive_sync.go:561`) walks the RollingSync steps
and stops as soon as it finds a step in which some Application is not `Healthy`. Everything up to and
including that step is allowed to sync. It reads that `Healthy` from
`ApplicationSet.status.applicationStatus[].status` and never asks **which revision** it belongs to.

`UpdateApplicationSetApplicationStatus` (`progressive_sync.go:396`) is what maintains those entries.
An Application is moved out of `Healthy` only when something it can observe has changed:

- `revisionsChanged` (`:447`) compares the recorded `targetRevisions` against the Application's own
  `status.sync.revisions`, and
- `specChanged` (`:450-470`) compares the generated spec against the live one.

Both are properties of the **Application's status**, which only moves when the Application controller
refreshes that Application. So an Application that has not been refreshed since the previous rollout
still reports `Synced` against the previous commit, `revisionsChanged` is false, and its entry keeps
the `Healthy` it earned last time. The gate reads that stale `Healthy` as "step 1 is done" and
releases step 2 — which *has* been refreshed, so it is in `Waiting` and gets promoted to `Pending`.

Two details make it worse:

- Nothing is logged for the Application that is left alone. The status line at `:541` is guarded on
  the pointer identity `newAppStatus.LastTransitionTime == &now`, so an untouched entry is silent.
  In the controller log, the absence of a line for step 1 is part of the signature.
- The sync operation the ApplicationSet stamps carries no revision at all
  (`Sync: &argov1alpha1.SyncOperation{}`, `progressive_sync.go:878`). Every RollingSync-triggered sync
  re-resolves its target at execution time, so ordering is the only thing protecting a rollout from
  being torn apart; there is no pinned revision that would make a late sync harmless.

In production this is a sub-second race between two Application refreshes, which is why it is rare and
why it hurts: it showed up as web pods rolling before the database migration they depend on had
finished.

## Prerequisites

- A cluster with Argo CD installed and progressive syncs **enabled** on the ApplicationSet
  controller: `--enable-progressive-syncs`, or
  `ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_PROGRESSIVE_SYNCS=true`, or
  `applicationsetcontroller.enable.progressive.syncs: "true"` in `argocd-cmd-params-cm`.
  `reproduce.sh` fails with a clear message if the ApplicationSet never reports a progressive-sync
  status.
- `kubectl` pointed at that cluster, and `git`.
- A git repository you can push to. It can be empty; the script adds `base/` and `manifests/` and
  moves one branch. This cannot be replaced by a read-only public repository: the reproduction needs
  a **resolved revision to move under the same `repoURL` and `targetRevision`**, which is only
  possible if you can move the branch.
- No webhook configured for that repository. A webhook refreshes both Applications at once, which
  puts the race back.

## Running it

```sh
REPO_URL=git@github.com:you/rollingsync-repro.git hack/reproduce-rollingsync-ordering/reproduce.sh
```

Environment: `REPO_BRANCH` (default `main`), `BASE_TAG` (default `rollingsync-repro-base`),
`NAMESPACE` (default `argocd`), `TIMEOUT` (default 300s per observation), `KEEP=1` to leave the
ApplicationSet behind. Run with `--help` for the same list.

The script restores `argocd-cm`'s `timeout.reconciliation`, restarts the Application controller and
deletes the ApplicationSet on exit, including on failure.

## What it does, and why each step is there

1. **Seeds the repository.** `base/` is tagged and never moves; `manifests/` tracks a branch and does.
   The ApplicationSet template uses both as two sources, so `status.sync.revisions` has two slots and
   only the second one moves — the shape the production incident had (a pinned chart plus a moving
   config repo).
2. **Runs a first rollout** and waits for both Applications to be `Healthy`. The bug is a *second*
   rollout starting from a completed first one; seeding the ApplicationSet with step 2 already in
   `Pending` would prove nothing, because by then the wrong decision has already been made.
3. **Disables periodic refresh** (`timeout.reconciliation: 0s`, then restarts the Application
   controller). After this an Application refreshes only when told to. The restart is not optional:
   no Go code reads that ConfigMap key: the upstream StatefulSet maps it into
   `ARGOCD_RECONCILIATION_TIMEOUT`
   (`manifests/base/application-controller/argocd-application-controller-statefulset.yaml:37-42`),
   which is the default for `--app-resync`
   (`cmd/argocd-application-controller/commands/argocd_application_controller.go:260`), and env is
   resolved when the pod starts. If your install passes `--app-resync` explicitly, or runs the
   controller as a local process, patching `argocd-cm` does nothing and you have to set the flag
   instead — the script does not silently continue in that case, it fails at step 4 or 5 when an
   Application observes the commit it was not supposed to see. Disabling refresh is done *after* the
   first rollout on purpose: the restart re-enqueues every Application for a refresh, which is
   harmless while the branch has not moved yet and would destroy the skew if it happened later.
4. **Pushes a commit**, then asserts that neither Application has noticed it. If one has, periodic
   refresh or a webhook is still active and the script stops rather than pretend.
5. **Refreshes step 2, and only step 2** (`argocd.argoproj.io/refresh=hard`), and waits until that
   Application reports the new revision — gating on the observation, not on a sleep. Then it asserts
   that step 1 has *not* moved. This is the whole trick: it does not fake the bug, it removes the need
   to win a coin flip to observe it. `hard` rather than `normal` because a `normal` refresh can be
   answered from the repo-server's resolved-revision cache (`--revision-cache-expiration`, default
   3m), in which case step 2 would never move to the new revision. The ApplicationSet controller will
   not strip the annotation before the Application controller consumes it:
   `AnnotationKeyRefresh` is in `defaultPreservedAnnotations`
   (`applicationset/controllers/applicationset_controller.go:84-88`).
6. **Watches the decision.** A `list` generator has no periodic requeue
   (`getMinRequeueAfter`, `applicationset_controller.go:632`), so the script also annotates the
   ApplicationSet to guarantee a reconcile rather than depend on an event arriving. That changes
   nothing about the decision under test: the controller re-runs the same gate over the same objects.

## Reading the result

The script prints its own verdict. The evidence behind it:

**Bug present.** The ApplicationSet controller log contains

```text
Application allowed to sync before maxUpdate?: map[rollingsync-repro-beta:true rollingsync-repro-web:true]
triggering sync for application: rollingsync-repro-web, prune enabled: true
```

with **both** Applications in one map, and no status line at all for `rollingsync-repro-beta`. On the
objects:

```sh
kubectl -n argocd get app rollingsync-repro-web -o jsonpath='{.operation.initiatedBy.username}'
# applicationset-controller
kubectl -n argocd get appset rollingsync-ordering-repro \
  -o jsonpath='{.status.applicationStatus[?(@.application=="rollingsync-repro-beta")].status}'
# Healthy   <- at the OLD targetRevisions, while step 2 is syncing the new one
```

**Bug fixed.** `Application allowed to sync before maxUpdate?` names only the step 1 Application, no
`triggering sync` line appears for step 2, and the ApplicationSet keeps step 2 in `Waiting` until step
1 has observed the new revision.

Two things to check before believing a "not reproduced" result — either of them means the setup did
not hold, not that the bug is gone:

- step 1's ApplicationSet entry must still read `Healthy` at the *old* `targetRevisions`, and
- step 2's must read `Waiting`.

## Cleaning up

The script cleans up after itself. If you ran it with `KEEP=1`:

```sh
kubectl -n argocd delete applicationset rollingsync-ordering-repro
kubectl delete namespace rollingsync-repro-beta rollingsync-repro-web
```

and check that `timeout.reconciliation` in `argocd-cm` is back to what it was.
