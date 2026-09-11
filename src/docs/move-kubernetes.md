# Moving a self-hosted Kubernetes hive to a new cluster

This guide is for an operator running the **self-hosted, hub-less** Kubernetes
deployment described in the root README's "Self-Hosted Deployment" section and
[`src/deploy/kustomize/overlays/standalone`](https://github.com/hivecommons/hive/tree/v4/src/deploy/kustomize/overlays/standalone)
— your own hive, your own cluster, no `kubestellar` hub involved.

If your hive was instead **provisioned by a hub** (dashboard reached through
the hub's nginx ingress, `hub-registry.json`/`meta.json` on the hub PVC), use
[cross-cluster-migration.md](cross-cluster-migration.md) instead — this guide
does not repeat that procedure and does not cover hub-registry bookkeeping. If
your self-hosted hive additionally reports heartbeats to a hub (`hub.url` /
`HIVE_HUB_URL` set, no hub-managed provisioning), also read
[move-hub-registered-cutover.md](move-hub-registered-cutover.md) before you
start the cutover in step 6 below — it covers the identity/heartbeat rules
that apply regardless of which runtime you are moving.

> **Status.** This procedure is **DOCUMENTED, NOT EXECUTED**: it is derived
> from reading the manifests under `src/deploy/k8s/` and
> `src/deploy/kustomize/overlays/standalone/`, `src/deploy/entrypoint.sh`, and
> `src/docs/backup-restore.md` / `cross-cluster-migration.md`, but it has not
> been run end-to-end against two real clusters in this environment. Every
> command below is a `kubectl`-based, generic procedure — no cloud-provider
> snapshot API is assumed, because a self-hosted operator's storage class is
> not known in advance.

## What identity and state live where

| What | Object | Path / key | Citation |
|---|---|---|---|
| Hive ID | PVC `hive-data` | `/data/hive-id` | `src/pkg/dashboard/api.go:8884` (`hiveIDFilePath = "/data/hive-id"`) |
| GitHub App private key (or PAT) | Secret `hive-secrets` | key `gh-app-key.pem` (or `HIVE_GITHUB_TOKEN`), mounted read-only at `/secrets` | `src/deploy/k8s/secret.yaml`, `src/deploy/k8s/deployment.yaml` (volume `secrets`, `defaultMode: 0440`) |
| Base config | ConfigMap `hive-config` | key `hive.yaml`, mounted at `/etc/hive/hive.yaml` | `src/deploy/k8s/configmap.yaml`, `src/deploy/k8s/kustomization.yaml` |
| Dashboard-saved config overlay | PVC `hive-data` | `/data/hive.yaml.dashboard` (merged over the ConfigMap seed at boot) | `src/deploy/entrypoint.sh:533,550` |
| Runtime config (legacy PVC-first path) | PVC `hive-data` | `/data/hive.yaml.runtime` (and legacy `/data/hive.yaml.bak`) | `src/deploy/entrypoint.sh:69,262,681` |
| Beads ledgers (per-agent work state) | PVC `hive-data` | `/data/beads/<agent>/` (symlinked to `/home/dev/<agent>-beads`) | `src/deploy/entrypoint.sh:894-903` |
| Agent backend credentials / home dirs | PVC `hive-data` | `/data/home` (bind-seeded to `/data/home/.config`, `.bashrc`, `.profile`, per-agent `$HOME`) | `src/deploy/entrypoint.sh:151-169` |
| Backup encryption key (if the dashboard "Set key" flow was used) | PVC `hive-data` | `/data/secrets/backup_encryption_key`, mode `0600` | `src/docs/backup-restore.md` "Setting the backup encryption key (hosted flow)" |
| Dashboard auth / bootstrap secrets | Secret `hive-secrets` | keys `HIVE_DASHBOARD_TOKEN`, optionally `bob_api_key` | `src/deploy/k8s/secret.yaml` |
| Own dashboard/ingress route-reader RBAC | Role/RoleBinding `hive-dashboard-route-reader` | in-namespace, lets the hive discover its own served host | `src/deploy/k8s/dashboard-route-rbac.yaml` |

The GitHub App key and `HIVE_DASHBOARD_TOKEN` live in the Kubernetes **Secret**
object, not on the PVC — moving them is a Secret export/import, not a file
copy. Everything else that must survive the move (`hive-id`, beads, home/
agent state, config overlays) is on the PVC.

## Target cluster prerequisites (#15)

> **UNCONFIRMED** for your specific cluster pair — verify each of these
> against the target before starting, since none is checked automatically:

- A `StorageClass` that supports the access mode your PVC needs. The shipped
  default (`src/deploy/k8s/pvc.yaml`) is `ReadWriteOnce`, 10Gi; README.md's
  "Kubernetes Deployment" step 4 recommends an NFS-backed `ReadWriteMany`
  class only if you want zero-downtime rolling upgrades, which a cluster move
  is not.
- An ingress controller (or OpenShift Route support) matching what you use on
  the source — the base manifest set ships no Ingress/Route object at all
  (`src/deploy/k8s/kustomization.yaml` lists no Ingress; the OpenShift Route is
  an **additive** overlay, `src/deploy/kustomize/overlays/openshift/route.yaml`).
- `cert-manager` (or equivalent) if you terminate TLS the way README.md's
  "Set up Ingress with TLS" example does (`cert-manager.io/cluster-issuer`
  annotation).
- A cluster that **grants `NET_ADMIN`** to pods, or a plan to apply the
  `openshift-netadmin` overlay / set `HIVE_PROXY_ADVISORY_OK=true` if it
  doesn't — see the capability comments in `src/deploy/k8s/deployment.yaml`
  and `src/docs/net-admin-requirement.md`. Without either, the pod is
  admission-rejected (OpenShift) or the ACMM egress gate silently degrades.
- Matching CPU architecture for the image you pull, or a `stable`/`candidate`
  multi-arch tag — the manifest pins `ghcr.io/hivecommons/hive:stable`
  (`src/deploy/k8s/deployment.yaml`); **whether a specific pinned digest is
  required to match source and target is UNCONFIRMED** (#5) — nothing in the
  manifests or entrypoint asserts an image-identity check across a restore, so
  this is inspection, not a demonstrated requirement.

## Procedure

### 1. Escrow the backup encryption key first (#14)

If the dashboard's **Governor Config → Security → Backup → Set key** flow was
ever used on this hive, `/data/secrets/backup_encryption_key` exists and any
spoke backup archive taken with it is unrestorable without it
(`src/docs/backup-restore.md` "Security note"). Before touching anything,
copy it out of the cluster:

```sh
kubectl -n hive exec deploy/hive -- cat /data/secrets/backup_encryption_key
```

Store the value in a password manager or other off-cluster escrow — the same
rule `HIVE_BACKUP_KEY` uses for the hub disaster-recovery path.

### 2. Copy namespace resources to the target

From the source cluster, export the objects that are not PVC-backed:

```sh
kubectl --context <source> -n hive get secret hive-secrets -o yaml \
  | kubectl --context <target> -n hive apply -f -
kubectl --context <source> -n hive get configmap hive-config -o yaml \
  | kubectl --context <target> -n hive apply -f -
```

Apply the rest of the base manifest set (namespace, PVC, RBAC, Deployment,
Service) on the target from `src/deploy/k8s/` or the standalone Kustomize
overlay, editing the placeholders as the overlay's README describes
(`src/deploy/kustomize/overlays/standalone/README.md`) — do **not** copy the
source Deployment verbatim if the target is OpenShift: drop any pinned
`fsGroup`/`runAsUser` the same way `cross-cluster-migration.md` step 1 does,
since OpenShift assigns its own UID range.

### 3. Copy the PVC data

This is a generic `kubectl`-based copy — no cloud snapshot API. **Stop the
source pod first** (see [move-hub-registered-cutover.md](move-hub-registered-cutover.md)
for why, if this hive reports to a hub) so the data is not written while it is
being read:

```sh
kubectl --context <source> -n hive scale deploy/hive --replicas=0
kubectl --context <source> -n hive wait --for=delete pod -l app.kubernetes.io/name=hive --timeout=120s

# One-shot pod, source cluster, tars the PVC to stdout:
kubectl --context <source> -n hive run hive-pvc-export --rm -i --restart=Never \
  --image=docker.io/library/alpine:3.22 --overrides='
{
  "spec": {
    "containers": [{
      "name": "hive-pvc-export",
      "image": "docker.io/library/alpine:3.22",
      "command": ["tar", "czf", "-", "-C", "/data", "."],
      "volumeMounts": [{"name": "data", "mountPath": "/data", "readOnly": true}]
    }],
    "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": "hive-data"}}]
  }
}' > hive-data.tar.gz

# Restore into the target PVC (create it first, e.g. by applying pvc.yaml):
kubectl --context <target> -n hive run hive-pvc-import --rm -i --restart=Never \
  --image=docker.io/library/alpine:3.22 --overrides='
{
  "spec": {
    "containers": [{
      "name": "hive-pvc-import",
      "image": "docker.io/library/alpine:3.22",
      "command": ["tar", "xzf", "-", "-C", "/data"],
      "volumeMounts": [{"name": "data", "mountPath": "/data"}]
    }],
    "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": "hive-data"}}]
  }
}' < hive-data.tar.gz
```

This mirrors the volume-copy step `cross-cluster-migration.md` step 1
describes for hub-hosted hives, adapted to run with plain `kubectl` (no
`kubectl cp`, which struggles with large trees and special files) instead of
assuming a hub-side tool. **UNCONFIRMED**: ownership/permission bits on
extracted files may need a follow-up `chown`/`chmod` pass matching the
`fsGroup: 1002` / per-agent-UID scheme the source used, depending on whether
the target's storage class preserves numeric UIDs across nodes — this was not
exercised.

### 4. Update host-bound settings (#12)

Edit the ConfigMap `hive-config`'s `hive.yaml` (not the pod — the pod
re-seeds from the ConfigMap on every restart, same gotcha
`cross-cluster-migration.md` step 3 documents) for anything that changed on
the move:

- If you use the dashboard's `public_url` / your ingress rewrites `Host`, set
  `dashboard.public_url` to the new externally reachable origin.
- If your GitHub App's **Setup URL** / OAuth **callback URL** point at the old
  hostname, update them in the GitHub App settings — see
  [github-app-setup.md](github-app-setup.md).
- If this hive also reports to a hub (`hub.url` configured), set
  `hub.dashboard_url` to the new URL — see
  [move-hub-registered-cutover.md](move-hub-registered-cutover.md) for why
  this field, not the hub side, is authoritative.
- TLS: provision a certificate for the new hostname (new `cert-manager`
  Certificate, or your CA of choice) before cutting traffic over.

### 5. Recreate Ingress/TLS and restart

Apply an Ingress on the target pointing at the new hostname (README.md's
"Set up Ingress with TLS" gives a working template), then restart the pod so
it picks up the ConfigMap:

```sh
kubectl --context <target> -n hive rollout restart deploy/hive
```

### 6. Verify

- `kubectl --context <target> -n hive exec deploy/hive -- cat /data/hive-id`
  matches the value recorded from the source before the move.
- GitHub App key SHA-256 matches:
  `kubectl --context <target> -n hive exec deploy/hive -- sha256sum /secrets/gh-app-key.pem`
  against the same command run on the source before it was scaled down.
- `kubectl --context <target> -n hive exec deploy/hive -- ls /data/beads/` shows
  the same per-agent directories the source had.
- `curl -sf https://<new-host>/api/health` returns `{"status":"ok"}`.
- Sign in and confirm each agent backend (Copilot, Claude, etc.) launches
  without an unexpected re-authentication prompt — see #13 below for what
  this depends on.
- If this hive reports to a hub, confirm the hub sees the new
  `dashboard_url` within one heartbeat interval (see
  [move-hub-registered-cutover.md](move-hub-registered-cutover.md) "Verify").

Only once every check above passes, delete the source:

```sh
kubectl --context <source> -n hive delete namespace hive
```

## Open questions this issue answers or narrows

| # | Question | Status |
|---|---|---|
| 9 | Self-hosted K8s → K8s guide | **DOCUMENTED, NOT EXECUTED** (this document) |
| 12 | Host-bound settings for self-hosted K8s | **DOCUMENTED, NOT EXECUTED** — step 4 above; no hub auth proxy or `meta.json` is involved for a non-hub-registered self-hosted hive |
| 13 | Agent backend credential location / re-auth need | `/data/home` is the backend credential and per-agent `$HOME` store (`src/deploy/entrypoint.sh:151-169`) and is carried by the PVC copy in step 3, so a full-PVC copy should not force re-authentication — **UNCONFIRMED**: whether every supported backend (Copilot/Claude/Codex/bob) tolerates being moved to a new pod/host without re-auth was not exercised here; bob specifically requires `bob_api_key` at `/secrets/bob_api_key` rather than an on-disk session (`src/deploy/k8s/secret.yaml` comment), so it is unaffected by a `/data/home` move either way |
| 14 | Backup-key escrow as a pre-move step | **DOCUMENTED, NOT EXECUTED** — step 1 above |
| 15 | Target cluster prerequisites | **DOCUMENTED, NOT EXECUTED** — "Target cluster prerequisites" section above |
| 5 | Same image digest required on target | **UNCONFIRMED** — no code path asserts image-digest equality across a restore; inspection only |

See the epic ([#6521](https://github.com/hivecommons/hive/issues/6521)) for
the full table and the other sub-issues.
