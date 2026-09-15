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
