# Dashboard-triggered standalone upgrades

Hive exposes its deployment runtime in `/api/version` and only shows the
dashboard upgrade button when the runtime is explicit and its upgrade bridge is
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

## Host request bridge (Podman/Quadlet)

Podman/Quadlet never executes an upgrade helper inside the container. Install
and enable the host bridge tracked in [#10416](https://github.com/hivecommons/hive/issues/10416),
bind-mount its request directory read-write, and set the **container-visible**
path using `HIVE_DEPLOYMENT_UPGRADE_REQUEST_DIR` (or YAML
`deployment.upgrade_request_dir`). The environment variable takes precedence.
For example, the mounted path can be `/run/hive/upgrade-requests`.
The dashboard does not create the directory or install host units. Runtime,
manager mode, and a successful write probe are required to enable the button.
Absent, missing, or unwritable directories retain the existing host-command
instructions; an executable `HIVE_DASHBOARD_UPGRADE_HELPER` does not enable
Podman upgrades.

The dashboard publishes one `hive-upgrade-<unique-id>.json` per accepted request:

```json
{"target_ref":"ghcr.io/hivecommons/hive:abcdef1","requester":"owner-login","requested_at":"2026-11-12T12:00:00Z"}
```

`target_ref` uses the existing dashboard target grammar: a 7–40 character
hexadecimal image tag, normalized to the first seven lowercase characters.
`requester` is the authenticated request's audit user (`local` for local token
access), not a value supplied in the JSON body. `requested_at` is UTC RFC3339.
Files have mode `0600` and are written and closed under a hidden temporary name
in the same directory before an atomic rename. The host must watch **only
`*.json`** in the directory's top level, ignoring `.hive-upgrade-*` temporary
files and `.hive-upgrade-probe-*` write probes.

This is an asynchronous trust boundary: the host bridge validates references
and request age before invoking the host lifecycle, then archives results in
`done/` or `failed/`. No Docker, Podman, or systemd socket is exposed to Hive.
Only the authorized Hive container and host bridge should have access to the
request directory; the host consumer must be able to read its `0600` files.
Rootless bridges run as the hive user, rootful bridges under the system manager.
The host installation, units, age limit, and result handling belong to #10416;
this dashboard change alone does not install a consumer.

`/api/self-upgrade` returns `status=accepted` and a message that the upgrade has
not completed as soon as the request is published. It does not wait for the
host, and acceptance is not proof of rollout success.

## Host helper contract (Docker Compose)

Standalone upgrades are host lifecycle operations. Hive must not mount Docker,
Podman, or systemd sockets into the Hive container. Compose retains its existing
executable-helper contract:

```sh
HIVE_DASHBOARD_UPGRADE_HELPER=/usr/local/libexec/hive-dashboard-upgrade-helper
```

The helper protocol is closed:

```sh
hive-dashboard-upgrade-helper upgrade \
  --runtime docker-compose \
  --ref ghcr.io/hivecommons/hive:<tag-or-digest>
```

`bin/hive-dashboard-upgrade-helper.sh` implements that protocol. It validates
the image reference with an allow-list for `ghcr.io/hivecommons/hive`, passes
arguments as argv, and delegates to existing reviewed host scripts:

- Docker Compose: `src/deploy/blue-green-deploy.sh --skip-build --image-ref
  <ref>`, which pulls the requested image, starts `hive-next`, proves health,
  then swaps the container and reloads the gateway.

For Compose, if the helper is missing or not executable, `/api/version` reports the detected
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
