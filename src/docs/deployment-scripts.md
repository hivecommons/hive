# Deployment helper scripts

Hive has **two supported standalone runtimes** — Docker Compose, which is the
default, and Podman under systemd via Quadlet, which is a parallel supported
choice — plus Kubernetes. See the [README's Quick Start](../../README.md#quick-start)
for the pair, and [ADR-0017](adr/0017-podman-quadlet-lifecycle.md) / [the Quadlet operator guide](podman-standalone-quadlet.md)
for the Podman path.

**Every helper script on this page is Docker-only.** They were written for the
Compose and LXC installs that predate the Podman runtime, none of them shells
out to `podman`, and none of them reads `HIVE_DEPLOY_RUNTIME` — the runtime
selector, which `bin/hive-standalone-runtime.sh` and the Podman preflights
honour. Running one against a Quadlet deployment does not fail cleanly with "not
supported": it looks for Docker containers, networks, and volumes that a Quadlet
install does not have.

That is a statement of current scope, not a deprecation. For the Docker installs
these scripts were written for they remain the supported path.

| Script | Runtime | If you are on Podman |
| --- | --- | --- |
| [`bin/hive-setup.sh`](#all-in-one-lxc-setup) | Docker only | [README Quick Start (Podman)](../../README.md#quick-start-podman) |
| [`deploy/create-lxc.sh`](#proxmox-lxc-helpers) | Docker only | no LXC equivalent; see below |
| [`deploy/bootstrap-lxc.sh`](#proxmox-lxc-helpers) | Docker only | no LXC equivalent; see below |
| [`deploy/blue-green-deploy.sh`](#blue-green-compose-deploy) | Docker only | [digest pin and rollback](podman-quadlet-update-rollback.md) |

## All-in-one LXC setup

`bin/hive-setup.sh --repo org/repo` does everything `bootstrap-lxc.sh` does and
also generates a project-specific `hive.yaml` and `.env` for a chosen ACMM
level. It is the fastest way from a fresh Ubuntu 24.04 LXC to a running hive.

**Docker-only.** It installs Docker Engine and the Compose plugin, builds or
pulls the image, and drives the stack with `docker compose`. It predates
`HIVE_DEPLOY_RUNTIME` (#4205) and has no concept that a runtime choice exists,
so setting that variable does not redirect it — the `--compose FILE` flag and
its own header ("a Hive instance (Docker-based)") are the honest description
of what it builds.

**On Podman**, install from the [README's Quick Start (Podman)](../../README.md#quick-start-podman),
which walks the preflights and the Quadlet units, and read
[the Quadlet operator guide](podman-standalone-quadlet.md) for what those units
do. To remove such an install again, use
[`bin/hive-podman-teardown.sh`](../../bin/hive-podman-teardown.sh), which selects
resources by the Hive ownership label and is described in
[the ownership and cleanup contract](podman-ownership-cleanup.md).

## Proxmox LXC helpers

`deploy/create-lxc.sh` runs on a Proxmox host. It creates an Ubuntu 24.04 LXC
with Docker-compatible settings (`nesting=1`, `keyctl=1`, unconfined AppArmor, no
capability drop), then starts SSH. Tunable flags include `--ctid`, `--hostname`,
`--password`, `--disk`, `--memory`, and `--cores`.

`deploy/bootstrap-lxc.sh` runs inside that LXC. It installs Docker Engine and the
Compose plugin, clones the `v4` branch (override with `HIVE_BRANCH`) to
`/opt/hive`, builds the Hive image, and writes `/opt/hive/.env` with
placeholders for GitHub, dashboard, model, and notification credentials.

Use this path when you operate Hive on Proxmox or another LXC-first host and want
a lightweight VM-like container that still runs the normal Docker Compose
deployment. Do not use it inside Kubernetes.

**Both are Docker-only.** The container tuning `create-lxc.sh` applies is chosen
for the Docker daemon, and `bootstrap-lxc.sh` installs Docker Engine
unconditionally; neither consults `HIVE_DEPLOY_RUNTIME`.

**On Podman there is no LXC equivalent** — nothing here creates or bootstraps a
container host for the Quadlet stack, and running rootless Podman inside an LXC
carries its own subordinate-ID and storage requirements that these scripts do not
set up. Install on the host directly, following the
[README's Quick Start (Podman)](../../README.md#quick-start-podman) and
[the Quadlet operator guide](podman-standalone-quadlet.md); the
[subordinate-ID, graphroot, and networking preflight](podman-preflight-ids.md)
is the check that reports whether a given host — LXC or not — can actually run it.

## Blue-green Compose deploy

`deploy/blue-green-deploy.sh [--skip-build]` is a zero/low-downtime Compose
upgrade helper. It builds a fresh `hive` image unless `--skip-build` is set,
starts it as `hive-next` on the existing Docker network and volumes, waits for
`/api/health`, then stops the old `hive`, renames `hive-next` to `hive`, and
reloads `hive-gateway` nginx.

Use it for production Docker Compose installs where an in-place
`docker compose up -d` restart would be too disruptive. Preconditions: existing
`hive` and `hive-gateway` containers, a `/data` volume, mounted `hive.yaml`,
mounted `/secrets`, Docker Compose, and enough disk for a second image/container
during the cutover.

**Docker-only, and there is no Podman equivalent.** Not "not yet" — the Quadlet
stack solves the same problem by a different technique, so a port of this script
is not what a Podman operator is waiting for. Under Quadlet the units set
`Notify=healthy`, which means `systemctl start` returns only once Hive has
answered its health check rather than merely once a process was spawned, so the
"start the new one, prove it healthy, then cut over" step this script builds by
hand is what starting the unit already does. What blue-green additionally buys —
a way back when the new image is bad — is
[the digest pin and rollback](podman-quadlet-update-rollback.md): the image is
pinned by manifest-list digest in a drop-in, the previous digest is recorded, and
reverting is restoring the earlier pin. That path is measured end to end, in both
root modes, including the failure case ([#4378](https://github.com/hivecommons/hive/issues/4378),
[#4411](https://github.com/hivecommons/hive/issues/4411)).

The two are not the same technique and they trade differently: blue-green keeps
the old container running until the new one is proven, and costs a second image
and container for the length of the cutover; the Quadlet path replaces in place
and costs one `TimeoutStartSec` of downtime when an update turns out bad. Neither
is a drop-in substitute for the other.

Fixes #2939.
