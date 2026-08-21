# Live-cluster reproduction of the RollingSync ordering bug

A **manual** harness. It is not wired into `make test`, `make test-e2e` or any CI workflow, and
nothing in the build depends on it. It exists so the claim in
[`EVIDENCE.md`](EVIDENCE.md) — that a RollingSync step can be released against a revision an earlier
step has never seen, in a real cluster, with real Argo CD components — can be checked rather than
taken on trust.

For the deterministic version of the same bug, which *does* run in CI, see
`applicationset/controllers/rollingsync_ordering_repro_test.go` and the parent directory's README.
This harness is the slow, high-fidelity counterpart: nothing is mocked, the observable is the
`ConfigMap` actually deployed into the cluster, and the verdict is computed from that.

## What it needs

| Tool | Used for |
|---|---|
| `docker` | k3d's node container and the redis container |
| `k3d` | creates the single-node k3s cluster, and deletes it when asked to |
| `kubectl` | everything against that cluster |
| `go` | builds `./cmd` twice, once per variant |
| `git` | the fixture repository, and `git http-backend` server-side |
| `python3` | a ~60-line CGI wrapper around `git http-backend` |

`up.sh` **creates a k3d cluster** named `argocd-repro` if one is not already there, and reuses it
otherwise. `down.sh` leaves it running, because creating it is the slowest part of `up.sh`; pass
`DELETE_CLUSTER=1` to remove it too. The harness binds `127.0.0.1:9080` (git),
`:8081` (repo-server), `:12345`/`:12346`/`:7001` (ApplicationSet controller), and starts a
container named `argocd-redis`. It writes only inside this directory and `/tmp/argocd-local`.

On rootless Docker the cluster needs `--kubelet-arg=feature-gates=KubeletInUserNamespace=true`;
`up.sh` passes it unconditionally, which is harmless on rootful Docker.

## Running it

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"   # or however go is on your PATH
cd hack/reproduce-rollingsync-ordering/e2e

# The fix does not live on this branch, so point FIX_TREE at a checkout of the branch under test.
git worktree add /tmp/argocd-fix fix/appset-rollingsync-revision-aware-gate
FIX_TREE=/tmp/argocd-fix ./up.sh

./reproduce.sh master 60    # Verdict A: the bug
./reproduce.sh fix1   60    # Verdict B: the fix holds
./reproduce.sh fix1  165    # Verdict C: the hold is bounded, and releases at 120s

./down.sh                   # --purge also drops binaries, the git root and logs
                            # DELETE_CLUSTER=1 ./down.sh --purge  removes the cluster as well
```

`up.sh` is the slow part: two `go build ./cmd` runs, several minutes each on a cold module cache,
plus the cluster and the `manifests/install.yaml` apply. Both binaries are cached in `bin/`, so a
second `up.sh` is quick. Each `reproduce.sh` takes roughly the hold seconds you pass plus ~40s of
setup and settle, so a full A/B/C cycle is about 15 minutes after the first build.

With `FIX_TREE` unset, `up.sh` builds only the baseline and says so; `reproduce.sh fix1` then refuses
with instructions rather than running something meaningless.

`BASELINE_TREE` overrides where `argocd-master` is built from. It defaults to this checkout, which is
correct on this branch because this branch changes no controller code — it adds a test and this
directory. Set it explicitly if you are running the harness from a branch that does.

## What produces the verdict

`reproduce.sh` runs one identical sequence for either binary, and the only thing it controls is
**when each Application re-reads git**:

1. reset the fixture repo to its baseline commit, refresh both Applications, wait until both are
   Healthy and both ConfigMaps read `release: r1`;
2. start the applicationset-controller from the chosen binary;
3. publish one commit changing both steps' config;
4. refresh **only** the step-2 Application, so step 1 still reports `Synced` against the old commit;
5. watch — does step 2 deploy while step 1 is still on the old commit?
6. refresh the step-1 Application and watch the remainder.

Step 4 is the whole experiment. In production that skew arrives on its own, because the Application
controller refreshes Applications independently; here it is injected deliberately so the window is
wide enough to observe rather than a ~100ms race.

The Application controller runs with `--app-resync 3600 --app-resync-jitter 0` so that nothing but
those deliberate refreshes ever re-reads git. Note that `timeout.reconciliation` in `argocd-cm` will
*not* do this — a locally-run controller never reads that key, since the manifests map it into
`ARGOCD_RECONCILIATION_TIMEOUT` through the deployment env. `EVIDENCE.md` records how that cost one
invalid run.

Each run writes `timeline.txt`, `verdict-phase1.txt`, `verdict-phase2.txt`, `appset.log` and
`appset-key-lines.log` into `runs/<variant>-<timestamp>/`. Those directories, and `logs/`, `bin/`,
`gitroot/`, `run/`, `state/` and `cmp/`, are all gitignored: they are output, not source.

## Known harness requirements that are not bugs in Argo CD

Both are recorded at more length in `EVIDENCE.md`; both cost real debugging time and neither points
at its own cause.

- **A dumb HTTP git server does not work.** Argo CD resolves revisions with go-git, whose HTTP
  transport speaks only the *smart* protocol. A static file server makes every Application fail with
  `failed to list refs: unexpected EOF` — while `git ls-remote` against that same server succeeds,
  because the git CLI still supports dumb HTTP. Hence `git-smart-http.py`, which delegates to
  `/usr/lib/git-core/git-http-backend`, the same binary this repo's own e2e git fixture serves
  through nginx + fcgiwrap.
- **`hack/git-verify-wrapper.sh` must be on the repo-server's `PATH`.** `GenerateManifest` calls
  `VerifyCommitSignature` unconditionally, and any failure — including "executable file not found" —
  is collapsed into `permission denied`. `up.sh` copies the script into `bin/`, which is on the
  repo-server's `PATH`.
