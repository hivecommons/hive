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

> **`app_id: 999999999` above is the placeholder sentinel, not an arbitrary
> number.** It is `config.PlaceholderAppID`, and the spoke recognises it as
> "this hive has no GitHub App yet". Use exactly this value — any other
> non-zero number is read as a real App ID and the hive will try (and fail) to
> authenticate as it. See [Placeholder `app_id`](#placeholder-app_id) below.

**`CreateHiveRequest` fields that matter:**

| Field | Notes |
|---|---|
| `org`, `repos` | **Required, non-empty.** `repos` is comma-separated. |
| `primary_repo` | Hosts the advisory issue and default markings. |
| `acmm_level` | Starting autonomy. **Default new hives to `2`** (Advisory (Instructed) / advisory-only). |
| `cluster_id` | `hive-oke` (the `defaultClusterID`). |
| `auth_method` | `app` or a token. |
| `app_id` / `installation_id` / `app_private_key` | Three auth shapes: **token** (`github_token` starts `ghp_`/`github_pat_`); **app now** (all three set); **app later** — `app_id` set, `installation_id` **and** `app_private_key` **empty**. The last is the placeholder case: pass the sentinel `app_id: 999999999` (see [Placeholder `app_id`](#placeholder-app_id)) and the hive provisions into dashboard-only mode until the owner installs the App from the dashboard. |
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

### A.3 RBAC the template emits

The provisioning template creates the namespace RBAC for you, including the
namespace-scoped read-only **`hive-route-reader`** Role and RoleBinding
(`get`/`list` on `networking.k8s.io/ingresses` and `route.openshift.io/routes`).

It is **not** unused. The spoke reads its own Route/Ingress through it to learn
the hostname it actually serves, and reports that to the hub as `dashboard_url`.
Remove it and the hub falls back to synthesising `<hiveID>.<hub host>`, which
503s for any spoke not fronted by the hub's wildcard domain — see
[`hive-route-reader`](#hive-route-reader--why-the-dashboard-link-503s-without-it)
under the manual path for the full rationale, the YAML, and the
ServiceAccount-derivation caveat.

The template binds `hive-sa` on `RequiresSCC` (OpenShift) clusters and `default`
elsewhere. Namespaces provisioned **before** this Role was added do not have it
and need it applied retroactively.

---

## Path B — Manual provisioning (vllm-d)

The hub cannot reach vllm-d, so every object is applied by hand with
`kubectl --context vllm-d`. The full set, in order:

1. Namespace
2. ServiceAccount (`hive-sa`)
3. RBAC — three Roles (`hive-secrets-writer`, `hive-self-upgrade`,
   `hive-route-reader`) and four RoleBindings (the three above **plus**
   `hive-anyuid`)
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

# The hub heartbeat secret — the SAME for every spoke on a given hub. Copy it
# from any working hive on the same cluster (see the Deployment gotcha). The
# spoke's heartbeats 401 without it.
HUB_SECRET=$(kubectl --context "$CTX" -n hive-hosted-<any-working-hive> \
  get deploy hive -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}={.value}{"\n"}{end}' \
  | grep '^HIVE_HUB_SECRET=' | cut -d= -f2-)
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
kind: Role
metadata: { name: hive-route-reader, namespace: ${NS} }
rules:
- apiGroups: ["networking.k8s.io"]
  resources: ["ingresses"]
  verbs: ["get","list"]
- apiGroups: ["route.openshift.io"]
  resources: ["routes"]
  verbs: ["get","list"]
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
metadata: { name: hive-route-reader, namespace: ${NS} }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: Role, name: hive-route-reader }
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

#### `hive-route-reader` — why the dashboard link 503s without it

The spoke discovers the external hostname its **own** Route/Ingress actually
serves and reports it to the hub as `dashboard_url` (`SpokeServedHost`, called
from the heartbeat path). `hive-route-reader` is what lets it read them.

Without the Role, that lookup returns "no answer" and the spoke falls back to
synthesising `<hiveID>.<hub host>`. That is correct **only** for spokes fronted
by the hub's own wildcard domain. Anywhere else it is a guaranteed **503**: the
wildcard resolves, so DNS looks healthy, but it sends the name to the **hub's**
router, which has no backend for a hive living on another cluster. This was a
live user-visible outage on the vllm-d pool — the hub minted links at
`<id>.hive.kubestellar.io` while the spoke's Route served
`<id>.apps.fmaas-vllm-d.fmaas.res.ibm.com`.

The spoke is the only party that **can** answer this on a heartbeat-only
cluster, since the hub has no `kubectl` path there by design.

Notes on the shape of the Role:

- **Read-only and namespace-scoped.** `get`/`list`, no write verbs. A
  compromised spoke learns only its own hostname — which it already advertises
  — and can neither create nor retarget routing.
- **Routes are listed unconditionally**, not gated on OpenShift. A cluster can
  serve Routes without requiring an SCC, and a Role naming a CRD-backed
  resource is inert where that API is absent.
- **Ingress is consulted before Route**, matching the traffic path, so a
  cluster carrying both resolves to the same object.

> **Gotcha — derive the ServiceAccount, do not assume it.** The subject is
> **not** uniform across the fleet. OpenShift/SCC spokes bind `hive-sa`; others
> bind `default`. Measured live: hive-oke is 22× `default`; vllm-d is 2×
> `default` **and** 41× `hive-sa`; a-ks-wec2 is 5× `hive-sa`. Binding the wrong
> one fails silently — the pod runs, the lookup is denied, and you get the 503
> fallback with no error anywhere obvious. Read it off the Deployment:
>
> ```bash
> kubectl --context "$CTX" -n "$NS" get deploy hive \
>   -o jsonpath='{.spec.template.spec.serviceAccountName}'
> ```
>
> An **empty** result means the Deployment does not set one, which is Kubernetes
> for `default` — bind `default`, not the empty string. The manifests in B.2
> above use `hive-sa` because this manual path creates it in B.1; substitute
> whatever the command returns if you are patching an existing namespace.

> **Retrofit — namespaces provisioned before this change do not have it.**
> The Role/RoleBinding is emitted by the provisioning template, so **new**
> hosted hives get it automatically. Every namespace created before it was
> added needs it applied retroactively, or its dashboard link keeps 503ing.
> Check the fleet:
>
> ```bash
> kubectl --context "$CTX" get rolebinding -A \
>   --field-selector metadata.name=hive-route-reader
> ```
>
> The spoke also has to be running a build new enough to perform the lookup, so
> a namespace that already has the RBAC may still need a `rollout restart` to
> start reporting.

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
      org: myorg                      # a POOL PLACEHOLDER can seed any bootstrap org here; the
      repos: [myrepo]                 # AUTHORITATIVE identity is the meta.json org
      primary_repo: myrepo            # (available-vllmd-<date>-<suffix>), which is what the hub
                                      # renders — see Placeholder pools below
    agents:
      guide:   { backend: copilot, model: claude-sonnet-4-6, enabled: true }
      scanner: { backend: copilot, model: claude-sonnet-4-6, enabled: true }
    governor:
      eval_interval_s: 300
      modes:
        idle: { threshold: 0,  guide: 4h, scanner: 4h }
        busy: { threshold: 10, guide: 2h, scanner: 2h }
    github:
      app_id: 999999999               # the placeholder sentinel (config.PlaceholderAppID)
      installation_id:
      key_file: /secrets/gh-app-key.pem
      oauth_client_id: Ov23ligE2p0gjXg6xAUf   # public device-flow client (no secret)
    dashboard:
      port: 3002
      authorized_users:
        - owner-github-login:owner    # a POOL PLACEHOLDER lists the pool owner: clubanderson:owner
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

<a id="placeholder-app_id"></a>

> **Gotcha — `github.app_id` cannot be empty.** Config validation requires
> **either** `github.token` **or** `github.app_id` to be set
> (`pkg/config/config.go`: `github.token or github.app_id is required`). A hive
> awaiting its real App must carry the **placeholder sentinel**
> `app_id: 999999999` or it will refuse to boot.

> **Use `999999999` exactly — it is a recognised sentinel, not a spare number.**
> It is declared as `config.PlaceholderAppID`, and `GitHubConfig.HasApp()`
> reports a hive carrying it as having **no** GitHub App. Such a hive boots into
> dashboard-only mode: the dashboard serves, the heartbeat runs, and the "GitHub
> App required" banner shows the install link. Agents stay idle until a real App
> is linked.
>
> Any *other* non-zero placeholder is read as a **real** App ID. The hive will
> then try to authenticate as an App that does not exist — and once
> `installation_id` also becomes non-zero (which happens the moment the owner
> installs an App), it attempts to load a private key that was never
> provisioned. Before this was fixed, that combination exited the process
> **before the HTTP listener bound**: permanent `CrashLoopBackOff`, no
> heartbeat, the hive shown offline on the hub, and any in-flight rollout stuck
> forever because the new pod never went Ready. The spoke now degrades instead
> of exiting, but the hive still cannot do any GitHub work until the `app_id` is
> corrected.
>
> Replacing the placeholder with the real value is normally automatic — the hub
> delivers `app_id`, `installation_id` and the private key over the heartbeat
> when the App is installed. To do it by hand, patch the PVC overlay
> (`/data/hive.yaml.dashboard`, **not** the ConfigMap) and restart the pod.

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
      # ── init containers — REQUIRED. The ConfigMap is mounted read-only at
      # /etc/hive-seed and copied into a WRITABLE emptyDir at /etc/hive. The hive
      # process must WRITE /etc/hive/hive.yaml at runtime (the entrypoint seeds
      # it, then merges the PVC overlay over it, and the dashboard's Save writes
      # it). Mounting the ConfigMap DIRECTLY at /etc/hive makes it read-only and
      # every config save fails with "open /etc/hive/hive.yaml: read-only file
      # system" — and dashboard-installed GitHub App auth / ACMM changes are lost.
      initContainers:
      - name: copy-config
        image: ${IMAGE}
        command: ["sh","-c","cp /etc/hive-seed/hive.yaml /etc/hive/hive.yaml && echo configmap-copied; if [ -f /data/hive.yaml.runtime ]; then echo runtime-config-exists-for-recovery; elif [ -f /data/hive.yaml.bak ]; then echo legacy-runtime-config-exists-for-recovery; fi"]
        volumeMounts:
        - { name: config,          mountPath: /etc/hive-seed, readOnly: true }
        - { name: config-writable, mountPath: /etc/hive }
        - { name: data,            mountPath: /data }
      - name: init-permissions
        image: ${IMAGE}
        command: ["sh","-c","chown -R 1001:1000 /data 2>/dev/null || true; echo permissions-set"]
        volumeMounts:
        - { name: data, mountPath: /data }
      containers:
      - name: hive
        image: ${IMAGE}
        imagePullPolicy: Always
        ports:
        - { name: dashboard, containerPort: 3002 }
        - { name: terminal,  containerPort: 3001 }
        env:
        - { name: HIVE_ID, value: "${ID}" }
        # HIVE_HUB_SECRET authenticates every heartbeat to the hub. WITHOUT it,
        # the hub rejects the spoke's heartbeats with 401 and the hive shows
        # OFFLINE forever (see the gotcha below). HIVE_HUB_URL points at the hub.
        #
        # SECURITY (C2 domain separation): the hub no longer signs everything with
        # this one key. It derives a distinct sub-key per trust domain
        # (heartbeat / session cookie / SSO), so a spoke never needs the master.
        # Automated hub-hosted provisioning injects ONLY the derived keys
        # (HIVE_HEARTBEAT_KEY, HIVE_SESSION_KEY, HIVE_SSO_KEY) and NEVER
        # HIVE_HUB_SECRET. Manual provisioning MAY still set HIVE_HUB_SECRET as
        # shown below: the spoke derives the same sub-keys from it locally, so
        # heartbeats/SSO keep working. If you prefer least privilege, set the three
        # HIVE_*_KEY vars instead (each = HMAC-SHA256(master, "hive-<domain>-v1")
        # rendered as lowercase hex) and omit HIVE_HUB_SECRET entirely.
        - { name: HIVE_HUB_SECRET, value: "${HUB_SECRET}" }
        - { name: HIVE_HUB_URL,    value: "https://hive.kubestellar.io" }
        # startupProbe keeps the liveness clock from starting until boot
        # finishes, so a slow start can't race liveness into a restart loop.
        startupProbe:
          httpGet: { path: /api/health, port: 3002 }
          initialDelaySeconds: 10
          periodSeconds: 5
          failureThreshold: 30
        livenessProbe:
          # /api/livez (NOT /api/health) — it also fails when the heartbeat
          # loop stops *attempting* sends, so a wedged/dead heartbeat
          # goroutine gets the pod auto-restarted. It does NOT fail merely
          # because the hub is unreachable (see the gotcha below).
          httpGet: { path: /api/livez, port: 3002 }
          periodSeconds: 30
          failureThreshold: 3
          timeoutSeconds: 2
        readinessProbe:
          httpGet: { path: /api/health, port: 3002 }
          initialDelaySeconds: 5
          periodSeconds: 5
          failureThreshold: 3
          timeoutSeconds: 2
        volumeMounts:
        - { name: data,            mountPath: /data }
        - { name: config-writable, mountPath: /etc/hive }         # WRITABLE (emptyDir), not the ConfigMap
        - { name: secrets,         mountPath: /secrets, readOnly: true }
      volumes:
      - { name: data,            persistentVolumeClaim: { claimName: hive-data } }
      - { name: config,          configMap: { name: hive-config } }   # seed only → /etc/hive-seed
      - { name: config-writable, emptyDir: {} }                       # runtime /etc/hive
      - { name: secrets, secret: { secretName: hive-secrets } }
YAML
```

> **`maxUnavailable: 0`** keeps the current pod serving until the new one is
> ready — uninterrupted upgrades. It only works because the PVC is RWX.

> **Gotcha — `HIVE_HUB_SECRET` is REQUIRED or the hive is permanently offline.**
> The hub authenticates every heartbeat against a shared secret. A spoke without
> `HIVE_HUB_SECRET` in its deployment env sends unauthenticated heartbeats, the
> hub replies **`401 unauthorized`**, and the hive shows **offline** in the
> fleet even though the pod is `1/1 Running`. Copy the value from any working
> hive on the same hub — it is the same for all spokes:
>
> ```bash
> HUB_SECRET=$(kubectl --context "$CTX" -n hive-hosted-<any-working-hive> \
>   get deploy hive -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}={.value}{"\n"}{end}' \
>   | grep '^HIVE_HUB_SECRET=' | cut -d= -f2-)
> ```
>
> Symptom to look for in the spoke logs: `hub heartbeat rejected status=401`.

> **Gotcha — point the liveness probe at `/api/livez`, not `/api/health`.**
> `/api/health` only checks the HTTP server is up, so a pod whose heartbeat
> goroutine has silently died still passes it and is never restarted — the hive
> shows a persistent "offline" dot while `1/1 Running`. `/api/livez` additionally
> fails (503) when the heartbeat loop stops *attempting* to send, so the kubelet
> restarts the pod and the heartbeat revives. It returns 200 unauthenticated
> once healthy. Existing deployments provisioned before this note still point at
> `/api/health` — migrate them:
> `kubectl -n <ns> patch deploy hive --type json -p '[{"op":"replace","path":"/spec/template/spec/containers/0/livenessProbe/httpGet/path","value":"/api/livez"}]'`

> **Note — `/api/livez` does not fail on an unreachable hub.** It keys off
> heartbeat *attempts*, not successes. A hub that is down, firewalled, or
> rejecting beats leaves the process perfectly healthy, and restarting it
> cannot fix a network partition — an earlier version gated liveness on
> heartbeat freshness and crash-looped healthy spokes on firewalled clusters
> (vllm-d). Heartbeat freshness is reported by `/api/health/deep` under the
> `hub_heartbeat` check, and the hub greys the hive's dot on its own.
> Spokes deployed before this fix need a redeploy (or a probe patch) to stop
> restarting on hub outages.

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
management action (upgrade, claim) returns **`hive not found`**.

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

**Hub access roles.** Manage Access grants four ordered roles (separate from the ClankeR trust tiers shown on `/contribute`):
`read` < `read-write` < `merger` < `owner`. `merger` inherits read-write
dashboard access and can approve/queue **other people's** PRs for the hive
auto-merge-on-green flow. The spoke enforces the self-merge ban against the
GitHub login bound to the session; owners are exempt because they already have
repository-level merge authority.

Queueing a PR approves it as the hive App and applies a label. That label is
configurable, because it is applied in a repository the hive does not own and
whose review conventions already exist:

```yaml
governor:
  labels:
    automerge: lgtm
```

It defaults to `lgtm`, the long-standing Prow/Kubernetes label for "a second
person signed off", which is the decision the merger tier records. Set it to
whatever the managed repositories already use — the label is created on demand
if it does not exist.

> **`meta.json` requirement is why heartbeats can be accepted but upgrades fail.**
> The heartbeat path is separate from `loadSaaSHive`. A hive can heartbeat and
> appear "online" while a missing `meta.json` makes the **Upgrade** button (and
> claim, toggle-auto-upgrade) return `hive not found`. If you see that,
> the fix is almost always a missing `meta.json`.

---

## Config precedence — the PVC overlay is authoritative

At runtime the effective config is **not** the ConfigMap. The entrypoint:

1. seeds `/etc/hive/hive.yaml` from the `hive-config` ConfigMap on boot, then
2. merges the PVC dashboard overlay `/data/hive.yaml.dashboard` **over** it, and
3. writes `/data/hive.yaml.runtime`.

The dashboard's `Config.Save()` writes the overlay. Therefore:

- **Editing the ConfigMap after first boot does nothing** to the running config —
  the PVC overlay wins.
- To change a running hive's config, edit `/data/hive.yaml.dashboard` on the PVC
  (while scaled to 0, or the running process re-saves from memory), **or** clear
  `/data/hive.yaml.dashboard` + `/data/hive.yaml.runtime` (and the legacy
  `/data/hive.yaml.bak`, if the hive predates the rename) to let it reseed.
- Verify effective config by `grep`-ing `/etc/hive/hive.yaml` in the running pod,
  **never** the ConfigMap.

> **Gotcha — access grants (`dashboard.authorized_users`) only take effect on a
> pod roll.** The overlay merge in step 2 runs **at boot**. When you grant a user
> in the dashboard's **Manage Access** dialog, `Config.Save()` appends them to
> `/data/hive.yaml.dashboard`, but the *running* device-flow authorizer keeps the
> access list it built at startup — config hot-reload does **not** rebuild it.
> Symptom: the user shows as `read-write` in Manage Access yet the spoke logs
> `device-flow login rejected: user not authorized for this hive`.
>
> This bites hardest **right after provisioning or a cross-cluster migration onto
> a fresh PVC**, where the first grants are written after the pod already booted.
> Always finish provisioning with the verification below.
>
> ```sh
> # 1. Confirm the grant reached the effective config:
> kubectl -n hive-hosted-<id> exec deploy/hive -c hive -- \
>   grep -A6 authorized_users /etc/hive/hive.yaml
> # 2. If the user is present but still rejected, roll the pod so the authorizer
> #    rebuilds from the current overlay:
> kubectl -n hive-hosted-<id> rollout restart deploy/hive
> # 3. Confirm no post-roll rejections:
> kubectl -n hive-hosted-<id> logs deploy/hive -c hive | grep 'not authorized'
> ```
>
> Do **not** hand-create `/data/hive.yaml` to "fix" a missing config — the
> effective config is ConfigMap-seed + `.dashboard` overlay; a stray
> `/data/hive.yaml` is never read and only adds confusion.

---

## Placeholder pools

A **placeholder** is a fully-provisioned but idle hive, waiting to be claimed —
so a request for access is satisfied in seconds instead of the full 5–10 minute
provision. Two pools, one per method type:

- **vllm-d** placeholders serve **private** methods — provisioned manually
  (Path B) and left running at **`replicas: 1`**.
- **hive-oke** placeholders serve **public** methods — provisioned via the API
  (Path A) and left running at **`replicas: 1`**.

> **Gotcha — placeholder IDs are date + random suffix, NOT sequential integers.**
> The live pool uses `hosted-available-vllmd-<YYMMDD>-<suffix>`, where `<suffix>`
> is a short unique token (4 lowercase base36 chars) — e.g.
> `hosted-available-vllmd-260731-fikn`, `hosted-available-vllmd-260806-1bo3`.
> **Do not** use sequential numbers like `hosted-available-vllmd-01`: they
> collide with existing pool slots and don't match the ID convention the fleet
> uses, so renumbering is needed every time the pool grows. Unique suffixes let
> you add a batch on any date without touching existing slots. Generate a batch
> of `N` suffixes:
>
> ```bash
> tr -dc 'a-z0-9' < /dev/urandom | fold -w4 | head -N | sort -u
> ```
>
> Then **collision-check** each suffix before creating — skip any that already
> exist — against both the hub meta dirs and the target cluster's namespaces:
>
> ```bash
> kubectl --context hive-oke -n hive-hub exec "$HUB_POD" -- ls /data/saas/hives/   # existing hive IDs
> kubectl --context vllm-d get ns | grep hive-hosted-hosted-available-vllmd        # existing namespaces
> ```

Provision each placeholder as above at **`replicas: 1`**. The pod boots
immediately, lands on the `anyuid` SCC, and heartbeats to the hub as an
available slot ready to be claimed. Both pools stay at `replicas: 1`:

```bash
# manual (vllm-d): apply the Deployment at replicas: 1 (it boots and heartbeats).
# If you applied it at 0, scale it up:
kubectl --context vllm-d -n "$NS" scale deploy/hive --replicas=1

# automated (hive-oke): the API already provisions at replicas=1 — leave it there.
```

> **Gotcha — do NOT scale placeholders to 0.** A `replicas: 0` slot is **not
> assignable**: the claim/assign path delivers the claimant's config to a
> **running, heartbeating** spoke, so a slot that isn't booted can never be
> claimed. Every assignable slot in the live pool runs at `replicas: 1`,
> `ready 1/1`. (Earlier guidance to "scale to 0" was incorrect.)

Give every placeholder a `meta.json` with **`owner: clubanderson`** (admin-only
visibility), **`status: "available"`**, and **`acmm_level: 2`**. Set `org` to a
**unique** value per slot — `available-vllmd-<YYMMDD>-<suffix>` mirroring the
`id`'s date-suffix — with **empty** `repos: []` and `primary_repo: ""` (they get
their real values when the slot is claimed). The `meta.json` also carries
`github_host` because vllm-d placeholders target GitHub Enterprise
(`github.ibm.com`), not `github.com`:

```json
{
  "id": "hosted-available-vllmd-260806-1bo3",
  "owner": "clubanderson",
  "project_name": "Available slot (private) 260806-1bo3",
  "org": "available-vllmd-260806-1bo3",
  "repos": [],
  "primary_repo": "",
  "acmm_level": 2,
  "status": "available",
  "created_at": "",
  "subdomain": "hosted-available-vllmd-260806-1bo3.apps.fmaas-vllm-d.fmaas.res.ibm.com",
  "claim_delivered": false,
  "auto_upgrade": true,
  "is_public": false,
  "cluster_id": "vllm-d",
  "github_host": "github.ibm.com"
}
```

> **The `org` must be unique per slot** (mirror the `id`'s date-suffix) so each
> renders as a distinct **"Available slot (private) `<date>-<suffix>`"** row in
> the hub. A literal shared `org: "placeholder"` (with `repos: ["placeholder"]`)
> makes every slot render identically as "placeholder / placeholder" —
> indistinguishable — and is wrong.

### Verify a provisioned placeholder

A correct vllm-d placeholder runs at `replicas: 1` with its pod up (`Running`,
`ready`, `restartCount: 0`) on the `anyuid` SCC, its `hive-anyuid` binding points
at its **own** namespace, its PVC is `Bound`, its `livez` returns 200, and its hub
`meta.json` exists. This set verified all ten placeholders in the 2026-08-06 batch:

```bash
ID=hosted-available-vllmd-<date>-<suffix>; NS=hive-hosted-$ID
kubectl --context vllm-d -n $NS get deploy hive -o jsonpath='{.spec.replicas}'          # must be 1
POD=$(kubectl --context vllm-d -n $NS get pod -l app=hive -o jsonpath='{.items[0].metadata.name}')
kubectl --context vllm-d -n $NS get pod $POD -o jsonpath='{.status.phase}'              # must be Running
kubectl --context vllm-d -n $NS get pod $POD -o jsonpath='{.metadata.annotations.openshift\.io/scc}'  # must be anyuid
kubectl --context vllm-d -n $NS get pod $POD -o jsonpath='{.status.containerStatuses[0].ready}'       # must be true
kubectl --context vllm-d -n $NS get pod $POD -o jsonpath='{.status.containerStatuses[0].restartCount}' # must be 0
kubectl --context vllm-d -n $NS exec $POD -c hive -- curl -s -o /dev/null -w '%{http_code}' localhost:3002/api/livez  # must be 200
kubectl --context vllm-d -n $NS get rolebinding hive-anyuid -o jsonpath='{.subjects[0].namespace}'  # must equal $NS
kubectl --context vllm-d -n $NS get pvc hive-data -o jsonpath='{.status.phase}'         # must be Bound
kubectl --context hive-oke -n hive-hub exec $HUB_POD -- test -f /data/saas/hives/$ID/meta.json  # must exist
```

> **The `hive-anyuid` subject-namespace check is still load-bearing at apply
> time** (it is the same `restricted-v2` crash-loop gotcha from B.2): a wrong
> subject namespace means the pod can't get the `anyuid` SCC. But because a
> placeholder now **boots at `replicas: 1`**, you confirm the SCC directly —
> verify the pod reaches `phase=Running`, `scc=anyuid`, `ready=true`,
> `restartCount=0`, and `livez` == 200.

> **Note — placeholders carry a fleet-registry entry.** Because both pools run
> at `replicas: 1`, every placeholder heartbeats and holds a registry entry that
> pins its version and last-known ACMM and shows it "online". This is expected
> and required: the claim/assign path delivers config to the running, heartbeating
> spoke, so the entry is what makes the slot assignable — do **not** strip it.

### Claiming a placeholder

Until the dashboard "assign" flow lands, claiming is manual:

1. Edit the placeholder's `meta.json`: set the real `owner`, `org`, `repos`,
   `primary_repo`, `acmm_level`, and `is_public`.
2. Update the running config on the PVC overlay (`/data/hive.yaml.dashboard`) —
   real `project.org` / `repos`, and the claimant's GitHub App `app_id` /
   `installation_id`.
3. Install the GitHub App key (owner does this from the dashboard's *Install
   GitHub App* flow — the hive is already awake at `replicas: 1`).
4. Roll the pod so it picks up the new config (`kubectl rollout restart
   deploy/hive`) — it is already running, so there is nothing to scale up.

---

## Mapping a namespace back to its hive (identity labels)

A claimed placeholder's namespace **keeps its original placeholder name
forever** — e.g. `hive-hosted-hosted-available-oke-01-placeholder-bb95` — even
after the hub API's claim/assign flow gives the hive a real name and org.
Kubernetes has no atomic namespace-rename primitive, and renaming would mean
recreating every object inside it (including the PVC), so the namespace name
is never changed. Historically this meant there was no way to look at a
namespace on the cluster and tell which hive/org it belonged to without
cross-referencing the hub registry and reverse-engineering the placeholder
slug.

Instead, the hub stamps **identity labels** and a **display-name annotation**
onto the namespace itself, keeping it self-describing even though its name
never changes:

| Key | Kind | Holds |
|---|---|---|
| `hive.kubestellar.io/hive-name` | label | The hive's display name (`ProjectName`), **sanitized** to a valid RFC-1123 label value. |
| `hive.kubestellar.io/org` | label | The hive's forge org, sanitized the same way. |
| `hive.kubestellar.io/hive-id` | label | The hive's internal `hive_id`, sanitized the same way. |
| `hive.kubestellar.io/display-name` | annotation | The **exact, unsanitized** hive name — the recoverable source of truth when the sanitized label value has been lossily transformed. |

**Sanitization**: label values are lower-cased, restricted to `[a-z0-9.-]`
(everything else — spaces, underscores, unicode, punctuation — is dropped
outright rather than transliterated), trimmed of leading/trailing separators,
and capped at 63 characters (the RFC-1123 label-value limit). So a hive named
`"TradingAsBuddies"` gets the label value `tradingasbuddies`, while the
`hive.kubestellar.io/display-name` annotation preserves the original
`TradingAsBuddies` exactly. A field that sanitizes to empty (or wasn't set
yet — e.g. a fresh placeholder with no real name/org) is **omitted** from the
labels rather than written as an empty/invalid label.

Given a hive name, find its namespace:

```bash
kubectl --context hive-oke get ns \
  -l hive.kubestellar.io/hive-name=tradingasbuddies
```

Given a namespace, find the hive that owns it:

```bash
kubectl --context hive-oke get ns hive-hosted-hosted-available-oke-01-placeholder-bb95 \
  -o jsonpath='{.metadata.annotations.hive\.kubestellar\.io/display-name}'
# -> TradingAsBuddies
```

**When the labels/annotation are (re)written** — the hub calls the same
`stampHostedNamespaceIdentity` helper at all three points a hive's identity is
known or changes, so the namespace never goes stale:

1. **Automatic provisioning** (`provisionHive`, Path A above) — stamps
   whatever is known at namespace-creation time. For a freshly-created pool
   placeholder this is often just `hive.kubestellar.io/hive-id`, since
   `org`/`name` are still placeholder values at this point.
2. **Claim** (`handleApproveProvision`) — the real name/org become known here
   for the first time, so the labels/annotation are (re)written.
3. **Assign / reassign** (`handleAssignHive`) — same, so a placeholder that
   gets reassigned to a different owner has its namespace identity refreshed
   too.

The stamp is applied via idempotent `kubectl label`/`kubectl annotate
--overwrite` (merged into whatever the namespace already carries, not
replaced) and is **best-effort**: a failure to label (no `kubectl` on PATH, a
transient API error) is logged and does not fail the claim/assign request —
this is a cosmetic/operability nicety, not a required step for the hive to
work.

### Name-bearing vanity Route

The same problem — a URL permanently encoding the placeholder slug — applies
to the dashboard's public URL. The **assign** path (`handleAssignHive`) also
creates an **additional** OpenShift Route whose host is derived from the
hive's own display name, rather than the org/repo-derived host used
previously:

```
<sanitized-hive-name>-<4-char-suffix>.<cluster-domain>
```

For example, a hive named `TradingAsBuddies` on a cluster with domain
`apps.example.com` gets a Route host like
`tradingasbuddies-a1b2.apps.example.com`, adopted as
`https://tradingasbuddies-a1b2.apps.example.com`. The random suffix keeps two
hives with similar/identical sanitized names from colliding on the same host.

This is an **additional** Route, not a rename: the original Route (host
derived from org/repo or the placeholder ID) is left completely alone, so any
in-flight bookmark or callback against it keeps working. **The namespace
itself is still never renamed or recreated** — only a second Route pointing at
the same Service is added. If the hive has no display name set, the vanity
host falls back to the previous org/repo-derived scheme.

### Namespace shown in the dashboards

The namespace is now surfaced in two read-only UI spots, so an operator or
hive owner doesn't need `kubectl` at all to find it:

- **Spoke dashboard → Hub tab.** The hive's own dashboard shows its
  Kubernetes namespace next to the hub URL/name info. The spoke learns its own
  namespace from the `POD_NAMESPACE` env var, set via the Kubernetes downward
  API (`fieldRef: metadata.namespace`) on the hive Deployment — falling back to
  a `NAMESPACE` env var, then to the in-cluster service-account namespace file,
  if `POD_NAMESPACE` isn't set (e.g. a spoke provisioned before this env var
  existed). The row is omitted entirely when none of the three resolve.
- **Hub dashboard → per-spoke status hover.** Hovering a hive in the hub's
  fleet view shows its namespace, computed client-side as `"hive-hosted-" +
  <hive-id>` — the same convention the hub uses server-side — so it can never
  disagree with the namespace the identity labels above are stamped on.

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
| `Config save failed ... open /etc/hive/hive.yaml: read-only file system` (ACMM/App-auth lost on restart) | ConfigMap mounted DIRECTLY at `/etc/hive` (read-only) | Mount ConfigMap at `/etc/hive-seed` + a writable emptyDir at `/etc/hive` + the `copy-config` init container (see B.8) |
| Pod `1/1 Running` but hive shows **offline**; spoke logs `hub heartbeat rejected status=401` | No `HIVE_HUB_SECRET` in the deployment env | Add `HIVE_HUB_SECRET` (+ `HIVE_HUB_URL`) env from a working hive; the pod rolls and heartbeats |
| Pod `1/1 Running` but hive shows **offline**; no 401, heartbeat just stopped | Heartbeat goroutine died; liveness probe on `/api/health` can't detect it | Point livenessProbe at `/api/livez`; restart to revive now |
| Pod restarting repeatedly (`RESTARTS` climbing) while the app looks fine; hub unreachable/firewalled | Old liveness probe failed on stale *heartbeat success* — a connectivity condition a restart can't fix | Redeploy to pick up the attempt-based `/api/livez` (+ `startupProbe`); check `/api/health/deep` → `hub_heartbeat` for the real connectivity state |
| Pod `CrashLoopBackOff` exit 255, SCC `restricted-v2` | `hive-anyuid` RoleBinding subject points at the wrong namespace | Set `subjects[].namespace` to the hive's own `$NS`, delete the pod |
| Pod won't boot: `github.token or github.app_id is required` | `github.app_id` empty in the seed | Set the placeholder sentinel `app_id: 999999999` — exactly that value, see [Placeholder `app_id`](#placeholder-app_id). Any other stand-in number is treated as a real App |
| Pod `CrashLoopBackOff` with `failed to init GitHub App auth` / `reading app key ...: no such file`, restarts climbing, hive offline on the hub, rollout stuck with two crashlooping pods | A non-sentinel placeholder `app_id` plus a real `installation_id`, and no private key at `key_file`. Older builds exited before the listener bound, so nothing was visible in the dashboard | Set `app_id` to the real App ID and install the PEM at `key_file` (or set `app_id` to the sentinel `999999999` to park the hive in dashboard-only mode). Patch `/data/hive.yaml.dashboard` and restart. Current builds boot degraded and show the reason in the GitHub App banner instead of crashlooping |
| Dashboard banner: "GitHub App private key could not be loaded from ..." | A real `app_id` + `installation_id`, but no readable PEM at the resolved path | The banner names the exact path tried and the resolution order (`$GH_APP_KEY_FILE` → `github.key_file` → PVC `/data/gh-app-key.pem` → provisioning mount `/secrets/gh-app-key.pem`). Write the PEM to that path, or point `github.key_file` at one |
| Dashboard sign-in redirect loop (vllm-d) | `hub_proxied: true` on a cluster with no hub auth proxy | Set `hub_proxied: false` on the PVC overlay |
| Hive online but **Upgrade** → `hive not found` | No `meta.json` on the hub | Create `/data/saas/hives/<id>/meta.json` |
| Hive online, but **My Hives → Dashboard** returns a branded **503**; the link reads `<hive-id>.<hub-host>` instead of the spoke cluster's own domain | Missing `hive-route-reader` RBAC, so the spoke can't read its own Route/Ingress and the hub falls back to the hub-wildcard host, which has no backend for a hive on another cluster | Apply the `hive-route-reader` Role + RoleBinding, binding the SA the hive Deployment actually uses (see [B.2](#hive-route-reader--why-the-dashboard-link-503s-without-it)), then `rollout restart deploy/hive` |
| Not visible in My Hives | No `meta.json`, or `owner` doesn't match | Create/patch `meta.json` with the right `owner` |
| ConfigMap edits have no effect | PVC overlay is authoritative | Edit `/data/hive.yaml.dashboard`, not the ConfigMap |
| User has `read-write` in Manage Access but login fails with `device-flow login rejected: user not authorized` | Grant written to `/data/hive.yaml.dashboard` after the pod booted; the running authorizer only rebuilds `authorized_users` at startup (common right after provisioning / cross-cluster migration onto a fresh PVC) | Confirm the user is in the pod's `/etc/hive/hive.yaml`, then `rollout restart deploy/hive` so the authorizer rebuilds |
| Admission warnings on apply | Cluster requires an `owner` label | Add `owner: <login>` to every `metadata.labels` |
| Placeholder rows all render as "placeholder / placeholder" | `meta.json` used the literal `org: "placeholder"` | Give each slot a unique `org: available-vllmd-<date>-<suffix>` with `repos: []` |
