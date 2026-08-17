# Deployment helper scripts

The main supported paths remain Docker Compose and Kubernetes. `src/deploy/` also contains operator helpers for smaller bare-metal installs and safer Compose upgrades.

## Proxmox LXC helpers

`deploy/create-lxc.sh` runs on a Proxmox host. It creates an Ubuntu 24.04 LXC with Docker-compatible settings (`nesting=1`, `keyctl=1`, unconfined AppArmor, no capability drop), then starts SSH. Tunable flags include `--ctid`, `--hostname`, `--password`, `--disk`, `--memory`, and `--cores`.

`deploy/bootstrap-lxc.sh` runs inside that LXC. It installs Docker Engine and the Compose plugin, clones the `v2` branch to `/opt/hive`, builds the Hive image, and writes `/opt/hive/.env` with placeholders for GitHub, dashboard, model, and notification credentials.

Use this path when you operate Hive on Proxmox or another LXC-first host and want a lightweight VM-like container that still runs the normal Docker Compose deployment. Do not use it inside Kubernetes.

## Blue-green Compose deploy

`deploy/blue-green-deploy.sh [--skip-build]` is a zero/low-downtime Compose upgrade helper. It builds a fresh `hive` image unless `--skip-build` is set, starts it as `hive-next` on the existing Docker network and volumes, waits for `/api/health`, then stops the old `hive`, renames `hive-next` to `hive`, and reloads `hive-gateway` nginx.

Use it for production Docker Compose installs where an in-place `docker compose up -d` restart would be too disruptive. Preconditions: existing `hive` and `hive-gateway` containers, a `/data` volume, mounted `hive.yaml`, mounted `/secrets`, Docker Compose, and enough disk for a second image/container during the cutover.

Fixes #2939.
