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
volume *semantics*, stop/start/recreate behaviour, auto-update, and
backup/restore/migration. Manual update and rollback used to be on that list and
now has its own page,
[podman-quadlet-update-rollback.md](podman-quadlet-update-rollback.md) (#4378).

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
- Run the preflight first — **and export `HIVE_DEPLOY_RUNTIME=podman` before you
  do**, or all three exit 0 having checked nothing. Docker is the default
  runtime, and with it selected they print
  `Podman preflight: skipped — HIVE_DEPLOY_RUNTIME selects docker` and return
  success, which reads exactly like a pass:

  ```sh
  export HIVE_DEPLOY_RUNTIME=podman
  bin/hive-podman-preflight.sh          # engine, root mode, cgroups
  bin/hive-podman-preflight-ids.sh      # subordinate IDs, graphroot, networking
  ```

  `bin/hive-podman-preflight-host.sh` is the third, and it is worth running
  **after** step 1 below rather than here, because it checks the files that step
  creates: SELinux labels on the bind sources, config and secrets readability,
  `hive.env`, and port 3001. Point it at the configuration directory:

  ```sh
  HIVE_SRC_DIR="$CONF" bin/hive-podman-preflight-host.sh
  ```

  It recognises this layout — `nginx.conf` flat in `%E/hive` rather than under a
  `deploy/` subdirectory — and says which one it picked on its first line
  (`layout: quadlet (detected)`). It also recommends `:Z` for these files, which
  is what the units mount them with, rather than the `:z` that suits a checked-out
  source tree. Force either shape with `HIVE_PODMAN_LAYOUT=source|quadlet` if you
  run it against a directory that is still mid-install (#4422).

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

`%E` is the difference the unit file *shows* you. There is a second one it
could not show, and it is why the units are no longer wanted by
`default.target` ([#4478](https://github.com/hivecommons/hive/issues/4478)):
the two modes read that line differently.

| | `[Install] WantedBy=default.target` would mean | A Hive that never becomes healthy |
| --- | --- | --- |
| rootful (system unit) | the **boot transaction** | **held the boot** until the unit gave up — `TimeoutStartSec` is 5min for `hive.service`, 2min for `hive-gateway.service` |
| rootless (`--user` unit) | the *user* manager's default target, which logind reaches after the system boot has already finished | delays nothing but Hive |

Measured on two real reboots of one host: on the rootful boot systemd's own
`FinishTimestampMonotonic` was **549µs** after `hive-gateway.service` went
active — the boot was not declared finished until Hive was serving — and a
never-ready stand-in held a boot for its whole `TimeoutStartSec`
(`src/deploy/probe_boot_transaction_coupling.sh`).

Since #4478 the units are wanted by **`hive-boot.target`** instead, and
`hive-boot-gate.service` — the one Hive unit wanted by `default.target` —
starts that target only after the manager declares startup finished. Measured
in the same probe: the never-ready stand-in wired this way lets the boot
finish in **+0.122s** where the old wiring held it **20.188s** of a 20s
stand-in timeout, while the unit still auto-starts, still records
`Result=timeout`, and still restarts. Both modes read the new wiring
identically, so this difference between them is gone. See
[Boot persistence](#4-boot-persistence) below and
[the lifecycle page](podman-quadlet-lifecycle.md#rootful-hive-is-inside-the-system-boot-transaction-rootless-is-not)
for the numbers.

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

**`dashboard.port` has to agree with the FIRST port in `HealthCmd=`.** The unit
probes two:

```
HealthCmd=curl -sf http://127.0.0.1:3002/api/health && curl -sf http://127.0.0.1:3001/api/health
```

3002 is the Go API and is the one `dashboard.port` is coupled to. 3001 is the
Node auth proxy, whose port comes from `HIVE_PROXY_PORT` rather than from
`hive.yaml`, and it is there because the unit used to report `healthy` while the
dashboard was refusing connections (#4476 — see the trap below). Podman runs a
`--health-cmd` that is not a JSON array under `/bin/sh -c`, so the `&&` is the
shell's and either half failing is unhealthy.

3002 is where the Go default puts the API
(`defaultDashboardPort` in `src/pkg/config/config.go`), where the `hive`
service's healthcheck in `src/docker-compose.yaml` looks for it, and what
`src/deploy/hive.yaml` sets. Deleting the `port:` line outright works too, since
3002 is what the default gives you. What does not work is keeping the example's
3001, and nothing in the install catches it: the config parses, the generator is
happy, the container starts, and the failure arrives five minutes later. That is
why the `sed` above is a step rather than a remark.

**`$CONF/hive.env` must exist**, even if every line in it stays commented out.
Start from the tracked template, which lists every variable the Compose stack's
`environment:` block sets:

```sh
cp src/deploy/quadlet/hive.env.example "$CONF/hive.env"
chmod 600 "$CONF/hive.env"
```

`EnvironmentFile=` in a `.container` unit becomes `podman run --env-file`, and
`podman run` fails if the file is missing. Quadlet does **not** honour
systemd's leading `-` optional-file prefix here — see
[Traps](#traps-measured-not-guessed).

`hive.env.example` is tracked rather than left to prose because
`EnvironmentFile=` is opaque: the unit names a path and nothing in the unit says
what belongs in it, so a variable added to `src/docker-compose.yaml` used to
reach Docker and silently never reach Podman. `src/deploy/
test_standalone_runtime_parity.sh` ([#4404](https://github.com/hivecommons/hive/issues/4404))
asserts the two lists are the same set, so that divergence now fails CI. Add a
variable to both or to neither.

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

Install all four Quadlet units — the gateway will not generate without the
network unit it references — plus the two plain systemd units that wire the
deployment to boot without putting it on the boot's critical path (#4478).
The plain units go into the **systemd** unit directory, not the Quadlet one:
the Quadlet generator ignores `.service`/`.target` files in its own directory,
so putting them there installs nothing.

```sh
# rootless
install -Dm644 src/deploy/quadlet/hive.container         ~/.config/containers/systemd/hive.container
install -Dm644 src/deploy/quadlet/hive-data.volume       ~/.config/containers/systemd/hive-data.volume
install -Dm644 src/deploy/quadlet/hive.network           ~/.config/containers/systemd/hive.network
install -Dm644 src/deploy/quadlet/hive-gateway.container ~/.config/containers/systemd/hive-gateway.container
install -Dm644 src/deploy/systemd/hive-boot.target       ~/.config/systemd/user/hive-boot.target
install -Dm644 src/deploy/systemd/hive-boot-gate.service ~/.config/systemd/user/hive-boot-gate.service
systemctl --user daemon-reload
systemctl --user enable hive-boot-gate.service

# rootful
sudo install -Dm644 src/deploy/quadlet/hive.container         /etc/containers/systemd/hive.container
sudo install -Dm644 src/deploy/quadlet/hive-data.volume       /etc/containers/systemd/hive-data.volume
sudo install -Dm644 src/deploy/quadlet/hive.network           /etc/containers/systemd/hive.network
sudo install -Dm644 src/deploy/quadlet/hive-gateway.container /etc/containers/systemd/hive-gateway.container
sudo install -Dm644 src/deploy/systemd/hive-boot.target       /etc/systemd/system/hive-boot.target
sudo install -Dm644 src/deploy/systemd/hive-boot-gate.service /etc/systemd/system/hive-boot-gate.service
sudo systemctl daemon-reload
sudo systemctl enable hive-boot-gate.service
```

The `enable` works, and is required, precisely because `hive-boot-gate.service`
is a real unit file rather than a generated one — it is the only Hive unit
wanted by `default.target`, and without it Hive never starts at boot.

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

**There is exactly one thing to enable, and it is not a Quadlet unit.** The
generated units cannot be enabled — systemd refuses:

```sh
sudo systemctl enable hive.service
# Failed to enable unit: Unit /run/systemd/generator/hive.service is transient or generated
```

The boot wiring has two halves since
[#4478](https://github.com/hivecommons/hive/issues/4478):

1. `[Install] WantedBy=hive-boot.target` inside `hive.container` and
   `hive-gateway.container`. The generator turns it into
   `hive-boot.target.wants/` symlinks in its own output directory on every
   `daemon-reload`, so step 2 above already did it. Confirm they landed:

   ```sh
   # rootless
   ls -l "/run/user/$(id -u)/systemd/generator/hive-boot.target.wants/"
   # rootful
   sudo ls -l /run/systemd/generator/hive-boot.target.wants/
   ```

2. `hive-boot-gate.service`, enabled in step 2, the only Hive unit wanted by
   `default.target`. Nothing at boot wants `hive-boot.target` itself: the gate
   waits for the manager to declare startup finished
   (`systemctl is-system-running --wait`) and then starts the target as a
   **new** transaction. Skip its `enable` and the symlinks above are decoration
   — Hive never starts at boot.

**Why the indirection exists.** `WantedBy=default.target`, which these units
carried before #4478, means two different things: in the user manager it is
reached after the system boot has already finished, but in the **system**
manager it is the boot transaction itself, and systemd does not declare a boot
finished until every job in it is done. `Notify=healthy` holds these units in
`activating` until `/api/health` answers, so a rootful Hive that never became
healthy held the **whole boot** for its `TimeoutStartSec` — 5 minutes for
`hive.service` — on **every** boot until the cause was fixed. A wrong
`dashboard.port`, an unreachable registry, or a missing `HIVE_DASHBOARD_TOKEN`
all reach it — see [Traps](#traps-measured-not-guessed). Measured in
`src/deploy/probe_boot_transaction_coupling.sh`: a never-ready stand-in wired
the old way held a boot 20.188s of a 20s timeout; wired through the gate the
same boot finished in +0.122s, and the unit still auto-started, still recorded
`Result=timeout` on its start job (which is what `podman auto-update
--rollback` reads, so rollback semantics are untouched), and still cycled
under `Restart=always`. Ordering directives cannot do this — a start job in
the boot transaction delays the boot-finished timestamp no matter when it
runs; the same probe measured `DefaultDependencies=no` holding the boot just
as long.

What the failure now costs instead: the timeout is paid *after* the boot, by
the Hive units alone, in both modes — the host's boot, `systemd-analyze`, and
anything gated on startup finishing are unaffected. An interactive
`systemctl start hive.service` still blocks until healthy, exactly as before.

**Rootless additionally needs lingering**, or the user manager never starts at
boot and none of the wiring above is ever read:

```sh
loginctl enable-linger "$USER"
loginctl show-user "$USER" -p Linger      # -> Linger=yes
```

`loginctl enable-linger` is not optional for a rootless install that must
survive a reboot. Rootful needs no equivalent; the system manager is PID 1.

`bin/hive-podman-setup.sh` checks this at the end of every rootless install
(#4489): with `Linger=no` it says, loudly, that the install will not survive a
reboot and prints the command above, and its closing summary never claims
reboot safety it did not read back from `loginctl`. It does not enable
lingering unasked — that reconfigures the host — but `--enable-linger` opts
in, and the flag failing is an install failure rather than a shrug.

**Do not check any of this with `systemctl is-enabled hive.service`.** For a
generated unit it reports `generated` — with lingering on, with lingering off,
and with the symlink deleted. It cannot tell you whether Hive will come back.
Use

```sh
bin/hive-podman-lifecycle-probe.sh check              # or --rootful
```

which reads the generator output, the gate's enablement, and the linger state
instead, and see [the lifecycle page](podman-quadlet-lifecycle.md) for the
measurements behind that advice.

## Traps, measured not guessed

**`dashboard.port` must be 3002, the first port `HealthCmd=` probes.**
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

**A missing `HIVE_DASHBOARD_TOKEN` used to leave `hive.service` reporting
`healthy`.** The container has two listeners and the unit probed one of them.
The Node proxy on 3001 enforces the token and *fails closed without it* —
`Refusing to start. Set HIVE_DASHBOARD_TOKEN.` — so it never binds, while the Go
API on 3002 answers 200 throughout. Measured, rootful, with the line omitted
from `$CONF/hive.env`:

```
$ sudo systemctl is-active hive.service hive-gateway.service
active                                   <- hive
activating                               <- gateway, about to time out
$ sudo podman ps
hive   Up 2 minutes (healthy)            <- for the whole two minutes
$ sudo podman exec hive curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:3001/api/health
000                                      <- nothing listening
```

The only red was `hive-gateway.service`, 120s later, as an nginx
`connect() failed (111: Connection refused)` naming neither the port nor the
variable — while the unit that owned the broken configuration was green. The
`HealthCmd=` above now probes both listeners, so the failure surfaces on
`hive.service`, where the cause is. `src/deploy/test_quadlet_config_contract.sh`
holds the second port to what `nginx.conf` dials, and
`src/proxy/health_probe.test.js` holds `GET /api/health` on 3001 to answering
without a credential in every mode the proxy boots in — if it ever needed one,
this probe could never pass and every start would hang for `TimeoutStartSec`
(#4476).

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

## Docker-free: the quick start run with Docker scrubbed

`README.md`'s Quick Start (Podman) asserts *"Docker is not required and is not
used."* That was a design statement. This is the measurement, and it closes one
of [#4188](https://github.com/hivecommons/hive/issues/4188)'s acceptance criteria
— **but not the other**; see [what stays open](#what-this-does-not-establish)
below.

### The host's Docker facts, first

Whether `/usr/bin/docker` is the real CLI or the `podman-docker` compatibility
shim changes what a scrub proves, and #4188 names the shim as disqualifying in
its own right. So it is recorded rather than assumed:

```
$ command -v docker
/usr/bin/docker

$ rpm -qf /usr/bin/docker
docker-ce-cli-29.7.2-1.fc44.x86_64

$ file /usr/bin/docker
/usr/bin/docker: ELF 64-bit LSB pie executable, x86-64 …

$ docker version
Client: Docker Engine - Community
 Version:           29.7.2
```

Real Docker CLI, not the shim — the same package family
`podman-compose-provider-spike.md` measured for #4201.

### The scrub

Docker was **removed rather than trusted to be absent**, the method that spike
established: on a host where Docker is present, an accidental dependency is
exercised and fails loudly, whereas on a clean host it is simply never reached.

`PATH` was rebuilt as a directory mirroring every binary from `/usr/bin`,
`/bin`, `/usr/sbin`, `/sbin` and `/usr/local/bin` — 3014 of them — **omitting
`docker`, `dockerd` and `docker-proxy`**. Mirroring everything else matters: a
curated allow-list would make a missing ordinary tool look like a Docker-free
failure. `DOCKER_HOST` was pointed at a socket that does not exist.

```
$ command -v docker
<not found>

$ docker version
bash: command not found: docker

DOCKER_HOST=unix:///nonexistent/docker.sock
```

### The run

`README.md`'s Quick Start (Podman), verbatim, under that environment. Only the
`git clone` was skipped, an existing checkout standing in for it — recorded
because it is a deviation, though cloning is not what is under test.

| step | result |
| --- | --- |
| `bin/hive-podman-preflight.sh` | `pass=4 warn=0 fail=0` |
| `bin/hive-podman-preflight-ids.sh` | `pass=4 warn=0 fail=0` |
| config + secrets staging, `dashboard.port` rewrite | `port:3002` |
| `HIVE_SRC_DIR="$CONF" bin/hive-podman-preflight-host.sh` | `pass=8 warn=3 fail=0` |
| `podman pull ghcr.io/kubestellar/hive:stable` | exit 0 |
| four `install -Dm644` + `daemon-reload` | all four services `generated` |
| `systemctl --user start hive-gateway.service` | **exit 0, 13s** |

End state — and `Notify=healthy` means `active` already implies serving, so both
were checked rather than one inferred from the other:

```
Id=hive.service          ActiveState=active  SubState=running  Result=success
Id=hive-gateway.service  ActiveState=active  SubState=running  Result=success

$ curl -sf http://127.0.0.1:3001/api/health
{"status":"ok"}

hive          Up 28 seconds (healthy)  ghcr.io/kubestellar/hive:stable
hive-gateway  Up 17 seconds (healthy)  docker.io/library/nginx@sha256:4a73073b…
```

The `curl` went through the **published** port to the gateway, which proxied it
to Hive over the shared network — so it also proves name resolution worked.

### No Docker socket anywhere

#4188's socket-isolation rule, checked by enumerating every mount rather than
grepping the unit files:

```
hive mounts:
  …/volumes/hive-data/_data          -> /data
  …/.config/hive/hive.yaml           -> /etc/hive/hive.yaml
  …/.config/hive/secrets             -> /secrets
hive-gateway mounts:
  …/.config/hive/nginx.conf          -> /etc/nginx/nginx.conf

docker socket references across both: 0
```

### What this does not establish

**#4188's second criterion is still open**, and this run cannot close it:

> A clean supported Linux host with Podman and **no Docker installation** can
> start the real Hive and gateway services using only documented commands.

Docker Engine is installed on this host. Scrubbing `PATH` and `DOCKER_HOST`
proves the Quadlet path *does not reach for* Docker — the "no Docker Engine,
daemon, socket, CLI shim or Compose provider" criterion. It does not prove what
happens on a machine where Docker was never installed: a shared library, a
`containers.conf` default, or a package dependency pulled in alongside Docker
could still be doing work here invisibly. That needs a genuinely Docker-free
host, and it remains unexecuted.

Recording that gap is part of this result, not a shortfall against it.

Everything created by this run was removed afterwards and the host verified back
to its prior state: units, containers, the `hive-data` volume, the `hive`
network, and the configuration directory — which on this host also holds live
contributor-relay credentials and was restored from a backup taken first.

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

Lifecycle behaviour — stop, start, restart, recreate, and boot wiring — has its
own repeatable check, `bin/hive-podman-lifecycle-probe.sh`, covered by
`bin/test_hive_podman_lifecycle_probe.sh` with every input mocked. The
measurements it was written from are in
[podman-quadlet-lifecycle.md](podman-quadlet-lifecycle.md).

Changing the image these units run — and getting back when the new one does not
work — is [podman-quadlet-update-rollback.md](podman-quadlet-update-rollback.md)
(#4378), with `bin/hive-podman-update.sh` as its repeatable path. The `Image=`
line below stays a floating tag on purpose, because it has to agree with
`src/deploy/standalone-images.sh`; the operator's digest pin goes in a Quadlet
drop-in, `hive.container.d/10-image.conf`, which that page describes. A floating
tag is not something you can roll back to, so pin before you need to.

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

A **live rootful start was performed** by [#4377](podman-quadlet-lifecycle.md),
on the same host and against the same image, closing the gap recorded below:

| | Observed |
| --- | --- |
| `sudo systemctl daemon-reload` | generated `hive.service` and `hive-data-volume.service` |
| generated properties | `Type=notify`, `NotifyAccess=all`, `TimeoutStartUSec=5min` |
| ordering | `Requires=hive-data-volume.service`, `After=hive-data-volume.service` |
| `sudo systemctl start hive.service` | returned **0** after 11.7s, `ActiveState=active`, `Result=success` |
| container | `healthy` |
| `%E` in a system unit | measured as `/etc`: `/etc/hive/hive.yaml` and `/etc/hive/secrets` were the mount sources on the running container |
| `AddCapability=NET_ADMIN` | enforcing, same as rootless: `[entrypoint] iptables (iptables-nft): outbound :443 -> :18443` and `ambient CAP_NET_ADMIN granted` |

#4377 also exercised stop, start, restart, and recreate in both root modes, and
found that a clean `systemctl stop` left the unit `failed` until
`SuccessExitStatus=143 SIGTERM` was added to the unit. See
[the lifecycle page](podman-quadlet-lifecycle.md).

Recorded honestly rather than left implied:

- One observation outside this slice's scope, recorded because it was seen:
  `systemctl --user restart hive.service` propagated through `Requires=` and
  restarted `hive-gateway.service` 11s later, after which `:3001/api/health`
  still answered 200 on Hive's new container address. Gateway restart/recreate
  semantics are their own slice and are **not** characterised here — this is one
  observation, not a claim about the general case.
- **Rootful was not started live *by #4354*.** It was dry-run generated only,
  and the rootful half of `%E` (`/etc`) was read from `systemd.unit(5)` rather
  than measured; the rootless half (`/home/<user>/.config`) was measured
  directly with a probe unit. #4377 closed both, as recorded above.
- **No reboot was performed by this slice**, in either root mode. The boot
  *wiring* is measured here — the generator's `default.target.wants/` symlink,
  `default.target` being reached, and lingering starting a sessionless user
  manager — but their composition across an actual kernel boot is not. #4413
  executed that composition in both modes, on a host that could be rebooted:
  [the lifecycle page](podman-quadlet-lifecycle.md#reboot-persistence-executed-both-modes).
- The live runs used a scratch directory for `hive.yaml`, `secrets/`,
  `hive.env`, and `nginx.conf` instead of `%E/hive`, so they could not touch a
  real operator configuration. Nothing else in the units differed.
- An earlier attempt additionally pinned an isolated image store via
  `GlobalArgs=`. That run never reached healthy, which is what produced the
  `GlobalArgs` finding in [Traps](#traps-measured-not-guessed); the successful
  run above does not use it.
