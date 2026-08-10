# Hive backup and restore

Hive has two backup paths with different scopes.

## Hub disaster recovery: `hive-backup`

`v2/cmd/hive-backup` creates encrypted hub disaster-recovery archives. It captures:

- hub SaaS state under `/data/saas/**`;
- `/data/hub-registry.json`;
- required hub Kubernetes Secrets (`hive-hub-secrets`, `oci-api-key`, `hive-hub-kubeconfigs`, and `hive-hub-tls` when present);
- each spoke's authoritative config files (`hive.yaml.dashboard`, `hive.yaml.runtime` or legacy `hive.yaml.bak`, `hive-id`) and GitHub App key files.

It deliberately excludes regenerable bulk state such as hub `nous/`, `home/`, `beads/`, and `logs/` so a nightly fleet backup stays small and restorable.

`HIVE_BACKUP_KEY` is required and must be a 64-character hex AES-256 key. Escrow it outside the cluster; a backup encrypted by a key that only exists in the lost cluster is not recoverable.

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

`extract` decrypts into a directory and verifies hashes. It does **not** mutate a live cluster. A restore operator should inspect the extracted `MANIFEST.json`, re-apply the captured Secret JSON to the new hub namespace, copy `hub/` files back to the hub PVC, and recreate or patch spokes from `spokes/<hive-id>/`. Any `spoke_errors` in the manifest are honest gaps: those spokes were not captured and need separate recovery.

## Kubernetes CronJob

`v2/deploy/k8s/backup-cronjob.yaml` wires the hub DR path into Kubernetes. It creates:

- ServiceAccount `hive-hub-backup` in namespace `hive-hub`;
- a ClusterRole/Binding that can read Secrets/namespaces/pods and exec into spoke pods to read their PVC state;
- a daily `CronJob` scheduled at `17 3 * * *`, `concurrencyPolicy: Forbid`, one-hour active deadline;
- ConfigMap `hive-hub-backup-config` with object-storage bucket and retention.

Before applying it, create Secret `hive-hub-backup-key` with key `backup-key`, ensure `oci-api-key` and `hive-hub-kubeconfigs` exist, and confirm the PVC claim name (`hive-hub-data-rwx`) matches your deployment.

## Owner-triggered spoke backup

`pkg/spokebackup` is complementary: it backs up one spoke on demand for its owner and includes the spoke's bead ledger. It is encrypted with the same `HIVE_BACKUP_KEY` mechanism and is sized for browser download, not nightly fleet DR. It includes `hive.yaml.dashboard`, runtime config files, `hive-id`, `hive-state.json`, GitHub App keys, and `/data/beads/*`; it skips bulk/derived data and live dashboard sessions.

Fixes #2986.
Fixes #2942.
