# Dashboard-triggered standalone upgrades

Hive exposes its deployment runtime in `/api/version` and only shows the
dashboard upgrade button when the runtime is explicit and an upgrade helper is
available. If Hive cannot prove the runtime, it reports `unknown` and the
dashboard says to update manually. This is deliberate: running the wrong host
lifecycle is worse than hiding an unsafe button.

## Runtime contract

Set the runtime where the container can read it:

- Docker Compose: `HIVE_DEPLOYMENT_RUNTIME=docker-compose`
- Podman/Quadlet: `HIVE_DEPLOYMENT_RUNTIME=podman-quadlet`
- Hub/Kubernetes: `HIVE_DEPLOYMENT_RUNTIME=kubernetes`

Podman/Quadlet also requires an explicit manager mode:

- `HIVE_DEPLOYMENT_PODMAN_MODE=rootless` for `systemctl --user`
- `HIVE_DEPLOYMENT_PODMAN_MODE=rootful` for the system manager

New Compose assets set the Compose runtime in `src/docker-compose.yaml`. New
Quadlet installs append the runtime and mode to `hive.env`. Existing installs
must reconcile or add these values manually; until then the dashboard refuses
to offer a standalone upgrade action.

Podman/Quadlet installs additionally get `HIVE_DEPLOYMENT_UPGRADE_REQUEST_DIR`,
the container-side path of the host request bridge below. Without it the
runtime is still detected, but the Upgrade button stays disabled with the
"upgrade from the host" reason — a container cannot drive its own host
lifecycle, and Hive will not pretend otherwise.

## Host helper contract

Standalone upgrades are host lifecycle operations. Hive must not mount Docker,
Podman, or systemd sockets into the Hive container. Instead, install a
host-side helper and expose only that helper to Hive:

```sh
HIVE_DASHBOARD_UPGRADE_HELPER=/usr/local/libexec/hive-dashboard-upgrade-helper
```

The helper protocol is closed:

```sh
hive-dashboard-upgrade-helper upgrade \
  --runtime podman-quadlet \
  --ref ghcr.io/hivecommons/hive:<tag-or-digest> \
  --podman-mode rootless

hive-dashboard-upgrade-helper upgrade \
  --runtime docker-compose \
  --ref ghcr.io/hivecommons/hive:<tag-or-digest>
```

`bin/hive-dashboard-upgrade-helper.sh` implements that protocol. It validates
the image reference with an allow-list for `ghcr.io/hivecommons/hive`, passes
arguments as argv, and delegates to existing reviewed host scripts:

- Podman/Quadlet: `bin/hive-podman-update.sh pin <ref>` with `--rootless` or
  `--rootful`, preserving digest-pin history, rollback, gateway health checks,
  and the systemd manager split.
- Docker Compose: `src/deploy/blue-green-deploy.sh --skip-build --image-ref
  <ref>`, which pulls the requested image, starts `hive-next`, proves health,
  then swaps the container and reloads the gateway.

If the helper is missing or not executable, `/api/version` reports the detected
runtime but `upgradeSupported=false`, and `/api/self-upgrade` returns an honest
error instead of relaying to the Hub.

## Host request bridge (Podman/Quadlet)

A Hive under Quadlet runs in its own mount, PID and user namespace, so the
helper contract above cannot be satisfied from inside it: the helper is not in
the image, and the lifecycle it has to drive — `systemctl`, `podman`, the
Quadlet drop-ins under `%E/hive` — is not reachable from a container that
deliberately mounts no Podman, Docker, or systemd socket. On such a host the
Upgrade button could only ever be disabled with an honest reason
([#10344](https://github.com/hivecommons/hive/issues/10344)).

The bridge closes that without opening a socket. The container writes a
**request file** into one bind-mounted directory; a host-side `.path` unit
notices and runs the real update script outside every container namespace.

```
container  %E/hive/upgrade-requests -> /run/hive/upgrade-requests   (rw, the only writable mount)
host       hive-upgrade.path  --PathExistsGlob-->  hive-upgrade.service
                                                   -> hive-upgrade-request.sh apply
                                                      -> bin/hive-podman-update.sh pin <ref>
```

Assets: `src/deploy/systemd/hive-upgrade.path`,
`src/deploy/systemd/hive-upgrade.service`, `bin/hive-upgrade-request.sh`, and
the `Volume=` line in `src/deploy/quadlet/hive.container`.
`bin/hive-podman-setup.sh` installs all of them on a new install, creates the
request directory mode 770 / group 1002, and sets
`HIVE_DEPLOYMENT_UPGRADE_REQUEST_DIR` in `hive.env` to the **container-side**
path. Rootless installs get `--user` units in `~/.config/systemd/user`;
rootful installs get system units in `/etc/systemd/system`. The enable step is
the `.path` unit and never the service:

```sh
systemctl --user enable --now hive-upgrade.path   # rootless, as the hive user
sudo systemctl enable --now hive-upgrade.path     # rootful
```

Enabling `hive-upgrade.service` instead would run an upgrade at every boot.

### Request format

One JSON object per file, named `*.json`, written **atomically** — to a
temporary name in the same directory and then `rename(2)`d into place, so the
`.path` unit can never observe a half-written request:

```json
{"ref":"ghcr.io/hivecommons/hive:a1b2c3d","requester":"owner-login","requestedAt":"2026-10-03T11:04:05Z"}
```

`ref` is required. `requester` and `requestedAt` are advisory: they are
recorded with the result and nothing is decided from them. Unknown fields are
ignored.

Each handled request is **moved** into `done/` or `failed/` beside a
`.result` file before its outcome is recorded, so neither a success nor a
failure can be replayed by the watch re-triggering. Inspect either with:

```sh
hive-upgrade-request.sh status
```

### Trust boundary

Everything in a request was written by the container and is treated as
untrusted input:

- The ref is matched against the same closed allow-list
  `bin/hive-dashboard-upgrade-helper.sh` uses — `ghcr.io/hivecommons/hive` by
  tag or by `sha256` digest, and nothing else. A ref that does not match never
  reaches `podman`.
- Every value is passed as **argv**. Nothing read from a request is ever
  evaluated as shell text, sourced, or interpolated into a command line.
- The root mode comes from the host's own manager (`$MANAGERPID`), never from
  the request: a container cannot ask for the system manager.
- A request older than `HIVE_UPGRADE_REQUEST_MAX_AGE` (default 3600s) is
  archived **unapplied**, so a request left behind by a host that was down
  cannot silently install a stale ref days later. The age is the file's mtime,
  not the timestamp in the file, because the container writes the latter.
- The container's entire capability across this boundary is "cause
  `hive-upgrade-request.sh` to run".

The bridge changes **where** an upgrade runs, not what it does: it still
delegates to `bin/hive-podman-update.sh pin <ref>`, with the digest-pin
consequences described under operational notes below.

## Operational notes

- Owner authorization is still required on `/api/self-upgrade`.
- Podman dashboard upgrades write a digest pin. On hosts using
  `podman-auto-update.timer`, the pin shadows automatic registry updates until
  the operator runs `bin/hive-podman-update.sh unpin` or otherwise chooses that
  posture.
- Podman dashboard upgrades are image-only. The update script refreshes the
  gateway config but does not rewrite Quadlet/boot units; run
  `bin/hive-podman-update.sh reconcile check` or `reconcile apply` for host
  asset drift.
- Compose dashboard upgrades are blue-green image upgrades. They do not change
  the Watchtower profile; operators should avoid simultaneously using the
  60-second Watchtower poller and manual dashboard upgrades unless they accept
  that Watchtower may observe the image as already current.
- Rollback remains runtime-specific: Podman uses the update script's newest
  healthy pin; Compose keeps the old image available for an operator/helper to
  run with Docker if a post-swap rollback is needed.
