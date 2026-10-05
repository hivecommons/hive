# CI runner cluster configuration

Manifests for the self-hosted GitHub Actions runners that serve
`hivecommons/hive`. These are **not** applied by any workflow in this
repository — they are applied by an operator with cluster access. They live
here so the configuration is reviewable, and so a cluster rebuild does not
silently lose it.

| Target | Value |
| --- | --- |
| Context | `vllm-d` |
| Namespace | `arc-systems` |
| Object | `RunnerDeployment/hivecommons-hive-runners` |
| Controller | summerwind ARC (`actions.summerwind.dev/v1alpha1`) |
| Runner labels | `["self-hosted","linux","openshift","hive"]` |

> **Moving off vllm-d:** the dedicated Linode cluster that replaces this
> arrangement is documented in [`lke/README.md`](lke/README.md).

## Incident: apt egress failure (#6648)

Every job that installed a toolchain package (`gcc`, `libc6-dev`, `tmux`)
failed. `apt-get update` consumed its full timeout and then reported that
`archive.ubuntu.com` was unreachable.

Reproduced from inside a runner pod:

```console
$ getent hosts archive.ubuntu.com
2620:2d:4000:1::101  archive.ubuntu.com      # AAAA only — no A record returned
$ curl -s -o /dev/null -w '%{http_code}\n' -4 http://archive.ubuntu.com/ubuntu/
000
$ curl -s -o /dev/null -w '%{http_code}\n' -4 http://azure.archive.ubuntu.com/ubuntu/
200
```

The cluster has **no IPv6 egress whatsoever** (GitHub over IPv6 fails too), and
IPv4 to `archive.ubuntu.com` / `security.ubuntu.com` is blocked as well. apt
was resolving the mirrors to addresses that could never be routed, and burning
its retry budget there before failing.

`azure.archive.ubuntu.com` carries the same archive — including the security
suite — and is reachable over IPv4.

### Fix

1. `apt-mirror-configmap.yaml` — repoints apt at the reachable mirror and sets
   `Acquire::ForceIPv4`.
2. `runnerdeployment-patch.yaml` — mounts both files into the runner pods.

```console
kubectl -n arc-systems apply -f src/deploy/ci-runners/apt-mirror-configmap.yaml
kubectl -n arc-systems patch runnerdeployment hivecommons-hive-runners \
  --type merge --patch-file src/deploy/ci-runners/runnerdeployment-patch.yaml
```

Patching rolls all runner pods, so apply it while runners are idle.

### Verification

On a fresh pod from the new generation, as the runner user (uid 1001, which
does not own `/etc/apt` but does have `sudo`):

```console
$ ls /etc/apt/sources.list.d/ubuntu.sources /etc/apt/apt.conf.d/99-force-ipv4
$ sudo apt-get update            # rc=0
$ sudo apt-get install -y gcc libc6-dev tmux   # rc=0
$ gcc --version | head -1        # gcc (Ubuntu 13.3.0-...) 13.3.0
$ tmux -V                        # tmux 3.4
```

`sudo` emits `unable to send audit message` warnings under the pod's security
context. They are harmless and do not affect the exit code.

## Baking the CI toolchain into the runner image (#7206)

The `#6648` fix above makes apt *work*. It does not stop CI from *needing* apt
on every job, and that dependency is what kept the failure class alive:
#6648 → #6870 → #6935 → #7124 → #7201, closed four times and back every time.

Every `-race` shard needs a C toolchain, the stock runner image has none, so
each shard installed one over the network. `#7009` added an offline `.deb`
cache to avoid that, but it cannot survive: measured 2026-09-16, the repo sits
at **9.99 GB of its 10 GB** Actions cache ceiling, buildkit blobs are **96.6%**
of it, and **6.57 GB** of blobs were written in one day. LRU always evicts the
small apt entry, so shards are forced back onto the mirrors.

`Dockerfile` here removes the dependency instead of mitigating it and also
adds the GitHub CLI (`gh`) from GitHub's official apt repository. The CLI is
intentionally baked into the image before moving the hosted-only issue/PR
automation workflows: those workflow migrations should land only after this
tag has been published and rolled out to the runner pool.
`.github/scripts/ci-install-tool.sh` already no-ops when a tool is present, so
**no workflow change is needed** — the moment pods run this image, every
`ci-install-tool.sh` call site takes the zero-network path.

### Build and push

CI does this part (#7289): dispatch **CI Runner Image**
(`.github/workflows/ci-runner-image.yml`) from `v5`. It builds the
Dockerfile on a GitHub-hosted runner — deliberately *not* on the self-hosted
cluster, whose egress is the problem the image exists to remove — and pushes
with the job's own `GITHUB_TOKEN`, so no personal GHCR write access is needed.

```console
gh workflow run ci-runner-image.yml --ref v5 \
  -f runner_base_image=summerwind/actions-runner:v2.337.0-ubuntu-24.04 \
  -f tag_suffix=toolchain-gh-1
gh run watch   # the job summary prints the pushed tag, its digest, and the apply commands
```

The pushed tag is `<runner version>-<tag_suffix>`, e.g.
`ghcr.io/hivecommons/hive-ci-runner:v2.337.0-ubuntu-24.04-toolchain-gh-1`, and
the workflow refuses to overwrite a tag that already exists — bump
`tag_suffix` for every rebuild on the same base. The same workflow also runs
on any PR that touches the Dockerfile, build-only, as the gate that keeps a
Dockerfile change from shipping an image that quietly sends CI back to the
network or lacks the GitHub CLI: the build verifies `gcc --version`, `tmux -V`
and `gh --version` itself, so a stale base image or a dead mirror fails there.

**First push only:** GHCR creates the `hive-ci-runner` package **private**,
and ARC pulls images anonymously (the RunnerDeployment carries no
`imagePullSecrets`). Make the package public in the org's package settings —
as `hive`, `hive-hub` and `hive-contributor` already are — before applying the
patch, or every runner pod will sit in `ImagePullBackOff`.

Use an immutable tag or a digest. ARC does not re-pull an unchanged tag, so
`:latest` makes "which toolchain are the runners on?" unanswerable.

If the deployed runner version has moved past the `RUNNER_BASE_IMAGE` default,
pass the current one as `runner_base_image` — and re-read the suite caveat
below if the Ubuntu **release** changed. The tag derives from that input, so
it always records which runner release the image was built on.

The workflow is a convenience, not a requirement. The same build works from a
laptop with `docker` and GHCR write access:

```console
cd "$(git rev-parse --show-toplevel)"
docker build -f src/deploy/ci-runners/Dockerfile \
  --build-arg RUNNER_BASE_IMAGE=summerwind/actions-runner:v2.337.0-ubuntu-24.04 \
  -t ghcr.io/hivecommons/hive-ci-runner:v2.337.0-ubuntu-24.04-toolchain-gh-1 \
  src/deploy/ci-runners
docker push ghcr.io/hivecommons/hive-ci-runner:v2.337.0-ubuntu-24.04-toolchain-gh-1
```

### Apply

This is the step CI cannot do — it needs `vllm-d` cluster access. Nothing in
this repository can perform it, and until it is done the runners keep the old
image and CI keeps hitting the mirrors (#7398).

`runner-image-patch.yaml` already names the next rollout tag
`ghcr.io/hivecommons/hive-ci-runner:v2.337.0-ubuntu-24.04-toolchain-gh-1`, so the
patch applies as-is after the workflow publishes that tag; if you pushed a
newer tag, set it there first. Then:

```console
kubectl -n arc-systems patch runnerdeployment hivecommons-hive-runners \
  --type merge --patch-file src/deploy/ci-runners/runner-image-patch.yaml
```

Rolls all runner pods — apply while runners are idle.

### Verification

On a fresh pod from the new generation, as the runner user:

```console
$ gcc --version | head -1   # no sudo, no apt — already present
$ tmux -V
$ gh --version | head -1
```

Then on the next `v2 Tests` run, **Prepare cgo toolchain for race tests**
should report the tool already present and perform no apt network operations,
and no `hive-cgo-race-apt-*` cache restore should be needed for a race shard to
pass. After the pool reports the new `toolchain-gh-*` tag, the hosted-only
workflows that invoke `gh` can be moved to the same fork-safe runner expression
used by the rest of CI.

### Rollback

```console
kubectl -n arc-systems patch runnerdeployment hivecommons-hive-runners \
  --type json -p '[{"op":"remove","path":"/spec/template/spec/image"}]'
```

CI keeps working: `ci-install-tool.sh` falls back to installing over the
network, with the failure mode it has always had. Nothing in `.github/`
requires this image to exist.

## Caveats

- `ubuntu.sources` pins the **noble** suites, matching the current runner
  image. If the runner image is upgraded to a new Ubuntu release, the suite
  names in the ConfigMap must be updated in the same change or `apt-get update`
  will fail again — with a different error, so re-read this file before
  assuming a recurrence of #6648. The same suites are written into
  `Dockerfile` (it needs them at build time, when no ConfigMap is mounted);
  `TestCIRunnerDockerfileAptConfigMatchesConfigMap` fails if the two drift.
- The cluster's admission policy (`require-ownership-labels`) rejects resources
  with no `owner` label. The ConfigMap carries `owner: andy` to match its peers.
- While #6648 was open, the `HIVE_RUNNER_LABELS` repository variable was
  temporarily set to `["ubuntu-latest"]` to divert jobs to GitHub-hosted
  runners. It has since been restored to `["self-hosted","hive"]`. If jobs are
  unexpectedly running on GitHub-hosted runners, check that variable first.

## Shared Go cache volume: bounding growth (#9338 follow-up)

The v2 ARC scale set (`autoscalingrunnerset/hive-runners`, namespace
`arc-v2-hive`) mounts PVC `hive-runner-gocache` (cephfs RWX) at `/mnt/gocache`
in every runner pod; `GOCACHE=/mnt/gocache/build`, `GOMODCACHE=/mnt/gocache/mod`.
Nothing evicted from it, so it filled 200Gi in ~3.5 days and every Go step on
every runner failed with `disk quota exceeded` (2026-09-28, ~03:00Z–12:13Z).

Two controls now exist:

| Control | Where | When it acts |
| --- | --- | --- |
| `go-cache-guard.sh` | `.github/scripts/`, run after every `setup-go` | Only once a job finds the cache unwritable: prunes files untouched >12h, else falls back to `$RUNNER_TEMP`. Safety net. |
| `hive-gocache-prune` CronJob | `gocache-prune-cronjob.yaml` | Daily 03:41 UTC: deletes `build/` files untouched >3d; if usage is still ≥85% tightens to >12h. Steady state. |

Apply / verify:

```sh
kubectl -n arc-v2-hive apply -f src/deploy/ci-runners/gocache-prune-cronjob.yaml
kubectl -n arc-v2-hive create job --from=cronjob/hive-gocache-prune gocache-prune-now
kubectl -n arc-v2-hive logs job/gocache-prune-now
```

The PVC was grown 200Gi → 500Gi on 2026-09-28 (`allowVolumeExpansion: true` on
`ocs-storagecluster-cephfs`; online, no pod restart). If usage still trends up
week over week, lower `MAX_AGE_DAYS` before growing the volume again.

## Shared Go build cache: per-job isolation (2026-09-29)

Bounding the volume's size (above) did not stop Go jobs failing on it. On
2026-09-29, with the PVC at ~238G of 500G (far from full), a sample of 371
failed self-hosted jobs showed:

| Symptom | Jobs | Where |
| --- | --- | --- |
| `open /mnt/gocache/build/<xx>/<id>-a: permission denied` | 73 | every runner node, 766 distinct cache files |
| `can't find export data (bufio: buffer full)` | ~22 | every runner node |
| golangci-lint `no go files to analyze` | 10 | follows from the two above (package load fails) |
| `write /mnt/gocache/build/...: no space left on device` | 5 | while the volume had ~300G free |

The `-a` error is Go failing to *reopen an existing index entry for write*
(`os.OpenFile(O_WRONLY|O_CREATE)`), and the export-data error is a torn
build output. Neither can be seen by `go-cache-guard.sh`'s writability probe,
because the directory itself stays writable. Both come from up to
`maxRunners` pods on several nodes writing one cephfs directory tree at once;
Go only promises safe concurrent cache use on a local filesystem.

`go-cache-guard.sh` now takes `GO_CACHE_GUARD_BUILD_CACHE`:

| Value | Effect |
| --- | --- |
| `job` | GOCACHE under `/mnt/gocache` is moved to `$RUNNER_TEMP/go-cache-guard/gocache` for the job. GOMODCACHE stays shared. |
| `shared` | Previous behaviour: shared GOCACHE, pruned / fallen back only when unwritable. |

Every workflow that runs the guard sets it from the repository variable
`HIVE_GO_BUILD_CACHE`, defaulting to `job`. To go back to the shared build
cache without a code change:

```sh
gh variable set HIVE_GO_BUILD_CACHE --repo hivecommons/hive --body shared
```

Cost: each job compiles cold (~2 minutes for a `-race` shard, the same cost
GitHub-hosted runners pay without a warm-cache hit) and holds its build cache
in the pod's `runner-home` emptyDir (node disk, gone with the pod). A GOCACHE
outside `/mnt/gocache` (GitHub-hosted runners) is never moved, so their
Actions-cache warm restore keeps working. With `job` as the default the
`build/` tree on the PVC stops growing; `hive-gocache-prune` ages it out.

## Classifying infra failures, rerunning once, and alerting on the rate (#9664)

The failures above were found by hand, after hours of reruns, and misled the
fleet into "fixing" code. Three pieces now catch them automatically:

| Piece | Where | What it does |
| --- | --- | --- |
| Classifier | `.github/scripts/ci_infra_classify.py` + `ci-infra-signatures.tsv` | Sorts one failed job into `infra:<class>`, `derived` (a shard gate that only reports other jobs) or `code`, from the failing step's output and the job's annotations. Anything unrecognised is `code`. |
| Rerun once | `.github/workflows/ci-infra-rerun.yml` | On every failed run of the watched CI workflows, classifies the failed jobs and reruns them once when all are infra. Never past attempt 1, never when any job is `code`, never for a fork. |
| Runner canary | `.github/workflows/ci-runner-canary.yml` (every 20 min) | Four parallel self-hosted jobs run setup-go/setup-node, verify tool-cache integrity, dind and free disk, while a GitHub-hosted watchdog fails if they are not picked up within 10 minutes or any run has been queued over 15 minutes. One `ci-runner-canary` issue is opened/updated on red and closed on the next green. |
| Rate alert | `.github/workflows/ci-infra-rate.yml` (hourly) | Share of the last 100 completed CI runs with at least one infra failure. At 15% or more it opens or updates one tracking issue with a class x runner breakdown; below that it closes it. |

Classes shipped: `gocache-permission`, `build-cache-corrupt`, `disk-full`,
`lint-no-go-files`, `lint-timeout`, `runner-lost` (the runner pod died; seen
only as the job annotation "The self-hosted runner lost communication with the
server"), `test-list-empty` (a `go test -list` step that exited without
printing anything) and `toolcache-clobbered` (setup-go/setup-node on a fresh
node whose shared tool cache was overwritten mid-extraction, #10234). To add one, append a row to `ci-infra-signatures.tsv` and a
trimmed real log under `.github/scripts/testdata/ci-infra/`; the self-test
(`python3 .github/scripts/test-ci-infra.py`, run in v2 CI) fails if a class has
no fixture.

Repository variables, all optional:

| Variable | Default | Effect |
| --- | --- | --- |
| `HIVE_CI_INFRA_RERUN` | `on` | `off` keeps classifying (the rate still counts) but never reruns. |
| `HIVE_CI_INFRA_ALERT` | on | `off` disables the rate workflow. |
| `HIVE_CI_INFRA_WINDOW_RUNS` | `100` | Completed runs in the window. |
| `HIVE_CI_INFRA_ALERT_THRESHOLD` | `0.15` | Alert fraction, 0-1, inclusive. |
| `HIVE_CI_INFRA_MIN_RUNS` | `20` | Fewer runs than this: no open or close. |

A run counts as infra-hit even when the automatic rerun turned it green: the
rate measures the pool, not the final verdict. Runner pods are ephemeral and
the Actions API does not expose the Kubernetes node, so the breakdown groups
pods by scale set; map a pod to its node with `kubectl` while it is alive.
