# Hive Hub — Backup & Disaster Recovery

How to back up the Hive hub and its spoke fleet, and how to rebuild everything
from zero after a catastrophic loss.

---

## ⚠️ Read this first

### 1. `HIVE_BACKUP_KEY` must be escrowed OUTSIDE the cluster

Backups are encrypted with AES-256-GCM using `HIVE_BACKUP_KEY`. **If that key
exists only inside the cluster, the backup is worthless the moment the cluster
is gone.** Store it in a password manager and/or an offline copy, today.

There is no key-recovery mechanism. A lost key means an unreadable archive.

```bash
openssl rand -hex 32     # generate; store the output OUT OF BAND
```

The key is deliberately **independent of `/data/saas/hmac.key`**. `hmac.key` is
itself backup *payload*; deriving the backup key from it would make the archive
undecryptable in exactly the disaster it exists for.

### 2. `hmac.key` is a single point of failure for all user tokens

`/data/saas/hmac.key` (32 bytes) is the AES key for **every** user's
`encrypted_token` (see `encryptToken`/`decryptToken` in `pkg/hub/saas.go`).
There is no KMS, no escrow, no second copy anywhere.

**If `hmac.key` is lost, all ~88 users' stored GitHub tokens become permanently
undecryptable ciphertext and every user must re-authorize via OAuth.** No
restore procedure recovers it. This backup is the only thing standing between
you and that outcome.

### 3. The authoritative spoke config is the PVC overlay — usually NOT `hive.yaml.runtime`

> **Corrected.** This section previously stated that `copy-config` restores from
> `/data/hive.yaml.runtime`, and that restoring that file is what brings a spoke
> back. **That is true on only 3 of 51 live hives.** Following it on the other
> 48 produces a differently-configured spoke with no error — the worst kind of
> DR bug, because it fails only when you are already in an emergency.

The fleet runs **two different `copy-config` variants**, baked into each
spoke's Deployment at provision time and never migrated.

| Variant | Init-container behaviour | Hives |
|---|---|---|
| **A/B — `.runtime` wins** | restores `/data/hive.yaml.runtime` over the seed | **3**: `kubestellar-console`, `projectbluefin-knuckle`, `aslom-hive-agent` |
| **C — seed wins** | copies the ConfigMap seed; merely *echoes* whether `hive.yaml.runtime` exists | **48**: everything else (15 on hive-oke, 33 on vllm-d) |

Variant C, which is what new hives get:

```sh
cp /etc/hive-seed/hive.yaml /etc/hive/hive.yaml && echo configmap-copied
if [ -f /data/hive.yaml.runtime ]; then echo runtime-config-exists-for-recovery
elif [ -f /data/hive.yaml.bak ]; then echo legacy-runtime-config-exists-for-recovery; fi
```

Note it only *reports* the runtime config. It never restores from it.

> **Naming.** This file was `hive.yaml.bak` until the rename to
> `hive.yaml.runtime`. The migration is copy-forward: writers emit the new name,
> readers fall back to the legacy one, and nothing renames or deletes the legacy
> file on a PVC. A hive that has not saved since upgrading still carries only
> `hive.yaml.bak` and boots normally from it. Backups capture **both** names, so
> restore whichever the archive contains — under the new name.
>
> Outside Kubernetes — Docker, Podman, LXC, or a bare host binary; the branch is
> chosen by the absence of a Kubernetes pod, not by the runtime — this file is not
> a snapshot at all but the boot-time source of truth, since there is no ConfigMap
> and no overlay there. Restoring such a hive means restoring the data volume that
> holds it, not the seed. See `src/docs/config-layering.md`.

**How to tell which variant a hive runs** — this is the only way to know:

```sh
kubectl -n hive-hosted-<id> get deploy -o jsonpath\
='{.items[*].spec.template.spec.initContainers[?(@.name=="copy-config")].command}'
```

#### What actually holds the running config

On **every** hive, the config in effect is dominated by the PVC overlay
`/data/hive.yaml.dashboard`, which the entrypoint merges over the seed at boot.
The ConfigMap seed is 4–11% of the running config on sampled hives and omits
whole top-level blocks (`policies`, `data`, `knowledge`, `notifications`,
`hive_id`).

**So the file to restore is `/data/hive.yaml.dashboard`.** It is the only
writable layer and the one that wins. See `src/docs/config-layering.md` for the
full precedence order, and `GET /api/config/provenance` on a running spoke to
ask which layer set any given field.

#### `hive.yaml.runtime` is a snapshot, not a restore source

The name misleads. `hive.yaml.runtime` is written by the entrypoint **after** the merge
(`src/deploy/entrypoint.sh:150`) — it is a snapshot of the *result*, not an
input. On variant-C hives nothing reads it during a normal boot.

It has exactly one non-redundant role: the **disaster fallback** at
`entrypoint.sh:152-161`, which restores from `hive.yaml.runtime` when the ConfigMap is
missing or empty. That is a genuinely different scenario from the overlay path,
which is why the file is still worth capturing.

`hive.yaml.runtime` and the overlay are **near-copies but not interchangeable**. The overlay
is written secret-free on purpose (`dashboardOverlayBytes` collapses
`HIVE_GITHUB_TOKEN` back to `${HIVE_GITHUB_TOKEN}` and blanks a
pod-env-derived `dashboard.auth_token`), while `hive.yaml.runtime` is a verbatim copy of the
merged config **including** those secrets. Observed live on
`projectbluefin-knuckle`:

```
/data/hive.yaml.runtime        14609 B   auth_token: 55c076f9…
/data/hive.yaml.dashboard  14547 B   auth_token: ""
```

On a hive with no such env vars set the two files are byte-identical — but that
is incidental, not structural. **Capture both.**

### 4. A PVC-only backup is NOT restorable

Four Kubernetes Secrets live **outside** the hub PVC and are required:

| Secret | Without it |
|---|---|
| `hive-hub-secrets` | OAuth login broken for all users; Slack messaging disabled |
| `oci-api-key` | Hub cannot provision spoke storage or write backups |
| `hive-hub-kubeconfigs` | Hub cannot reach remote spoke clusters (vllm-d) |
| `hive-hub-tls` | Cert-manager reissues automatically (not fatal) |

`hive-backup` captures these automatically and **fails the backup** if any of
the first three is missing.

> **Adding a new hub credential?** Put it in the **existing `hive-hub-secrets`**
> Secret rather than creating a new one. That Secret is already in
> `hubbackup.DefaultHubSecrets`, so a key added to it is captured — and restored
> — for free. A brand-new Secret is **silently lost in a restore** unless it is
> also added to `DefaultHubSecrets`.
>
> This is why the Slack bot token (`HIVE_HUB_SLACK_BOT_TOKEN`, see
> [Slack messaging](#slack-messaging)) is a key on `hive-hub-secrets`.

---

## What is backed up

| Included | Why |
|---|---|
| `/data/saas/**` | users, hives, keys, timeline, provisioning state |
| `/data/hub-registry.json` | the 50-hive fleet registry |
| 4 Kubernetes Secrets | credentials outside the PVC |
| per-spoke `hive.yaml.dashboard`, `hive.yaml.runtime`, `gh-app-key*.pem`, `hive-id` | rebuild each spoke |

**Deliberately excluded** as regenerable agent scratch: `nous/`, `home/`,
`beads/`, `logs/`. These are ~110MB of the hub's ~125MB and ~796MB per spoke.
Excluding them keeps an archive at roughly **300–400KB**, small enough to retain
30 daily copies and restore in minutes.

Losing the excluded data costs agent history and learned context — not the
ability to run.

### What is NOT recoverable by any backup

- **GitHub App installation consent** — if the App registration is recreated,
  each org owner must re-approve the installation.
- **In-flight agent work** at the moment of loss.
- **`hmac.key` if it was never backed up** — see above.

---

## Backup

Runs as a CronJob in `hive-hub` — **opt-in, not deployed by default**: `src/deploy/k8s/backup-cronjob.yaml` is deliberately excluded from the kustomization and must be applied explicitly, after creating Secret `hive-hub-backup-key` (key `backup-key`) with the escrowed `HIVE_BACKUP_KEY` and reviewing ConfigMap `hive-hub-backup-config`. The CronJob image tracks the `stable` release channel. Each run builds the archive, encrypts it,
uploads it to OCI Object Storage, **downloads it again and verifies checksums**,
then prunes beyond the retention count. A run that cannot self-verify fails
rather than reporting a good backup.

```bash
hive-backup run                    # to object storage
hive-backup run -local out.enc     # to a local file
hive-backup run -skip-spokes=true  # hub-only backup (no spoke exec)
hive-backup verify                 # verify newest stored archive
hive-backup list                   # list stored archives
hive-backup extract -file f.enc -dest ./restore
```

Environment:

| Variable | Meaning |
|---|---|
| `HIVE_BACKUP_KEY` | **required** AES-256 key (64-char hex or base64). No default; unset aborts. |
| `HIVE_BACKUP_BUCKET` | OCI Object Storage bucket |
| `HIVE_BACKUP_RETENTION` | archives to keep (default 30) |
| `HIVE_BACKUP_DATA_DIR` | hub data dir (default `/data`) |
| `HIVE_BACKUP_OCI_ENDPOINT` | Object Storage endpoint override (default: regional endpoint) |
| `HIVE_HUB_NAMESPACE` | hub namespace for Secret collection (default `hive-hub`) |
| `HIVE_KUBECONFIG_DIR` | per-cluster kubeconfigs for remote spokes (default `/etc/hive/kubeconfigs`) |
| `OCI_*` | credentials, from the existing `oci-api-key` Secret |

### Spoke gaps are recorded, not hidden

A spoke scaled to zero, or with no Running pod, cannot be read. Those hives are
listed in the archive's `MANIFEST.json` under `spoke_errors`, and printed by
`run` and `verify`. **Check this output** — a "successful" backup can still be
missing spokes, and the manifest is where you find out before you need it.

---

## Restore runbook — rebuilding from zero

Assumes: total loss of the hub cluster. You have the escrowed
`HIVE_BACKUP_KEY` and access to the OCI bucket.

### Step 0 — Get the archive

```bash
export HIVE_BACKUP_KEY=<from your password manager>
export HIVE_BACKUP_BUCKET=<bucket>
hive-backup list
hive-backup verify                      # confirm it is intact BEFORE relying on it
hive-backup extract -file <archive> -dest ./restore
```

`./restore` now contains `hub/`, `secrets/`, `spokes/`, `MANIFEST.json`.
Read `MANIFEST.json` → `spoke_errors` to learn which spokes were not captured.

### Step 1 — Provision a cluster

Create a Kubernetes cluster and the `hive-hub` namespace. Recreate the hub PVC
(RWX; on OKE this is an NFS/FSS-backed PV).

```bash
kubectl create namespace hive-hub
kubectl apply -f <your hub PVC manifest>
```

### Step 2 — Restore the Kubernetes Secrets

```bash
for f in restore/secrets/*.json; do
  kubectl apply -n hive-hub -f "$f"
done
```

`hive-hub-tls` may be omitted; cert-manager reissues it.

### Step 3 — Restore the hub PVC data

Start a helper pod mounting the hub PVC, then stream the data in:

```bash
kubectl apply -n hive-hub -f - <<'YAML'
apiVersion: v1
kind: Pod
metadata: {name: restore-helper}
spec:
  restartPolicy: Never
  containers:
  - name: shell
    image: busybox:1.36
    command: ["sh","-c","sleep 3600"]
    volumeMounts: [{name: data, mountPath: /data}]
  volumes:
  - name: data
    persistentVolumeClaim: {claimName: hive-hub-data-rwx}
YAML

kubectl wait --for=condition=Ready pod/restore-helper -n hive-hub --timeout=120s
tar cf - -C restore/hub saas hub-registry.json \
  | kubectl exec -i -n hive-hub restore-helper -- sh -c 'cd /data && tar xf -'
```

Verify before continuing — especially `hmac.key`:

```bash
kubectl exec -n hive-hub restore-helper -- sh -c \
  'ls /data/saas/users | wc -l; sha256sum /data/saas/hmac.key'
kubectl delete pod restore-helper -n hive-hub
```

### Step 4 — Start the hub

Deploy the hub. On boot it reads `/data/saas` and `/data/hub-registry.json`.

Because `hmac.key`, `hub-secret.key` and `webhook-secret.key` were restored
rather than regenerated:

- existing user tokens still decrypt — **no mass re-OAuth**
- spokes still authenticate with their existing `hub-secret`
- GitHub webhooks still validate

**Hive IDs and vanity URLs stay stable** because they come from
`hub-registry.json` and `saas/hives/<id>/meta.json`, both restored verbatim.

### Step 5 — Rebuild spokes

For each hive in the registry, provision the namespace, PVC and Deployment as
normal, then restore its config **before first start** (or restart after):

```bash
HIVE=hosted-example-abcd
NS=hive-hosted-$HIVE
POD=$(kubectl get pods -n $NS --field-selector=status.phase=Running \
        -o jsonpath='{.items[0].metadata.name}')

# Restore BOTH config files. The overlay is what wins on a normal boot
# (all 51 hives); hive.yaml.runtime is what the disaster fallback reads when the
# ConfigMap is missing/empty, and what variant-A/B hives boot from.
#
# Archives taken before the hive.yaml.bak -> hive.yaml.runtime rename carry the
# legacy name. Restore whichever the archive has, under the NEW name.
kubectl cp restore/spokes/$HIVE/hive.yaml.dashboard $NS/$POD:/data/hive.yaml.dashboard -c hive
RUNTIME_SRC=restore/spokes/$HIVE/hive.yaml.runtime
[ -f "$RUNTIME_SRC" ] || RUNTIME_SRC=restore/spokes/$HIVE/hive.yaml.bak
kubectl cp "$RUNTIME_SRC" $NS/$POD:/data/hive.yaml.runtime -c hive
kubectl cp restore/spokes/$HIVE/hive-id       $NS/$POD:/data/hive-id       -c hive
for k in restore/spokes/$HIVE/gh-app-key*.pem; do
  kubectl cp "$k" $NS/$POD:/data/$(basename "$k") -c hive
done

kubectl rollout restart deploy/hive -n $NS
```

Confirm the spoke used the restored config. **Which log line to expect depends
on the variant** (see §3):

```bash
kubectl logs -n $NS -c copy-config $POD
#   variant A/B  -> "override-used"     (runtime config restored)
#   variant C    -> "configmap-copied"  (seed copied; runtime config NOT read)

# On variant C the overlay is what matters, and the merge happens in the
# main container, not copy-config:
kubectl logs -n $NS -c hive $POD | grep '\[entrypoint\]'
#   expect: "dashboard overlay merged over ConfigMap seed"
#   NOT:    "dashboard overlay invalid, using ConfigMap seed as-is"
```

The best confirmation is to ask the spoke directly:

```bash
kubectl -n $NS exec $POD -c hive -- \
  curl -s localhost:3002/api/config/provenance | jq '.overlay_rejected, .fields'
```

### Step 6 — Re-link GitHub

Manual, and unavoidable:

- If the **OAuth App** still exists, restoring `hive-hub-secrets` is enough.
- If the **GitHub App** was recreated, every org owner must re-approve the
  installation. Its private keys are in `restore/hub/saas/app-keys/` and
  `restore/spokes/*/gh-app-key*.pem` — reuse them if the App survived.
- Re-point DNS / ingress for `hive.hivecommons.dev`.

### Step 7 — Verify

- [ ] user count matches (`ls /data/saas/users | wc -l`)
- [ ] users can log in without re-authorizing (proves `hmac.key` restored)
- [ ] hive count and vanity URLs unchanged
- [ ] spokes appear online and heartbeat to the hub
- [ ] `copy-config` logged the expected line for the spoke's variant (`override-used` on A/B, `configmap-copied` on C), and the hive container logged "dashboard overlay merged over ConfigMap seed"
- [ ] a fresh `hive-backup run` succeeds against the rebuilt hub

---

## Automated vs manual

| Automated | Manual |
|---|---|
| Archive creation, encryption, upload | Escrowing `HIVE_BACKUP_KEY` |
| Integrity self-verification, retention | Cluster/PVC provisioning |
| Hub state + Secrets + spoke config capture | Applying Secrets and PVC data |
| Gap reporting via manifest | GitHub App re-consent, DNS |

---

## Testing status

Verified end-to-end against production data (read-only against live systems;
writes confined to a throwaway namespace that was deleted afterwards).
Counts below are as measured at the time of that drill — expect different
numbers on a grown fleet, not a restore failure:

- Backup of the real hub: **423 files, all 50 spokes, all 4 Secrets**
- Archive is opaque ciphertext — no plaintext leakage
- Wrong key and bit-flips are rejected (GCM auth tag)
- Restored `hmac.key` byte-identical to production
- **All 88 restored user tokens decrypt successfully**
- Restored `hive.yaml.dashboard` and `hive.yaml.runtime` byte-identical to the live spoke
- Secrets restored into a throwaway namespace matched live values
- Hub PVC data restored into a live pod: 88 users, 50 hives

**UNTESTED** — no production meltdown was simulated:

- Restoring onto a genuinely fresh/empty cluster
- Starting the hub server against restored data (Step 4)
- Spoke re-adoption and heartbeat after restore (Step 5)
- GitHub App/OAuth re-linkage (Step 6)
- The CronJob running in-cluster on its schedule
- Upload to OCI Object Storage from *inside* the cluster (write permission was
  verified out-of-band with the hub's own credentials; the in-cluster upload
  path itself has not been exercised)

---

# Spoke self-service backup (owner-triggered)

This is a **second, complementary** backup, separate from the fleet-wide hub
backup described above. A spoke owner triggers it themselves from the avatar
menu on their spoke dashboard (**Back up this hive**), and it downloads an
encrypted archive to their browser.

## Why it exists — and why it includes beads

The hub backup is deliberately config-only. It excludes `/data/beads`,
`/data/nous`, `/data/home` and `/data/logs`, because including them across ~50
spokes would be roughly 40GB — too slow and expensive to run nightly and too
slow to restore in an emergency. That trade-off is recorded in issue #2318.

The cost profile of a *single owner backing up their own hive on demand* is
completely different. So this backup **includes `/data/beads`** — the agent
work ledger that #2318 identifies as the most painful documented loss. After a
hub-backup-only restore, agents come back configured but with no memory of what
they had already found, claimed, deferred or closed. This backup preserves that.

## What is captured

| Path | Why |
|---|---|
| `hive.yaml.dashboard` | The **overlay** — the layer that wins the boot merge (see below) |
| `hive.yaml.runtime` | Post-merge snapshot; disaster fallback + variant-A/B boot source |
| `hive-id` | Hive identity as known to the hub |
| `hive-state.json` | Agent runtime state |
| `gh-app-key*.pem` | GitHub App private keys — **credentials** |
| `beads/<agent>/**` | The agent work ledger, one directory per agent |

Bead directories are **discovered, not hardcoded**. A production spoke had ten
(`architect`, `brainstorm`, `ci-maintainer`, `guide`, `outreach`, `quality`,
`scanner`, `sec-check`, `strategist`, `supervisor`), not the five named in
#2318, so a fixed list would silently miss ledgers.

### What is excluded, and why

| Path | Size (measured) | Why excluded |
|---|---|---|
| `nous/` | **287MB** | Timestamped learned-context snapshots; regenerable and by far the largest directory |
| `logs/` | 32MB | Diagnostic, not state |
| `graph/`, `snapshots/`, `vaults/` | ~19MB | Derived/bulk artifacts |
| `prompt-history.jsonl` | 3.1MB | Diagnostic transcript |
| `audit.jsonl` | 860KB | A record *of* actions, not state needed to reconstitute the hive |
| `dashboard-sessions.json` | 1KB | **Live browser session tokens** — credentials with no restore value; restoring them would resurrect sessions that should have expired |
| `hive.yaml` | 16KB | The *stale* ConfigMap seed — not authoritative |

**Resulting archive: roughly 3–5MB before compression** on a measured
production spoke (beads 2.6MB dominating), versus ~796MB for the whole PVC.
Small enough to stream to a browser and stay responsive.

## Capture the overlay and `hive.yaml.runtime`, not `hive.yaml`

The same trap as the hub backup, and worth repeating because it is silent.

`/etc/hive/hive.yaml` is **not** the file to back up. On a sampled production
spoke it was a root-owned copy days older than the live config. It is
regenerated from the seed plus the overlay on every boot.

The two files worth capturing are:

| File | Why |
|---|---|
| `/data/hive.yaml.dashboard` | The **overlay** — the only writable layer, and the one that wins the boot-time merge on all 51 hives. This is the real running config. |
| `/data/hive.yaml.runtime` | The post-merge snapshot. Read by the disaster fallback when the ConfigMap is missing/empty, and by variant-A/B hives on every boot. Also the only copy that retains env-derived secrets. |

They are near-copies but **not interchangeable** — the overlay is deliberately
secret-free. Backing up the wrong one produces no error at backup time and no
symptom until a restore quietly reverts the owner's settings. Tests lock this
in.

## Encryption is mandatory

The archive contains GitHub App private keys, so it is **always** sealed with
AES-256-GCM using the same code path as the hub backup.

- The key comes from **governor config** (`governor.backup.key_file`, set by the
  hive owner in the dashboard under **Governor Config → Security → Backup**), or
  from **`HIVE_BACKUP_KEY`** on the spoke as a fallback. There is **no default**.
  Governor config exists because hosted spoke owners have no deployment-env
  access, so an env-only key made backup unavailable to them (#4129). The value
  is written to `/data/secrets/backup_encryption_key` (mode `0600`); `hive.yaml`
  records the path only, and the value is never returned by an API or logged.
  If no source supplies a key the endpoint returns `412 Precondition Failed` and
  refuses — it never streams plaintext credentials to a browser.
- The key is **not in the archive** and is not derivable from it. An artifact
  carrying its own decryption key is not encrypted. The owner must have the key
  escrowed separately, or the backup is unreadable.
- The response is `Cache-Control: no-store` and is served over `POST` only, so
  it cannot be pulled by a cross-origin navigation or `<img>` tag.

The UI states plainly that the file is encrypted and that the key is not
included, and a `412` refusal deep-links the owner to the Governor Config panel
where they can set the key themselves. Clearing the key from that panel restores
the refusal — there is no path that produces an unencrypted archive.

## Authorization

**Owner-only**, enforced server-side on both `POST /api/backup` and
`GET /api/backup/status`, matching `handleConfigDownload` and
`handleSelfUpgrade`. Viewers and read-write members get `403`. Hiding the menu
entry for non-owners is UX; the server check is the boundary.

## Restore procedure

There is **no restore button** — restoring is a deliberate, disruptive act on a
running hive, so it is a documented operator procedure rather than a one-click
control. The archive shares the hub format, so the same tooling reads it.

```bash
# 0. Decrypt and verify. Uses the SAME archive format as the hub backup,
#    so hive-backup verifies and extracts it unchanged.
export HIVE_BACKUP_KEY=<the key escrowed for THIS hive — the same value set in
                        Governor Config → Security → Backup, if that is where
                        it was configured>
hive-backup verify -file hive-spoke-backup-<id>-<ts>.tar.gz.enc
hive-backup extract -file hive-spoke-backup-<id>-<ts>.tar.gz.enc -dest ./restore

# Layout:
#   ./restore/MANIFEST.json
#   ./restore/spoke/{hive.yaml.dashboard,hive.yaml.runtime,hive-id,hive-state.json,gh-app-key*.pem}
#   ./restore/beads/<agent>/**

# 1. Identify the target spoke pod.
NS=hive-hosted-hosted-<org>-<repo>-<suffix>
POD=$(kubectl -n $NS get pods --field-selector=status.phase=Running \
        -o jsonpath='{.items[0].metadata.name}')

# 2. Scale down so agents are not writing beads while you restore them.
kubectl -n $NS scale deploy/hive --replicas=0
kubectl -n $NS wait --for=delete pod/$POD --timeout=120s
```

Then bring up a pod with the PVC mounted and copy the files back:

```bash
kubectl -n $NS scale deploy/hive --replicas=1
POD=$(kubectl -n $NS get pods --field-selector=status.phase=Running \
        -o jsonpath='{.items[0].metadata.name}')

# 3. Config FIRST. Restore BOTH: the overlay is what wins the boot merge on
#    every hive; hive.yaml.runtime covers the disaster fallback and variant-A/B hives.
#    Pre-rename archives carry hive.yaml.bak — restore it under the new name.
kubectl -n $NS cp ./restore/spoke/hive.yaml.dashboard $POD:/data/hive.yaml.dashboard -c hive
RUNTIME_SRC=./restore/spoke/hive.yaml.runtime
[ -f "$RUNTIME_SRC" ] || RUNTIME_SRC=./restore/spoke/hive.yaml.bak
kubectl -n $NS cp "$RUNTIME_SRC" $POD:/data/hive.yaml.runtime -c hive
kubectl -n $NS cp ./restore/spoke/hive-id         $POD:/data/hive-id       -c hive
kubectl -n $NS cp ./restore/spoke/hive-state.json $POD:/data/hive-state.json -c hive

# 4. GitHub App keys. Mode 0600 — never world-readable.
for k in ./restore/spoke/gh-app-key*.pem; do
  kubectl -n $NS cp "$k" $POD:/data/$(basename "$k") -c hive
  kubectl -n $NS exec $POD -c hive -- chmod 600 /data/$(basename "$k")
done

# 5. The bead ledger.
for d in ./restore/beads/*/; do
  agent=$(basename "$d")
  kubectl -n $NS exec $POD -c hive -- mkdir -p /data/beads/$agent
  kubectl -n $NS cp "$d" $POD:/data/beads/$agent -c hive
done

# 6. Restart so the boot-time merge picks up the restored config.
kubectl -n $NS rollout restart deploy/hive
kubectl -n $NS rollout status  deploy/hive --timeout=300s
```

**Verify:** the dashboard loads, the hive ID matches, config changes the owner
made are present (not the ConfigMap defaults), and agents show their prior bead
counts rather than starting from an empty ledger.

### Bead ownership

Bead directories are owned per-agent UID on the spoke (`hive-scanner`,
`hive-quality`, …). If agents cannot write their ledgers after a restore, fix
ownership to match the surrounding directories rather than loosening the mode.

## Testing status

**Verified:**

- Unit tests cover: `hive.yaml.dashboard` and `hive.yaml.runtime` captured and `hive.yaml` excluded; beads
  captured for every discovered agent; `nous`/`logs`/`home`/`audit.jsonl`/
  `dashboard-sessions.json` excluded; App keys captured; sealed archive is
  opaque ciphertext with no plaintext key/config leakage; wrong key and
  bit-flips rejected; manifest digests non-empty so `Verify` is not vacuous;
  build refuses with no key; missing files recorded rather than silently
  dropped; hostile hive IDs cannot escape the filename.
- Handler tests cover: `403` for `read`/`read-write`/`write`/`viewer` on both
  endpoints and on all three backup-key config endpoints, `412` with no key from
  any source and no download offered, `no-store` caching, an `.enc` attachment,
  a backup that succeeds (and decrypts) with a key set only through governor
  config, a `412` again after the key is cleared, and no key value in any
  response, in `hive.yaml`, or in the stored config struct.
- Archives are extracted through `hubbackup.Extract`, proving both backups
  share one format and one restore path.
- Spoke layout, `hive.yaml`/`hive.yaml.runtime` divergence, bead directory names
  and all sizes above were read from a live production spoke (read-only).

**UNTESTED:**

- The restore procedure above has **not** been executed against a live spoke.
- No real backup was run against a user's hive; the endpoint was exercised
  only against temporary directories in tests.
- Browser download of a multi-MB archive was not exercised end-to-end in a
  real browser.

---

## Slack messaging

The hub can send Slack DMs to a single user, to the owner of a hive, or to every
user. Recipients are resolved from the **existing** `slack_id` contact field on
each user record (`/data/saas/users/<GitHubUsername>.json`), which admins edit
from the Users panel.

### Credential

The bot token is read from the environment with **no default**:

| Variable | Source |
|---|---|
| `HIVE_HUB_SLACK_BOT_TOKEN` | key on the existing `hive-hub-secrets` Secret |

It is deliberately a key on `hive-hub-secrets` rather than a new Secret, so it is
already inside the DR-captured set (`hubbackup.DefaultHubSecrets`) and survives a
restore. **The token must never be committed to this repository** — a previous
leak required scrubbing the git history.

Add it without disturbing the other keys:

```bash
kubectl -n <hub-namespace> patch secret hive-hub-secrets \
  --type merge \
  -p "{\"stringData\":{\"slack-bot-token\":\"$SLACK_BOT_TOKEN\"}}"
```

...and reference it from the hub Deployment:

```yaml
- name: HIVE_HUB_SLACK_BOT_TOKEN
  valueFrom:
    secretKeyRef:
      name: hive-hub-secrets
      key: slack-bot-token
      optional: true   # unset simply disables Slack messaging
```

The token needs the `chat:write` scope. A webhook will **not** work: a webhook is
bound to one channel at creation time and cannot DM a user.

If the variable is unset, the send endpoints return **503** naming the variable.
They never report a successful send that did not happen.

### Endpoints

| Endpoint | Auth | Notes |
|---|---|---|
| `POST /api/saas/slack/user/{username}` | admin, or the user themselves | |
| `POST /api/saas/hives/{id}/slack` | hive owner or admin | messages the hive's owner |
| `POST /api/saas/admin/slack/broadcast` | **admin only** | requires confirmation |

Body: `{"message": "...", "dry_run": false, "confirm": "..."}`.

### Broadcast safety

Broadcast reaches every user with a `slack_id` and cannot be recalled, so:

1. **Dry-run first.** `{"dry_run": true}` sends nothing and returns the recipient
   count, the **names** of users who will be skipped, a message preview, and an
   estimated duration.
2. **Confirm explicitly.** A real broadcast requires `"confirm": "SEND TO ALL
   USERS"` — a typed string, not a boolean, so no client can set it by accident.
   Without it the request returns **409** and reports the blast radius.
3. **Sending is paced and backgrounded.** Slack throttles `chat.postMessage` at
   roughly 1/sec, so sends are spaced one second apart and run in a goroutine;
   ~88 users takes about a minute and a half. The HTTP response returns
   immediately with the estimate.

### Users without a Slack ID are skipped VISIBLY

A user with no `slack_id` cannot be reached. Every response reports how many were
skipped **and names them**, and the full skipped list is written to the hub log on
a broadcast. Silent drops are not acceptable: "sent to 61" while 27 people never
heard anything is worse than a clear failure.
