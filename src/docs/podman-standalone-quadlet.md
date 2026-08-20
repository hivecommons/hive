# Standalone Hive under Podman: the service Quadlet unit

The unit that starts Hive on Podman, in both root modes.
[ADR-0017](adr/0017-podman-quadlet-lifecycle.md) chose Quadlet as the
persistent lifecycle; this is the first slice of that decision turned into
something an operator can start.

## Scope

This page covers **the Hive service and the volume it needs**:

- `src/deploy/quadlet/hive.container`
- `src/deploy/quadlet/hive-data.volume`

Deliberately **not** here, each being its own slice: the authenticating
gateway and the network boundary, persistence and SELinux volume *semantics*,
update/rollback/auto-update, and backup/restore/migration.

One consequence of that scope is worth stating up front: **nothing is
published**. Hive listens on 3002 (API) and 3001 (proxy), and 7681 is the raw
writable ttyd terminal that must never be published. Reaching the dashboard
from another host is the gateway's job, and the gateway is the next slice.
Until then this unit gives you a running, healthy service reachable from the
host it runs on.

**Docker is untouched.** `src/docker-compose.yaml` is unchanged and remains the
default, fully supported runtime.

Both units carry the [#4210 ownership labels](podman-ownership-cleanup.md) —
`io.kubestellar.hive.owned=true` plus the component, instance, and runtime
keys — so the container and the volume are visible to
`bin/hive-podman-teardown.sh`, whose selection is those labels and nothing
else. The instance label is the contract's default (`default`); a second Hive
deployment on the same host needs its own unit files with their own instance
value.

## `systemctl start` returning means *healthy*

This is the property ADR-0017 chose Quadlet for, so it is worth being precise.

The unit sets `Notify=healthy`, which generates `Type=notify`,
`NotifyAccess=all`, and `podman run --sdnotify=healthy`. systemd holds the unit
in `activating` until the container's healthcheck passes. So:

```
systemctl --user start hive.service      # returns only once /api/health answers
```

A start that returns is a service that answered `GET /api/health` — the same
endpoint the Compose healthcheck and the Kubernetes readiness probe use. A
service that never becomes healthy leaves the unit in `activating` until
`TimeoutStartSec` and then fails; it does **not** report started.

`HealthCmd=` is set explicitly for this reason. The Quadlet generator accepts
`Notify=healthy` with no healthcheck and exits 0 — producing a unit that claims
"started" the moment conmon is up. A dry-run gate cannot catch that. Do not
remove `HealthCmd=`.

## Requirements

- **Podman.** ADR-0017 **recommends 5.6.0**. The **verified floor is unknown** —
  nothing between 5.0.0 and 5.8.4 has been exercised by any spike, and 5.8.4 is
  only the version those spikes happened to run on, not a floor. The hard
  minimum implied by the features used here is **5.0.0**, which is where
  `Notify=healthy` in `.container` units first shipped. This unit joins no
  `.pod`, so the podman#26105 wrong-`--pod`-name regression that motivates the
  5.6.0 recommendation does not apply to it.
- **cgroup v2**, which Quadlet requires. Check with
  `podman info --format '{{.Host.CgroupsVersion}}'`.
- The Quadlet generator itself, at `/usr/libexec/podman/quadlet`. It ships with
  the distribution `podman` package; a hand-installed podman binary may not
  carry it.
- Run the preflight first: `bin/hive-podman-preflight.sh`,
  `bin/hive-podman-preflight-ids.sh`, `bin/hive-podman-preflight-host.sh`.

## Unit search paths

Quadlet reads units from these directories, in precedence order.

**Rootful** — `podman-systemd.unit(5)`:

| Path | Use |
| --- | --- |
| `/run/containers/systemd/` | temporary, usually testing |
| `/etc/containers/systemd/` | **where an administrator installs these units** |
| `/usr/share/containers/systemd/` | distribution-provided |

**Rootless**:

| Path | Use |
| --- | --- |
| `$XDG_RUNTIME_DIR/containers/systemd/` | temporary |
| `$XDG_CONFIG_HOME/containers/systemd/` or `~/.config/containers/systemd/` | **where a user installs these units** |
| `/etc/containers/systemd/users/$(UID)` | administrator-provided, one user |
| `/etc/containers/systemd/users/` | administrator-provided, all users |

## One unit file, both root modes

The bind-mount sources use `%E`, systemd's configuration-directory specifier:

| | `%E` expands to | So Hive's config lives in |
| --- | --- | --- |
| rootful (system unit) | `/etc` | `/etc/hive/` |
| rootless (`--user` unit) | `~/.config` | `~/.config/hive/` |

ADR-0017 warns that `%h` expands to `/root` in a system unit, so a unit written
with `%h` is not portable across the rootful/rootless boundary. `%E` is, and it
puts the operator's files where each mode already expects them.

## Install

Below, `%E/hive` means `/etc/hive` if you are installing rootful and
`~/.config/hive` if you are installing rootless.

### 1. Configuration and secrets

```sh
# rootless
CONF=~/.config/hive
# rootful:  CONF=/etc/hive

mkdir -p "$CONF/secrets"

# src/hive.yaml is NOT in the repo — src/.gitignore excludes it, because it is
# the file you create. Start from the shipped example, the way every other Hive
# install does.
cp src/hive.yaml.example "$CONF/hive.yaml"

# REQUIRED, and not cosmetic. The example ships `dashboard.port: 3001` for local
# source runs; this unit's HealthCmd probes 3002. Skipping this line costs a
# silent 300-second hang with no container left to inspect — see Traps.
sed -i 's/^  port: 3001$/  port: 3002/' "$CONF/hive.yaml"
grep -A1 '^dashboard:' "$CONF/hive.yaml"        # -> dashboard: / port: 3002

# then edit the rest of "$CONF/hive.yaml" for your project

# The container reads keys as dev (1001) through the hive-launch group (1002),
# so the directory needs the group traverse bit — 0700 would exclude the
# container itself and read as an SELinux problem it is not (#4359).
chmod 750 "$CONF/secrets"
# rootless: container GID 1002 is not host GID 1002 — podman unshare translates
podman unshare chown -R 0:1002 "$CONF/secrets"
# rootful instead:  chgrp -R 1002 "$CONF/secrets"
```

**`dashboard.port` has to agree with the port in `HealthCmd=`.** The unit probes
`http://127.0.0.1:3002/api/health`. 3002 is where the Go default puts the API
(`defaultDashboardPort` in `src/pkg/config/config.go`), where the `hive`
service's healthcheck in `src/docker-compose.yaml` looks for it, and what
`src/deploy/hive.yaml` sets. Deleting the `port:` line outright works too, since
3002 is what the default gives you. What does not work is keeping the example's
3001, and nothing in the install catches it: the config parses, the generator is
happy, the container starts, and the failure arrives five minutes later. That is
why the `sed` above is a step rather than a remark.

**`$CONF/hive.env` must exist**, even if it is empty:

```sh
touch "$CONF/hive.env"
chmod 600 "$CONF/hive.env"
```

`EnvironmentFile=` in a `.container` unit becomes `podman run --env-file`, and
`podman run` fails if the file is missing. Quadlet does **not** honour
systemd's leading `-` optional-file prefix here — see
[Traps](#traps-measured-not-guessed).

At minimum set `HIVE_DASHBOARD_TOKEN` in it:

```sh
printf 'HIVE_DASHBOARD_TOKEN=%s\n' "$(openssl rand -hex 32)" >> "$CONF/hive.env"
```

Hive **refuses to start** without it unless the hive is hub-hosted:

```
[SECURITY] HIVE_DASHBOARD_TOKEN is not set and this hive is not hub-hosted
(no HIVE_SESSION_KEY / HIVE_HUB_SECRET) — all mutation endpoints would be
unauthenticated. Refusing to start. Set HIVE_DASHBOARD_TOKEN.
```

With `Notify=healthy` that refusal surfaces the way it should: the unit stays
in `activating` and then fails on timeout, rather than reporting a started
service that is not serving. If a start hangs, this is the first thing to check
— `journalctl --user -u hive.service` shows the line above. The second thing to
check is `dashboard.port`; see [Traps](#traps-measured-not-guessed).

### 2. The units

```sh
# rootless
install -Dm644 src/deploy/quadlet/hive.container    ~/.config/containers/systemd/hive.container
install -Dm644 src/deploy/quadlet/hive-data.volume  ~/.config/containers/systemd/hive-data.volume
systemctl --user daemon-reload

# rootful
sudo install -Dm644 src/deploy/quadlet/hive.container   /etc/containers/systemd/hive.container
sudo install -Dm644 src/deploy/quadlet/hive-data.volume /etc/containers/systemd/hive-data.volume
sudo systemctl daemon-reload
```

`daemon-reload` is what runs the generator. Confirm it produced the services:

```sh
systemctl --user list-unit-files 'hive*'
# hive-data-volume.service  generated
# hive.service              generated
```

### 3. Start

```sh
systemctl --user start hive.service        # rootless
sudo systemctl start hive.service          # rootful
```

The volume unit does not need starting by hand: the generated `hive.service`
carries `Requires=hive-data-volume.service` and `After=hive-data-volume.service`.

### 4. Boot persistence

**Rootful** — enable it:

```sh
sudo systemctl enable hive.service
```

**Rootless** — enable it *and* turn on lingering, or the user manager exits at
logout and the service goes with it:

```sh
systemctl --user enable hive.service
loginctl enable-linger "$USER"
```

`loginctl enable-linger` is not optional for a rootless install that must
survive a reboot. Verify with `loginctl show-user "$USER" -p Linger`.

## Traps, measured not guessed

**`dashboard.port` must be 3002, the port `HealthCmd=` probes.**
`src/hive.yaml.example` sets it to 3001 for source runs, and that is the file an
operator reaches for — `src/docs/network-requirements.md` describes it as the
one "for local/source runs", which is what a standalone Podman install reads as.
Install it unchanged and Hive comes up serving on 3001, so
`curl -sf http://127.0.0.1:3002/api/health` never answers and `Notify=healthy`
does exactly what it should: the unit stays in `activating` until
`TimeoutStartSec=300` expires. The generated `ExecStart` carries `--rm`, so the
container that would have shown you a perfectly healthy API on the wrong port is
deleted on the way out. What is left is `Job for hive.service failed because a
timeout was exceeded`, an empty `podman ps -a`, and a five-minute feedback loop
per attempt. Measured on Fedora 44, Podman 5.8.4 rootless, SELinux enforcing
(#4367).

**`EnvironmentFile=` does not take systemd's `-` prefix.** Writing
`EnvironmentFile=-%E/hive/hive.env` generates cleanly and then resolves to
`<unit directory>/-%E/hive/hive.env`, a path that will never exist: the `-`
stops the value looking absolute, so Quadlet joins it to the unit directory.
The dry-run does not warn. Measured on Podman 5.8.4. Create the file instead.

**`TimeoutStartSec` must be set explicitly and must exceed
`HealthStartPeriod` plus the retry budget.** It is 300s here against a 120s
start period and 3×10s of retries. The host default is not a safe fallback —
45s was measured on Fedora and 90s on the GitHub runners, both below the start
period. The generated `ExecStart` carries `--rm`, so a start timeout deletes
the container holding the evidence of why it timed out.

**Do not point these units at a non-default image store.** Adding
`GlobalArgs=--root ... --runroot ...` generates cleanly and the container
starts — and then the healthcheck never runs, so `Notify=healthy` never fires
and the unit sits in `activating` until it times out. The reason is that
Podman schedules the healthcheck as its own transient systemd timer whose
command is a bare `podman healthcheck run <id>`, with **no** global store
arguments, so it looks in the default store and reports
`no container with name or ID ... found`. Measured on Podman 5.8.4 while
qualifying these units.

**`:Z`, not `:z`, on the secrets mount.** `:z` is the *shared* SELinux label;
`:Z` is private to one container, which is what a secrets directory wants.
Exactly one container uses these paths. See the
[SELinux release qualification](podman-selinux-release-qualification.md) for
the measured difference.

## What was verified

Dry-run generation is gated in CI: `.github/workflows/quadlet-gate.yml` runs
`src/deploy/test_quadlet_generator_gate.sh` on every PR, in **both** rootful and
`--user` modes, and fails on any generator diagnostic rather than on exit
status alone. These units are picked up by it automatically.

The generator gate cannot see the config coupling, because a mismatched
`dashboard.port` generates perfectly — so that has its own gate.
`src/deploy/test_quadlet_config_contract.sh` runs in `v2-ci.yml` and asserts the
port in `HealthCmd=` against the Go default, `src/deploy/hive.yaml`, and the
Compose healthcheck, that the unit still names `dashboard.port` where a reader
will meet it, and that every file this page tells you to copy is one the repo
actually tracks. Change any one of those and the PR fails instead of the
operator's start (#4367).

A **live rootless start was performed** on Fedora 44, Podman 5.8.4, cgroup v2,
SELinux enforcing, against the real `ghcr.io/kubestellar/hive:stable` image:

| | Observed |
| --- | --- |
| `systemctl --user daemon-reload` | generated `hive.service` and `hive-data-volume.service` |
| generated properties | `Type=notify`, `NotifyAccess=all`, `TimeoutStartUSec=5min` |
| ordering | `Requires=hive-data-volume.service`, `After=hive-data-volume.service` |
| `systemctl --user start hive.service` | returned **0** after 52s, `ActiveState=active`, `Result=success` |
| container | `healthy` |
| `AddCapability=NET_ADMIN` | enforcing egress path, not advisory: `[entrypoint] iptables (iptables-nft): outbound :443 -> :18443` and `ambient CAP_NET_ADMIN granted` |

`Notify=healthy` was also confirmed **negatively**, which is the more useful
half. On a first attempt with no `HIVE_DASHBOARD_TOKEN`, Hive refused to start,
`/api/health` never answered, and the unit correctly stayed at
`ActiveState=activating` / `SubState=start` and never reported started. That is
the behaviour ADR-0017 chose Quadlet for: a service that is not serving does
not get to look started.

#4367 confirmed it negatively a second time, from the other direction and on the
same host. With `src/hive.yaml.example` installed as `$CONF/hive.yaml` — API on
3001, probe on 3002, nothing else changed — the start failed after **300s** with
`Result=timeout` and `podman ps -a` empty. With `dashboard.port: 3002` it
returned **0 in 11s**, `ActiveState=active`, and `podman ps` showed
`Up 11 seconds (healthy)`. (11s rather than the 52s above because the image was
already pulled.) The unit was not changed between the two runs; only the port
in the config was.

Recorded honestly rather than left implied:

- **Rootful was not started live.** It is dry-run generated only. The rootful
  half of `%E` (`/etc`) is from `systemd.unit(5)`; the rootless half
  (`/home/<user>/.config`) was measured directly with a probe unit.
- The live run used a scratch directory for `hive.yaml`, `secrets/`, and
  `hive.env` instead of `%E/hive`, so it could not touch a real operator
  configuration. Nothing else in the unit differed.
- An earlier attempt additionally pinned an isolated image store via
  `GlobalArgs=`. That run never reached healthy, which is what produced the
  `GlobalArgs` finding in [Traps](#traps-measured-not-guessed); the successful
  run above does not use it.
