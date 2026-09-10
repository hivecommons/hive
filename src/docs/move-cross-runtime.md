# Cross-runtime moves: Podman ↔ Docker, Compose/Quadlet ↔ Kubernetes

Part of the [move-a-Hive-deployment epic](https://github.com/hivecommons/hive/issues/6521), secondary scope: [#6528](https://github.com/hivecommons/hive/issues/6528). This is a single combined guide for every runtime *pair* other than same-host Docker → Podman, which is already documented and executed in [Docker → Podman migration](backup-restore.md#docker--podman-migration) — this page references that section rather than duplicating it.

> **Status vocabulary used throughout this page**, matching the epic:
> **OBSERVED** — executed and recorded, cited to where. **DOCUMENTED, NOT EXECUTED** — a procedure written from reading the code and existing docs, but not run. **UNCONFIRMED** — a suspected risk, phrased as a question, not a known bug.
>
> **Everything on this page is DOCUMENTED, NOT EXECUTED** unless a sentence says otherwise and cites an existing doc that recorded the execution. No Podman→Docker, Compose/Quadlet↔Kubernetes move was run while writing this page; there was no second host or cluster available to move a live deployment to.

For same-host, same-runtime moves see [move-host.md](move-host.md) (Docker Compose ↔ Docker Compose, Podman Quadlet ↔ Podman Quadlet) and [move-kubernetes.md](move-kubernetes.md) (Kubernetes cluster ↔ Kubernetes cluster). This page is only the pairs where the runtime itself changes.

## What is common to every pair on this page

Regardless of which two runtimes are involved, a cross-runtime move always has to reconcile the same four things, because they are stored differently by each runtime:

| Concern | Docker Compose | Podman Quadlet | Kubernetes |
| --- | --- | --- | --- |
| Bulk/identity state (`hive-id`, config, beads, GitHub App keys) | Docker named volume `src_hive-data`, mounted `/data` (`backup-restore.md`, "Docker Compose: Durable state") | Podman named volume `hive-data`, no project prefix (`backup-restore.md`, "The volume is called `hive-data`, with no prefix") | PersistentVolumeClaim, default `hive-data`, `ReadWriteOnce`, 10Gi (`src/deploy/k8s/pvc.yaml`) |
| Secrets (GitHub App key / PAT, dashboard token, backend API keys) | `./secrets` bind mount, read-only at `/secrets` (`backup-restore.md`, "Durable state") | `%E/hive/secrets` bind mount, read-only at `/secrets`, `:Z`-labelled *except* the secrets mount itself, which is deliberately not relabelled shared (`src/deploy/quadlet/hive.container:129-137`) | Kubernetes `Secret` (`src/deploy/k8s/secret.yaml`), mounted or read via the API — never a bind-mounted file |
| Runtime config / env | `./hive.yaml` bind mount at `/etc/hive/hive.yaml`; env vars in the Compose `environment:` block | `%E/hive/hive.yaml` bind mount; env vars in `%E/hive/hive.env`, read via `EnvironmentFile=` (`src/deploy/quadlet/hive.container:143-148`) | `ConfigMap` seed (`src/deploy/k8s/configmap.yaml`) merged at boot with the PVC dashboard overlay (`config-layering.md`) |
| Host-bound settings that must change on the target | Published port, any reverse-proxy TLS config | Same, plus SELinux context and rootless UID mapping (below) | `dashboard_url`/heartbeat `dashboardUrl`, TLS Route/Ingress, GitHub App Setup/callback URLs — see [cross-cluster-migration.md](cross-cluster-migration.md) and epic item #12 |

Three things every pair below shares, all still open per the epic and **not resolved by this page**:

- **Concurrent identity (epic item #11).** Every procedure below stops the source before the target starts. Running source and target with the same `hive-id` and GitHub App key at once — on any runtime pair — risks duplicate heartbeats and duplicate PRs; the cutover ordering itself is [#6526](https://github.com/hivecommons/hive/issues/6526)'s scope, not this page's.
- **Agent backend credentials (epic item #13).** None of the procedures below move `home/` (agent CLI state and credential caches) — it is excluded from `pkg/spokebackup` on purpose (`src/pkg/spokebackup/backup.go`, `includedRootFiles` doc comment: "carries third-party tokens that should not be duplicated into an archive") and is not part of the volume-level moves either, by convention with the same reasoning. **UNCONFIRMED**: whether a target that never receives `home/` needs the agent backends (Claude, Codex, Copilot, etc.) re-authenticated after any of these moves — no doc in this repository states either answer.
- **Backup encryption key escrow before a move (epic item #14).** If a move goes through an encrypted spoke-backup archive (as the Compose/Quadlet ↔ Kubernetes procedures below do), the key that sealed it must be available at the target, per [Restoring a spoke backup archive](backup-restore.md#restoring-a-spoke-backup-archive) — the archive never carries its own key.

## Podman → Docker

This is the reverse of the [Docker → Podman migration](backup-restore.md#docker--podman-migration) already executed and documented in `backup-restore.md`. Reversing direction does not just reverse the commands, because the two asymmetries that section documents are direction-specific:

1. **Ownership/UID remap (rootless Podman only).** Rootless Podman's user namespace means files on disk are recorded under the *mapped* host UID (observed `525288:525287` on the host in `backup-restore.md`'s Podman section), not the in-container UID (`1001:1000`). Docker has no such namespace: whatever UID the container runs as (`1001:1000` per `backup-restore.md`'s ownership table) is the UID on disk, full stop. Moving **out of** rootless Podman therefore requires the same namespace-aware export used for backup — `podman volume export`, a container-mediated `tar`, or `podman unshare tar` (`backup-restore.md`, "Rootless: never `tar` the volume from the host shell") — so the archive records the in-container `1001:1000` identity rather than the host-mapped one. A plain host-shell `tar` on the rootless volume would (a) fail to read the `0600` GitHub App key, exactly as documented for the Docker→Podman direction, and (b) if it did read everything, would bake in the *wrong* UIDs for Docker to consume directly.

   Rootful Podman has no user namespace — disk UIDs are already `1001:1000`, matching Docker — so for the rootful case this asymmetry does not apply and a plain `tar` (or the same namespaced forms, which also work under `sudo`) is fine either way.

2. **SELinux labels do not need to be, and should not be, carried across.** `backup-restore.md`'s Podman section is explicit that the private MCS category pair Podman stamps on a `:Z`-labelled mount (observed `system_u:object_r:container_file_t:s0:c269,c605` rootless, `:s0:c580,c750` rootful) "belongs to the mount, not to the backup" and that Podman re-applies it on the next mount. Docker does not use SELinux categories on bind mounts or named volumes at all (no `:z`/`:Z` option exists in Compose), so there is nothing to preserve or translate on the Docker side — extracting a tar archive into a fresh Docker volume produces ordinary root-privileged files with no SELinux context, which is exactly what Docker expects. Do not attempt to recreate the Podman MCS pair on the Docker host; it has no meaning there and Docker's own file access is DAC-only for a named volume.

### Procedure (DOCUMENTED, NOT EXECUTED)

```sh
# 1. stop the Podman service. Not destructive.
systemctl --user stop hive.service           # rootless; drop --user, prefix sudo for rootful

# 2. export the volume through the namespace (rootless) or plainly (rootful) —
#    same commands as backup-restore.md's "Rootless: never tar the volume
#    from the host shell", form 1 or 2:
podman volume export hive-data -o hive-data.tar
#    ...or, to land directly on a gzip'd tar Docker's own restore command expects:
podman run --rm -v hive-data:/data -v "$PWD":/backup:z docker.io/library/alpine:3.22 \
  tar czf /backup/hive-data.tar.gz -C /data .

# 3. create the Docker named volume and extract into it — mirrors the
#    Docker-side half of backup-restore.md's host-level restore procedure:
docker volume create src_hive-data
docker run --rm -v src_hive-data:/data -v "$PWD":/backup alpine \
  tar xzf /backup/hive-data.tar.gz -C /data
#    (podman volume export's own .tar is uncompressed; xzf vs xf accordingly)

# 4. copy config and secrets over -- Podman's %E/hive/hive.yaml and
#    %E/hive/secrets become Docker's ./hive.yaml and ./secrets. The reverse
#    of backup-restore.md's note under Docker -> Podman: hive.env's
#    variables move INTO the Compose `environment:` block by hand, and
#    dashboard.port no longer needs to be pinned to 3002 (that pin exists
#    only for the Quadlet unit's HealthCmd).

# 5. start Docker Compose, and verify BEFORE tearing down the Podman side:
docker compose -f src/docker-compose.yaml up -d
docker compose -f src/docker-compose.yaml exec hive cat /data/hive-id   # must match the Podman source
docker compose -f src/docker-compose.yaml exec hive sh -c 'sha256sum /data/gh-app-key*.pem'

# 6. only once step 5 matches: remove the Podman side (backup-restore.md's
#    "docker compose down -v, and its Podman counterpart" gives the destructive
#    commands for the Podman side specifically).
```

### Cross-host Podman → Docker

The procedure above is written host-local (export on the Podman host, extract on the same host's Docker). Extending it across hosts is, per the epic's own framing, contingent on the same-host case: **UNCONFIRMED** whether a cross-host copy of the intermediate tarball (e.g. `scp`) changes anything about the ownership/UID or SELinux-label reasoning above — nothing in this repository's Podman docs addresses a cross-host restore for **any** direction (epic item #4 already marks Podman restore onto a different host, same runtime, as DOCUMENTED, NOT EXECUTED with an open `/etc/subuid` caveat; a cross-host **and** cross-runtime move compounds that same open question rather than resolving it). Treat cross-host Podman → Docker as out of scope for this pass.

## Docker Compose or Podman Quadlet → Kubernetes

Standalone Compose/Quadlet has no PVC, no ConfigMap, and no Kubernetes `Secret` object — the equivalent state lives in the volume, a bind-mounted `hive.yaml`, and a bind-mounted `secrets/` directory (see the table above). Moving to Kubernetes means repackaging each into its Kubernetes equivalent, not attaching the same storage.

### Volume → PVC (DOCUMENTED, NOT EXECUTED)

1. Take a host-level volume backup exactly as `backup-restore.md` already documents for the source runtime: [Docker Compose host-level backup pattern](backup-restore.md#host-level-backup-pattern) or [Podman's namespace-aware export](backup-restore.md#rootless-never-tar-the-volume-from-the-host-shell).
2. Create the target PVC (`src/deploy/k8s/pvc.yaml`, `ReadWriteOnce`, default 10Gi — resize if the source volume is larger).
3. Populate it before the hive Deployment's pod first starts: run a temporary pod (or an init container ahead of the main one) that mounts the new PVC, `kubectl cp` the tarball in, and extract it — the same container-mediated pattern used for the Podman restore case, substituted onto a Kubernetes pod instead of a Podman-managed volume.
4. As with every archive-based restore in this repository, verify `/data/hive-id` and the GitHub App key SHA-256 on the target match the source **before** decommissioning it — see [Restoring a spoke backup archive](backup-restore.md#restoring-a-spoke-backup-archive) for the same verification pattern applied to a spoke-backup archive specifically, which is the lighter-weight alternative to a full volume tar for this exact file set (`hive.yaml.dashboard`, `hive.yaml.runtime`/`hive.yaml.bak`, `hive-id`, `hive-state.json`, `gh-app-key*.pem`, `beads/`) if you do not need the excluded bulk state (`nous/`, `home/`, `logs/`) to come along.

### Secrets → Kubernetes Secret (DOCUMENTED, NOT EXECUTED)

The source-side `secrets/` directory (Compose `./secrets`, Quadlet `%E/hive/secrets`) holds the same file set `src/deploy/k8s/secret.yaml` expects as `stringData` keys — `gh-app-key.pem` (or `HIVE_GITHUB_TOKEN`), `HIVE_DASHBOARD_TOKEN`, `bob_api_key`, etc. (`src/deploy/k8s/secret.yaml:8-27`). There is no automated converter; build the target Secret by hand (or `kubectl create secret generic hive-secrets --from-file=...`) from the same file contents, then `kubectl apply -f`. This is a content copy, not a mount-type translation — nothing here is bind-mounted on the Kubernetes side.

### Config → ConfigMap + dashboard overlay (DOCUMENTED, NOT EXECUTED)

The source `hive.yaml` becomes the `ConfigMap` seed (`src/deploy/k8s/configmap.yaml`). Two things do not carry over as a flat copy:

- Compose's `environment:` block / Quadlet's `hive.env` variables have no ConfigMap equivalent for secret-shaped values — anything secret-shaped belongs in the Kubernetes `Secret` above, not the ConfigMap.
- Once the target hive boots and saves through the dashboard even once, the PVC dashboard overlay (`hive.yaml.dashboard`) becomes the layer that wins the merge (`config-layering.md`; `src/pkg/spokebackup/backup.go`'s `configOverlayFile` doc comment: "the layer that wins the boot-time merge over the ConfigMap seed"). If the volume-to-PVC copy above already placed an overlay file on the new PVC, the ConfigMap seed only supplies whatever the overlay does not — same precedence rules `config-layering.md` documents for any Kubernetes hive.

### Host-bound settings that must change (epic item #12)

Moving onto Kubernetes introduces settings the standalone runtimes have no equivalent of at all: `hub.dashboard_url`, the heartbeat-owned `dashboardUrl`, an Ingress/Route, and (for a GitHub App) updating the App's Setup/callback URL to the new host — see [cross-cluster-migration.md](cross-cluster-migration.md)'s "Manual migration procedure" for the fields, which is written for a Kubernetes→Kubernetes hub-hosted move but names the same settings a Compose/Quadlet→Kubernetes move must also set for the first time (there is no source-side value to preserve, only a target-side value to create correctly).

## Kubernetes → Docker Compose or Podman Quadlet

The reverse direction, unpacking a PVC/Secret/ConfigMap-shaped deployment onto a standalone host:

1. **PVC → volume.** `kubectl cp` (or `kubectl exec ... tar` piped to the local shell) the PVC contents out of a running pod, into a tarball; extract into a fresh Docker named volume or Podman volume using the same runtime-appropriate commands as the Podman ↔ Docker section above. **UNCONFIRMED** whether file ownership recorded inside a Kubernetes pod (typically the container's own UID, no host-side remapping the way rootless Podman has) needs any adjustment before it is valid on a standalone host — no doc in this repository has executed this direction to confirm either way.
2. **Kubernetes Secret → secrets/ directory.** `kubectl get secret hive-secrets -o json` (or per-key `-o jsonpath`), write each key's value to the corresponding file under `./secrets` (Compose) or `%E/hive/secrets` (Quadlet, then re-apply the ownership commands `backup-restore.md`'s Podman section documents for that directory).
3. **ConfigMap + overlay → hive.yaml.** The ConfigMap seed becomes the standalone `hive.yaml` bind mount; if the PVC copy in step 1 carried a dashboard overlay file, it continues to win the merge exactly as it did on Kubernetes — `config-layering.md`'s precedence rules do not change based on runtime.
4. **Host-bound settings unwind, they don't transfer.** `dashboard_url`, the Ingress/Route, and any GitHub App callback URL pointed at the Kubernetes host have no meaning on a standalone deployment and should be cleared/reset rather than copied — the standalone runtimes discover their own dashboard host differently (published Compose/Quadlet port, no Route to read).

Nothing in this direction was executed while writing this page; the same verification discipline as every other procedure here applies: confirm `hive-id` and the GitHub App key SHA-256 on the target before removing the Kubernetes source.

## Image digest across a runtime change (epic item #5)

**UNCONFIRMED.** No doc in this repository states whether the same image digest is required (or even recommended) across a runtime change specifically, as opposed to across a same-runtime host move. `backup-restore.md`'s epic-adjacent table (item #5) already marks "restore from an archive taken with an older Hive image" as DOCUMENTED, NOT EXECUTED for the same-runtime case; nothing here narrows or resolves that for a cross-runtime move.

## What is explicitly out of scope for this pass

- Cross-host Podman → Docker (see above).
- Any pair not listed above (e.g. moving directly between two different standalone hosts running different runtimes without an intermediate archive step) — not addressed by this page; use the per-runtime host-level backup/restore procedures in `backup-restore.md` plus the cross-runtime repackaging steps above as building blocks.
- Automated tooling for any of the repackaging steps above (Secret generation, ConfigMap generation, PVC population) — every step here is a manual, documented procedure using existing kubectl/docker/podman commands, not a new script or command this repository ships.

## See also

- [Backup and restore](backup-restore.md) — the executed same-host Docker → Podman migration, the Podman backup/restore cycle, and restoring a `pkg/spokebackup` archive.
- [Cross-cluster migration](cross-cluster-migration.md) — the hub-hosted Kubernetes → Kubernetes cluster move and its host-bound-settings checklist.
- [Config layering](config-layering.md) — ConfigMap seed vs. PVC dashboard overlay precedence, referenced above for the Compose/Quadlet ↔ Kubernetes config translation.
- [move-host.md](move-host.md) — same-runtime host moves (Docker Compose ↔ Docker Compose, Podman Quadlet ↔ Podman Quadlet).
- [move-kubernetes.md](move-kubernetes.md) — same-runtime Kubernetes cluster ↔ cluster moves.
