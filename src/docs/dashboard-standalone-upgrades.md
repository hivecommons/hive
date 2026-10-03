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
Quadlet installs set the runtime and mode in `hive.env`. For an existing
Quadlet install, use the migration below rather than editing those values by
hand.

## Upgrading an existing Podman install

From a current `v5` checkout containing the host request bridge, run **one**
command as the account that owns the install (choose the matching manager):

```sh
# Rootless: run as the user who installed Hive, not with sudo.
bin/hive-podman-update.sh reconcile migrate --rootless

# Rootful: the script uses sudo for system-manager operations.
bin/hive-podman-update.sh reconcile migrate --rootful
```

This is an explicit downtime operation: it reconciles repo-owned units and
helpers, repairs/deduplicates `HIVE_DEPLOYMENT_RUNTIME`,
`HIVE_DEPLOYMENT_PODMAN_MODE`, and `HIVE_DEPLOYMENT_UPGRADE_REQUEST_DIR` in
`hive.env`, creates/repairs the host request directory with mode 770 and the
container launch group (using `podman unshare` for rootless ownership), enables
only `hive-upgrade.path`, and recreates Hive so the environment and writable
request mount actually reach the container. The request path is read from the
checkout's Quadlet mount, not guessed. Both host helpers are installed together
so the drain can invoke the update script without depending on the checkout's
location. Gateway health is checked before success is reported.

`hive.yaml`, `secrets/`, and all other environment lines (including tokens) are
preserved. A second run reports `already current`, changes no files, and exits
0. A failed recreate leaves a pending marker so rerunning the same command
retries activation even if the files already match. Missing bridge assets fail
before modifying the install. This migrates host assets, not the image or its
pin: the running image must also contain dashboard request-bridge support for
the button to become available. No Podman or systemd socket is mounted.

**Ownership:** `hive-podman-setup.sh` provisions new installs;
`hive-podman-update.sh reconcile migrate` upgrades existing ones, delegating
repo-owned file copying to its existing `reconcile apply` path. Ordinary
`reconcile check` remains read-only and `reconcile apply` still leaves
`hive.env` and the running Hive container untouched. Do not use `setup --force`
for this migration: that can replace operator configuration and tokens.

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
