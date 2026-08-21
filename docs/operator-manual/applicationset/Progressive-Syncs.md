# Progressive Syncs

> [!WARNING]
> **Beta Feature (Since v3.3.0)**
>

    This feature is in the [Beta](https://github.com/argoproj/argoproj/blob/main/community/feature-status.md#beta) stage. It is generally considered stable, but there may be unhandled edge cases.
    This feature allows you to control the order in which the ApplicationSet controller will create or update the Applications
    owned by an ApplicationSet resource. 

## Use Cases

The Progressive Syncs feature set is intended to be light and flexible. The feature only interacts with the health of managed Applications. It is not intended to support direct integrations with other Rollout controllers (such as the native ReplicaSet controller or Argo Rollouts).

- Progressive Syncs watch for the managed Application resources to become "Healthy" before proceeding to the next stage.
- Deployments, DaemonSets, StatefulSets, and [Argo Rollouts](https://argoproj.github.io/argo-rollouts/) are all supported, because the Application enters a "Progressing" state while pods are being rolled out. In fact, any resource with a health check that can report a "Progressing" status is supported.
- [Argo CD Resource Hooks](../../user-guide/sync-waves.md) are supported. We recommend this approach for users that need advanced functionality when an Argo Rollout cannot be used, such as smoke testing after a DaemonSet change.

## Enabling Progressive Syncs

As an experimental feature, progressive syncs must be explicitly enabled, in one of these ways.

1. Pass `--enable-progressive-syncs` to the ApplicationSet controller args.
1. Set `ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_PROGRESSIVE_SYNCS=true` in the ApplicationSet controller environment variables.
1. Set `applicationsetcontroller.enable.progressive.syncs: "true"` in the Argo CD `argocd-cmd-params-cm` ConfigMap.

## Strategies

ApplicationSet strategies control both how applications are created (or updated) and deleted. These operations are configured using two separate fields:

- **Creation Strategy** (`type` field): Controls application creation and updates
- **Deletion Strategy** (`deletionOrder` field): Controls application deletion order

### Creation Strategies

The `type` field controls how applications are created and updated. Available values:

- **AllAtOnce** (default)
- **RollingSync**

#### AllAtOnce

This default Application update behavior is unchanged from the original ApplicationSet implementation.

All Applications managed by the ApplicationSet resource are updated simultaneously when the ApplicationSet is updated.

```yaml
spec:
  strategy:
    type: AllAtOnce # explicit, but this is the default
```

#### RollingSync

This update strategy allows you to group Applications by labels present on the generated Application resources.
When the ApplicationSet changes, the changes will be applied to each group of Application resources sequentially.

- Application groups are selected using their labels and `matchExpressions`.
- All `matchExpressions` must be true for an Application to be selected (multiple expressions match with AND behavior).
- The `In` and `NotIn` operators must match at least one value to be considered true (OR behavior).
- The `NotIn` operator has priority in the event that both a `NotIn` and `In` operator produce a match.
- All Applications in each group must become Healthy before the ApplicationSet controller will proceed to update the next group of Applications.
- In addition, a best-effort, time-bounded [revision-aware hold](#revision-aware-step-gating) withholds a later group while an earlier group's `Healthy` is known to belong to a different revision. It reduces how often a group is released against a revision an earlier group has not applied; it is not a guarantee that every group is Healthy for the same revision.
- The number of simultaneous Application updates in a group will not exceed its `maxUpdate` parameter (default is 100%, unbounded).
- RollingSync will capture external changes outside the ApplicationSet resource, since it relies on watching the OutOfSync status of the managed Applications.
- RollingSync will force all generated Applications to have autosync disabled. Warnings are printed in the applicationset-controller logs for any Application specs with an automated syncPolicy enabled.
- Sync operations are triggered the same way as if they were triggered by the UI or CLI (by directly setting the `operation` status field on the Application resource). This means that a RollingSync will respect sync windows just as if a user had clicked the "Sync" button in the Argo UI.
- When a sync is triggered, the sync is performed with the same syncPolicy configured for the Application. For example, this preserves the Application's retry settings.
- If an Application is not selected in any step, it will be excluded from the rolling sync and needs to be manually synced through the CLI or UI.

```yaml
spec:
  strategy:
    type: RollingSync
    rollingSync:
      steps:
        - matchExpressions:
            - key: envLabel
              operator: In
              values:
                - env-dev
        - matchExpressions:
            - key: envLabel
              operator: In
              values:
                - env-prod
          maxUpdate: 10%
```

In the above example, the sync will be performed in two steps:

1. All Applications with the label `envLabel=env-dev` will be selected to sync first. Since `maxUpdate` is not defined, a default of 100% applies and all matched Applications will be synced simultaneously. The controller waits until every selected Application reaches a `Healthy` status
before proceeding to the next step.

2. Next, Applications with the label `envLabel=env-prod` will be selected to sync. Here, only 10% of the matched Applications will be synced at a time.
Once each batch of Applications reaches a `Healthy` status, the next batch is synced until all matched

If there are any applications that don't match the listed expressions, they will not be synced by the RollingSync strategy and must be manually synced as describe above.

##### Revision-aware step gating

The Application controller refreshes each Application independently, so at the moment a step is
evaluated an Application in an earlier step may not have been refreshed yet. It then still reports
`Synced` against the previous commit, and its progressive sync status is still the `Healthy` it earned
in the *previous* rollout. Releasing the next step on that reading starts a later step against a
rollout the earlier step has not begun — which is how a RollingSync ends up updating two steps at once.

The ApplicationSet controller therefore withholds the Applications in a *later* step while an
Application in an *earlier* step reports `Healthy` for a set of revisions that differ from the ones an
Application in that later step has observed while sitting in `Waiting`. `Waiting` is the status the
controller assigns the moment it observes a revision or spec change, so it is the one status that
proves the change has reached that Application.

Two cases are deliberately not held back, because in both of them the comparison proves nothing and a
hold would only add latency:

- Applications resolving different Git coordinates (`repoURL`, `targetRevision`, `chart`, `tagPrefix`).
  They are allowed to sit at different revisions indefinitely.
- A later step whose Applications are not in `Waiting`. Nothing has been observed there that could be
  raced. This is the common case of an Application that has simply not been refreshed yet.

The comparison establishes that two Applications disagree about the revisions they have observed, not
which of them is newer: an ApplicationSet status carries no commit ancestry, and the ApplicationSet
controller resolves no revisions of its own. So one uncommon shape is also held: a later step in
`Waiting` because its *generated spec* changed, while it still reports the previous commit and the
earlier step has already moved to the new one. The bound below is what keeps that a delay rather than
a problem.

The hold is bounded at two minutes, measured from the **earliest** `Waiting` transition among the
later-step Applications whose observed revisions differ from the step being waited on. Neither a
sibling Application entering `Waiting` as its own refresh lands mid-hold nor the order in which the
Applications happen to be listed can move that deadline.

If no such `Waiting` transition carries a timestamp at all, the wave is released immediately with a
warning naming that reason, rather than a fresh two-minute window being started. Every code path that
moves an Application into `Waiting` records the transition time, so this is not reachable through
normal operation — but the gate is re-evaluated on every requeue, so deriving a new window each time
would withhold the step forever. A hold whose end cannot be computed is not a bounded hold, and this
gate blocks only on what it can prove.

That bound matters when a commit does not touch an earlier step's
[`manifest-generate-paths`](../high_availability.md#manifest-paths-annotation): the earlier
step's Application is then never refreshed for that commit, and from the ApplicationSet controller this
is indistinguishable from a refresh that is merely late. Past the bound the step is released and a
warning is logged, rather than the rollout stalling for as long as the Application controller's
`timeout.reconciliation`. While a step is withheld the ApplicationSet is re-examined every ten seconds,
because a revision-only change to an Application produces no watch event.

The bound is per disagreement, not per rollout. A **new** commit reaching those later-step
Applications changes their observed revisions, which stamps a new `Waiting` transition and starts a new
two-minute window against the disagreement that commit created. A stream of commits landing on a later
step faster than the bound, while an earlier step is never refreshed for any of them, can therefore
keep that later step withheld for as long as the stream lasts. Every individual hold still expires and
logs its release.

This gating is enabled by default. It can be turned off, in one of these ways, which restores the
previous behavior of gating purely on `Healthy`:

1. Pass `--progressive-sync-revision-aware-gate=false` to the ApplicationSet controller args.
1. Set `ARGOCD_APPLICATIONSET_CONTROLLER_PROGRESSIVE_SYNC_REVISION_AWARE_GATE=false` in the ApplicationSet controller environment variables.
1. Set `applicationsetcontroller.progressive.sync.revision.aware.gate: "false"` in the Argo CD `argocd-cmd-params-cm` ConfigMap.

### Deletion Strategies

The `deletionOrder` field controls the order in which applications are deleted when they are removed from the ApplicationSet. Available values:

- **AllAtOnce** (default)
- **Reverse**

#### AllAtOnce Deletion

This is the default behavior where all applications that need to be deleted are removed simultaneously. This works with both `AllAtOnce` and `RollingSync` creation strategies.

```yaml
spec:
  strategy:
    type: RollingSync # or AllAtOnce
    deletionOrder: AllAtOnce # explicit, but this is the default
```

#### Reverse Deletion

When using `deletionOrder: Reverse` with RollingSync strategy, applications are deleted in reverse order of the steps defined in `rollingSync.steps`. This ensures that applications deployed in later steps are deleted before applications deployed in earlier steps.
This strategy is particularly useful when you need to tear down dependent services in the particular sequence.

**Requirements for Reverse deletion:**

- Must be used with `type: RollingSync`
- Requires `rollingSync.steps` to be defined
- Applications are deleted in reverse order of step sequence

**Important:** The ApplicationSet finalizer is not removed until all applications are successfully deleted. This ensures proper cleanup and prevents the ApplicationSet from being removed before its managed applications. 

**Note:** ApplicationSet controller ensures there is a finalizer when `deletionOrder` is set as `Reverse` with progressive sync enabled. This means that if the applicationset is missing the required finalizer, the applicationset controller adds the finalizer to ApplicationSet before generating applications.

```yaml
spec:
  strategy:
    type: RollingSync
    deletionOrder: Reverse
    rollingSync:
      steps:
        - matchExpressions:
            - key: envLabel
              operator: In
              values:
                - env-dev # Step 1: Created first, deleted last
        - matchExpressions:
            - key: envLabel
              operator: In
              values:
                - env-prod # Step 2: Created second, deleted first
```

In this example, when applications are deleted:

1. `env-prod` applications (Step 2) are deleted first
2. `env-dev` applications (Step 1) are deleted second

This deletion order is useful for scenarios where you need to tear down dependent services in the correct sequence, such as deleting frontend services before backend dependencies.

### Example

The following example illustrates how to stage a progressive sync over Applications with explicitly configured environment labels.

Once a change is pushed, the following will happen in order.

- All `env-dev` Applications will be updated simultaneously.
- The rollout will wait for all `env-qa` Applications to be manually synced via the `argocd` CLI or by clicking the Sync button in the UI.
- 10% of all `env-prod` Applications will be updated at a time until all `env-prod` Applications have been updated.

```yaml
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: guestbook
spec:
  generators:
    - list:
        elements:
          - cluster: engineering-dev
            url: https://1.2.3.4
            env: env-dev
          - cluster: engineering-qa
            url: https://2.4.6.8
            env: env-qa
          - cluster: engineering-prod
            url: https://9.8.7.6/
            env: env-prod
  strategy:
    type: RollingSync
    deletionOrder: Reverse # Applications will be deleted in reverse order of steps
    rollingSync:
      steps:
        - matchExpressions:
            - key: envLabel
              operator: In
              values:
                - env-dev
          #maxUpdate: 100%  # if undefined, all applications matched are updated together (default is 100%)
        - matchExpressions:
            - key: envLabel
              operator: In
              values:
                - env-qa
          maxUpdate: 0 # if 0, no matched applications will be updated
        - matchExpressions:
            - key: envLabel
              operator: In
              values:
                - env-prod
          maxUpdate: 10% # maxUpdate supports both integer and percentage string values (rounds down, but floored at 1 Application for >0%)
  goTemplate: true
  goTemplateOptions: ['missingkey=error']
  template:
    metadata:
      name: '{{.cluster}}-guestbook'
      labels:
        envLabel: '{{.env}}'
    spec:
      project: my-project
      source:
        repoURL: https://github.com/infra-team/cluster-deployments.git
        targetRevision: HEAD
        path: guestbook/{{.cluster}}
      destination:
        server: '{{.url}}'
        namespace: guestbook
```
