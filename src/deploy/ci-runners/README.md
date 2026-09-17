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

`Dockerfile` here removes the dependency instead of mitigating it.
`.github/scripts/ci-install-tool.sh` already no-ops when a tool is present, so
**no workflow change is needed** — the moment pods run this image, every
`ci-install-tool.sh` call site takes the zero-network path.

### Build and push

CI does this part (#7289): dispatch **CI Runner Image**
(`.github/workflows/ci-runner-image.yml`) from `v4`. It builds the
Dockerfile on a GitHub-hosted runner — deliberately *not* on the self-hosted
cluster, whose egress is the problem the image exists to remove — and pushes
with the job's own `GITHUB_TOKEN`, so no personal GHCR write access is needed.

```console
gh workflow run ci-runner-image.yml --ref v4 \
  -f runner_base_image=summerwind/actions-runner:v2.337.0-ubuntu-24.04 \
  -f tag_suffix=toolchain-1
gh run watch   # the job summary prints the pushed tag, its digest, and the apply commands
```

The pushed tag is `<runner version>-<tag_suffix>`, e.g.
`ghcr.io/hivecommons/hive-ci-runner:v2.337.0-ubuntu-24.04-toolchain-1`, and
the workflow refuses to overwrite a tag that already exists — bump
`tag_suffix` for every rebuild on the same base. The same workflow also runs
on any PR that touches the Dockerfile, build-only, as the gate that keeps a
Dockerfile change from shipping an image that quietly sends CI back to the
network: the build verifies `gcc --version` and `tmux -V` itself, so a stale
base image or a dead mirror fails there.

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
  -t ghcr.io/hivecommons/hive-ci-runner:v2.337.0-ubuntu-24.04-toolchain-1 \
  src/deploy/ci-runners
docker push ghcr.io/hivecommons/hive-ci-runner:v2.337.0-ubuntu-24.04-toolchain-1
```

### Apply

This is the step CI cannot do — it needs `vllm-d` cluster access. Nothing in
this repository can perform it, and until it is done the runners keep the old
image and CI keeps hitting the mirrors (#7398).

`runner-image-patch.yaml` already names the published tag
`ghcr.io/hivecommons/hive-ci-runner:v2.337.0-ubuntu-24.04-toolchain-1`, so the
patch applies as-is; if you pushed a newer tag, set it there first. Then:

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
```

Then on the next `v2 Tests` run, **Prepare cgo toolchain for race tests**
should report the tool already present and perform no apt network operations,
and no `hive-cgo-race-apt-*` cache restore should be needed for a race shard to
pass.

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
