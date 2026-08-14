# Hive backup and restore

Hive has two backup paths with different scopes.

## Hub disaster recovery: `hive-backup`

`v2/cmd/hive-backup` creates encrypted hub disaster-recovery archives. It captures:

- hub SaaS state under `/data/saas/**`;
- `/data/hub-registry.json`;
- required hub Kubernetes Secrets — a missing `hive-hub-secrets`, `oci-api-key`, or `hive-hub-kubeconfigs` **fails the backup**, while a missing `hive-hub-tls` only warns (cert-manager can reissue it);
- each spoke's authoritative config files (`hive.yaml.dashboard`, `hive.yaml.runtime` or legacy `hive.yaml.bak`, `hive-id`) and GitHub App key files.

It deliberately excludes regenerable bulk state such as hub `nous/`, `home/`, `beads/`, and `logs/` so a nightly fleet backup stays small and restorable.

`HIVE_BACKUP_KEY` is required and must be an AES-256 key, 64-character hex or base64 encoded (deliberately independent of `/data/saas/hmac.key`, which is itself backup payload). Escrow it outside the cluster; a backup encrypted by a key that only exists in the lost cluster is not recoverable.

```bash
openssl rand -hex 32   # generate the value to store as HIVE_BACKUP_KEY
hive-backup run                 # encrypt, upload, verify, and prune object storage
hive-backup run -local backup.enc
hive-backup run -skip-spokes=true
hive-backup verify              # newest object-storage archive
hive-backup verify -file backup.enc
hive-backup list
hive-backup extract -file backup.enc -dest restore-dir
```

Environment:

| Variable | Purpose |
|---|---|
| `HIVE_BACKUP_KEY` | Required AES-256 key, hex encoded. |
| `HIVE_BACKUP_BUCKET` | OCI Object Storage bucket. |
| `HIVE_BACKUP_DATA_DIR` | Hub data directory; default `/data`. |
| `HIVE_BACKUP_RETENTION` | Archive count to retain; default `30`. |
| `HIVE_HUB_NAMESPACE` | Hub namespace for Secret collection; default `hive-hub`. |
| `HIVE_KUBECONFIG_DIR` | Mounted kubeconfig directory for remote spoke clusters; default `/etc/hive/kubeconfigs`. |
| `HIVE_BACKUP_OCI_ENDPOINT` | OCI Object Storage endpoint override; defaults to the regional endpoint. |

`extract` decrypts into a directory and verifies hashes. It does **not** mutate a live cluster. A restore operator should inspect the extracted `MANIFEST.json`, re-apply the captured Secret JSON to the new hub namespace, copy `hub/` files back to the hub PVC, and recreate or patch spokes from `spokes/<hive-id>/`. Any `spoke_errors` in the manifest are honest gaps: those spokes were not captured and need separate recovery.

## Kubernetes CronJob

`v2/deploy/k8s/backup-cronjob.yaml` wires the hub DR path into Kubernetes. It is **not deployed by default** — it is absent from `v2/deploy/k8s/kustomization.yaml` and nothing in the install path applies it; deployment is an explicit `kubectl apply -f`, gated on the operator having escrowed `HIVE_BACKUP_KEY` first. It creates:

- ServiceAccount `hive-hub-backup` in namespace `hive-hub`;
- a namespaced Role granting `get` (only) on exactly four named Secrets — `hive-hub-secrets`, `oci-api-key`, `hive-hub-kubeconfigs`, `hive-hub-tls` — plus a ClusterRole limited to `pods [get,list]` and `pods/exec [create]` for reading spoke PVC state (hardened in [#3719](https://github.com/kubestellar/hive/pull/3719) and [#3810](https://github.com/kubestellar/hive/pull/3810); the unused `namespaces` grant was removed — spoke namespaces derive as `hive-hosted-<id>` from the registry);
- a daily `CronJob` scheduled at `17 3 * * *`, `concurrencyPolicy: Forbid`, one-hour active deadline, running `ghcr.io/kubestellar/hive:stable` with `imagePullPolicy: Always` (the backup image tracks the stable [release channel](release-channels.md), [#3810](https://github.com/kubestellar/hive/pull/3810));
- ConfigMap `hive-hub-backup-config` with object-storage bucket (default `hive-hub-backups`, inherited silently if you don't edit it) and retention.

Before applying it, create Secret `hive-hub-backup-key` with key `backup-key`, ensure `oci-api-key` and `hive-hub-kubeconfigs` exist, and confirm the PVC claim name (`hive-hub-data-rwx`) matches your deployment.

## Owner-triggered spoke backup

`pkg/spokebackup` is complementary: it backs up one spoke on demand for its owner and includes the spoke's bead ledger. It is encrypted with the same `HIVE_BACKUP_KEY` mechanism and is sized for browser download, not nightly fleet DR. It includes `hive.yaml.dashboard`, runtime config files, `hive-id`, `hive-state.json`, GitHub App keys, and `/data/beads/*`; it skips bulk/derived data and live dashboard sessions.

## Docker Compose

`v2/docker-compose.yaml` is a standalone deployment. It runs the hive container on a single Docker host, without a Kubernetes cluster.

### Durable state

The compose file persists three things:

- the Docker named volume `hive-data`, mounted at `/data`;
- the host directory `./secrets`, bind-mounted read-only at `/secrets`;
- the host file `./hive.yaml`, mounted at `/etc/hive/hive.yaml`.

On Docker and LXC the entrypoint treats `/data` as the boot-time source of truth. It restores `/data/hive.yaml.runtime` over the config path at every boot. `/data/hive.yaml.bak` is the legacy runtime name. `/data/hive.yaml.dashboard` is the dashboard save overlay.

`/data` holds the durable identity and state:

- `hive-id`;
- `hive-state.json`;
- `gh-app-key*.pem`, the GitHub App private keys;
- `beads/`, the agent work ledger.

Hub deployments also keep `hub-registry.json` and `clusters.json` in `/data`. A standalone Compose spoke has neither.

Regenerable bulk state such as `nous/`, `home/`, and `logs/` can be excluded from a backup. `docker compose down -v` deletes the named volume.

### `hive-backup run` does not apply

`hive-backup run` is the Kubernetes hub disaster-recovery path. It cannot run on a Docker Compose deployment. `pkg/hubbackup/run.go` calls `LoadTargets(dataDir)`, which hard-fails when `clusters.json` is absent. `KubectlSecretCollector` collects hub Kubernetes Secrets through in-cluster kubectl. A Compose deployment has no `clusters.json` and no in-cluster kubectl. Use the spoke backup and the host-level pattern below.

### Spoke backup works

The `pkg/spokebackup` path works for a Compose spoke. It reads the data directory from `HIVE_SPOKE_BACKUP_DATA_DIR`, which defaults to `/data`. The dashboard serves it from inside the same hive container.

- `GET /api/backup/status` reports whether a backup can run.
- `POST /api/backup` builds and streams an encrypted archive.

Both endpoints are owner-only. They require the `X-Hive-Role: owner` header. They require `HIVE_BACKUP_KEY` in the container environment, a 64-character hex AES-256 key. The shipped compose files do not pass `HIVE_BACKUP_KEY`. Add it to `.env` and add `HIVE_BACKUP_KEY=${HIVE_BACKUP_KEY}` to the `environment` block of the `hive` service.

The archive contains the spoke config files (`hive.yaml.dashboard`, `hive.yaml.runtime` or the legacy `hive.yaml.bak`), `hive-id`, `hive-state.json`, the GitHub App private keys, and `beads/`. It is encrypted and sized for browser download, not nightly fleet DR.

### Host-level backup pattern

Back up the named volume and the secrets directory from the host:

```bash
docker run --rm -v v2_hive-data:/data -v "$(pwd)":/backup alpine tar czf /backup/hive-data-$(date +%F).tar.gz -C /data .
tar czf secrets-$(date +%F).tar.gz ./secrets
```

Docker Compose prefixes named volumes with the project name. With `cd v2 && docker compose up -d`, the project is `v2`, so the volume is `v2_hive-data`. `docker volume ls` shows the real name.

To restore:

1. Create a fresh named volume with `docker volume create v2_hive-data`. `docker compose up -d` also creates it when it is missing.
2. Extract the volume tarball into the volume:

   ```bash
   docker run --rm -v v2_hive-data:/data -v "$(pwd)":/backup alpine tar xzf /backup/hive-data-$(date +%F).tar.gz -C /data
   ```

   Use the archive name from the backup step. The example recreates the current-date name.

3. Extract the secrets tarball over the host directory:

   ```bash
   tar xzf secrets-$(date +%F).tar.gz
   ```

4. Run `docker compose up -d`.

The entrypoint restores the runtime config at boot. Escrow `HIVE_BACKUP_KEY` outside the host.

Fixes #2986.
Fixes #2942.
