# Podman CI runner map

Which Podman test goes on which runner, and which ones cannot run on ordinary
hosted CI at all. Inventory and recommendation only — no workflow is added
here, and the proposed implementation is split into one small issue per lane at
the end.

## Recommendation

**Everything except SELinux fits on GitHub-hosted runners, on both
architectures.** That is a stronger result than #4188 assumed, and it was
measured rather than inferred: a probe workflow ran on `ubuntu-latest` and
`ubuntu-24.04-arm` and the two came back capability-identical.

| Required coverage | Lane | Runner |
| --- | --- | --- |
| Rootless startup, exit-77, egress gate | hosted | `ubuntu-latest` |
| Rootful startup and egress gate | hosted | `ubuntu-latest` |
| Compose-provider selection | hosted | `ubuntu-latest` |
| Quadlet generator syntax gate | hosted, **after installing the generator** | `ubuntu-latest` |
| Image build/pull + service startup, `arm64` | hosted | `ubuntu-24.04-arm` |
| Preflight checks (#4207–#4209) | hosted | `ubuntu-latest` |
| SELinux-enforcing mounts, labels, secrets | **not hosted** | self-hosted or release-qualification VM |

Only the last row needs infrastructure the project does not already have.

## Measured runner capabilities

A probe workflow ran both runners and printed what Podman actually has to work
with. Identical unless noted.

| | `ubuntu-latest` (amd64) | `ubuntu-24.04-arm` (arm64) |
| --- | --- | --- |
| Image | `ubuntu-24.04` 20260816.277.1, kernel 6.17.0-1022-azure | same, `aarch64` |
| Podman | **5.8.4 preinstalled** at `/usr/local/bin/podman` | same |
| Buildah / Skopeo | 1.33.7 / 1.13.3 | same |
| cgroups | v2 | v2 |
| Rootless stack | netavark + pasta, crun, overlay | same |
| `/etc/subuid`, `/etc/subgid` | `runner:165536:65536` | same |
| `max_user_namespaces` | 63786 | 63642 |
| `ip_unprivileged_port_start` | 1024 | 1024 |
| Passwordless `sudo` | **yes** | yes |
| SELinux | **absent** — no `getenforce`, no `selinuxfs` | absent |
| AppArmor | enabled | enabled |
| Quadlet generator | **absent** | absent |
| User manager `DefaultTimeoutStartUSec` | 90s | 90s |
| `Linger` for the runner user | `yes` | `yes` |

Behavioral probes, both architectures:

```
rootless run                                   rootless-run-OK
rootless + --cap-add NET_ADMIN, iptables nat   NAT_CHAIN_OK
rootful (sudo podman run)                      rootful-run-OK
nested user namespaces inside a container      2147483647
```

Four of those matter more than the rest.

**`NAT_CHAIN_OK` under `--cap-add NET_ADMIN`.** Creating a nat chain in the
container's own network namespace is the exact prerequisite for Hive's
forced-proxy egress redirect. It works on hosted runners, on both
architectures, which means the enforcing rootless lane — the one #4188 treated
as the blocking design gate — is testable in ordinary CI. This reproduces the
local result recorded in the rootless startup spike
(`src/docs/podman-rootless-startup-spike.md`, #4199 / PR #4280, not yet
merged).

**Passwordless `sudo` and a working `sudo podman run`.** The rootful lane needs
no self-hosted machine either.

**Subordinate IDs are present and are exactly 65536**, the minimum Podman
expects, so the subordinate-ID preflight (#4208) has something real to pass
against rather than being skipped.

**The Quadlet generator is absent even though Podman is present.** The runner
image ships a hand-installed `/usr/local/bin/podman` rather than the
distribution package, so `/usr/libexec/podman/quadlet` does not exist. A
Quadlet syntax gate that does not install the generator first will silently
test nothing. See the lane below.

Two smaller cross-checks worth keeping: the user manager's
`DefaultTimeoutStartUSec` is 90s here and 45s on the Fedora host used for
[the Quadlet spike](podman-quadlet-container-pod-spike.md), which confirms that
the startup-timeout default is host-specific and must be set explicitly rather
than assumed. And nested user namespaces are effectively unlimited, so a
rootless-Podman-inside-a-container arrangement is not blocked if a lane ever
needs one.

## Lanes

### 1. Rootless, hosted — the primary lane

Everything in the rootless matrix runs here: default startup and exit 77,
`--cap-add NET_ADMIN` with the redirect installed, deliberate advisory mode,
bypass resistance, the preflight checks, and Compose-provider selection.
The probe script proposed in #4199 (PR #4280,
`src/deploy/probe_podman_rootless_netadmin.sh`) already runs this matrix and
exits non-zero when a case stops matching, so once it lands the lane is mostly
a workflow wrapper around a script that exists. If it does not land, this lane
grows that script itself.

Podman is preinstalled, so the lane needs no setup step — but it should
**assert** the version it found rather than trusting it, because a runner-image
change is silent and would otherwise turn a real regression into a skipped
test.

### 2. Rootful, hosted

`sudo -n` works and `sudo podman run` works. The rootful egress-gate baseline
(#4200) belongs here.

The safety boundary matters more than the mechanics: a rootful lane runs
containers as real root on the runner VM. That is acceptable **only** because
hosted runners are ephemeral and single-use. It must never be combined with
`pull_request_target`, and it must not have access to secrets — see below.

### 3. `arm64`, hosted

`ubuntu-24.04-arm` is already used by `.github/workflows/docker.yml` for the
multi-architecture image build, so the runner label is proven in this
repository. The probe shows it is capability-identical to amd64 for Podman.

#4188 asks for image build/pull plus service startup on `arm64`, not the whole
matrix. Keep it that way: a full duplicate matrix doubles cost for coverage
that the identical-capability result says will not diverge. If the two ever do
diverge, that is itself the signal to widen the lane.

### 4. SELinux-enforcing — the only lane that needs new infrastructure

Hosted runners have no SELinux at all: no `getenforce`, no `/sys/fs/selinux`,
AppArmor instead. There is no way to make an Ubuntu hosted runner enforce
SELinux, so `:z`/`:Z` relabeling, mount labels, and secret access under
enforcement cannot be covered by hosted CI. This is exactly the case #4188
anticipated when it allowed "a documented, repeatable release-qualification
job" as an alternative to CI.

Two options, in order of preference:

1. **Release-qualification VM.** A documented, repeatable job on a
   Fedora/CentOS Stream/RHEL image, run before a release rather than per PR.
   Cheapest, no persistent infrastructure, no untrusted-code exposure. The
   result is recorded against the release.
2. **Ephemeral self-hosted runner.** Per-PR coverage, at the cost of running
   a machine. Only worth it if SELinux regressions turn out to be frequent.

Do not reach for a third option that looks tempting and is not equivalent:
running a Fedora *container* on an Ubuntu runner does not give you SELinux
enforcement, because the host kernel is what enforces, and that kernel has no
SELinux at all.

### 5. Quadlet generator gate

Small and hosted, but it needs its own issue precisely because of the
generator-absent finding. The lane must install the generator (the distribution
`podman` package, or the generator binary alone) and then **verify it is
present** before running `quadlet --dryrun`, so an environment change cannot
turn the gate into a no-op.

What it should gate is recorded in
[the Quadlet spike](podman-quadlet-container-pod-spike.md): `--dryrun` catches
unsupported keys, dangling unit references, and missing images, but not
`Notify=healthy` without a healthcheck, `PublishPort=` on a pod member, or a
short-name image. Those three need Hive's own assertions on top.

### 6. The existing Podman workflow does not run

`.github/workflows/podman-contract.yml` triggers only on `push` and
`pull_request` against branch **`v2`**. Mainline development is on `v4`, so
this check has effectively stopped running. It is a static grep gate over the
`Justfile`, so nothing failed loudly — it just went quiet.

That is a one-line fix and it should not be bundled into a new lane.

## Permissions and safety boundaries

Applies to every lane above.

- **`permissions: contents: read`.** No lane needs more. The current Podman
  workflow already does this and the new ones should copy it.
- **No secrets, and therefore no `pull_request_target`.** Fork PRs get no
  secrets, so any lane that needs a secret cannot cover contributor PRs — which
  is most of the value. Live tests must work against a locally built image or a
  public pull. `pull_request_target` runs untrusted code with repository
  credentials and must not appear in any of these lanes, least of all the
  rootful one.
- **Never mount an engine socket** into any test container. #4188 forbids it
  for deployment; it is equally wrong in CI, where the socket is the runner's.
- **Cleanup uses the ownership contract**, not pruning. Hosted runners are
  ephemeral so cleanup barely matters there, but a self-hosted SELinux runner
  is persistent and shares one store across jobs. `podman system prune` on that
  machine destroys concurrent jobs. Use the label selectors in
  [the cleanup contract](podman-ownership-cleanup.md).
- **Isolated storage for anything that pulls.** The #4199 probe builds a
  throwaway store with a private graphroot and runroot; a self-hosted lane
  should do the same so parallel jobs cannot collide.
- **A self-hosted runner must be ephemeral and must not run fork PRs.**
  Untrusted code plus a persistent machine plus `sudo` is the standard
  self-hosted-runner compromise. Restrict it to `workflow_dispatch` and pushes
  to protected branches.
- **Assert the environment, do not assume it.** Every lane should print and
  check the Podman version, root mode, and — where relevant — SELinux state, so
  a runner-image change shows up as a failure rather than as silently reduced
  coverage.

## Proposed issues, one per lane

Deliberately not opened here; a maintainer can file them as-is. Each is
`Part of #4188` and none should close it.

1. **CI: rootless Podman lane on hosted amd64.** Wrap the #4199 probe script
   in a workflow on `ubuntu-latest`. Assert the discovered Podman version and root mode. AC: the
   three-case matrix runs per PR; a gate that fails to install fails the job.
2. **CI: rootful Podman lane on hosted amd64.** `sudo podman`, the #4200
   baseline. AC: no secrets, no `pull_request_target`, rootful mode asserted.
3. **CI: `arm64` build/pull and startup lane.** `ubuntu-24.04-arm`, scoped to
   image build/pull plus service startup. AC: no duplicate full matrix.
4. **Release qualification: SELinux-enforcing Podman.** A documented,
   repeatable Fedora/CentOS Stream job covering `:z`/`:Z` mounts, labels, and
   secret access. AC: reproducible from the doc; result recorded per release;
   explicitly not a hosted lane, with the reason.
5. **CI: Quadlet generator syntax gate.** Install the generator, verify it is
   present, then `quadlet --dryrun` for `--user` and rootful. AC: the job fails
   if the generator is missing rather than passing vacuously.
6. **Fix `podman-contract.yml` branch triggers.** It targets `v2` and no longer
   runs. AC: triggers on the current mainline branch.

## Reproducing the probe

The capability numbers above came from a throwaway workflow on a fork, run once
on each runner label and then deleted. It printed OS and kernel, the presence
and version of `podman`/`buildah`/`skopeo`, cgroup version, SELinux and
AppArmor state, `max_user_namespaces`, subordinate IDs,
`ip_unprivileged_port_start`, whether `sudo -n` works, `podman info` for both
root modes, a rootless run, a rootless `--cap-add NET_ADMIN` run creating a nat
chain, a rootful run, and whether the Quadlet generator and user-systemd
lingering are available.

Re-run it when the runner image changes materially; the numbers in this
document are a snapshot of image `20260816.277.1`, not a contract.
