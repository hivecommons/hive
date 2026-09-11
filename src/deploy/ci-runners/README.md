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

## Caveats

- `ubuntu.sources` pins the **noble** suites, matching the current runner
  image. If the runner image is upgraded to a new Ubuntu release, the suite
  names in the ConfigMap must be updated in the same change or `apt-get update`
  will fail again — with a different error, so re-read this file before
  assuming a recurrence of #6648.
- The cluster's admission policy (`require-ownership-labels`) rejects resources
  with no `owner` label. The ConfigMap carries `owner: andy` to match its peers.
- While #6648 was open, the `HIVE_RUNNER_LABELS` repository variable was
  temporarily set to `["ubuntu-latest"]` to divert jobs to GitHub-hosted
  runners. It has since been restored to `["self-hosted","hive"]`. If jobs are
  unexpectedly running on GitHub-hosted runners, check that variable first.
