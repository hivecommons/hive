# Provisioning a Hosted Hive

This guide documents how hosted hives are provisioned, covering both paths:

- **Automated provisioning** — the hub creates everything itself, on any cluster
  the hub can reach with `kubectl`. This is the normal path.
- **Manual provisioning** — hand-applied manifests, required when the hub has
  **no network path** to the target cluster (a *heartbeat-only* cluster). This
  is the situation on **vllm-d**.

Every command and manifest below was executed and verified while standing up a
pool of ten placeholder hives — five on **vllm-d** (manual) and five on
**hive-oke** (automated) — so the procedure is real, not aspirational. Where a
step has a non-obvious failure mode, it is called out as a **Gotcha**.

---

## Background: how the two clusters differ

The provisioning path is dictated entirely by whether the hub can `kubectl` to
the target cluster.

| | **hive-oke** (vanilla Kubernetes) | **vllm-d** (OpenShift) |
|---|---|---|
| Hub reachability | Hub **can** `kubectl` → **automated** provisioning | Hub is **heartbeat-only** → **manual** provisioning |
| Dashboard routing | nginx **Ingress**, host `<id>.hive.kubestellar.io` | OpenShift **Route**, host `<id>.apps.fmaas-vllm-d.fmaas.res.ibm.com` |
| Auth in front of the dashboard | Hub's nginx ingress runs `auth-url` → `/api/saas/auth-check`, injecting `X-Hive-User` / `X-Hive-Role`. Spokes need **no** own OAuth. | No hub auth proxy. Each spoke runs its **own** GitHub device-flow login (`oauth_client_id`, `hub_proxied: false`). |
| Pod security | Standard Kubernetes; empty `securityContext`. | OpenShift SCC. The pod **must** run under the `anyuid` SCC (the entrypoint `chown`s the PVC as root). Without it the pod lands on `restricted-v2` and crash-loops. |
| Storage | RWX volume, default storage class. | RWX on `ocs-storagecluster-cephfs`. |
| Which methods it serves | **Public** methods (claude / copilot / gemini — subscription CLIs). | **Private** methods (litellm / vllm / llm-d — self-hosted inference). |

> **Why methods map to clusters.** Public-method hives run subscription CLIs and
> need no in-cluster inference, so they live on the hub's own cluster (hive-oke).
> Private-method hives point at a self-hosted inference endpoint that lives on
> vllm-d, so the hive runs there too — and vllm-d is heartbeat-only, which is
> exactly why it needs the manual path.

---

## Prerequisites (both paths)

- `kubectl` access with a context per cluster (`hive-oke`, `vllm-d`).
- The hub is running on **hive-oke** in namespace `hive-hub`, backed by a RWX
  PVC named `hive-hub-data-rwx` mounted at `/data`. The hub's SaaS store lives
  at `/data/saas/hives/<id>/meta.json`; its fleet registry at
  `/data/hub-registry.json`.
- For manual (vllm-d) provisioning: the `system:openshift:scc:anyuid`
  ClusterRole must exist (it does by default on OpenShift).

---

## Path A — Automated provisioning (hive-oke)

The hub exposes `POST /api/saas/hives` (`handleCreateHive`). It generates the
hive ID, creates the namespace + RBAC + PVC + Service + Ingress + Deployment,
and writes the SaaS `meta.json` — all automatically.

### A.1 Call the API as the hub admin

`handleCreateHive` authenticates via the `hive_hub_user` cookie, whose value is
simply the username, validated against the SaaS user store. From **inside the
hub pod** (localhost), that is all you need:

```bash
HUB_POD=$(kubectl --context hive-oke -n hive-hub get pods -l app=hive-hub \
  --no-headers | grep Running | awk '{print $1}' | head -1)

kubectl --context hive-oke -n hive-hub exec "$HUB_POD" -- curl -s -X POST \
  -H "Cookie: hive_hub_user=clubanderson" \
  -H "Content-Type: application/json" \
  -d '{
        "org": "myorg",
        "repos": "myrepo",
        "primary_repo": "myrepo",
        "project_name": "My Hive",
        "acmm_level": 2,
        "cluster_id": "hive-oke",
        "auth_method": "app",
        "app_id": "999999999",
        "installation_id": "",
        "app_private_key": "",
        "is_public": false
      }' \
  http://localhost:80/api/saas/hives
```

Response:

```json
{"id":"hosted-myorg-myrepo-ab12","status":"provisioning",
 "subdomain":"hosted-myorg-myrepo-ab12.hive.kubestellar.io"}
```

The generated ID is `hosted-<org>-<primary_repo>-<4char>`.

**`CreateHiveRequest` fields that matter:**

| Field | Notes |
|---|---|
| `org`, `repos` | **Required, non-empty.** `repos` is comma-separated. |
| `primary_repo` | Hosts the advisory issue and default markings. |
| `acmm_level` | Starting autonomy. **Default new hives to `2`** (Instructed / advisory-only). |
| `cluster_id` | `hive-oke` (the `defaultClusterID`). |
| `auth_method` | `app` or a token. |
| `app_id` / `installation_id` / `app_private_key` | Three auth shapes: **token** (`github_token` starts `ghp_`/`github_pat_`); **app now** (all three set); **app later** — `app_id` set, `installation_id` **and** `app_private_key` **empty**. The last is the placeholder case: the hive provisions, then 401s until the owner installs the App from the dashboard. |
| `is_public` | Pointer. **Absent defaults to public.** Send `false` explicitly for a private hive. |

> **Gotcha — `auth_method: app` with no key provisions but reports `error`.**
> The "app later" path leaves the hive with no valid credentials, so its
> `meta.json` `status` becomes `"error"` with
> `"provisioning failed — check hub logs for details"`, and it 401-loops until
> the App is installed. This is expected for a not-yet-claimed hive. If you want
> it to read as `available` instead of `error`, patch the `meta.json` status
> (see [Placeholder pools](#placeholder-pools)).

### A.2 What the automated path creates that the manual path does not

The automated provisioner also provisions an **OCI file-system export** for the
PVC and records it in `meta.json` (`oci_file_system_id`, `oci_export_id`). The
manual path uses an in-cluster RWX PVC directly and has no OCI export. This is
the main structural difference between the two `meta.json` shapes.

---

## Path B — Manual provisioning (vllm-d)

The hub cannot reach vllm-d, so every object is applied by hand with
`kubectl --context vllm-d`. The full set, in order:

1. Namespace
2. ServiceAccount (`hive-sa`)
3. RBAC — two Roles (`hive-secrets-writer`, `hive-self-upgrade`) and three
   RoleBindings (the two above **plus** `hive-anyuid`)
4. PVC (`hive-data`, RWX cephfs, 50Gi)
5. ConfigMap (`hive-config`) — the first-boot config **seed**
6. Secret (`hive-secrets`) — dashboard token, GitHub App key, LiteLLM key
7. Service (`hive`, ports 3002 dashboard / 3001 terminal)
8. Routes (`hive-dashboard` on `/`, `hive-terminal` on `/terminal`)
9. Deployment (`hive`)
10. Hub SaaS record — `meta.json` on the hub PVC

Set these shell variables for the target hive:

```bash
CTX=vllm-d
ID=hosted-myorg-myrepo          # the hive ID
NS=hive-hosted-$ID              # namespace is always hive-hosted-<id>
ROUTE_HOST=$ID.apps.fmaas-vllm-d.fmaas.res.ibm.com
IMAGE=ghcr.io/kubestellar/hive:v2-latest
SC=ocs-storagecluster-cephfs
```

### B.1 Namespace + ServiceAccount

```bash
kubectl --context "$CTX" create ns "$NS"
kubectl --context "$CTX" label ns "$NS" app=hive
kubectl --context "$CTX" -n "$NS" create sa hive-sa
```

### B.2 RBAC — Roles and RoleBindings

```bash
cat <<YAML | kubectl --context "$CTX" apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: { name: hive-secrets-writer, namespace: ${NS} }
rules:
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get","list","watch","create","update","patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: { name: hive-self-upgrade, namespace: ${NS} }
rules:
- apiGroups: ["apps"]
  resources: ["deployments","deployments/scale"]
  verbs: ["get","list","watch","update","patch"]
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get","list","watch","delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: { name: hive-secrets-writer, namespace: ${NS} }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: Role, name: hive-secrets-writer }
subjects:
- { kind: ServiceAccount, name: hive-sa, namespace: ${NS} }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: { name: hive-self-upgrade, namespace: ${NS} }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: Role, name: hive-self-upgrade }
subjects:
- { kind: ServiceAccount, name: hive-sa, namespace: ${NS} }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: { name: hive-anyuid, namespace: ${NS} }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: system:openshift:scc:anyuid }
subjects:
- { kind: ServiceAccount, name: hive-sa, namespace: ${NS} }
YAML
```

> **Gotcha — the RoleBinding subject namespace must be the hive's own namespace.**
> If you build these by copying another hive's manifests, the `subjects[].namespace`
> fields will still point at the **source** namespace. The bindings then bind a
> ServiceAccount that does not exist here, the pod does **not** get the `anyuid`
> SCC, it falls to `restricted-v2`, and it **crash-loops with exit 255**. Always
> template `subjects[].namespace` to `$NS`. Verify after applying:
>
> ```bash
> kubectl --context "$CTX" -n "$NS" get rolebinding hive-anyuid \
>   -o jsonpath='{.subjects[0].namespace}'   # must equal $NS
> ```

### B.3 PVC

```bash
cat <<YAML | kubectl --context "$CTX" apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: hive-data, namespace: ${NS}, labels: { app: hive } }
spec:
  accessModes: ["ReadWriteMany"]
  resources: { requests: { storage: 50Gi } }
  storageClassName: ${SC}
YAML
```

> **Use RWX (`ReadWriteMany`).** Both the old and new pod mount the same volume
> during a rolling upgrade (`maxUnavailable: 0`), so the volume must support
> multi-attach — hence cephfs, not RWO block storage.

### B.4 ConfigMap (the config **seed**)

```bash
cat <<YAML | kubectl --context "$CTX" apply -f -
apiVersion: v1
kind: ConfigMap
metadata: { name: hive-config, namespace: ${NS}, labels: { app: hive } }
data:
  hive.yaml: |
    project:
      org: myorg
      repos: [myrepo]
      primary_repo: myrepo
    agents:
      guide:   { backend: copilot, model: claude-sonnet-4-6, enabled: true }
      scanner: { backend: copilot, model: claude-sonnet-4-6, enabled: true }
    governor:
      eval_interval_s: 300
      modes:
        idle: { threshold: 0,  guide: 4h, scanner: 4h }
        busy: { threshold: 10, guide: 2h, scanner: 2h }
    github:
      app_id: 999999999               # placeholder until the real App is installed
      installation_id:
      key_file: /secrets/gh-app-key.pem
      oauth_client_id: Ov23ligE2p0gjXg6xAUf   # public device-flow client (no secret)
    dashboard:
      port: 3002
      authorized_users:
        - owner-github-login:owner
      hub_proxied: false              # vllm-d has no hub auth proxy → direct device-flow
    hub:
      enabled: true
      url: https://hive.kubestellar.io
      dashboard_url: https://${ROUTE_HOST}
      hive_type: hosted
      is_public: false
    acmm_level: 2
YAML
```

> **Gotcha — `github.app_id` cannot be empty.** Config validation requires
> **either** `github.token` **or** `github.app_id` to be set
> (`pkg/config/config.go`: `github.token or github.app_id is required`). A hive
> awaiting its real App must carry a **placeholder** `app_id` (e.g. `999999999`)
> or it will refuse to boot.

> **Gotcha — `hub_proxied` must be `false` on vllm-d.** vllm-d has no hub nginx
> auth proxy. If `hub_proxied` is `true`, dashboard sign-in enters an OAuth
> redirect loop. On hive-oke it is the opposite — the hub proxy handles auth.

### B.5 Secret

```bash
DASH_TOKEN=$(openssl rand -hex 32)
cat <<YAML | kubectl --context "$CTX" apply -f -
apiVersion: v1
kind: Secret
metadata: { name: hive-secrets, namespace: ${NS}, labels: { app: hive } }
type: Opaque
stringData:
  dashboard-token: "${DASH_TOKEN}"
  gh-app-key.pem: "PLACEHOLDER-awaiting-github-app-key"
  litellm_api_key: "sk-PLACEHOLDER"
YAML
```

### B.6 Service

```bash
cat <<YAML | kubectl --context "$CTX" apply -f -
apiVersion: v1
kind: Service
metadata: { name: hive, namespace: ${NS}, labels: { app: hive } }
spec:
  selector: { app: hive }
  ports:
  - { name: dashboard, port: 3002, targetPort: 3002 }
  - { name: terminal,  port: 3001, targetPort: 3001 }
YAML
```

### B.7 Routes (OpenShift)

```bash
cat <<YAML | kubectl --context "$CTX" apply -f -
apiVersion: route.openshift.io/v1
kind: Route
metadata: { name: hive-dashboard, namespace: ${NS}, labels: { app: hive } }
spec:
  host: ${ROUTE_HOST}
  path: /
  port: { targetPort: dashboard }
  tls: { termination: edge, insecureEdgeTerminationPolicy: Redirect }
  to: { kind: Service, name: hive, weight: 100 }
  wildcardPolicy: None
---
apiVersion: route.openshift.io/v1
kind: Route
metadata: { name: hive-terminal, namespace: ${NS}, labels: { app: hive } }
spec:
  host: ${ROUTE_HOST}
  path: /terminal
  port: { targetPort: terminal }
  tls: { termination: edge, insecureEdgeTerminationPolicy: Redirect }
  to: { kind: Service, name: hive, weight: 100 }
  wildcardPolicy: None
YAML
```

### B.8 Deployment

```bash
cat <<YAML | kubectl --context "$CTX" apply -f -
apiVersion: apps/v1
kind: Deployment
metadata: { name: hive, namespace: ${NS}, labels: { app: hive } }
spec:
  replicas: 1
  strategy: { type: RollingUpdate, rollingUpdate: { maxUnavailable: 0, maxSurge: 1 } }
  selector: { matchLabels: { app: hive } }
  template:
    metadata: { labels: { app: hive } }
    spec:
      serviceAccountName: hive-sa
      containers:
      - name: hive
        image: ${IMAGE}
        imagePullPolicy: Always
        ports:
        - { name: dashboard, containerPort: 3002 }
        - { name: terminal,  containerPort: 3001 }
        env:
        - { name: HIVE_ID, value: "${ID}" }
        volumeMounts:
        - { name: data,    mountPath: /data }
        - { name: config,  mountPath: /etc/hive }
        - { name: secrets, mountPath: /secrets }
      volumes:
      - { name: data,    persistentVolumeClaim: { claimName: hive-data } }
      - { name: config,  configMap: { name: hive-config } }
      - { name: secrets, secret: { secretName: hive-secrets } }
YAML
```

> **`maxUnavailable: 0`** keeps the current pod serving until the new one is
> ready — uninterrupted upgrades. It only works because the PVC is RWX.

> **Gotcha — some clusters enforce an `owner` label.** A `ValidatingAdmissionPolicy`
> on vllm-d requires an `owner` label on `PersistentVolumeClaim`, `ConfigMap`,
> `Secret`, `Service`, and `Deployment`. Objects still apply without it, but each
> emits a warning. Add `owner: <github-login>` to every `metadata.labels` block
> to silence them.

### B.9 Verify the pod comes up on `anyuid`

```bash
kubectl --context "$CTX" -n "$NS" rollout status deploy/hive --timeout=120s
POD=$(kubectl --context "$CTX" -n "$NS" get pods -l app=hive \
  -o jsonpath='{.items[0].metadata.name}')
# MUST be "anyuid" — if it says "restricted-v2", the anyuid binding is wrong
kubectl --context "$CTX" -n "$NS" get pod "$POD" \
  -o jsonpath='{.metadata.annotations.openshift\.io/scc}'
```

---

## The hub SaaS record (`meta.json`) — required for both paths

Automated provisioning writes this for you. **Manual provisioning does not** —
you must create it by hand, or the hive is invisible in **My Hives** and every
management action (upgrade, migrate, claim) returns **`hive not found`**.

The read is `loadSaaSHive(id)` → `/data/saas/hives/<id>/meta.json`, read **live
from disk on every call**, so a new or edited file takes effect immediately with
no hub restart.

```bash
ID=hosted-myorg-myrepo
HUB_POD=$(kubectl --context hive-oke -n hive-hub get pods -l app=hive-hub \
  --no-headers | grep Running | awk '{print $1}' | head -1)

cat > /tmp/meta.json <<JSON
{
  "id": "${ID}",
  "owner": "owner-github-login",
  "project_name": "My Hive",
  "org": "myorg",
  "repos": ["myrepo"],
  "primary_repo": "myrepo",
  "acmm_level": 2,
  "status": "",
  "created_at": "",
  "subdomain": "",
  "auto_upgrade": true,
  "is_public": false,
  "cluster_id": "vllm-d"
}
JSON

kubectl --context hive-oke -n hive-hub exec "$HUB_POD" -- \
  sh -c "mkdir -p /data/saas/hives/${ID}"
kubectl --context hive-oke -n hive-hub cp /tmp/meta.json \
  hive-hub/"$HUB_POD":/data/saas/hives/${ID}/meta.json
```

**Visibility rule.** `handleMyHives` shows a hive to a user when the user is its
`owner`, has a role on it, **or** is the hub admin (`clubanderson`) — even if the
hive has no fleet-registry entry (never heartbeated). So a hive with
`owner: <someone>` in its `meta.json` appears in that person's My Hives (and the
admin's) as soon as the file exists.

> **`meta.json` requirement is why heartbeats can be accepted but upgrades fail.**
> The heartbeat path is separate from `loadSaaSHive`. A hive can heartbeat and
> appear "online" while a missing `meta.json` makes the **Upgrade** button (and
> claim, migrate, toggle-auto-upgrade) return `hive not found`. If you see that,
> the fix is almost always a missing `meta.json`.

---

## Config precedence — the PVC overlay is authoritative

At runtime the effective config is **not** the ConfigMap. The entrypoint:

1. seeds `/etc/hive/hive.yaml` from the `hive-config` ConfigMap on boot, then
2. merges the PVC dashboard overlay `/data/hive.yaml.dashboard` **over** it, and
3. writes `/data/hive.yaml.bak`.

The dashboard's `Config.Save()` writes the overlay. Therefore:

- **Editing the ConfigMap after first boot does nothing** to the running config —
  the PVC overlay wins.
- To change a running hive's config, edit `/data/hive.yaml.dashboard` on the PVC
  (while scaled to 0, or the running process re-saves from memory), **or** clear
  `/data/hive.yaml.dashboard` + `/data/hive.yaml.bak` to let it reseed.
- Verify effective config by `grep`-ing `/etc/hive/hive.yaml` in the running pod,
  **never** the ConfigMap.

---

## Placeholder pools

A **placeholder** is a fully-provisioned but idle hive, waiting to be claimed —
so a request for access is satisfied in seconds instead of the full 5–10 minute
provision. Two pools, one per method type:

- **vllm-d** placeholders serve **private** methods — provisioned manually
  (Path B), then **scaled to 0**.
- **hive-oke** placeholders serve **public** methods — provisioned via the API
  (Path A), then **scaled to 0**.

Provision each placeholder as above, then:

```bash
# manual (vllm-d): set replicas: 0 in the Deployment (or scale after apply)
kubectl --context vllm-d -n "$NS" scale deploy/hive --replicas=0

# automated (hive-oke): the API provisions at replicas=1 — scale it down
kubectl --context hive-oke -n "hive-hosted-$ID" scale deploy/hive --replicas=0
```

Give every placeholder a `meta.json` with **`owner: clubanderson`** (admin-only
visibility), **`status: "available"`**, and **`acmm_level: 2`**:

```json
{ "id": "hosted-available-vllmd-01", "owner": "clubanderson",
  "org": "available-vllmd-01", "repos": [], "primary_repo": "",
  "acmm_level": 2, "status": "available", "auto_upgrade": false,
  "is_public": false, "cluster_id": "vllm-d" }
```

> **Gotcha — automated placeholders leave a fleet-registry entry.** Because the
> API provisions at `replicas=1`, an hive-oke placeholder heartbeats once before
> you scale it down, leaving a registry entry that pins its version and
> last-known ACMM and shows it "online" for `staleThreshold` (15 min). A
> manually-provisioned placeholder created at `replicas=0` never heartbeats and
> has no registry entry. To make the two pools render identically (no version,
> no online dot, ACMM from `meta.json`), remove the placeholder's registry entry:
> scale the hub to 0, edit `/data/hub-registry.json` on the hub PVC to drop the
> entry, then scale the hub back up. (The hub holds the registry in memory and
> rewrites the file, so a live edit is clobbered — you must stop it first.)

### Claiming a placeholder

Until the dashboard "assign" flow lands, claiming is manual:

1. Edit the placeholder's `meta.json`: set the real `owner`, `org`, `repos`,
   `primary_repo`, `acmm_level`, and `is_public`.
2. Update the running config on the PVC overlay (`/data/hive.yaml.dashboard`) —
   real `project.org` / `repos`, and the claimant's GitHub App `app_id` /
   `installation_id`.
3. Install the GitHub App key (owner does this from the dashboard's *Install
   GitHub App* flow once the hive is awake).
4. Scale the deployment to 1.

---

## Deprovisioning

```bash
# delete the workload namespace
kubectl --context "$CTX" delete ns "$NS"
# remove the hub SaaS record
kubectl --context hive-oke -n hive-hub exec "$HUB_POD" -- \
  rm -rf /data/saas/hives/"$ID"
# if it ever heartbeated, also drop its registry entry (see the placeholder gotcha)
```

---

## Quick failure-mode reference

| Symptom | Cause | Fix |
|---|---|---|
| Pod `CrashLoopBackOff` exit 255, SCC `restricted-v2` | `hive-anyuid` RoleBinding subject points at the wrong namespace | Set `subjects[].namespace` to the hive's own `$NS`, delete the pod |
| Pod won't boot: `github.token or github.app_id is required` | `github.app_id` empty in the seed | Set a placeholder `app_id` (e.g. `999999999`) |
| Dashboard sign-in redirect loop (vllm-d) | `hub_proxied: true` on a cluster with no hub auth proxy | Set `hub_proxied: false` on the PVC overlay |
| Hive online but **Upgrade** → `hive not found` | No `meta.json` on the hub | Create `/data/saas/hives/<id>/meta.json` |
| Not visible in My Hives | No `meta.json`, or `owner` doesn't match | Create/patch `meta.json` with the right `owner` |
| ConfigMap edits have no effect | PVC overlay is authoritative | Edit `/data/hive.yaml.dashboard`, not the ConfigMap |
| Admission warnings on apply | Cluster requires an `owner` label | Add `owner: <login>` to every `metadata.labels` |
| Placeholder shows a version / "online" dot | It heartbeated once (automated path) | Remove its `/data/hub-registry.json` entry (hub scaled to 0) |
