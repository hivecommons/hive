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

### 3. The authoritative spoke config is `hive.yaml.bak` — NOT `hive.yaml`

Each spoke's running config is **`/data/hive.yaml.bak`** on its own PVC. The
ConfigMap is only a fallback seed read at startup. The spoke's `copy-config`
init container does:

```sh
if [ -f /data/hive.yaml.bak ]; then cp /data/hive.yaml.bak /etc/hive/hive.yaml
else cp /etc/hive-seed/hive.yaml /etc/hive/hive.yaml; fi
```

**A restore that omits `hive.yaml.bak` silently falls back to the stale
ConfigMap seed with no error**, losing every customization the user made. There
is no warning — the spoke just comes up wrong. Always restore `hive.yaml.bak`.

### 4. A PVC-only backup is NOT restorable

Four Kubernetes Secrets live **outside** the hub PVC and are required:

| Secret | Without it |
|---|---|
| `hive-hub-secrets` | OAuth login broken for all users |
| `oci-api-key` | Hub cannot provision spoke storage or write backups |
| `hive-hub-kubeconfigs` | Hub cannot reach remote spoke clusters (vllm-d) |
| `hive-hub-tls` | Cert-manager reissues automatically (not fatal) |

`hive-backup` captures these automatically and **fails the backup** if any of
the first three is missing.

---

## What is backed up

| Included | Why |
|---|---|
| `/data/saas/**` | users, hives, keys, timeline, provisioning state |
| `/data/hub-registry.json` | the 50-hive fleet registry |
| 4 Kubernetes Secrets | credentials outside the PVC |
| per-spoke `hive.yaml.bak`, `gh-app-key*.pem`, `hive-id` | rebuild each spoke |

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

Runs as a CronJob in `hive-hub`. Each run builds the archive, encrypts it,
uploads it to OCI Object Storage, **downloads it again and verifies checksums**,
then prunes beyond the retention count. A run that cannot self-verify fails
rather than reporting a good backup.

```bash
hive-backup run                    # to object storage
hive-backup run -local out.enc     # to a local file
hive-backup verify                 # verify newest stored archive
hive-backup list                   # list stored archives
hive-backup extract -file f.enc -dest ./restore
```

Environment:

| Variable | Meaning |
|---|---|
| `HIVE_BACKUP_KEY` | **required** 64-hex AES-256 key. No default; unset aborts. |
| `HIVE_BACKUP_BUCKET` | OCI Object Storage bucket |
| `HIVE_BACKUP_RETENTION` | archives to keep (default 30) |
| `HIVE_BACKUP_DATA_DIR` | hub data dir (default `/data`) |
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

# hive.yaml.bak is authoritative — without it the spoke silently uses the
# stale ConfigMap seed.
kubectl cp restore/spokes/$HIVE/hive.yaml.bak $NS/$POD:/data/hive.yaml.bak -c hive
kubectl cp restore/spokes/$HIVE/hive-id       $NS/$POD:/data/hive-id       -c hive
for k in restore/spokes/$HIVE/gh-app-key*.pem; do
  kubectl cp "$k" $NS/$POD:/data/$(basename "$k") -c hive
done

kubectl rollout restart deploy/hive -n $NS
```

Confirm the spoke used the restored config, not the seed:

```bash
kubectl logs -n $NS -c copy-config $POD    # expect "override-used", not "seed-copied"
```

### Step 6 — Re-link GitHub

Manual, and unavoidable:

- If the **OAuth App** still exists, restoring `hive-hub-secrets` is enough.
- If the **GitHub App** was recreated, every org owner must re-approve the
  installation. Its private keys are in `restore/hub/saas/app-keys/` and
  `restore/spokes/*/gh-app-key*.pem` — reuse them if the App survived.
- Re-point DNS / ingress for `hive.kubestellar.io`.

### Step 7 — Verify

- [ ] user count matches (`ls /data/saas/users | wc -l`)
- [ ] users can log in without re-authorizing (proves `hmac.key` restored)
- [ ] hive count and vanity URLs unchanged
- [ ] spokes appear online and heartbeat to the hub
- [ ] `copy-config` logged `override-used` on each spoke
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
writes confined to a throwaway namespace that was deleted afterwards):

- Backup of the real hub: **423 files, all 50 spokes, all 4 Secrets**
- Archive is opaque ciphertext — no plaintext leakage
- Wrong key and bit-flips are rejected (GCM auth tag)
- Restored `hmac.key` byte-identical to production
- **All 88 restored user tokens decrypt successfully**
- Restored `hive.yaml.bak` byte-identical to the live spoke
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
