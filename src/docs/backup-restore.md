# Hive backup and restore

Hive has two backup paths with different scopes: nightly encrypted hub disaster-recovery archives, and on-demand per-spoke backups an owner can download from the dashboard.

## Hub disaster recovery: `hive-backup`

`src/cmd/hive-backup` creates encrypted hub disaster-recovery archives — everything needed to rebuild a hub. It captures:

- hub SaaS state under `/data/saas/**` (users, hives, keys);
- `/data/hub-registry.json`, the fleet registry;
- required hub Kubernetes Secrets — a missing `hive-hub-secrets`, `oci-api-key`, or `hive-hub-kubeconfigs` **fails the backup**, while a missing `hive-hub-tls` only warns (cert-manager can reissue it);
- each spoke's authoritative config files (`hive.yaml.dashboard`, `hive.yaml.runtime` or legacy `hive.yaml.bak`, `hive-id`) and GitHub App key files, read from the spoke via `kubectl exec`. Spokes that could not be read are recorded honestly in `MANIFEST.json` under `spoke_errors` rather than silently dropped.

It deliberately excludes regenerable bulk state such as hub `nous/`, `home/`, `beads/`, and `logs/` so a nightly fleet backup stays small and restorable.

The archive is a tar.gz sealed with AES-256-GCM. Every run re-downloads and verifies the archive that actually landed in object storage before pruning old archives — a run that cannot self-verify fails rather than reporting a good backup.

### The `HIVE_BACKUP_KEY` escrow gate

`HIVE_BACKUP_KEY` is required and must be an AES-256 key, 64 hex characters (deliberately independent of `/data/saas/hmac.key`, which is itself backup payload — deriving the backup key from it would make the archive undecryptable in exactly the disaster it exists for). It has **no default**: an unset key aborts the run rather than writing plaintext.

> **Escrow the key outside the cluster.** A backup encrypted by a key that only exists in the lost cluster is not recoverable.

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
| `HIVE_BACKUP_KEY` | Required AES-256 key, 64 hex characters. No default. |
| `HIVE_BACKUP_BUCKET` | OCI Object Storage bucket. |
| `HIVE_BACKUP_DATA_DIR` | Hub data directory; default `/data`. |
| `HIVE_BACKUP_RETENTION` | Archive count to retain; default `30`. |
| `HIVE_HUB_NAMESPACE` | Hub namespace for Secret collection; default `hive-hub`. |
| `HIVE_KUBECONFIG_DIR` | Mounted kubeconfig directory for remote spoke clusters; default `/etc/hive/kubeconfigs`. |
| `HIVE_BACKUP_OCI_ENDPOINT` | OCI Object Storage endpoint override; defaults to the regional endpoint. |

## Restore

`hive-backup verify -file <archive>` checks integrity: the GCM auth tag rejects a wrong key or any bit-flip. `hive-backup extract -file <archive> -dest ./restore` decrypts into a directory and verifies hashes. Neither mutates a live cluster.

A restore operator should inspect the extracted `MANIFEST.json`, re-apply the captured Secret JSON to the new hub namespace, copy `hub/` files back to the hub PVC, and recreate or patch spokes from `spokes/<hive-id>/`. Any `spoke_errors` in the manifest are honest gaps: those spokes were not captured and need separate recovery.

## Kubernetes CronJob

`src/deploy/k8s/backup-cronjob.yaml` wires the hub DR path into Kubernetes. It is **not deployed by default** — it is absent from `src/deploy/k8s/kustomization.yaml` and nothing in the install path applies it; deployment is an explicit `kubectl apply -f`, gated on the operator having escrowed `HIVE_BACKUP_KEY` first. It creates:

- ServiceAccount `hive-hub-backup` in namespace `hive-hub`;
- a namespaced Role granting `get` (only) on exactly four named Secrets — `hive-hub-secrets`, `oci-api-key`, `hive-hub-kubeconfigs`, `hive-hub-tls` — plus a ClusterRole limited to `pods [get,list]` and `pods/exec [create]` for reading spoke PVC state (hardened in [#3719](https://github.com/kubestellar/hive/pull/3719) and [#3810](https://github.com/kubestellar/hive/pull/3810); the unused `namespaces` grant was removed — spoke namespaces derive as `hive-hosted-<id>` from the registry);
- a daily `CronJob` scheduled at `17 3 * * *`, `concurrencyPolicy: Forbid`, one-hour active deadline, running `ghcr.io/kubestellar/hive:stable` with `imagePullPolicy: Always` (the backup image tracks the stable [release channel](release-channels.md), [#3810](https://github.com/kubestellar/hive/pull/3810));
- ConfigMap `hive-hub-backup-config` with object-storage bucket (default `hive-hub-backups`, inherited silently if you don't edit it) and retention.

Before applying it, create Secret `hive-hub-backup-key` with key `backup-key`, ensure `oci-api-key` and `hive-hub-kubeconfigs` exist, and confirm the PVC claim name (`hive-hub-data-rwx`) matches your deployment. Archives upload to OCI Object Storage, so allow egress to `objectstorage.<region>.oraclecloud.com` from the hub.

## Owner-triggered spoke backup

`pkg/spokebackup` is complementary: it backs up one spoke on demand for its owner and includes the spoke's bead ledger. It uses the same AES-256-GCM sealing as the hub path and is sized for browser download, not nightly fleet DR. It includes `hive.yaml.dashboard`, runtime config files, `hive-id`, `hive-state.json`, GitHub App keys, and `/data/beads/*`; it skips bulk/derived data and live dashboard sessions.

### Setting the backup encryption key (hosted flow)

A hive owner sets the key from the dashboard: **Governor Config → Security → Backup → Set key**. This is the supported path for hosted hives, whose owners have no deployment-env or cluster access.

```bash
openssl rand -hex 32   # paste the 64-hex-character result into the dialog
```

What happens on save:

- the value is written to `/data/secrets/backup_encryption_key`, mode `0600`;
- `hive.yaml` records only the **path** (`governor.backup.key_file`) and the optional label (`governor.backup.key_name`) — never the value;
- the key never appears in an API response, in a log line, or in the archive itself.

Resolution order at backup time is governor config first, then the environment:

1. `governor.backup.key_file` (set by the dialog above);
2. `/data/secrets/backup_encryption_key` (the PVC default);
3. `/secrets/backup_encryption_key` (an admin-managed Kubernetes Secret mount);
4. `governor.backup.key_env`, if named;
5. `HIVE_BACKUP_KEY` on the deployment — the original path, still supported.

**Security note.** There is no default key and no plaintext fallback: with no key from any source, `GET /api/backup/status` reports `available: false` and `POST /api/backup` returns `412` — a backup is refused rather than written unencrypted, because the archive carries this hive's GitHub App private keys. Clearing the key (**Clear key** in the same panel) restores that refusal.

**Escrow the key.** It is not stored inside the archive, so a backup without its key is unrestorable. Replacing the key does not re-encrypt existing archives; keep the old key to restore them.

## Docker Compose

`src/docker-compose.yaml` is a standalone deployment. It runs the hive container on a single Docker host, without a Kubernetes cluster.

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

Both endpoints are owner-only. They require the `X-Hive-Role: owner` header. They require a 64-character hex AES-256 key from either source: set it from **Governor Config → Security → Backup** (stored on the `/data` volume, so it survives `docker compose up` cycles), or pass `HIVE_BACKUP_KEY` in the container environment. The shipped compose files do not pass `HIVE_BACKUP_KEY`; to use the env path, add it to `.env` and add `HIVE_BACKUP_KEY=${HIVE_BACKUP_KEY}` to the `environment` block of the `hive` service.

Managing the key from the dashboard is also owner-only:

- `GET /api/config/governor/backup` — presence, usability, and a safe source label (`file:<path>` / `env:<NAME>`); never the value.
- `PUT /api/config/governor/backup` — `{"encryptionKey":"<64 hex>","keyName":"escrowed in 1Password"}`.
- `DELETE /api/config/governor/backup` — removes the stored key; backups are refused again.

The archive contains the spoke config files (`hive.yaml.dashboard`, `hive.yaml.runtime` or the legacy `hive.yaml.bak`), `hive-id`, `hive-state.json`, the GitHub App private keys, and `beads/`. It is encrypted and sized for browser download, not nightly fleet DR.

### Host-level backup pattern

Back up the named volume and the secrets directory from the host:

```bash
docker run --rm -v src_hive-data:/data -v "$(pwd)":/backup alpine tar czf /backup/hive-data-$(date +%F).tar.gz -C /data .
tar czf secrets-$(date +%F).tar.gz ./src/secrets
```

Docker Compose prefixes named volumes with the project name. When running `docker compose -f src/docker-compose.yaml up -d`, the volume is `src_hive-data`. `docker volume ls` shows the real name.

To restore:

1. Create a fresh named volume with `docker volume create src_hive-data`. `docker compose -f src/docker-compose.yaml up -d` also creates it when it is missing.
2. Extract the volume tarball into the volume:

   ```bash
   docker run --rm -v src_hive-data:/data -v "$(pwd)":/backup alpine tar xzf /backup/hive-data-$(date +%F).tar.gz -C /data
   ```

   Use the archive name from the backup step. The example recreates the current-date name.

3. Extract the secrets tarball over the host directory:

   ```bash
   tar xzf secrets-$(date +%F).tar.gz
   ```

4. Run `docker compose -f src/docker-compose.yaml up -d`.

The entrypoint restores the runtime config at boot. Escrow the backup encryption key outside the host — the dashboard-set key lives on the `hive-data` volume, so losing the volume loses both the backups' key and the data it protects.

Fixes #2986.
Fixes #2942.
