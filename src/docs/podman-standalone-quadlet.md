# Standalone Hive under Podman: the Quadlet units

The units that start Hive on Podman, in both root modes.
[ADR-0017](adr/0017-podman-quadlet-lifecycle.md) chose Quadlet as the
persistent lifecycle; this is that decision turned into something an operator
can start.

## Scope

This page covers **the Hive service, the gateway in front of it, and the
volume and network they need**:

| Unit | What it is |
| --- | --- |
| `src/deploy/quadlet/hive.container` | the Hive service |
| `src/deploy/quadlet/hive-data.volume` | its persistent state |
| `src/deploy/quadlet/hive.network` | the network the two containers share |
| `src/deploy/quadlet/hive-gateway.container` | the authenticating nginx gateway, and the only published port |

Deliberately **not** here, each being its own slice: persistence and SELinux
volume *semantics*, stop/start/recreate behaviour,
update/rollback/auto-update, and backup/restore/migration.

**Docker is untouched.** `src/docker-compose.yaml` is unchanged and remains the
default, fully supported runtime.

All four units carry the [#4210 ownership labels](podman-ownership-cleanup.md)
— `io.kubestellar.hive.owned=true` plus the component, instance, and runtime
keys — so the containers, the volume, and the network are visible to
`bin/hive-podman-teardown.sh`, whose selection is those labels and nothing
else. The instance label is the contract's default (`default`); a second Hive
deployment on the same host needs its own unit files with their own instance
value.

## The published-port boundary

This is the property the gateway unit exists for, and it is the one thing in
these units worth reading before anything else.

`src/docker-compose.yaml` publishes `3001:3001` for the gateway and leaves
7681 unpublished, and says why in the file:

> SECURITY: only the authenticating proxy port is published. :7681 (the raw
> writable ttyd terminal) is intentionally NOT published — the terminal is
> reached through :3001/terminal, which authenticates first. Publishing 7681
> exposed an unauthenticated shell into the credential-holding container.

The Podman units encode the same split:

| | Docker Compose | Quadlet |
| --- | --- | --- |
| gateway 3001 | `ports: ["3001:3001"]` | `PublishPort=3001:3001` in `hive-gateway.container` |
| Hive 3001 / 3002 | `expose:` only | no `PublishPort=` anywhere in `hive.container` |
| ttyd 7681 | `expose:` only | no `PublishPort=` anywhere at all |

**Container-to-container reachability needs no key in Podman.** Every member of
a netavark network can reach every port of every other member, so Compose's
`expose:` list — which is documentation in Docker too — has no counterpart
here and none is added. Quadlet spells `--expose` as `ExposeHostPort=`, and
`--expose` turns into a real published port the moment anything adds `-P`. On
this particular port that risk is not worth a documentation-only key.

Two independent things keep 7681 off the host, and it is worth knowing that
both hold rather than relying on either:

1. **Nothing publishes it.** The only `--publish` in the generated units is the
   gateway's `3001:3001`.
2. **ttyd binds loopback inside the container.** `src/deploy/entrypoint.sh`
   defaults `HIVE_TTYD_BIND` to `127.0.0.1`, so 7681 is not even reachable from
   the gateway.

Both were measured rather than assumed — see
[What was verified](#what-was-verified) for the commands and their output.

`podman ps` will still show `3001-3002/tcp, 7681/tcp` against the `hive`
container. That is the `EXPOSE` metadata baked into `src/Dockerfile`, not a
publish; the published ports are the ones printed with a host side, as in
`0.0.0.0:3001->3001/tcp` on the gateway. `ss -ltn` on the host is the check
that cannot be misread.

`src/deploy/test_quadlet_port_boundary.sh` fails the build if any of this is
edited away: it is the Podman counterpart to
`src/deploy/test_standalone_service_contract.sh`, which asserts the same
boundary for Compose and cannot see a Quadlet unit.

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
- **`aardvark-dns`**, which netavark uses to answer container-name lookups on a
  user-defined network. Without it the gateway starts and nginx cannot resolve
  `hive`, so `:3001` serves 502s. It ships alongside `netavark` on Fedora and
  most distributions; `podman info --format '{{.Host.NetworkBackend}}'` should
  say `netavark`.
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

# The gateway config. This is the same file the Compose stack mounts, verbatim;
# it is mounted read-only, so the gateway cannot rewrite what it serves from.
cp src/deploy/nginx.conf "$CONF/nginx.conf"
```

`nginx.conf` is not optional and does not have a working default: the unit
mounts it over `/etc/nginx/nginx.conf`, and the stock nginx config in the image
proxies nothing. Its `upstream hive_api { server hive:3001; }` is what the
shared network exists to make resolvable — leave the hostname alone unless you
also change `ContainerName=` in `hive.container`.

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

Install all four, not a subset — the gateway will not generate without the
network unit it references.

```sh
# rootless
install -Dm644 src/deploy/quadlet/hive.container         ~/.config/containers/systemd/hive.container
install -Dm644 src/deploy/quadlet/hive-data.volume       ~/.config/containers/systemd/hive-data.volume
install -Dm644 src/deploy/quadlet/hive.network           ~/.config/containers/systemd/hive.network
install -Dm644 src/deploy/quadlet/hive-gateway.container ~/.config/containers/systemd/hive-gateway.container
systemctl --user daemon-reload

# rootful
sudo install -Dm644 src/deploy/quadlet/hive.container         /etc/containers/systemd/hive.container
sudo install -Dm644 src/deploy/quadlet/hive-data.volume       /etc/containers/systemd/hive-data.volume
sudo install -Dm644 src/deploy/quadlet/hive.network           /etc/containers/systemd/hive.network
sudo install -Dm644 src/deploy/quadlet/hive-gateway.container /etc/containers/systemd/hive-gateway.container
sudo systemctl daemon-reload
```

`daemon-reload` is what runs the generator. Confirm it produced the services:

```sh
systemctl --user list-unit-files 'hive*'
# hive-data-volume.service  generated
# hive-gateway.service      generated
# hive-network.service      generated
# hive.service              generated
```

### 3. Start

Start the **gateway**; it pulls everything else up in order.

```sh
systemctl --user start hive-gateway.service        # rootless
sudo systemctl start hive-gateway.service          # rootful
```

Nothing else needs starting by hand. The generator turns each `.volume` /
`.network` reference into a dependency, and the gateway unit declares the one
that matters:

```
hive-gateway.service  Requires=/After=  hive.service, hive-network.service
hive.service          Requires=/After=  hive-network.service, hive-data-volume.service
```

`After=hive.service` means **after Hive is healthy**, not merely after its
process started, because `hive.container` sets `Notify=healthy` — the same
statement Compose makes as `depends_on: hive: condition: service_healthy`. So a
`start` that returns is a stack where Hive answered `/api/health` *and* nginx
proxied a request to it. Check it:

```sh
curl -sf http://127.0.0.1:3001/api/health     # -> {"status":"ok"}
```

That response came through the gateway, so it also proves name resolution over
the shared network worked.

Starting `hive.service` alone is still valid and gives you Hive with no
published port, which is what the previous slice shipped.

### 4. Boot persistence

**Rootful** — enable it:

```sh
sudo systemctl enable hive.service hive-gateway.service
```

**Rootless** — enable them *and* turn on lingering, or the user manager exits at
logout and the services go with it:

```sh
systemctl --user enable hive.service hive-gateway.service
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

**Do not enable IPv6 on `hive.network`.** The forced-proxy egress gate that
`src/deploy/entrypoint.sh` installs is `iptables-nft` only — the file contains
zero `ip6tables` references — so on a network with working IPv6 the gate
installs, reports success, and simply does not exist for half the traffic. That
was measured, not inferred: 5 of 5 IPv4 connections to `:443` from the agent UID
hit the redirect and 0 of 5 IPv6 connections did, in the same run against the
same endpoint (see [the IPv6 egress
bypass](podman-ipv6-egress-bypass.md)). `IPv6=` is therefore left off, and
`src/deploy/test_quadlet_port_boundary.sh` fails the build if it is turned on.
The same test guards `Internal=` (an internal network has no route off the host,
and Hive's agents have to reach GitHub) and `DisableDNS=` (aardvark-dns is what
makes `hive` resolve for the gateway).

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

**Do not add `:z` or `:Z` to the `hive-data` volume line.** It is the one
`Volume=` in the unit without a relabel suffix, and that is deliberate: podman
labels a named volume `container_file_t:s0` with no MCS category at create time,
which is what lets a recreated container read the data. `:Z` stamps a private
category, and the next mount without the flag is denied — with **no AVC
recorded**, so `ausearch` says nothing. Measured, with the restore, in
[what `hive-data` guarantees](podman-volume-persistence.md#the-one-thing-not-to-do-z-on-the-volume-line)
(#4376). The bind mounts below are the opposite case.

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

The generator gate cannot see what a unit *means*, only whether it parses —
`PublishPort=7681:7681` generates as cleanly as anything else. So the boundary
has its own gate. `src/deploy/test_quadlet_port_boundary.sh` runs in `v2-ci.yml`
and asserts, over every unit in `src/deploy/quadlet/`, that the gateway
publishes host 3001 to container 3001 and nothing else, that no unit names 7681
on either side of a `PublishPort=`, that `hive.container` publishes nothing,
that both containers join `hive.network` by unit name, that the gateway is
ordered `After=hive.service` and that `hive.container` still sets
`Notify=healthy` (which is what makes that ordering mean "after healthy"), and
that `DisableDNS`/`Internal`/`IPv6` stay off. Each assertion was confirmed to
FAIL under the corresponding mutation before being trusted.

The generator gate cannot see the config coupling either, because a mismatched
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

### The port boundary, measured (#4375)

The gateway and network units were started live in the same way — Fedora 44,
Podman 5.8.4 rootless, netavark, cgroup v2, SELinux enforcing, against the real
`ghcr.io/kubestellar/hive:stable` and the digest-pinned `nginx:alpine`.
`systemctl --user start hive-gateway.service` returned **0 after 11s**, having
pulled `hive.service` up first; both containers reported `healthy`.

**Only 3001 is on the host.** Before the start, nothing was listening on 3001,
3002, or 7681. After it:

```
$ ss -ltnp | grep -E ':(3001|3002|7681)\b'
LISTEN 0 4096 *:3001 *:* users:(("rootlessport",pid=2912635,fd=11))

$ curl -s -w 'HTTP %{http_code}\n' http://127.0.0.1:3001/api/health
{"status":"ok"}HTTP 200

$ curl -s --max-time 5 http://127.0.0.1:7681/ ; echo "exit=$?"
exit=7            # 7 = could not connect
$ curl -s --max-time 5 http://127.0.0.1:3002/ ; echo "exit=$?"
exit=7
```

The `:3001` response is end-to-end evidence on its own: nginx has no
`/api/health` of its own to serve, so a `{"status":"ok"}` means it resolved
`hive` over the shared network and proxied to it.

Reaching the container address directly does not work either — a rootless
network is not routable from the host:

```
$ podman inspect hive --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}'
10.89.1.2
$ timeout 5 bash -c 'exec 3<>/dev/tcp/10.89.1.2/7681'; echo "exit=$?"
exit=124          # 124 = timed out, no route
```

**Name resolution and container-to-container reach.** From inside the gateway:

```
$ podman exec hive-gateway getent hosts hive
10.89.1.2  hive.dns.podman hive.dns.podman hive
$ podman exec hive-gateway sh -c 'cat /etc/resolv.conf | grep nameserver'
nameserver 10.89.1.1                      # aardvark-dns on the hive network

$ for p in 3001 3002 7681; do podman exec hive-gateway nc -z -w3 hive $p; echo "$p -> $?"; done
3001 -> 0        # open
3002 -> 0        # open
7681 -> 1        # refused

$ podman exec hive-gateway curl -s http://hive:3001/api/health
{"status":"ok"}
```

**Why 7681 was refused, and why that is not the control.** ttyd binds loopback
inside the container, so it is not listening on the network at all:

```
$ podman exec hive sh -c 'cat /proc/net/tcp'      # state 0A = LISTEN, decoded
127.0.0.1:7681
127.0.0.1:18443
127.0.0.1:18444
$ podman exec hive sh -c 'cat /proc/net/tcp6'
[::]:3001
[::]:3002
```

That is the image's own defence and it says nothing about the unit's. To test
the unit's, `HIVE_TTYD_BIND=0.0.0.0` was set in `hive.env` and `hive.service`
restarted, putting ttyd on the wildcard — the worst case these units have to
hold under:

```
$ podman exec hive sh -c 'cat /proc/net/tcp'      # decoded
0.0.0.0:7681                              # now listening on the network

$ podman exec hive-gateway nc -z -w3 hive 7681; echo "exit=$?"
exit=0            # the network permits container-to-container, as required

$ curl -s --max-time 5 http://127.0.0.1:7681/ ; echo "exit=$?"
exit=7            # the host still cannot reach it
$ timeout 5 bash -c 'exec 3<>/dev/tcp/10.89.1.4/7681'; echo "exit=$?"
exit=124
$ ss -ltnH | awk '{print $4}' | grep -E ':(3001|3002|7681)$'
*:3001                                    # still the only published port
```

So both halves of the acceptance criterion hold, and they hold for independent
reasons: the network does not restrict container-to-container traffic, and the
absence of a `PublishPort=` is what keeps 7681 off the host. Neither is relying
on the other.

The probe was reverted and the scratch install torn down afterwards; no unit
file in the repository sets `HIVE_TTYD_BIND`.

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

- One observation outside this slice's scope, recorded because it was seen:
  `systemctl --user restart hive.service` propagated through `Requires=` and
  restarted `hive-gateway.service` 11s later, after which `:3001/api/health`
  still answered 200 on Hive's new container address. Restart/recreate
  semantics are their own slice and are **not** characterised here — this is one
  observation, not a claim about the general case.
- **Rootful was not started live.** It is dry-run generated only. The rootful
  half of `%E` (`/etc`) is from `systemd.unit(5)`; the rootless half
  (`/home/<user>/.config`) was measured directly with a probe unit.
- The live runs used a scratch directory for `hive.yaml`, `secrets/`,
  `hive.env`, and `nginx.conf` instead of `%E/hive`, so they could not touch a
  real operator configuration. Nothing else in the units differed.
- An earlier attempt additionally pinned an isolated image store via
  `GlobalArgs=`. That run never reached healthy, which is what produced the
  `GlobalArgs` finding in [Traps](#traps-measured-not-guessed); the successful
  run above does not use it.
