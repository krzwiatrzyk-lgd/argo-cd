# Live reproduction of the ApplicationSet RollingSync ordering bug, and of its fix

Real k3d/k3s API server, real Argo CD repo-server, Application controller and ApplicationSet
controller, real git over HTTP. Nothing is mocked and no test framework is involved. The only thing
this harness controls is *when* each Application re-reads git — see
[What this does NOT prove](#what-this-does-not-prove).

**Verdict A (master, `62bc0807`): the bug reproduces.** Step 2 deployed the new commit 4 seconds
after it was pushed, while step 1 was still on the previous commit and had not started. Step 2
finished 69 seconds before step 1 began.

**Verdict B (fix branch, `73f31bc1`): the fix holds.** Same commands, same cluster, same seconds. Step
2 was withheld for the whole 66-second window, and was released only after step 1 had observed the
commit, synced, and reached Healthy — in that order.

**Verdict C (fix branch, bounded hold): the hold is bounded and does not deadlock.** When step 1 is
never refreshed, the gate releases step 2 after exactly 120 s with a WARNING. The ordering violation
is then the same as on master. This is the fix's documented trade-off, not a second bug; it is
spelled out in [The fix's limit, measured](#the-fixs-limit-measured) rather than buried.

---

## Environment

| Component | Version / commit |
|---|---|
| k3d | v5.9.0, cluster `argocd-repro` (single server node) |
| k3s / kubelet | v1.35.5+k3s1 |
| kubectl | v1.36.3 |
| go | go1.26.6 linux/arm64 |
| git | 2.43.0 (server side: `git http-backend`) |
| python | 3.12.3 (git HTTP wrapper only) |
| redis | `docker.io/library/redis:8.10.0-alpine`, via `hack/start-redis-with-password.sh` |
| **`argocd-master`** | `master` @ **`62bc080798ced7f198836a2a33ce522a55ebc7a9`** (the merge base of every branch below) |
| **`argocd-fix1`** | `fix/appset-rollingsync-revision-aware-gate` @ **`73f31bc14c51017ab25a23bcc2140c9bfa6cbf2a`** (based on `62bc0807`) |

Docker here is rootless, which is why the cluster needs
`--k3s-arg '--kubelet-arg=feature-gates=KubeletInUserNamespace=true@server:*'`.

The two fix commits under test:

```
73f31bc14 fix(applicationset): scope the revision-skew hold to the step it is waiting on
4f3122651 fix(applicationset): do not unlock the next RollingSync step on a stale Healthy
```

**Two further commits landed on the fix branch after this evidence was produced,** and neither is
exercised by any fixture here: `43cabb92` makes a hold whose anchor is missing release the wave instead
of restarting the two-minute window, and the commit after it is documentation only. Every fixture below
carries a real `LastTransitionTime` on the `Waiting` status, so the missing-anchor path is never
entered and the three verdicts stand as measured against `73f31bc1`. Re-running against the branch head
should reproduce them unchanged; the anchor change only affects an ApplicationSet whose status was
written by something other than this controller.

The `argocd-master` and `argocd-fix1` binaries differ **only** in those two commits. Across every run
below, the repo-server and the Application controller are the *same* `argocd-master` process; only the
ApplicationSet controller binary is swapped. So nothing but the gate changes between Verdict A and
Verdict B.

## Shape of the experiment

Per `docs/developer-guide/running-locally.md`: the cluster holds only CRDs, RBAC, ConfigMaps and
Secrets (`manifests/install.yaml`, all workloads scaled to 0); the components run as local processes
against it. No container images are built.

The ApplicationSet (`hack/reproduce-rollingsync-ordering/e2e/manifests/appset.yaml`) is a list generator with two elements and a
two-step `RollingSync`, each Application carrying **two sources** — deliberate, to exercise the
multi-source revision comparison and to mirror the production shape of an identical chart revision
with a differing config revision:

- source 0: `http://127.0.0.1:9080/repo.git` at `targetRevision: v1`, path `base` — pinned, never moves
- source 1: same repo at `targetRevision: master`, path `config/{{.name}}` — moves

| | step 1 | step 2 |
|---|---|---|
| Application | `app-beta` | `app-web` |
| label | `step: beta` | `step: web` |
| destination ns | `e2e-beta` | `e2e-web` |
| deployed manifest | `beta-config` ConfigMap | `web-config` ConfigMap |

`app-beta` stands in for the database migration, `app-web` for the web pods. The `release` key of each
ConfigMap is the observable: **what is actually deployed in the cluster**, not merely what a status
field claims. Every verdict below is computed from those two ConfigMaps.

## Reproducing it

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
cd hack/reproduce-rollingsync-ordering/e2e
./up.sh                  # cluster install, git server, binaries, components, sanity gate, ApplicationSet
./reproduce.sh master 60  # Verdict A
./reproduce.sh fix1   60  # Verdict B
./reproduce.sh fix1  165  # Verdict C (waits past the 120s bound)
./down.sh                 # add --purge to also drop binaries, git root and logs
```

`reproduce.sh` runs one identical sequence for either binary:

1. reset git to the baseline commit, refresh both Applications, wait until both are Healthy on it and
   both ConfigMaps read `release: r1`;
2. start the applicationset-controller from the chosen binary;
3. publish one commit changing **both** `config/beta` and `config/web`;
4. refresh **only** `app-web` (step 2), so step 1 still reports `Synced` against the old commit;
5. watch for N seconds — does step 2 sync while step 1 is still on the old commit?
6. refresh `app-beta` (step 1) and watch the remainder.

All four scripts are `shellcheck -x` clean (shellcheck 0.11.0); `lib.sh` carries one justified
`disable=SC2016`, explained inline.

Artifacts per run in `hack/reproduce-rollingsync-ordering/e2e/runs/<variant>-<timestamp>/`: `timeline.txt`, `verdict-phase1.txt`,
`verdict-phase2.txt`, `appset.log` (the controller log slice for that run only),
`appset-key-lines.log`.

---

## Verdict A — the bug, on master

Run `hack/reproduce-rollingsync-ordering/e2e/runs/master-20260821-141108/`. Commit published:
`dd1481f4d58203f9b5e7dfb12d5b30c7d32887a2`, marker `rel-20260821-141108`.

Baseline, then the skew injected at 14:11:20 by refreshing **only** `app-web`
(`timeline.txt`, `rev` = the moving source's resolved revision, `cm` = the deployed ConfigMap):

```
14:11:12 beta{rev=2a2e03cb sync/health=Synced/Healthy cm=r1} web{rev=2a2e03cb sync/health=Synced/Healthy cm=r1}
14:11:20 beta{rev=2a2e03cb sync/health=Synced/Healthy cm=r1} web{rev=dd1481f4 sync/health=OutOfSync/Healthy cm=r1}
14:11:24 beta{rev=2a2e03cb sync/health=Synced/Healthy cm=r1} web{rev=dd1481f4 sync/health=Synced/Healthy cm=rel-20260821-141108}
...
14:12:26 beta{rev=2a2e03cb sync/health=Synced/Healthy cm=r1} web{rev=dd1481f4 sync/health=Synced/Healthy cm=rel-20260821-141108}
```

The line at 14:11:20 **is the exact state from the incident**: step 2 has resolved the new commit
(`dd1481f4`) and is `OutOfSync`; step 1 still resolves the old one (`2a2e03cb`) and therefore still
reports `Synced`, so its progressive-sync status remains the `Healthy` it earned in the previous
rollout. Four seconds later step 2 has deployed the new commit. Step 1 sat on the old commit for the
next 70 seconds.

```
### phase 1 verdict (step 2 released while step 1 still on the old commit?)
ordering_violation=yes
beta_config_release=r1   (r1 = has NOT deployed the new commit)
web_config_release=rel-20260821-141108
```

The gate naming both Applications — this is `getAppsToSync` at
`applicationset/progressivesync/progressive_sync.go:561` releasing step 2 (verbatim, `appset.log`):

```json
{"applicationset":{"Namespace":"argocd","Name":"rolling"},"level":"info","msg":"Application allowed to sync before maxUpdate?: map[app-beta:true app-web:true]","time":"2026-08-21T14:11:20+02:00"}
```

`app-web` promoted `Waiting → Pending` — note `status.targetRevisions` already carries the new commit
while step 1 has not moved (verbatim):

```json
{"app.name":"app-web","applicationset":{"Namespace":"argocd","Name":"rolling"},"level":"info","msg":"Progressive sync application changed status","new_status.message":"Application moved to Pending status, watching for the Application resource to start Progressing","new_status.status":"Pending","new_status.step":"2","new_status.targetRevisions":"2a2e03cb56d1a94f25b86302e10be3bc249ec2e5,dd1481f4d58203f9b5e7dfb12d5b30c7d32887a2","status.message":"Application has pending changes, setting status to Waiting","status.status":"Waiting","status.step":"2","status.targetRevisions":"2a2e03cb56d1a94f25b86302e10be3bc249ec2e5,dd1481f4d58203f9b5e7dfb12d5b30c7d32887a2","time":"2026-08-21T14:11:20+02:00"}
```

The whole rollout, in order (from the `Progressive sync application changed status` lines):

```
14:11:20  app-web   Healthy -> Waiting      step 2
14:11:20  app-web   Waiting -> Pending      step 2
14:11:21  app-web   Pending -> Progressing  step 2
14:11:21  app-web   Progressing -> Healthy  step 2     <-- step 2 COMPLETE
14:12:30  app-beta  Healthy -> Waiting      step 1     <-- step 1 has not even started yet
14:12:30  app-beta  Waiting -> Pending      step 1
14:12:31  app-beta  Pending -> Progressing  step 1
14:12:31  app-beta  Progressing -> Healthy  step 1
```

**Step 2 completed 69 seconds before step 1 started.** `app-beta` never appears in the log between
14:11:20 and 14:12:30: it emitted no status transition at all, because from the gate's point of view
nothing about it had changed. `Holding the next progressive sync wave` never appears — master has no
such code path (`grep -c` = 0).

Confirming finding F8 — the operation the ApplicationSet controller stamps carries no revision at all,
so a late sync is not harmless; it re-resolves `master` at execution time:

```
app-web operationState: Succeeded  syncRevisions=["2a2e03cb...","2957f936..."]  opRevision=  opRevisions=
```

(`opRevision`/`opRevisions` empty = `Sync: &argov1alpha1.SyncOperation{}`, `progressive_sync.go:878`.)

### How the reconcile was triggered

Not by me. I annotated only the *Application* (`argocd.argoproj.io/refresh=normal` on `app-web`); the
ApplicationSet controller reconciled on its own, and I never needed the
`annotate appset ... refresh=true` nudge the spec offers as a fallback. The mechanism is finding F1:
the Application controller *deletes* the refresh annotation when it consumes it, which is an
annotation diff, which requeues the ApplicationSet. Nothing about the trigger changed the outcome —
the same trigger is used identically in Verdicts B and C, and produces the opposite result there.

---

## Verdict B — the fix, same sequence

Run `hack/reproduce-rollingsync-ordering/e2e/runs/fix1-20260821-142030/`, binary built from `73f31bc1`. Commit published:
`d3938bbef78902c3a953236b88027bc1d412762e`, marker `rel-20260821-142030`.

Skew injected at 14:20:42, identically. `timeline.txt`, first and last lines of the hold window
(holds ran 14:20:42 to 14:21:48, 66 s, one per 10 s requeue):

```
14:20:42 beta{rev=2a2e03cb sync/health=Synced/Healthy cm=r1} web{rev=d3938bbe sync/health=OutOfSync/Healthy cm=r1}
...
14:21:44 beta{rev=2a2e03cb sync/health=Synced/Healthy cm=r1} web{rev=d3938bbe sync/health=OutOfSync/Healthy cm=r1}
```

Step 2 stayed `OutOfSync` and **nothing was deployed**, where master had deployed within 4 seconds:

```
### phase 1 verdict (step 2 released while step 1 still on the old commit?)
ordering_violation=no
beta_config_release=r1   (r1 = has NOT deployed the new commit)
web_config_release=r1
```

The gate, in the same reconcile second, first computing the old answer and then withholding step 2:

```json
{"applicationset":{"Namespace":"argocd","Name":"rolling"},"level":"info","msg":"Application allowed to sync before maxUpdate?: map[app-beta:true app-web:true]","time":"2026-08-21T14:20:42+02:00"}
{"applicationset":{"Namespace":"argocd","Name":"rolling"},"level":"info","msg":"Application allowed to sync before maxUpdate?: map[app-beta:true]","time":"2026-08-21T14:20:42+02:00"}
```

The first line is a reconcile from before `app-web` had observed the commit (nothing to withhold, and
releasing step 2 there is a no-op); the second is the revision-aware gate removing `app-web`. Compare
against Verdict A, where `map[app-beta:true app-web:true]` is the *only* form that ever appears.

The hold itself (verbatim, first of 9 — one per 10-second requeue over the 66-second window):

```json
{"app.later":"app-web","app.later.revisions":"2a2e03cb56d1a94f25b86302e10be3bc249ec2e5,d3938bbef78902c3a953236b88027bc1d412762e","app.step":"app-beta","app.step.revisions":"2a2e03cb56d1a94f25b86302e10be3bc249ec2e5,2a2e03cb56d1a94f25b86302e10be3bc249ec2e5","applicationset":{"Namespace":"argocd","Name":"rolling"},"level":"info","msg":"Holding the next progressive sync wave: an Application in a later step has observed different revisions than this step, so this step's Healthy may belong to the previous rollout","step":1,"time":"2026-08-21T14:20:42+02:00"}
```

The two revision lists in that one line are the whole bug, stated by the controller itself: step 1 is
on `2a2e03cb,2a2e03cb`, step 2 on `2a2e03cb,d3938bbe`.

At 14:21:47 I refreshed `app-beta` — the event a production webhook would have delivered. The
resulting order:

```
14:20:42  app-web   Healthy -> Waiting      step 2     <-- observes the commit, then waits 66s
14:21:48  app-beta  Healthy -> Waiting      step 1     <-- step 1 finally observes it
14:21:48  app-beta  Waiting -> Pending      step 1
14:21:49  app-beta  Pending -> Progressing  step 1
14:21:49  app-beta  Progressing -> Healthy  step 1     <-- step 1 COMPLETE
14:21:49  app-web   Waiting -> Pending      step 2     <-- released only now
14:21:50  app-web   Pending -> Progressing  step 2
14:21:50  app-web   Progressing -> Healthy  step 2
```

`app-web` leaves `Waiting` only after `app-beta` reaches `Healthy`, in the same second. Both
ConfigMaps then carry the new marker (`verdict-phase2.txt`), and no bound-expiry warning was emitted
in this run (`grep -c "Releasing the next progressive sync wave"` = 0) — the hold was released by its
condition clearing, not by its timeout.

### Verdict B is exactly inverted from Verdict A

| | master `62bc0807` | fix `73f31bc1` |
|---|---|---|
| `ordering_violation` (from deployed ConfigMaps) | **yes** | **no** |
| step 2 deployed while step 1 on old commit | after 4 s | never |
| `appsToSync` while step 1 was behind | `map[app-beta:true app-web:true]` | `map[app-beta:true]` |
| `Holding the next progressive sync wave` lines | 0 | 9 |
| step 1 Healthy → step 2 Pending | step 2 finished 69 s *first* | step 2 released 0 s *after* |

## The fix's limit, measured

Run `hack/reproduce-rollingsync-ordering/e2e/runs/fix1-20260821-142208/`, same binary, same sequence, but step 1 is left
un-refreshed past the bound. `maxRevisionSkewHold` is 2 minutes.

14 holds from 14:22:20 to 14:24:19, then at 14:24:20 — **exactly 120 s** after step 2's `Waiting`
transition — the gate gives up and says so (verbatim):

```json
{"app.later":"app-web","app.later.revisions":"2a2e03cb56d1a94f25b86302e10be3bc249ec2e5,6603ffc217fb147765e8e716e1f48f0a0ab18034","app.step":"app-beta","app.step.revisions":"2a2e03cb56d1a94f25b86302e10be3bc249ec2e5,2a2e03cb56d1a94f25b86302e10be3bc249ec2e5","applicationset":{"Namespace":"argocd","Name":"rolling"},"level":"warning","maxHold":120000000000,"msg":"Releasing the next progressive sync wave with a revision skew still present: the two steps did not converge on the same revisions within the hold window","step":1,"time":"2026-08-21T14:24:20+02:00"}
```

Step 2 deployed two seconds later, step 1 still on the old commit:

```
14:24:22 beta{rev=2a2e03cb sync/health=Synced/Healthy cm=r1} web{rev=6603ffc2 sync/health=Synced/Healthy cm=rel-20260821-142208}
### phase 1 verdict
ordering_violation=yes
```

State it plainly: **the fix does not make the ordering violation impossible. It makes it require a
step to not observe the commit for two whole minutes.** That is the deliberate trade against F3 — a
commit that touches only step 2's `manifest-generate-paths` never refreshes step 1's Application, and
from the ApplicationSet controller that is indistinguishable from a refresh that is merely late, so an
unbounded hold would stall such a rollout for as long as the Application controller's resync period,
which installs routinely raise well past its 2-minute default.
The evidence for "no deadlock" and the evidence for "the violation is still reachable" are the same
log line. Both belong in the PR description.

---

## Deviations from the spec, and why

1. **The git server is `git http-backend`, not `python3 -m http.server`.** The spec's dumb-HTTP server
   does not work: Argo CD resolves revisions with go-git (`util/git/client.go:909` `LsRemote`, and the
   file imports `github.com/go-git/go-git/v5` at :29), and go-git's HTTP transport speaks only the
   *smart* protocol — it issues `GET /info/refs?service=git-upload-pack` and expects a pkt-line body.
   A static file server answers with the dumb refs file, so every Application parked on
   `Failed to load target state: ... failed to list refs: unexpected EOF`. `git ls-remote` from the CLI
   *succeeded* against that same server, which is what makes this misleading — the git CLI still
   supports dumb HTTP and go-git does not, so the CLI is not a valid test of whether Argo CD can read
   the server. Replaced with a ~60-line CGI wrapper around `git http-backend`
   (`hack/reproduce-rollingsync-ordering/e2e/git-smart-http.py`). `git update-server-info` is now irrelevant but harmless, and
   `up.sh` still runs it.

   **Why this transport and not another.** The wrapper hand-rolls no git protocol logic: it marshals
   CGI environment variables and delegates entirely to `/usr/lib/git-core/git-http-backend`, the same
   binary this repo's own e2e git fixture uses — `test/fixture/testrepos/nginx.conf` serves it through
   nginx + fcgiwrap (`fastcgi_param SCRIPT_FILENAME /usr/lib/git-core/git-http-backend`). So this is
   the sanctioned server with a ~60-line front end substituted for nginx + fcgiwrap, chosen over that
   fixture only because the fixture costs a ~1 GB image pull. `git daemon` over `git://` would have
   worked too (go-git registers the `git` protocol by default). Confirmed working end to end from the
   repo-server side, not from the CLI: `"POST /repo.git/git-upload-pack HTTP/1.1" 200` in
   `hack/reproduce-rollingsync-ordering/e2e/logs/gitserver.log`, and resolved commit SHAs in every Application status quoted below.

   **This choice does not affect what the experiment demonstrates.** The bug is in the ApplicationSet
   controller's gate, which never touches git: it compares revision strings already resolved and
   recorded in Application statuses. Any transport that lets the repo-server resolve a commit produces
   the same input to the gate.

2. **`hack/git-verify-wrapper.sh` must be on the repo-server's `PATH`.** Without it every manifest
   generation failed with a bare `permission denied`, and the message points nowhere near the cause.
   `reposerver/repository/repository.go:549` calls `gitClient.VerifyCommitSignature` **unconditionally**
   in the manifest-generation operation context — it is the deprecated backwards-compatibility path
   (the `// nolint:staticcheck` call under the TODO referencing argoproj/argo-cd#27695), with no
   GPG-enabled guard at that call site, so `ARGOCD_GPG_ENABLED=false` does not help. That method shells
   out to `git-verify-wrapper.sh` and `util/git/client.go:1150-1157` collapses *any* failure — including
   `executable file not found in $PATH` — into `errors.New("permission denied")`. So a missing script
   on `PATH` fails every `GenerateManifest` with a message that suggests a filesystem or git-auth
   problem and is neither. The Procfile hides this by putting `dist/` on `PATH`; running the binary
   directly does not. `up.sh` copies the script into the bin dir. Harness detail, unrelated to the bug
   under investigation.

3. **`manifests/install.yaml` ships no `AppProject` object.** Every Application sat on
   `Application referencing project default which does not exist`. (The file's only `kind: AppProject`
   occurrence, at line 30724, is the CRD's `spec.names.kind` — not a resource.) It is argocd-server
   that normally creates the project, in `initializeDefaultProject` at `server/server.go:293`, and
   argocd-server is deliberately not running here. `up.sh` creates the project itself; the spec it
   applies is field-for-field what that function creates — `sourceRepos: ['*']`, destinations `*`/`*`,
   `clusterResourceWhitelist` `*`/`*` — so this substitutes for the missing component rather than
   loosening anything.

4. **`timeout.reconciliation: 1h` in `argocd-cm` does nothing here — this one matters, see below.**

5. **Each Application gets its own destination namespace** (`e2e-beta`, `e2e-web`). Source 0 is shared
   and pinned, so with one shared namespace the two Applications would fight over the same `base-cm`.

6. **A third run (Verdict C) was added.** The spec asks for two; the bound is a load-bearing property
   of this fix and asserting it without measuring it would have been a claim, not evidence.

### Deviation 4 in full: the knob in the spec is the wrong knob

The spec's `kubectl -n argocd patch cm argocd-cm ... timeout.reconciliation: 1h` was applied and is
present in the ConfigMap (`kubectl -n argocd get cm argocd-cm -o jsonpath='{.data.timeout\.reconciliation}'`
returns `1h`), **and the Application controller ignored it.** Demonstrated back-to-back, with that
ConfigMap value in place the whole time — only the flag differs
(`hack/reproduce-rollingsync-ordering/e2e/state/appresync-default-proof.log`, verbatim):

```json
{"level":"info","msg":"appResyncPeriod=2m0s, appHardResyncPeriod=0s, appResyncJitter=0s","time":"2026-08-21T14:29:25+02:00"}
{"level":"info","msg":"appResyncPeriod=1h0m0s, appHardResyncPeriod=0s, appResyncJitter=0s","time":"2026-08-21T14:29:28+02:00"}
```

Run as a local process it never reads that ConfigMap key — the manifests map it into the
`ARGOCD_RECONCILIATION_TIMEOUT` env var through the StatefulSet, which does not exist here. The real
knob is the `--app-resync` flag (default 120 s, jitter 60 s).

**This bit me, and I am reporting it because it invalidated my first attempt at Verdict B.** In that
first attempt (14:06:47–14:07:57, before the flag was corrected) the fix held step 2 for 68 s and then
released it — and I had not touched step 1. What released it was the Application controller's own 2 m
resync refreshing `app-beta`:

```json
{"app-namespace":"argocd","application":"app-beta","level":"info","msg":"Refreshing app status (comparison expired, requesting refresh. reconciledAt: 2026-08-21 14:05:50 +0200 CEST, expiry: 2m0s), level (2)","project":"default","time":"2026-08-21T14:07:55+02:00"}
```

**Provenance of that one line:** it is transcribed from this session's terminal output, not from a file
you can still read. Restarting the Application controller truncates
`logs/application-controller.log` (the launcher redirects with `>`), so the log that held it is gone
and no artifact in `runs/` contains it. The reproducible part of the claim is the two-line capture
above, which was taken afterwards and is on disk; the `expiry: 2m0s` in this line is consistent with
it. Treat this line as the narrative of how the mistake was found, not as load-bearing evidence — no
verdict rests on it.

The ordering that attempt produced was in fact correct (step 1 Healthy at 12:07:56Z, step 2 at
12:07:57Z), so the fix looked good — but the release had a cause I had not chosen and did not yet
understand, which makes it worthless as evidence. Every run quoted above was made *after* restarting
the Application controller with `--app-resync 3600 --app-resync-jitter 0`, which it confirmed at
startup with the `appResyncPeriod=1h0m0s, appHardResyncPeriod=0s, appResyncJitter=0s` line — the
second line of the capture quoted just above. (That restart happened at 14:09:53, before the
14:11:08 / 14:20:30 / 14:22:08 runs; its own log line has since been truncated by the later restart
that produced the durable capture, which is why the capture is quoted instead.)

With that in place the only thing that ever refreshes an Application is my annotation. `up.sh` sets
this via `APP_RESYNC_SECONDS` and keeps the `argocd-cm` patch only for the record, with a comment
saying it is inert.

Two further honesty notes:

- **The `argocd-fix1` binary was rebuilt mid-task.** My first fix build was `4f312265`; the fix author
  then pushed `73f31bc1`, which changed the gate's log strings and how the bound is measured. Both fix
  runs quoted above were re-run from scratch against `73f31bc1`, and `73f31bc1` was still the branch
  tip when they finished. The earlier `4f312265` runs (`fix1-20260821-141241`,
  `fix1-20260821-141455`) are kept in `runs/` and reached the same verdicts.
- **Verdict A was also produced twice.** The first Evidence A run (14:03:07) predates the
  `--app-resync` correction. It reproduced the bug identically, and the 2 m resync could not have
  contributed since the violation completed within 1 second. The run quoted above is the corrected
  one. Raw first-run captures: `hack/reproduce-rollingsync-ordering/e2e/state/10-evA-appset.log`, `11-evA-state.txt`.

---

## What this does NOT prove

- **The refresh ordering is imposed by hand, not raced for.** In production the skew comes from
  argocd-server fanning out webhook refreshes across Applications milliseconds apart, plus
  `manifest-generate-paths` skipping Applications a commit does not touch. Here I annotate step 2's
  Application, wait, then annotate step 1's. This is a *controlled stand-in* for that race. It
  produces the same input state the race produces — step 2 resolved to the new commit, step 1 still
  reporting `Synced` on the old one — and the controller cannot tell the difference, because nothing
  in the Application status records how a refresh was requested. But this is not a demonstration that
  the race occurs, only that the gate mishandles the state the race creates. The incident report is
  the evidence that the state occurs in production; this is the evidence for what the gate does with
  it.
- **No webhook was configured at all.** The `manifest-generate-paths` optimisation that made the race
  visible in the reported incident is not exercised.
- **Two Applications, one per step, one commit.** No `maxUpdate`, no multi-Application steps, no more
  than two steps, no cluster generator (this is a list generator, so unlike the incident's cluster
  generator it *does* have a periodic requeue — findings F1/F2 mean the fix's requeue hint is
  load-bearing in production in a way this harness does not test).
- **ConfigMaps, not workloads.** They go Synced/Healthy instantly. Nothing here exercises a step that
  takes real time to become Healthy, a step that fails, or a sync that has to be retried — and a
  slow or flapping step 1 is precisely where a bounded hold's 2-minute budget is most likely to run
  out. That gap matters and is not covered.
- **Single reconcile-loop timing, one machine, few runs.** Each verdict rests on one run of each
  variant (two, for the fix, across two commits). Nothing here is a statistical claim, and nothing
  probes controller restarts, leader election, or an ApplicationSet status write racing a concurrent
  Application update.
- **Verdict C shows the bound works as designed; it does not show that 2 minutes is the right
  number.** Choosing that value is a judgement about rollout latency versus ordering safety, and no
  measurement here speaks to it.
- **The fix's own unit tests are not evidence of anything here.** This document covers only the live
  behaviour of the two binaries.

## Files

| Path | What |
|---|---|
| `hack/reproduce-rollingsync-ordering/e2e/up.sh` | full bring-up incl. sanity gate |
| `hack/reproduce-rollingsync-ordering/e2e/reproduce.sh` | one experiment run: `reproduce.sh <master\|fix1> [hold-seconds]` |
| `hack/reproduce-rollingsync-ordering/e2e/down.sh` | tear down (`--purge` also drops binaries/git/logs) |
| `hack/reproduce-rollingsync-ordering/e2e/lib.sh` | shared config and component launchers |
| `hack/reproduce-rollingsync-ordering/e2e/git-smart-http.py` | `git http-backend` CGI wrapper |
| `hack/reproduce-rollingsync-ordering/e2e/manifests/appset.yaml` | the two-step RollingSync ApplicationSet |
| `hack/reproduce-rollingsync-ordering/e2e/runs/master-20260821-141108/` | **Verdict A** |
| `hack/reproduce-rollingsync-ordering/e2e/runs/fix1-20260821-142030/` | **Verdict B** |
| `hack/reproduce-rollingsync-ordering/e2e/runs/fix1-20260821-142208/` | **Verdict C** |
| `hack/reproduce-rollingsync-ordering/e2e/runs/fix1-20260821-1412*/`, `1414*/` | earlier runs against fix commit `4f312265` |
| `hack/reproduce-rollingsync-ordering/e2e/state/` | first (pre-`--app-resync`) captures |
| `hack/reproduce-rollingsync-ordering/e2e/logs/` | full logs: repo-server, application-controller, each appset run, redis, git server |

Only the first six rows are committed. `runs/`, `state/` and `logs/` are run output, produced under the
harness directory and ignored by `.gitignore` — the paths are listed so the quotations above can be
traced to the run that produced them, not because those files ship with the repository.
