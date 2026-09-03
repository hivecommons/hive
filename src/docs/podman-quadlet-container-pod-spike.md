# Podman Quadlet `.container` and `.pod` feasibility spike

## Recommendation

Explicit Quadlet `.container`, `.pod`, `.network`, and `.volume` units are a
**viable** persistent-installation model for standalone Hive, and are the
recommended Podman lifecycle target over `podman compose`.

The decisive result is readiness. `Notify=healthy` makes systemd hold the unit
in `activating` until the Podman healthcheck passes, so a started unit means a
healthy Hive rather than a started process. That is the property #4188 Phase 3
requires before Podman auto-update can be trusted, and Compose cannot supply
it.

Adopting Quadlet is not free. Five findings below have to be handled
explicitly by the follow-on implementation issue, and three of them are silent:
the generator emits a valid unit, exits 0, and the failure only appears at
runtime. The most urgent is the startup timeout: **the default configuration of
the obvious unit deadlocks against its own health start period.**

This is a feasibility result only. It adds no deployment asset and changes no
existing runtime. The probe units quoted here are throwaway reproduction
material, not a Hive Quadlet stack.

## Probe

Rootless Podman 5.8.4 with Quadlet generator 5.8.4, systemd 259, cgroup v2, on
Fedora 44.

The probe wrote four temporary unit files to a scratch directory and ran the
Podman systemd generator against it in both modes, then split the generated
units out and verified them:

```bash
QUADLET_UNIT_DIRS=/tmp/probe/units /usr/libexec/podman/quadlet --user --dryrun
QUADLET_UNIT_DIRS=/tmp/probe/units /usr/libexec/podman/quadlet --dryrun
systemd-analyze --user verify /tmp/probe/generated/*.service
```

`--dryrun` prints the generated units to stdout and touches nothing. No image
was pulled, no container, pod, network, or volume was created, and no unit was
installed into a systemd search path. `systemd-analyze --user verify` returned
clean with no warnings for all four generated units.

The probe modelled one Hive service — the `hive` container from
`src/docker-compose.yaml` — plus the pod, network, and volume it needs. It
deliberately did not translate the gateway, the Watchtower profile, or the
secrets mount.

## Recorded assumptions

| Assumption | Value | Source |
| --- | --- | --- |
| cgroup version | v2 **required** | `podman-systemd.unit(5)`: "Quadlet requires the use of cgroup v2". The generated `ExecStart` carries `--cgroups=split`, which is a cgroup-v2 mechanism. |
| Podman | verified on 5.8.4 | `.pod` units, the `Pod=` key, and `Notify=healthy` are all later additions than the base `.container` support and are absent from early Podman 4 releases. The implementation issue must pin a tested minimum rather than inherit this host's version. |
| systemd | any version providing `Type=notify` | Verified on 259; nothing in the generated units is version-specific. |
| Generator path | `/usr/libexec/podman/quadlet`, symlinked as `podman-system-generator` and `podman-user-generator` | Both `/usr/lib/systemd/system-generators/` and `/usr/lib/systemd/user-generators/` point at the same binary; mode is chosen with `--user`. |

Check the cgroup assumption with `podman info --format '{{.Host.CgroupsVersion}}'`.

## Unit locations and systemd mode

| Mode | systemd manager | Unit search path (highest precedence first) |
| --- | --- | --- |
| Rootful | system manager | `/run/containers/systemd/` (temporary/testing), `/etc/containers/systemd/` (administrator), `/usr/share/containers/systemd/` (distribution) |
| Rootless | per-user manager (`systemctl --user`) | `$XDG_RUNTIME_DIR/containers/systemd/`, `$XDG_CONFIG_HOME/containers/systemd/` (usually `~/.config/containers/systemd/`), `/etc/containers/systemd/users/$(UID)`, `/etc/containers/systemd/users/` |

Two consequences for a standalone install:

- **Rootless units are not root-installable in the usual place.** Quadlet units
  cannot run as another user through `User=`/`Group=`/`DynamicUser=`; a
  rootless Hive means a real user account with unit files in that user's search
  path. Boot persistence then needs `loginctl enable-linger`, which is #4208's
  preflight territory.
- The generated units differ between modes in exactly one respect. Rootful
  units order against `network-online.target`; rootless units order against
  `podman-user-wait-network-online.service`. Everything else — `ExecStart`,
  dependencies, `Type` — was byte-identical in the diff.

`%h` is a trap across that boundary: it expands to the invoking user's home in
a user unit and to `/root` in a system unit, so a `%h`-based bind mount silently
points somewhere else if the same file is installed rootful.

## What the generator produced

The probe `.container` unit and the relevant part of its generated service:

```ini
# probe unit
[Container]
Image=ghcr.io/hivecommons/hive:v2-latest
ContainerName=hive
Pod=hive.pod
AddCapability=NET_ADMIN
Volume=hive-data.volume:/data
Volume=%h/hive/hive.yaml:/etc/hive/hive.yaml:ro,Z
Environment=HIVE_LEVEL=2
HealthCmd=curl -sf http://127.0.0.1:3002/api/health
HealthStartPeriod=120s
Notify=healthy
```

```ini
# generated hive-app.service
[Unit]
Requires=hive-data-volume.service
After=hive-data-volume.service
BindsTo=hive-pod.service
After=hive-pod.service

[Service]
Delegate=yes
Type=notify
NotifyAccess=all
KillMode=mixed
ExecStart=/usr/bin/podman run --name hive --replace --rm --cgroups=split \
  --sdnotify=healthy -d --cap-add net_admin -v hive-data:/data \
  -v %h/hive/hive.yaml:/etc/hive/hive.yaml:ro,Z --env HIVE_LEVEL=2 \
  --health-cmd "curl\x20-sf\x20http://127.0.0.1:3002/api/health" \
  --health-start-period 120s ... --pod hive ghcr.io/hivecommons/hive:v2-latest
ExecStop=/usr/bin/podman rm -v -f -i hive
```

Against the five things this spike had to cover:

- **Image.** `Image=` maps straight to the `podman run` argument. Digest pinning
  works. See the short-name finding below.
- **Environment.** `Environment=` becomes `--env`. Note that this is the
  *container's* environment; a `[Service] Environment=` line in the same file
  goes to systemd instead, and the two are easy to confuse.
- **One mount.** Both mount styles worked. `Volume=hive-data.volume:/data`
  resolved the `.volume` unit to the volume name and added
  `Requires=`/`After=hive-data-volume.service`. A host bind mount kept its
  `:ro,Z` SELinux relabel suffix unchanged.
- **Readiness type.** `Notify=healthy` produced `Type=notify`,
  `NotifyAccess=all`, and `--sdnotify=healthy`. The unit stays in `activating`
  until the healthcheck reports healthy. `.pod` units get `Type=forking`,
  `.volume` and `.network` units get `Type=oneshot`.
- **Startup timeout.** See below. This is the finding that matters most.

The `.pod`, `.network`, and `.volume` units generated cleanly too. The volume
unit is worth quoting because it shows the #4210 ownership labels surviving the
generator intact:

```ini
ExecStart=/usr/bin/podman volume create --ignore --label io.kubestellar.hive.owned=true hive-data
```

## Findings

### 1. The default startup timeout is shorter than the health start period

Quadlet emits **no** `TimeoutStartSec` for `.container` units, so the unit
inherits the manager default. `podman-systemd.unit(5)` names 90 seconds, but
that is not universal — this host reports:

```
$ systemctl show -p DefaultTimeoutStartUSec --value
45s
$ systemctl --user show -p DefaultTimeoutStartUSec --value
45s
```

The probe's `HealthStartPeriod=120s` mirrors the existing Compose healthcheck
`start_period: 120s`. Combined with `Notify=healthy` and a 45-second default,
systemd kills the unit 75 seconds before Hive is even expected to be ready, and
`--rm` in the generated `ExecStart` removes the container on the way out — so
the operator gets a failed unit and no container to inspect.

The mechanism was confirmed live with a transient unit, no Podman involved:

```
$ systemd-run --user --wait --property=Type=notify --property=NotifyAccess=all \
    --property=TimeoutStartSec=3 /bin/sleep 30
rc=1 elapsed=3.11s
```

A Hive Quadlet unit must set `TimeoutStartSec` explicitly, larger than
`HealthStartPeriod` plus the retry budget, and larger again if the unit may
pull the image on first start. The value cannot be left to the host default,
and the assumption should be preflight-checked rather than assumed.

### 2. `Notify=healthy` without a healthcheck hangs instead of failing

A `.container` with `Notify=healthy` and no `HealthCmd` generated
`--sdnotify=healthy` with no warning and exit status 0. Podman would then wait
for a healthy signal that can never arrive, and the unit would hang until the
startup timeout and fail there. The `Notify=healthy`/`HealthCmd` pairing has to
be enforced by Hive's own contract test; the generator will not catch it.

### 3. Port publication belongs to the pod, and the generator does not say so

A `.container` with both `Pod=` and `PublishPort=` generated
`podman run --publish 3001:3001 --pod ...`, which Podman rejects at runtime
because ports must be declared when the pod is created. The generator exited 0.

For Hive this is exactly the boundary that matters: the gateway's authenticated
port 3001 must be published on the `.pod` unit, and the raw writable ttyd port
7681 must be published nowhere. That rule cannot be expressed as a per-container
`PublishPort=` in a pod-based layout, so a contract test has to assert it at the
pod level.

### 4. Pod and container ordering is weaker than it looks

The generated dependency pair is asymmetric. The container gets
`BindsTo=hive-pod.service` (hard), while the pod gets `Wants=hive-app.service`
(soft) plus `Before=`. Starting the pod therefore pulls the app in but succeeds
even if the app never becomes healthy, and `hive-pod.service` is `Type=forking`
around `podman pod start` — it reports started as soon as the pod exists.

Anything that gates on readiness must gate on the **container** unit, not the
pod unit. The pod unit's `ExecStopPost=podman pod rm --ignore --force` also
removes the pod on stop, so `systemctl stop` is a teardown, not a pause.

### 5. Short-name images warn but do not fail

```
Warning: gw.container specifies the image "nginx:alpine@sha256:4a73…" which not
a fully qualified image name.
```

The gateway image in `src/docker-compose.yaml` is exactly this shape: a digest
pin on a short name. Quadlet warned and exited 0. `podman auto-update` requires
a fully qualified reference, so the one image-reference source of truth in
#4206 should emit `docker.io/library/nginx:alpine@sha256:…` for the Podman
path rather than carrying the Compose spelling across.

## What the generator does and does not validate

Worth knowing, because it decides how much Hive's own tests have to cover.

Caught, exit status 1:

- An unsupported key: `unsupported key 'TotallyNotAKey' in group 'Container'`.
- A dangling unit reference: `quadlet pod unit nonexistent.pod does not exist`.
- A missing image: `no Image or Rootfs key specified`.

Not caught, exit status 0:

- `Notify=healthy` with no healthcheck (finding 2).
- `PublishPort=` on a pod member (finding 3).
- A short-name image (finding 5, warning only).

So `quadlet --dryrun` is a genuine syntax and reference checker and belongs in
CI, but it is not a semantic one. `systemd-analyze verify` adds nothing beyond
it here — it passed clean on units carrying two of the three silent problems.

## Follow-on work

A Quadlet implementation issue should carry, at minimum:

1. Explicit `TimeoutStartSec` on every `.container` unit, derived from the
   health start period and retry budget, plus a preflight check on
   `DefaultTimeoutStartUSec` (finding 1).
2. A contract test asserting `Notify=healthy` implies `HealthCmd`, port 3001 is
   published only at the pod level, and 7681 is published nowhere (findings 2
   and 3).
3. `quadlet --dryrun` for both `--user` and rootful in CI as a syntax gate.
4. Fully qualified image references from the #4206 source of truth (finding 5).
5. The rootless-user, linger, and `%h` decisions from the unit-location section.

No blocker was found. The open question this spike does not answer is whether
the `NET_ADMIN` egress gate actually enforces inside a rootless pod — that is
the #4188 security gate, and it needs a live run rather than a generator
dry-run.

## Reproducing

The four probe units, for reproduction only. **These are not Hive deployment
assets** — they name a retired image tag, mount a path that does not exist, and
omit the timeout fix from finding 1 on purpose, so that the generator output
above is reproducible as recorded.

```ini
# hive.network
[Network]
NetworkName=hive
Label=io.kubestellar.hive.owned=true

# hive-data.volume
[Volume]
VolumeName=hive-data
Label=io.kubestellar.hive.owned=true

# hive.pod
[Pod]
PodName=hive
Network=hive.network
PublishPort=3001:3001

[Install]
WantedBy=default.target

# hive-app.container
[Container]
Image=ghcr.io/hivecommons/hive:v2-latest
ContainerName=hive
Pod=hive.pod
AddCapability=NET_ADMIN
Volume=hive-data.volume:/data
Volume=%h/hive/hive.yaml:/etc/hive/hive.yaml:ro,Z
Environment=HIVE_LEVEL=2
HealthCmd=curl -sf http://127.0.0.1:3002/api/health
HealthInterval=10s
HealthTimeout=5s
HealthRetries=3
HealthStartPeriod=120s
Notify=healthy

[Service]
TimeoutStartSec=300
Restart=on-failure

[Install]
WantedBy=default.target
```

## References

- [`podman-systemd.unit(5)`](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html)
- [ADR-0017: Quadlet `.container`/`.pod` units as the Podman persistent lifecycle](adr/0017-podman-quadlet-lifecycle.md) — the decision this spike settles, with the minimum Podman version it implies.
- [`podman-auto-update(1)`](https://docs.podman.io/en/latest/markdown/podman-auto-update.1.html)
- Sibling spike for #4203: Podman Quadlet `.kube` compatibility (`src/docs/podman-quadlet-kube-spike.md`, PR #4259), which recommends explicit `.container`/`.pod` units over replaying the Kubernetes overlay.
