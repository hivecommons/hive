# ADR-0017: Quadlet `.container`/`.pod` units as the Podman persistent lifecycle

Status: Accepted

## Context

Standalone Hive under Podman (#4188) needs one persistent lifecycle mechanism
before anything downstream can be scoped: the Podman Hive service asset, reboot
persistence, stop/start/recreate, and both update paths all assume an answer to
"what actually keeps Hive running across a reboot?"

Three spikes bear on that question, and they were run independently:

- **#4201** — Compose-provider selection. `podman compose` is a thin exec
  wrapper; the provider it picks is not obvious.
- **#4202** — Quadlet `.container`/`.pod` feasibility. Viable, with five
  findings the implementation must handle.
- **#4203** — Quadlet `.kube` reuse of the standalone Kustomize overlay. Not
  viable as a direct source.

They do not actually disagree once read together — #4201 and #4203 both point at
#4202 — but no document had said so, and none of the three states a minimum
Podman version. This ADR records the choice and the version floor it implies.

**Scope.** This record decides the mechanism. It adds no deployment asset, no
unit file, and no preflight check; those are separate slices under #4188 and are
unblocked by this. No Docker configuration is changed, and Docker Engine and
Docker Compose remain the default, fully supported runtime.

## Decision

**Standalone Hive under Podman uses Quadlet units** — `.container`, `.pod`,
`.network`, and `.volume` — read by the Podman systemd generator, rootful from
the system manager's unit search path and rootless from the per-user manager's,
with `loginctl enable-linger` for boot persistence in the rootless case.

The decisive property is readiness, not convenience. `Notify=healthy` generates
`Type=notify`, `NotifyAccess=all`, and `--sdnotify=healthy`, which holds the unit
in `activating` until the Podman healthcheck passes — so "started" means healthy
rather than merely spawned (#4202). Nothing else evaluated supplies that, and
the update and rollback slices are exactly the ones that need it.

### Rejected: `podman compose` as the persistent mechanism — #4201

- **Provider selection is silent and wrong by default.** On a stock Fedora 44
  host with both providers installed, `podman compose` selected
  `/usr/libexec/docker/cli-plugins/docker-compose`, with `podman-compose` present
  on `PATH`. Nothing warned that a Docker-built provider had been chosen for a
  Podman deployment.
- **The Docker Compose plugin needs an always-on Podman API socket** and is
  Docker-packaged software. #4201 measured the socket dependency directly (the
  plugin failed against an absent socket; `podman-compose` did not) and read it
  as failing #4188's requirement that at least one supported path need no Docker
  Engine, daemon, socket, shim, or Compose provider.
- **No readiness gate.** Compose has no equivalent of `Notify=healthy`, and
  #4201's own recommendation is "use Quadlet for persistent installations."

Compose is **not** removed. #4201 keeps it for the development loop, where it
must name its provider explicitly (`PODMAN_COMPOSE_PROVIDER=podman-compose` plus
`[engine] compose_providers` in `containers.conf`) and must not set
`compose_warning_logs = false`, since that banner is currently the only signal
naming the provider that ran.

### Rejected: Quadlet `.kube` replay of the standalone Kustomize overlay — #4203

- **A zero exit status did not mean the topology survived.** Podman 5.8.4
  silently ignored every unsupported kind in the rendered overlay —
  `Namespace`, `ServiceAccount`, `Role`, `RoleBinding`, `Service`, and
  `InferencePool`.
- **Ports and DNS are gone with the `Service` objects.** `containerPort: 3002`
  published nothing, and cluster DNS names had no Podman equivalent. Because the
  YAML holds two `Deployment`s, a single Quadlet `PublishPort=` was applied to
  *both* pod infra containers, so the two pods contend for one host binding.
- **The secret-ownership contract breaks.** Pod-level `fsGroup: 1002` was not
  honored inside the rootless user namespace — the projected file came out
  `root:root` — which is precisely the mechanism that makes Forge App keys
  readable by `hive-launch` while excluding agent UIDs.
- **Readiness is lost.** The readiness probe did not survive into the container
  configuration, and the generated `.kube` service used `Type=notify` without
  waiting on Hive's `/api/health`.
- The gateway does not exist on the Kubernetes path at all.

#4203's smallest safe adaptation set runs to five items and amounts to a second
deployment definition with its own drift surface, not reuse.

### Not evaluated by any spike: `podman generate systemd`, bare `--restart`

Neither was measured, and neither is ruled out by a Hive result — recorded here
so the omission is visible rather than accidental. `podman generate systemd` is
deprecated upstream in favor of Quadlet (Podman 4.7.0 release notes: "The
`podman generate systemd` command is deprecated. Use Quadlet for running
containers and pods under systemd"), which is upstream evidence, not a Hive
measurement. A bare `--restart=always` container carries no boot-persistence
story for rootless, no ordering, and no readiness signal; that is reasoning from
the mechanism, and if anyone wants it on the table it needs its own spike rather
than an argument here.

## Minimum Podman version

The spikes all ran on 5.8.4. That is **a data point, not a floor**, and this
record does not quote it as one.

| | Version | How this is known |
| --- | --- | --- |
| **Measured on** | **5.8.4** | Directly observed — rootful and rootless, Fedora 44, cgroup v2, systemd 259, across #4201/#4202/#4203. |
| **Requires at least** | **5.0.0** | *Derived from upstream release history, never run.* Both features the chosen layout depends on first shipped in 5.0.0. |
| **Recommended floor** | **5.6.0** | Derived the same way: a 5.5.x regression and a 5.5.1 startup fix both land on this exact layout. See below. |
| **Verified floor** | **unknown** | Nothing between 5.0.0 and 5.8.4 has been exercised by any spike. No version below 5.8.4 has been run at all. |

What each requirement comes from, with the release that introduced it:

| Capability the layout depends on | First shipped |
| --- | --- |
| Quadlet generator itself | 4.4.0 |
| `AutoUpdate=` in `.container` units (the update slices) | 4.6.0 |
| `podman run --sdnotify=healthy` | 4.7.0 |
| **`.pod` unit files** | **5.0.0** |
| **`Notify=healthy` in `.container` units** | **5.0.0** |
| Fix: `.pod` units failing to start, storage not mounted | 5.5.1 |
| Fix: `.container` joining a `.pod` generating the wrong `--pod` name | 5.6.0 |

The two bolded rows are the binding constraint: a pod-based layout with a
readiness gate is not expressible below 5.0.0.

The 5.6.0 fix is worth stating precisely rather than treating as a general
"upgrade" argument. Podman
[#26105](https://github.com/containers/podman/issues/26105) generated
`--pod systemd-<container>` instead of `systemd-<pod>-pod`, so the container
could not start — and by the reporter's description it bites only when the
`.pod` unit does **not** set a custom `PodName=`. #4202's probe units set
`PodName=` explicitly, so Hive's layout is outside the regression window; the
implementation slice should set `PodName=` because it avoids this, not by
accident.

Requirements that are not version numbers:

- **cgroup v2 is mandatory.** `podman-systemd.unit(5)` states Quadlet requires
  it, and the generated `ExecStart` carries `--cgroups=split` (#4202). Check
  with `podman info --format '{{.Host.CgroupsVersion}}'`.
- **systemd is not the constraint.** Verified on 259; #4202 found nothing
  version-specific in the generated units beyond `Type=notify`. Rootless boot
  persistence additionally needs `loginctl enable-linger`, which is #4208's
  preflight territory.
- **`podman-compose`**, for the retained development loop only: measured at
  1.6.0, floor **unknown**. `depends_on: {condition: service_healthy}` is the
  field most likely to move it, since #4201 confirmed 1.6.0 implements it but
  did not establish when it arrived.

**What would settle the floor.** One follow-up, not done here: run #4202's
generator probe (`quadlet --dryrun` in both `--user` and system mode) plus one
live `systemctl start` to healthy on hosts at the candidate floor — 5.0.x, and
whatever version each target distribution ships — and record the oldest version
that both generates a correct unit and reaches healthy. Until that exists,
5.0.0 is a derived lower bound and nothing more, and the Podman preflight
(#4207) should *report* the detected version against these numbers rather than
silently assume any of them.

## What would reopen this decision

1. **A live start that does not behave like the generated unit says it will.**
   #4202 was a generator dry-run: `Notify=healthy` was read out of generated
   units, never watched holding a real unit in `activating` against a real Hive
   healthcheck. That property is the entire basis of this choice. If it does not
   hold live, the decision is void rather than amended.
2. **A supported target pinned below Podman 5.0.0.** A distribution shipping
   Podman 4.x that Hive must support forces either a pod-less `.container`-only
   layout with a different readiness story, or a reopen.
3. **The gateway boundary proving inexpressible at the pod level.** #4202's
   finding 3 is that `PublishPort=` on a pod member generates a unit Podman
   rejects at runtime, so publishing 3001 must happen on the `.pod` unit and
   7681 must be published nowhere. If that contract cannot be asserted at the
   pod level, the topology reopens.
4. **The rootless enforcing cell failing its promotion criteria.** The
   [support matrix](../podman-support-matrix.md) graded rootless + enforcing as
   experimental until its three promotion criteria were measured (#4487); the
   cell is now supported. That is a support question rather than a lifecycle
   one, but it bounds what the rootless half of this ADR can promise.
5. **Quadlet auto-update proving unable to carry health-aware rollback**, which
   would push the update slices toward a mechanism this ADR did not choose.
6. **Upstream deprecating Quadlet or changing the generator contract.**

## Consequences

- The service-asset, reboot-persistence, update, and rollback slices under #4188
  are unblocked and should be written against Quadlet units.
- Every `.container` unit must set `TimeoutStartSec` explicitly, larger than
  `HealthStartPeriod` plus the retry budget. #4202 measured a 45-second host
  default against a 120-second start period, and the generated `ExecStart`
  carries `--rm`, so the failure deletes its own evidence.
- Hive's own contract tests have to cover what the generator does not: it exits
  0 on `Notify=healthy` without a `HealthCmd`, on `PublishPort=` in a pod
  member, and on short-name images. `quadlet --dryrun` belongs in CI as a syntax
  and reference gate, but it is not a semantic one.
- Image references must be fully qualified from the #4206 source of truth;
  `podman auto-update` requires it, and the Compose spelling of the gateway
  image is a digest-pinned short name.
- A rootless install means a real user account with units in that user's search
  path, plus linger. `%h` expands to `/root` in a system unit, so unit files are
  not portable across the rootful/rootless boundary unchanged.
- Compose stays for development, with its provider named explicitly. The Docker
  Compose path is untouched and remains the default.

## References

- [Podman Quadlet `.container`/`.pod` feasibility spike](../podman-quadlet-container-pod-spike.md) — #4202.
- [Podman Compose-provider selection spike](../podman-compose-provider-spike.md) — #4201.
- [Podman Quadlet `.kube` compatibility spike](../podman-quadlet-kube-spike.md) — #4203.
- [Podman support matrix](../podman-support-matrix.md) — which rootful/rootless × enforcing/advisory combinations this lifecycle may be installed into.
- [`podman-systemd.unit(5)`](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html)
- [Podman release notes](https://github.com/containers/podman/blob/main/RELEASE_NOTES.md) — the source for every version in the table above.
