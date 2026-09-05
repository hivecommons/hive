# Migrating a Hosted Hive Between Clusters (vanilla Kubernetes → OpenShift)

This guide documents the manual procedure for moving a hosted hive from one
cluster to another, using a real migration as the reference: a hive moved from
a **hub-reachable cluster** (vanilla Kubernetes) to a **heartbeat-only cluster**
(OpenShift). Every gotcha below was hit during that migration.

Moving a hive between clusters is a **manual procedure**. The hub used to
expose a `POST /api/saas/hives/{id}/migrate` endpoint that automated it as
"fresh provision on the target + deprovision of the source"; it was removed
because the automation was not worth the maintenance it demanded — it could
not work at all when the hub had no `kubectl` path to the target (the
heartbeat-only case), and it was actively destructive when run after a manual
move, since it found no source secrets to copy and provisioned a
credential-less hive over a working deployment.

## Background: how the two clusters differ

**Hub-reachable cluster (vanilla Kubernetes).** Dashboards are reached via
`*.hive.hivecommons.dev`. The hub's nginx ingress performs authentication
(`auth-url` pointing at `/api/saas/auth-check`) and injects `X-Hive-User` /
`X-Hive-Role` headers into proxied requests. Spokes on this cluster do **not**
need their own OAuth configuration — the hub is the auth proxy.

**Heartbeat-only cluster (OpenShift).** The hub has **no network path** to this
cluster; the spoke reports in via heartbeats only. Dashboards are therefore
exposed directly via OpenShift Routes on `*.apps.<your-cluster-domain>`, and
each spoke must run its **own GitHub device-flow login** — there is no hub
auth proxy in front of it.

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

Mirror an existing hive already running on the target (heartbeat-only) cluster
rather than writing these from scratch.

**Service** named `hive`, selector `app=hive` plus the hive-id label, with
two ports:

| Name | Port |
|------|------|
| `terminal` | 3001 |
| `dashboard` | 3002 |

**Two OpenShift Routes**, both with host
`<hive-id>.apps.<your-cluster-domain>` and TLS edge termination with
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
  # Required on a heartbeat-only cluster because there is no hub auth proxy in
  # front of the dashboard. Without it the login page errors:
  #   "oauth_client_id not configured"
  oauth_client_id: Ov23ligE2p0gjXg6xAUf

hub:
  # The spoke reports this URL in its heartbeats.
  dashboard_url: https://<hive-id>.apps.<your-cluster-domain>
```

> **Gotcha — the heartbeat owns `dashboardUrl`.** The hub registry's
> `dashboardUrl` (and therefore the **My Hives → Dashboard** button) comes
> from the spoke's **heartbeat**, not from anything you edit on the hub. If
> `hub.dashboard_url` is unset on the spoke, hand-editing the hub registry
> gets silently overwritten within ~5 minutes by the next heartbeat.

> **Gotcha — a migrated namespace needs the `hive-route-reader` RBAC.** When
> `hub.dashboard_url` is unset, the spoke discovers the host by reading its
> **own** Route/Ingress, which requires the namespace-scoped read-only
> `hive-route-reader` Role and RoleBinding. Migration is exactly when this
> bites: the served host changes with the cluster, and a namespace recreated
> from an older template — or hand-built by copying manifests — may not carry
> the Role. Without it the spoke reports the synthesised `<hive-id>.<hub host>`
> and the **Dashboard** button 503s. Apply it as shown in
> [Provisioning a Hosted Hive → B.2](manual-provisioning.md#hive-route-reader--why-the-dashboard-link-503s-without-it),
> **binding the ServiceAccount the hive Deployment actually uses** (`hive-sa`
> on SCC clusters, `default` elsewhere — read it off the Deployment, do not
> assume).

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

- `cluster_id` → the target (heartbeat-only) cluster's ID
- `subdomain` → `<hive-id>.apps.<your-cluster-domain>`

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
kubectl --context <source-cluster> delete namespace hive-hosted-<hive-id>
```

### 6. Verify

- Dashboard returns 200 at
  `https://<hive-id>.apps.<your-cluster-domain>`.
- GitHub sign-in (device flow) works on the spoke's login page.
- Hub registry shows the hive on the new cluster with fresh heartbeats.
- The **My Hives → Dashboard** button targets the new URL (i.e. the
  heartbeat has propagated `dashboard_url`).

## Future work

Worth considering: a hub-side **"adopt existing deployment"** action that
takes a hive moved by hand (or living on a cluster the hub cannot reach) and
simply updates the hub-side records — `meta.json` and the registry entry — to
point at it. That is the genuinely useful half of what the removed migrate API
did, without the provision-and-deprovision machinery that made it fragile.
