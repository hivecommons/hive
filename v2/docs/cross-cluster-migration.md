# Migrating a Hosted Hive Between Clusters (vanilla Kubernetes → OpenShift)

This guide documents the manual procedure for moving a hosted hive from one
cluster to another, using a real migration as the reference: the `kellyaa`
hive, moved from **hive-oke** (vanilla Kubernetes, OKE) to **vllm-d**
(OpenShift). Every gotcha below was hit during that migration.

Use this procedure whenever the hub's built-in migrate API cannot do the job —
in particular when the target cluster is **heartbeat-only** (the hub has no
`kubectl` path to it), which is exactly the vllm-d situation.

## Background: how the two clusters differ

**hive-oke (vanilla Kubernetes).** Dashboards are reached via
`*.hive.kubestellar.io`. The hub's nginx ingress performs authentication
(`auth-url` pointing at `/api/saas/auth-check`) and injects `X-Hive-User` /
`X-Hive-Role` headers into proxied requests. Spokes on this cluster do **not**
need their own OAuth configuration — the hub is the auth proxy.

**vllm-d (OpenShift).** The hub has **no network path** to this cluster; the
spoke reports in via heartbeats only. Dashboards are therefore exposed
directly via OpenShift Routes on `*.apps.fmaas-vllm-d.fmaas.res.ibm.com`, and
each spoke must run its **own GitHub device-flow login** — there is no hub
auth proxy in front of it.

## Warning: do NOT use the built-in migrate API for this

The hub exposes `POST /api/saas/hives/{id}/migrate`, which implements
migration as **fresh provision on the target + deprovision of the source**.
Mid-flight it reads the `hive-secrets` secret and `hive-config` ConfigMap
**from the source cluster** to carry credentials over.

Two reasons it must not be used here:

1. **It cannot work when the hub can't `kubectl` to the target.** vllm-d is
   heartbeat-only, so the hub has no way to provision anything there.
2. **It is destructive after a manual migration.** If the source namespace
   was already deleted (because you migrated by hand), calling the API will
   find no secrets to read and will provision a **credential-less** hive on
   top of your working target deployment.

> **Gotcha — a stuck `migration_status` blocks future migrate calls.** The
> API rejects any hive whose `migration_status` is `"migrating"` with a 409.
> If a previous attempt died mid-flight, clear the field in the hive's
> `meta.json` (step 4 below).

## When to use the built-in migrate API

The built-in migrate API is the automatic path. It moves a hive from one
cluster to another. It works as fresh provision on the target plus
deprovision of the source. It copies no data. The hive rebuilds its state
from GitHub on the target.

Use it when all of these hold:

1. **The hub can run `kubectl` against both clusters.** Each cluster needs
   an entry in `/data/saas/clusters.json` on the hub PVC. The entry carries
   a `kubeconfig_path` and a `context` for the hub to use. The hub runs
   plain `kubectl` for the in-cluster entry.

   A cluster marked `"pull_only": true` is excluded. Its spokes connect
   outbound over the heartbeat and the hub cannot `kubectl` into them, so
   it can be neither the source nor the target of a migrate call — the API
   rejects both with a 400 before any migration state is recorded. Move
   those hives by hand using the manual procedure below.
2. **No hive data must be preserved.** The API does not copy volume data.
3. **You are the hive owner or the hub admin.** The hub rejects any other
   caller with a 403.

### The API call

The endpoint is `POST /api/saas/hives/{id}/migrate`. The body is JSON with
one field, `target_cluster_id`. The body is limited to 1024 bytes. The
endpoint requires hub auth.

```bash
curl -X POST -b "hive_hub_user=YOUR_USERNAME" \
  -H "Content-Type: application/json" \
  -d '{"target_cluster_id":"<target-cluster-id>"}' \
  https://hive.kubestellar.io/api/saas/hives/<hive-id>/migrate
```

The hub answers immediately with HTTP 200:

```json
{"status":"migrating","from":"<source-cluster-id>","to":"<target-cluster-id>"}
```

The migration runs in a background goroutine. The response above is not the
outcome. The dashboard Move-to-cluster modal calls the same endpoint.

### What the hub does

1. **Validates the request.** The hive must exist. The caller must be the
   owner or the hub admin. The hive must not be migrating already. The
   target cluster must exist and differ from the current cluster.
2. **Marks the hive as migrating.** It sets `migration_status` to
   `"migrating"` and saves the hive record.
3. **Reads the credentials from the source.** It reads the `hive-secrets`
   Secret and the `hive-config` ConfigMap in the source namespace. It
   carries the GitHub token, or the GitHub App private key with `app_id`
   and `installation_id`.
4. **Provisions a fresh hive on the target.** The new hive keeps the same
   org, repos, primary repo, and ACMM level. The subdomain becomes
   `<hive-id>.<target domain>`.
5. **On provision failure, marks the migration as failed.** It sets
   `migration_status` to `"failed"` and records the error in the hive
   record. It restores `status` to `"running"`.
6. **On success, deprovisions the source.** It deletes the namespace, the
   PV and OCI resources for NFS storage, the SCC binding on OpenShift, the
   on-disk hive directory, and the user quota records.
7. **Updates the hive record.** It sets `cluster_id` to the target, `status`
   to `"provisioning"`, and `migration_status` to `"completed"`.
8. **Updates the registry entry.** It sets the new cluster ID, name, and
   dashboard URL. A claimed hive keeps its vanity URL.
9. **The provision watcher finishes the move.** It polls the hive
   deployment on the target. Once the deployment has 1 available replica,
   it sets `status` to `"running"` and clears the migration fields.

### Failure signals

A failed migration leaves `migration_status` as `"failed"` in the hive
record. The `error` field of the record carries the message. The record
lives at `/data/saas/hives/<hive-id>/meta.json` on the hub PVC.

### Verify success

Check the deployment on the target cluster:

```sh
kubectl get deployment hive -n hive-hosted-<hive-id> \
  -o jsonpath={.status.availableReplicas}
```

A value of `1` means the hive runs on the target. The registry entry shows
the new cluster. The `migration_status`, `migration_from`, and
`migration_to` fields in `meta.json` are cleared.

### Gotchas

- A hive with `migration_status` set to `"migrating"` rejects any further
  migrate call with a 409. If an attempt failed mid-flight, clear the field
  in `meta.json` before you retry.
- The API is destructive. It provisions fresh and deprovisions the source.
  It copies no data. State must be rebuildable from GitHub.

Implementation: `handleMigrateHive` in `pkg/hub/saas.go`, plus `migrateHive`,
`deprovisionHive`, and `startProvisionWatcher` in `pkg/hub/saas_provision.go`.

## Manual migration procedure

### 1. Copy the namespace resources to the target

From the source namespace (`hive-hosted-<hive-id>`), copy to the target:

- **Deployment** — adjusted for OpenShift's restricted SCC: remove any
  `fsGroup` from the pod `securityContext` and do not pin `runAsUser`;
  OpenShift assigns an arbitrary UID from the namespace's range.
- **`hive-secrets` Secret** — keys `dashboard-token`, `github-token`,
  `gh-app-key.pem`.
- **`hive-config` ConfigMap** — the `hive.yaml` payload (edited in step 3).
- **PVC data** — if the hive is stateful, copy the volume contents before
  deleting the source.

### 2. Create the Service and Routes

Mirror an existing vllm-d hive (e.g. `osscar`) rather than writing these from
scratch.

**Service** named `hive`, selector `app=hive` plus the hive-id label, with
two ports:

| Name | Port |
|------|------|
| `terminal` | 3001 |
| `dashboard` | 3002 |

**Two OpenShift Routes**, both with host
`<hive-id>.apps.fmaas-vllm-d.fmaas.res.ibm.com` and TLS edge termination with
`insecureEdgeTerminationPolicy: Redirect`:

| Route | Path | targetPort |
|-------|------|-----------|
| `hive-dashboard` | `/` | `dashboard` |
| `hive-terminal` | `/terminal` | `terminal` |

### 3. Spoke config additions (in the `hive-config` ConfigMap)

> **Gotcha — edit the ConfigMap, not the pod.** The pod re-seeds
> `/etc/hive/hive.yaml` from the ConfigMap on **every restart**, so the
> ConfigMap is the durable location. Editing the file inside the pod is lost
> on the next restart.

Add to `hive.yaml`:

```yaml
github:
  # Public Hive GitHub App client ID — enables device-flow dashboard login.
  # Required on vllm-d because there is no hub auth proxy in front of the
  # dashboard. Without it the login page errors:
  #   "oauth_client_id not configured"
  oauth_client_id: Ov23ligE2p0gjXg6xAUf

hub:
  # The spoke reports this URL in its heartbeats.
  dashboard_url: https://<hive-id>.apps.fmaas-vllm-d.fmaas.res.ibm.com
```

> **Gotcha — the heartbeat owns `dashboardUrl`.** The hub registry's
> `dashboardUrl` (and therefore the **My Hives → Dashboard** button) comes
> from the spoke's **heartbeat**, not from anything you edit on the hub. If
> `hub.dashboard_url` is unset on the spoke, hand-editing the hub registry
> gets silently overwritten within ~5 minutes by the next heartbeat.

> **Gotcha — old `copy-config` init containers invert config precedence.**
> Deployments provisioned from older templates ship a `copy-config` init
> container that prefers the PVC backup over the ConfigMap:
>
> ```sh
> if [ -f /data/hive.yaml.runtime ]; then cp /data/hive.yaml.runtime /etc/hive/hive.yaml; ...
> ```
>
> If the migrated `/data` volume carries a stale runtime config, **every
> ConfigMap edit is silently overridden on restart**. Symptom: keys present
> in the ConfigMap but empty (or missing) in the pod's
> `/etc/hive/hive.yaml`. Fix during migration:
>
> 1. Move the stale file aside. Check **both** names — a hive that has not
>    saved since the `hive.yaml.bak` → `hive.yaml.runtime` rename carries only
>    the legacy one, and leaving it behind reintroduces the same override:
>
>    ```sh
>    for f in /data/hive.yaml.runtime /data/hive.yaml.bak; do
>      [ -f "$f" ] && mv "$f" "$f.stale-$(date +%s)"
>    done
>    ```
>
> 2. Patch the init container to the current template's command, where the
>    ConfigMap always wins:
>
>    ```sh
>    cp /etc/hive-seed/hive.yaml /etc/hive/hive.yaml && echo configmap-copied; if [ -f /data/hive.yaml.runtime ]; then echo runtime-config-exists-for-recovery; elif [ -f /data/hive.yaml.bak ]; then echo legacy-runtime-config-exists-for-recovery; fi
>    ```
>
> Checking the `copy-config` command should be a **standard migration step**
> whenever the source hive was provisioned before the template fix.

Then restart the spoke to pick up the ConfigMap:

```sh
kubectl -n hive-hosted-<hive-id> rollout restart deploy/hive
```

> **Gotcha — access grants are lost until the migrated spoke is rolled.** A
> migration lands the hive on a **fresh PVC**, so the first `authorized_users`
> the hub/dashboard writes go into `/data/hive.yaml.dashboard` *after* the new
> pod has already booted. The running device-flow authorizer builds its access
> list **at startup only** (config hot-reload does not rebuild it), so a user who
> is `read-write` in **Manage Access** is rejected at login with
> `device-flow login rejected: user not authorized for this hive`.
>
> Make this a **mandatory final migration step**:
>
> ```sh
> # 1. The grant IS in the effective config (overlay merged at boot):
> kubectl -n hive-hosted-<hive-id> exec deploy/hive -c hive -- \
>   grep -A8 authorized_users /etc/hive/hive.yaml
> # 2. Roll the spoke so the authorizer rebuilds from the current overlay:
> kubectl -n hive-hosted-<hive-id> rollout restart deploy/hive
> # 3. Verify no rejections after the roll:
> kubectl -n hive-hosted-<hive-id> logs deploy/hive -c hive | grep 'not authorized'
> ```
>
> Do **not** try to repair this by hand-creating `/data/hive.yaml` — the
> effective config is the ConfigMap seed merged with the `.dashboard` overlay;
> a stray `/data/hive.yaml` is never read and only muddies diagnosis.

### 4. Update the hub's records

These live on the hub pod's PVC — **back them up first** (`kubectl cp` or a
`tar` inside the pod).

**`/data/saas/hives/<hive-id>/meta.json`:**

- `cluster_id` → `vllm-d`
- `subdomain` → `<hive-id>.apps.fmaas-vllm-d.fmaas.res.ibm.com`
- `migration_status` → clear it (or set to `"completed"`). A leftover
  `"migrating"` value blocks any future migrate call with a 409.

**`/data/hub-registry.json`:** update the hive's entry — `clusterId` /
`clusterName`. You can leave `dashboardUrl` alone: once step 3 is done, the
next heartbeat corrects it.

Reload the hub:

```sh
kubectl rollout restart deploy/hive-hub
```

### 5. Delete the source namespace — only after verifying the target

Deleting the source namespace frees its RWO block volume (which can only be
attached to one node anyway). Before deleting, decide whether any
pre-migration PVC data needs to be kept.

```sh
kubectl --context hive-oke delete namespace hive-hosted-<hive-id>
```

### 6. Verify

- Dashboard returns 200 at
  `https://<hive-id>.apps.fmaas-vllm-d.fmaas.res.ibm.com`.
- GitHub sign-in (device flow) works on the spoke's login page.
- Hub registry shows the hive on the new cluster with fresh heartbeats.
- The **My Hives → Dashboard** button targets the new URL (i.e. the
  heartbeat has propagated `dashboard_url`).

## Future work

The migrate API should gain an **"adopt existing deployment"** mode: instead
of provision-and-deprovision, it would accept a hive that was moved manually
(or lives on a cluster the hub cannot reach) and simply update the hub-side
records (`meta.json`, registry entry) to point at it. That would eliminate
both failure modes above — the destructive re-provision over a working
target, and the hard requirement that the hub can `kubectl` to both clusters.
