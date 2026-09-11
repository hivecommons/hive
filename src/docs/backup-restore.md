# Hive backup and restore

Hive has two backup paths with different scopes: nightly encrypted hub disaster-recovery archives, and on-demand per-spoke backups an owner can download from the dashboard.

> **See also:** [Hub disaster recovery](https://github.com/hivecommons/hive/blob/v4/docs/HUB_DISASTER_RECOVERY.md) — the full hub-level runbook (key escrow, spoke fleet recovery, Slack blast, rebuild from zero) that the `hive-backup` archives described here feed into. For moving a live hive to a **different** host or cluster (as opposed to backing it up in place), see [Moving a Hive between hosts, same runtime](move-host.md), [Self-hosted Kubernetes cluster move](move-kubernetes.md) and [Hub-registered hive cutover](move-hub-registered-cutover.md); [Cross-runtime moves](move-cross-runtime.md) covers Podman ↔ Docker cross-host and Compose/Quadlet ↔ Kubernetes, using the same-host Docker → Podman migration below as its reference case.

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
- a namespaced Role granting `get` (only) on exactly four named Secrets — `hive-hub-secrets`, `oci-api-key`, `hive-hub-kubeconfigs`, `hive-hub-tls` — plus a ClusterRole limited to `pods [get,list]` and `pods/exec [create]` for reading spoke PVC state (hardened in [#3719](https://github.com/hivecommons/hive/pull/3719) and [#3810](https://github.com/hivecommons/hive/pull/3810); the unused `namespaces` grant was removed — spoke namespaces derive as `hive-hosted-<id>` from the registry);
- a daily `CronJob` scheduled at `17 3 * * *`, `concurrencyPolicy: Forbid`, one-hour active deadline, running `ghcr.io/hivecommons/hive:stable` with `imagePullPolicy: Always` (the backup image tracks the stable [release channel](release-channels.md), [#3810](https://github.com/hivecommons/hive/pull/3810));
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

## Restoring a spoke backup archive

> **Status: the decrypt/extract step and the `hive-backup restore` command below are OBSERVED — executed while writing this section, command output included. Loading the restored `/data` into a real container and rebooting it is DOCUMENTED, NOT EXECUTED — no container runtime was available to prove that half.**

This answers epic [#6521](https://github.com/hivecommons/hive/issues/6521) item #16, [#6527](https://github.com/hivecommons/hive/issues/6527) and [#6529](https://github.com/hivecommons/hive/issues/6529): **restoring a `pkg/spokebackup` archive into a fresh deployment is a single command.** `pkg/spokebackup` deliberately reuses `pkg/hubbackup`'s builder (`src/pkg/hubbackup/builder_export.go:1-22`, "so `Verify` and `Extract` accept both archive kinds unchanged") — it produces the same AES-256-GCM-sealed, SHA-256-manifested tar.gz that `hive-backup` already knows how to decrypt.

There are two verbs, and the difference matters:

| | `hive-backup extract` | `hive-backup restore` |
| --- | --- | --- |
| Output | a plain directory still carrying the archive's `spoke/` and `beads/` prefixes | a populated spoke data directory |
| Path mapping | none — prefixes preserved | `spoke/` → data-dir root, `beads/<agent>/` → `<data-dir>/beads/<agent>/` |
| Identity guard | none | refuses a destination that belongs to a different hive |
| Use it for | inspecting an archive; a **hub** disaster-recovery archive, whose Secret/PVC reassembly is manual | putting a **spoke** archive back on a deployment |

### What the archive contains, and where each path goes on restore

The archive has two top-level directories (`src/pkg/spokebackup/backup.go:111-116`):

| Archive path | Container path (relative to `HIVE_SPOKE_BACKUP_DATA_DIR`, default `/data`) | Source |
| --- | --- | --- |
| `spoke/hive.yaml.dashboard` | `/data/hive.yaml.dashboard` | `configOverlayFile`, `backup.go:65` |
| `spoke/hive.yaml.runtime` | `/data/hive.yaml.runtime` | `configRuntimeFile`, `backup.go:88` |
| `spoke/hive.yaml.bak` | `/data/hive.yaml.bak` | `configRuntimeFileLegacy`, `backup.go:94` (present only on hives that have not saved config since the rename) |
| `spoke/hive-id` | `/data/hive-id` | `hiveIDFile`, `backup.go:97` |
| `spoke/hive-state.json` | `/data/hive-state.json` | `stateFile`, `backup.go:100` |
| `spoke/gh-app-key*.pem` | `/data/gh-app-key*.pem` | `appKeyGlob`, `backup.go:105`, matched with `filepath.Glob` (`backup.go:255`) |
| `beads/<agent>/**` | `/data/beads/<agent>/**` | `beadsSubdir`/`beadsPrefix`, `backup.go:58,114` — one subtree per agent, discovered from the archive rather than a fixed list |
| `MANIFEST.json` | (not restored — it is metadata, verified by `Extract`, not spoke state) | `hubbackup` manifest format |

The mapping is a flat rename of the two archive prefixes (`spoke/` → data-dir root, `beads/` → data-dir `beads/`) — there is no repacking, renaming, or transformation needed. This is exactly the file set the entrypoint reads at boot: `HIVE_CONFIG_RUNTIME`/`HIVE_CONFIG_RUNTIME_LEGACY`/`hive.yaml.dashboard` (`src/deploy/entrypoint.sh:69-70,262-266,533-550`), the beads directory it symlinks into `/home/dev/<agent>-beads` and chowns per-agent (`entrypoint.sh:894-931`), and `gh-app-key*.pem`, read directly from `/data` (`src/pkg/dashboard/api.go:6861`, `src/pkg/hub/cluster_app_key.go:393`).

### Decrypting the archive — executed

`hive-backup extract` (`src/cmd/hive-backup/main.go`) calls `hubbackup.Extract`, which is format-agnostic: it verifies the manifest's SHA-256 digests, then untars every member under `-dest`, preserving the archive's own path prefixes. It was run here against a real spoke-backup archive built by `spokebackup.Build` from a synthetic `/data`-shaped directory (a `hive-id`, a `gh-app-key.pem`, both config files, `hive-state.json`, and one bead file under `beads/agentA/`), sealed with a throwaway AES-256 key:

```
$ hive-backup extract -file archive.tar.gz.enc -dest restored
extracted 6 files to restored
$ find restored -type f
restored/MANIFEST.json
restored/spoke/hive-id
restored/spoke/hive.yaml.dashboard
restored/spoke/hive.yaml.runtime
restored/spoke/hive-state.json
restored/spoke/gh-app-key.pem
restored/beads/agentA/bead-0001.json
$ cat restored/spoke/hive-id
test-hive-id-12345
$ sha256sum restored/spoke/gh-app-key.pem      # matches the pre-backup file exactly
411a57f4397884ca23a03fc467c8804a959cdbc5511b2645099013aaa079e790  restored/spoke/gh-app-key.pem
$ cat restored/beads/agentA/bead-0001.json
{"agent":"agentA","bead":1}
```

`hive-id`, the GitHub App key (byte-for-byte, confirmed by SHA-256), and the bead ledger all round-tripped unchanged. `hive-state.json` also round-tripped here only because nothing was running to rewrite it — on a live restore expect it to be overwritten at the next boot, the same caveat the Podman restore section above notes for the same file.

### Placing the files: `hive-backup restore` — executed

`hive-backup restore` does the extraction *and* the placement, applying the mapping in the table above and refusing to overwrite a destination that belongs to a different hive ([#6529](https://github.com/hivecommons/hive/issues/6529)).

```sh
export HIVE_BACKUP_KEY=<the SOURCE hive's escrowed 64-hex key>

# See what would happen first. Writes nothing, and still runs the identity check.
hive-backup restore -file hive-spoke-backup-<id>-<ts>.tar.gz.enc -dest /data -dry-run

hive-backup restore -file hive-spoke-backup-<id>-<ts>.tar.gz.enc -dest /data
```

Executed here against a real archive built by `spokebackup.Build` from a synthetic `/data`-shaped directory (`hive-id`, a `gh-app-key.pem`, both config files, `hive-state.json`, and two agents' bead ledgers):

```
$ hive-backup restore -file archive.tar.gz.enc -dest ./target-fresh -dry-run
would restore 7 files and 2 bead directories to ./target-fresh
  archive hive-id: hive-src-999
  beads/quality/beads.json
  beads/scanner/beads.json
  gh-app-key.pem
  hive-id
  hive-state.json
  hive.yaml.dashboard
  hive.yaml.runtime
  not restored (archive metadata or outside the spoke layout): 1
    - MANIFEST.json

$ hive-backup restore -file archive.tar.gz.enc -dest ./target-fresh
restored 7 files and 2 bead directories to ./target-fresh
  archive hive-id: hive-src-999
  ...
  (re)start the hive container to apply ownership and load the restored config

$ stat -c '%a %n' target-fresh/gh-app-key.pem target-fresh/hive-id
600 target-fresh/gh-app-key.pem
600 target-fresh/hive-id

$ sha256sum src-data/gh-app-key.pem target-fresh/gh-app-key.pem
997d7dab9ff6688a2f76e08fb77996becd53621bdfa1d21d0a453ee1d87bc779  src-data/gh-app-key.pem
997d7dab9ff6688a2f76e08fb77996becd53621bdfa1d21d0a453ee1d87bc779  target-fresh/gh-app-key.pem
```

Points worth knowing before you run it:

1. **Supplying the encryption key.** The key is never in the archive by design (`src/pkg/spokebackup/backup.go`, "Encryption" doc comment: "the archive must be encrypted... The key is deliberately NOT embedded in, or derivable from, the archive"). `restore` resolves it exactly as `extract` does — `hubbackup.LoadKey`, i.e. `HIVE_BACKUP_KEY` in the environment the `hive-backup` binary runs in. **The escrowed key from the source hive is required — the target's own key setting, if any, is irrelevant to decrypting a foreign archive.** If the key was rotated after the archive was taken, the *old* key is what decrypts it (see "Escrow the key" above).
2. **The identity guard, and when it refuses.** `restore` compares the archive's `spoke/hive-id` against `<dest>/hive-id` before writing anything:

   | Destination | Archive | Result |
   | --- | --- | --- |
   | no `hive-id` (fresh) | any | proceeds |
   | same `hive-id` | same | proceeds — the ordinary "restore my own hive" recovery |
   | different `hive-id` | different | **refuses**; `-force` overrides |
   | has a `hive-id` | no `hive-id` | **refuses** — nothing proves the archive is for this hive; `-force` overrides |

   A refusal is a no-op: the destination is left byte-for-byte as it was, and no GitHub App key is written. Observed:

   ```
   $ hive-backup restore -file archive.tar.gz.enc -dest ./target-other
   ERROR restore failed err="destination already belongs to hive \"hive-someone-else\" but the
     archive is for hive \"hive-src-999\"; restoring would splice one hive's config and GitHub
     App keys onto another's identity — pass -force to restore anyway"
   $ echo $?
   1
   ```
3. **Where "the target `/data`" is.** On Kubernetes it is the spoke's PVC — run `restore` from an init container or a temporary debug pod mounting the same PVC before the hive Deployment's pod starts (or into a scaled-down Deployment's pod). On Compose/Quadlet it is the `hive-data` volume — use the same container-mediated pattern as the [host-level backup pattern](#host-level-backup-pattern) / [Podman restore](#restore) sections above. The `hive-backup` binary ships inside the hive image (`src/Dockerfile`), so a debug pod or `podman exec` on the hive image already has it.
4. **A restore merges into the bead ledger rather than replacing it.** An agent present at the destination but absent from the archive keeps its beads; an agent in both gets the archive's copy. If you want a clean slate, empty `<dest>/beads` first.
5. **Modes, and what `restore` does not do.** Every restored root file — the config pair, `hive-id`, `hive-state.json` and the GitHub App keys — is written `0600`, so a hand-built archive cannot widen a credential to world-readable. Ownership is *not* set: the entrypoint re-applies `0600` to the config files (`entrypoint.sh:262-266`, `hive_harden_runtime_config`) and re-chowns `beads/<agent>` to each agent's runtime UID (`entrypoint.sh:894-931`) on every boot, so a restore run from a host shell does not need to reproduce container-internal UIDs.
6. **(Re)start the container**, then **verify** the same way the Podman section above does: `hive-id` and the GitHub App key SHA-256 must match the source hive; the bead ledger and dashboard config overlay must be present.

`hive-state.json` round-trips, but on a live restore expect it to be overwritten at the next boot — the same caveat the Podman restore section above notes for the same file.

### Still manual: the fully hosted, no-shell case

`restore` closes the tooling gap for anyone who can run a command against the target's `/data` — a debug pod, `podman exec`, an init container. It does **not** cover a hosted owner with no shell access at all: `src/pkg/dashboard/backup_api.go` still implements only `GET /api/backup/status` and `POST /api/backup` (download), with no inbound upload/restore endpoint. That remains the optional second half of [#6529](https://github.com/hivecommons/hive/issues/6529); it needs a decision on what an in-place restore means for a *running* dashboard, which holds config in memory and would rewrite it on the next save, and which cannot restart its own process to pick the restored files up.

## Standalone deployments: which section applies

A standalone Hive runs on one host with no Kubernetes cluster. There are two supported runtimes and the host-level procedures are **not** interchangeable — the volume has a different name, the ownership on disk is different, and one of them relabels its mounts:

| Runtime | Deployment asset | Section |
| --- | --- | --- |
| Docker Compose | `src/docker-compose.yaml` | [Docker Compose](#docker-compose) |
| Podman + Quadlet | `src/deploy/quadlet/*` | [Podman (Quadlet)](#podman-quadlet) |

Everything that runs *inside* the hive container is the same on both: the spoke backup endpoints, the encryption-key handling, and the reasons `hive-backup run` does not apply. Those are described once, in the Docker Compose section, and the Podman section says which of them carry over unchanged rather than repeating them.

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

Both endpoints are owner-only. They require the `X-Hive-Role: owner` header. They require a 64-character hex AES-256 key from either source: set it from **Governor Config → Security → Backup** (stored on the `/data` volume, so it survives `docker compose up` cycles), or pass `HIVE_BACKUP_KEY` in the container environment. The shipped compose files do not pass `HIVE_BACKUP_KEY`; to use the env path, add it to `src/.env` — the project directory `-f src/docker-compose.yaml` implies, and the only `.env` Compose reads (#4477) — and add `HIVE_BACKUP_KEY=${HIVE_BACKUP_KEY}` to the `environment` block of the `hive` service.

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

## Podman (Quadlet)

`src/deploy/quadlet/*` is the same standalone deployment on Podman, driven by systemd. [ADR-0017](adr/0017-podman-quadlet-lifecycle.md) chose Quadlet; [podman-standalone-quadlet.md](podman-standalone-quadlet.md) is the install, [podman-volume-persistence.md](podman-volume-persistence.md) characterises what `hive-data` guarantees, and [podman-quadlet-lifecycle.md](podman-quadlet-lifecycle.md) proves it survives a recreate. This section is how to get the data **out** and back **in** (#4406).

Observed values below are marked as such. Everything under [Podman: what was observed](#podman-what-was-observed) was executed on the host described there; anything not executed says so.

### What carries over from Docker unchanged

Say this first, because it keeps the rest short. These run *inside* the hive container and are runtime-agnostic — they work on a Quadlet deployment exactly as written above, with no Podman-specific step:

- `pkg/spokebackup`, and the owner-only `GET /api/backup/status` / `POST /api/backup` endpoints.
- The backup encryption key, its escrow gate, and the `GET`/`PUT`/`DELETE /api/config/governor/backup` management endpoints. The dashboard-set key lives on the `hive-data` volume, so the same warning applies with more force: losing that volume loses both the key and the data it protects.
- The archive contents, and the fact that regenerable bulk state (`nous/`, `home/`, `logs/`) can be excluded.
- `hive-backup run` still **does not apply**, for the reason already given: no `clusters.json`, no in-cluster kubectl. That is a property of a standalone deployment, not of Docker.

What is different is everything at the *host* level, and it is not a search-and-replace of `docker` for `podman`.

### The volume is called `hive-data`, with no prefix

Docker Compose prefixes named volumes with the project name, which is why the Docker section says `src_hive-data`. Quadlet does not: `hive-data.volume` sets `VolumeName=hive-data` explicitly, so the volume is `hive-data` in both root modes.

**Following the Docker commands under Podman restores into a volume nothing mounts.** `podman volume create src_hive-data` succeeds, the extract succeeds, and `hive.service` then starts on the empty `hive-data` — which, per [podman-quadlet-lifecycle.md](podman-quadlet-lifecycle.md), starts *cleanly* and healthy. There is no error to notice.

| | Docker Compose | Podman Quadlet (rootless) | Podman Quadlet (rootful) |
| --- | --- | --- | --- |
| volume name | `src_hive-data` | `hive-data` | `hive-data` |
| host path | `/var/lib/docker/volumes/src_hive-data/_data` | `~/.local/share/containers/storage/volumes/hive-data/_data` | `/var/lib/containers/storage/volumes/hive-data/_data` |
| owner on disk, of `/data/hive-id` | `1001:1000` | **`525288:525287`** | `1001:1000` |

That rootless row is the whole reason the next part exists.

### Rootless: never `tar` the volume from the host shell

A rootless container runs in a user namespace, so the UIDs on disk are not the UIDs in the container. On the host measured here, `/etc/subuid` reads `dbaggett:524288:65536`, so container UID 1001 (`dev`) is host UID 525288 and container GID 1000 is host GID 525287.

Two things follow, and the second one is the dangerous one:

1. A host-shell tar records the **mapped** IDs. Restoring that archive on another host — with a different subuid base — puts every file under an identity that host's container does not have.
2. A host-shell tar **cannot read the GitHub App private key at all.** It is mode `0600` owned by host UID 525288, and the operator is UID 1000. Observed:

   ```
   $ tar czf hive-data.tar.gz -C "$VOLUME_PATH" .
   tar: ./gh-app-key-restore-probe.pem: Cannot open: Permission denied
   tar: Exiting with failure status due to previous errors
   ```

   `tar` exits non-zero **and still writes a tarball**. An operator who does not check the exit status has a backup that is missing the one file the backup exists to protect.

Use any of the three forms that go through the namespace instead. All three were measured to record container-side ownership (`1001/1000`) and to read the key:

Podman image references here are fully qualified on purpose: the Quadlet generator warns on a short name, and a short name resolves through whatever `unqualified-search-registries` happens to say on the host.

```sh
# 1. podman's own export -- fastest, and it is namespace-aware
podman volume export hive-data -o hive-data-$(date +%F).tar

# 2. a container that already has the mapping -- portable, and the same
#    shape as the Docker command, which matters for migration
podman run --rm -v hive-data:/data -v "$PWD":/backup:z docker.io/library/alpine:3.22 \
  tar czf /backup/hive-data-$(date +%F).tar.gz -C /data .

# 3. a host shell placed inside the namespace
podman unshare tar czf hive-data-$(date +%F).tar.gz -C "$(podman volume inspect hive-data --format '{{.Mountpoint}}')" .
```

Rootful has no user namespace, so host UIDs *are* container UIDs and a host-shell tar is not wrong there. Use the same commands anyway, with `sudo`: it keeps one procedure for both modes, and `podman volume export` is still the fastest.

### Also back up the config and secrets

Besides the volume, `hive.container` mounts `%E/hive/hive.yaml` and `%E/hive/secrets`, and reads `%E/hive/hive.env` through `EnvironmentFile=`; `hive-gateway.container` mounts `%E/hive/nginx.conf`. `%E/hive` is `~/.config/hive` rootless and `/etc/hive` rootful, so one archive covers all four.

```sh
CONF=~/.config/hive          # rootful:  CONF=/etc/hive
tar czf hive-config-$(date +%F).tar.gz -C "$(dirname "$CONF")" "$(basename "$CONF")"
# -> hive.yaml, hive.env, nginx.conf, secrets/
```

Two Podman-specific notes on restoring that archive:

- **`%E/hive/secrets` needs its ownership re-applied after extraction**, with the same command the install uses — `podman unshare chown -R 0:1002 "$CONF/secrets"` rootless, `chgrp -R 1002 "$CONF/secrets"` rootful. Observed on disk: rootless `uid=1000 gid=525289 mode=750`, rootful `uid=0 gid=1002 mode=750`. The container reads the keys as `dev` through the `hive-launch` group (GID 1002); a plain extract as the operator does not reproduce that.
- **Do not try to restore the SELinux labels.** The bind mounts carry `:Z`, so Podman stamps a *private MCS category pair* on them — observed `system_u:object_r:container_file_t:s0:c269,c605` on the rootless deployment and `system_u:object_r:container_file_t:s0:c580,c750` on the rootful one. The pair belongs to the mount, not to the backup: it is not something an archive should capture or a restore should recreate, and Podman re-applies it. `hive.env` is the exception and stays at its ordinary host label (`unconfined_u:object_r:config_home_t:s0`), because `EnvironmentFile=` is read by systemd on the host and never bind-mounted.

### `docker compose down -v`, and its Podman counterpart

`docker compose down -v` removes the containers, the network, **and the named volume**. The Quadlet counterpart is a repository script:

```sh
bin/hive-podman-teardown.sh plan            # prints what it would remove; removes nothing
bin/hive-podman-teardown.sh run --yes       # DESTRUCTIVE: deletes the hive-data volume
```

It selects on the [#4210 ownership labels](podman-ownership-cleanup.md) and nothing else, and unlike `docker compose down -v` it has a `plan` mode. Add `--rootful` context by running it under `sudo`.

Stopping the service is **not** destructive and never has been: `systemctl stop hive.service` leaves the volume in place, measured in [podman-quadlet-lifecycle.md](podman-quadlet-lifecycle.md). The minimal destructive form, if you want to do it by hand:

```sh
systemctl --user stop hive.service              # not destructive
systemctl --user stop hive-data-volume.service  # not destructive
podman volume rm hive-data                      # DESTRUCTIVE -- this is the down -v step
```

### Restore

**Recreate the volume through its unit, not with `podman volume create`.** Both give you an empty `hive-data`; only one of them is Hive's. Observed:

| recreate with | labels on the volume | `bin/hive-podman-teardown.sh plan` sees it |
| --- | --- | --- |
| `podman volume create hive-data` | `{}` | **no** — reported as unlabelled, never removed |
| `systemctl --user start hive-data-volume.service` | the four `io.kubestellar.hive.*` labels | yes — "volumes (1): hive-data" |
| `bin/hive-podman-setup.sh`, volume unit stale-`active` | the four `io.kubestellar.hive.*` labels — the installer restarts the unit when the volume is missing (#4485) | yes |

A volume created the first way is invisible to the teardown's selection, which is deliberately the ownership labels and nothing else. It used to survive silently, forever; since #4485 three things stand between you and that state:

- `hive.service` refuses to start unless `hive-data` exists **with** its ownership labels, so `podman run -v hive-data:/data` never gets the chance to auto-create it unlabelled — the third row above used to produce exactly that, when the volume unit was stale-`active` from a previous boot while the volume it created was gone.
- `bin/hive-podman-setup.sh` checks the volume itself rather than trusting the unit's state, restarts the unit when the volume is missing, and stops the install when the volume exists without its labels.
- `bin/hive-podman-teardown.sh` reports an unlabelled `hive-data` — "exists but carries no Hive ownership labels; this teardown will not touch it" — instead of folding it into "no Hive-owned volumes". It still never removes it.

The full cycle, rootless (prefix each command with `sudo` and drop `--user` for rootful):

```sh
# 1. stop the service. Not destructive.
systemctl --user stop hive.service

# 2. replace the volume. THIS DELETES DATA.
systemctl --user stop hive-data-volume.service
podman volume rm hive-data

# 3. recreate it through its unit, so it carries the ownership labels
systemctl --user start hive-data-volume.service

# 4. load the archive back in
podman volume import hive-data hive-data-2026-08-21.tar
#    ...or, for a .tar.gz taken with form 2 above, or one that came from Docker:
podman run --rm -v hive-data:/data -v "$PWD":/backup:z docker.io/library/alpine:3.22 \
  tar xzf /backup/hive-data-2026-08-21.tar.gz -C /data

# 5. start. With Notify=healthy this returns only once /api/health answers.
systemctl --user start hive.service
```

`podman volume import` requires an **uncompressed** tar and an **existing** volume; it does not create one, which is why step 3 is separate.

Rootless, the `:z` form in step 4 only works on an archive the operator owns. Any archive written by a root process — anything that came from Docker, or that was taken under `sudo` — is root-owned, and rootless Podman cannot relabel a file it does not own, so `:z` skips it and the extract is denied. `sudo chown "$(id -u):$(id -g)" <archive>` first; the [migration section](#docker--podman-migration) has the mechanism.

Then check the thing that actually matters — that this is the same hive, not a fresh one wearing its name:

```sh
podman exec hive cat /data/hive-id
podman exec hive sh -c 'ls -ln /data/gh-app-key*.pem'
```

### Docker → Podman migration

**The two volumes are never shared, and there is no supported way to point Podman at Docker's storage.** This is not a caution, it is what the engines do. Observed, with a live Docker deployment on the same host:

```
$ podman volume inspect src_hive-data
Error: no such volume src_hive-data          # and identically under sudo
```

Podman keeps its own volume registry; a Docker volume is not in it under any name. The fast wrong thing — bind-mounting Docker's volume directory straight into a Podman container — does not work either, and fails before it can do damage:

```
$ podman run --rm -v /var/lib/docker/volumes/src_hive-data/_data:/data:ro alpine ls /data
Error: statfs /var/lib/docker/volumes/src_hive-data/_data: permission denied
```

`/var/lib/docker` is `0710 root:root`, so a rootless operator's own shell cannot read it either. Even where root could, doing it would leave two engines with different label and ownership expectations writing the same directory.

**Migration is therefore an export and import of contents.** Both sides go through a container, which is what makes the ownership come out right on the Podman side:

```sh
# 1. from Docker. Note the project-prefixed volume name.
docker run --rm -v src_hive-data:/data -v "$PWD":/backup alpine \
  tar czf /backup/docker-hive-data.tar.gz -C /data .

# 2. stop the Docker deployment. NOT `down -v` yet -- keep the source data
#    until the Podman side is proven healthy.
docker compose -f src/docker-compose.yaml stop

# 2a. take ownership of the archive. Docker's daemon is root, so the file it
#     just wrote is root-owned -- and rootless Podman cannot relabel a file it
#     does not own, so the `:z` in step 3 would silently skip it and the
#     extract would fail with "Permission denied". Rootful Podman does not
#     need this step.
sudo chown "$(id -u):$(id -g)" docker-hive-data.tar.gz

# 3. on the Podman side: create the volume through its unit, then extract.
#    If hive-data already exists with other content, wipe it first --
#    steps 1-3 of Restore above. `tar xzf` merges into whatever is there.
systemctl --user start hive-data-volume.service
podman run --rm -v hive-data:/data -v "$PWD":/backup:z docker.io/library/alpine:3.22 \
  tar xzf /backup/docker-hive-data.tar.gz -C /data

# 4. start, and confirm it is the SAME hive
systemctl --user start hive.service
podman exec hive cat /data/hive-id       # must match what Docker reported

# 5. only now, once step 4 matched: docker compose -f src/docker-compose.yaml down -v
```

**Step 2a is not housekeeping, and its failure is an SELinux denial rather than an ownership one.** The archive Docker writes is mode `0644` — world-readable, so ownership alone would not stop the extract. What differs is the label. Docker's rootful daemon writes into the operator's directory and the file inherits that directory's type, typically `user_tmp_t`; a file the operator wrote through Podman in the same directory carries `container_file_t`. The `:z` in step 3 exists to relabel the mount to `container_file_t`, but relabelling is `chcon`, and `chcon` needs ownership or `CAP_FOWNER` — neither of which a rootless user has over a root-owned file. Podman skips the file, says nothing about it, and the container domain is denied:

```
$ podman run --rm -v hive-data:/data -v "$PWD":/backup:z docker.io/library/alpine:3.22 \
    tar xzf /backup/docker-hive-data.tar.gz -C /data
tar: can't open '/backup/docker-hive-data.tar.gz': Permission denied
```

With `chown` run first, that identical command extracts, and the file comes out `container_file_t` — `:z` relabelled it once it was the operator's to relabel. Inside the container the file also appears as `65534:65534` (`nobody`), because host uid 0 is outside the rootless subuid range; that is cosmetic, since `0644` would still permit the read.

**Do not reach for `chmod 777` or `setenforce 0`.** Neither addresses the label, the first is a no-op here (the file is already readable), and the second turns off the enforcement the rest of this page depends on. `bin/hive-podman-preflight-host.sh` makes the same point about the host checks it runs: nothing in the Podman path requires weakening SELinux, and the kernel stays enforcing either way.

Doing it the other way round — `docker run --user "$(id -u):$(id -g)"` in step 1, so the archive is never root-owned in the first place — removes the step, but changes what the Docker-side `tar` can read. `/data` holds `gh-app-key-*.pem` at mode `0600` owned by the container's `dev` uid, not the operator's, so a `tar` run as the operator drops exactly the file the archive exists to carry — and, as [above](#rootless-never-tar-the-volume-from-the-host-shell), exits non-zero while still writing a tarball. Keep step 1 privileged and take ownership afterwards.

**The ordering of steps 4 and 5 is the safety property, not a formality.** A `hive-data` that stayed empty because the extract was denied still starts *cleanly*: `systemctl --user start hive.service` returns rc 0, the container reports `healthy`, and the dashboard answers on the published port — with a freshly minted identity. That is indistinguishable from a successful migration unless the `hive-id` check in step 4 is actually run. An operator who reads "healthy" and skips ahead to `down -v` destroys the source data they were still holding.

Also copy the Docker deployment's `src/hive.yaml` to `%E/hive/hive.yaml` and `src/secrets/` to `%E/hive/secrets/`, then re-apply the secrets ownership as described above. Two settings do **not** carry over and have to be moved by hand: the Compose `environment:` block becomes `%E/hive/hive.env`, and `dashboard.port` must be `3002` to match the unit's `HealthCmd` ([#4367](podman-standalone-quadlet.md)).

## Podman: what was observed

Fedora 44, Podman 5.8.4, systemd 259, cgroup v2, SELinux **enforcing**, Docker 29.7.2, against the real `ghcr.io/hivecommons/hive:stable` image (`sha256:ec8e69bc…`). Three separate deployments, each with its own identity, so a restore returning the wrong data would be visible rather than plausible.

### Backup, wipe, and restore — executed in both root modes

| | rootless | rootful |
| --- | --- | --- |
| hive identity before | `hive-pure-fox` | `hive-neat-ant` |
| `podman volume export` | 0.17s, 46,805,504 B (uncompressed) | executed |
| container `tar czf` | 1.56s, 20,256,240 B (gzip) | — |
| volume removed, then recreated through its unit | empty, 0 files, labels present | same |
| `podman volume import` | 0.38s | executed |
| `systemctl start hive.service` after the restore | rc 0 in **10s**, `active/running`, container `healthy` | rc 0 in **10s**, `active/running`, container `healthy` |
| `hive-id` after the restore | `hive-pure-fox` — unchanged | `hive-neat-ant` — unchanged |
| GitHub App key SHA-256 after the restore | unchanged | unchanged |
| `beads/` agent ledger | unchanged | unchanged |

The container came back **healthy**, not merely started: `Notify=healthy` means `systemctl start` returning is `/api/health` having answered on the restored data.

Of the files checked, `hive-state.json` was the one whose hash **did** change across the restore, and that is not a fault: the running service rewrites it at boot. Use `hive-id` and the key files as the witness that a restore worked — a checksum over the whole volume will not match and is not supposed to.

### Ownership and labels after restore — observed values

Rootless, `~/.local/share/containers/storage/volumes/hive-data/_data`:

| path | before | after `podman volume import` | after the container started |
| --- | --- | --- | --- |
| `_data` (the volume root) | `uid=525288 gid=525287 mode=755` | `uid=1000 gid=1000 mode=755` | corrected by the entrypoint |
| `hive-id` | `uid=525288 gid=525287 mode=644` | `uid=525288 gid=525287 mode=644` | unchanged |
| `gh-app-key-*.pem` | `uid=525288 gid=525287 mode=600` | `uid=525288 gid=525287 mode=600` | unchanged |
| SELinux context | `system_u:object_r:container_file_t:s0` | `unconfined_u:object_r:container_file_t:s0` | unchanged |

Rootful, `/var/lib/containers/storage/volumes/hive-data/_data`: the same, with `uid=1001 gid=1000` throughout because there is no namespace, and the volume root observed at `uid=0 gid=0` immediately after the import and back at `uid=1001 gid=1000` once the container had started.

Two readings of that table are worth having.

The SELinux **type** is `container_file_t` and there is **no MCS category** in every row — which is exactly the property [podman-volume-persistence.md](podman-volume-persistence.md) says a recreated container depends on. Only the SELinux *user* field differs after a restore (`unconfined_u` where it had been `system_u`), because the restore was driven from a host process rather than from a container; extracting through a container instead left it at `system_u`. Neither affects the access decision. **Do not "fix" this with `chcon`, and above all do not add `:Z` to the volume line** — that stamps a private category and the next mount is denied with no AVC to explain it.

The volume-root ownership landing on `0:0` (rootful) or container-root (rootless) after `podman volume import` is real, and it is repaired by the entrypoint on the next start rather than by the operator. Both modes came back healthy from that state.

### Docker → Podman migration — executed

| step | observed |
| --- | --- |
| Docker deployment identity | `hive-calm-colt`, volume `src_hive-data`, healthy |
| `podman volume inspect src_hive-data` | `Error: no such volume src_hive-data`, in **both** root modes |
| bind-mounting Docker's volume dir into a rootless container | `Error: statfs …: permission denied` |
| export from Docker via a container | 20,284,768 B, ownership recorded as `1001/1000` |
| the archive Docker's daemon wrote | root-owned, mode `0644`, ctx `system_u:object_r:user_tmp_t:s0` — against `container_file_t:s0` on a Podman-side archive in the same directory |
| extract of that archive **before** step 2a, rootless | `tar: can't open '/backup/docker-hive-data.tar.gz': Permission denied`. `:z` skipped the root-owned file silently; no AVC names it |
| the same command **after** `chown "$(id -u):$(id -g)"` | extracted; the archive came out `system_u:object_r:container_file_t:s0` — `:z` relabelled it once it was ours |
| extract into a fresh Podman `hive-data` via a container | host `uid=525288 gid=525287`, ctx `system_u:object_r:container_file_t:s0` |
| `systemctl --user start hive.service` on the volume the **denied** extract left empty | rc 0 in **11s**, `active`, container `healthy`, dashboard `{"status":"ok"}` — on a brand-new identity. This is why step 4 precedes step 5 |
| `systemctl --user start hive.service` | rc 0 in **10s**, container `healthy` |
| `hive-id` under Podman afterwards | **`hive-calm-colt`** — the Docker hive, now running on Podman |
| GitHub App key SHA-256 | identical to the Docker side |
| `docker compose down -v` afterwards | removed container, network, and `src_hive-data` |

The four SELinux rows come from a second, independent run of the same procedure on Bluefin 44 (Silverblue), SELinux enforcing, Podman 5.8.4 rootless, Docker 29.7.2 ([#4497](https://github.com/hivecommons/hive/issues/4497)), which is where the missing ownership step was found: that run reproduced the two "this cannot work" results above exactly, and completed the migration — `hive-able-moth` moved from Docker to Podman, carrying a `_migration_marker` planted on the Docker side — but only with step 2a added. The first run recorded here was executed as well; the step that made its rootless extract succeed did not make it onto the page, which is the defect [#4497](https://github.com/hivecommons/hive/issues/4497) reports.

### Not executed, and one caveat about the host

| | Status |
| --- | --- |
| the GitHub App key was a **synthetic** `gh-app-key-*.pem` planted for the test | no real GitHub App was configured on these deployments. What is demonstrated is that a `0600` file owned by `dev` in `/data` round-trips byte-identically, with its mode and ownership, which is the property that matters |
| restore onto a **different** host, with a different `/etc/subuid` base | **not executed** — the mapped-UID argument above is the reason the namespace-aware forms are given, and it is measured here only as the difference between what the two tar forms record |
| restore of an archive taken from an **older** Hive image | **not executed** — that is a schema question, not a volume question |
| `%E/hive` config/secrets archive round-trip | **not executed** as a full cycle; the ownership and label values quoted for it are observed from the live deployments, and the re-apply step is the documented install step |
| #4188's "clean host with **no Docker** installation" | **not demonstrated here.** This host has Docker 29.7.2 installed and running, which is what made the migration section executable rather than theoretical. The Podman procedures above use no Docker component, but that a host without Docker can run them was not proven on this host |

Fixes #2986.
Fixes #2942.
