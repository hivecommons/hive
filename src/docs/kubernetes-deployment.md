Use this guide when deploying a Hive spoke to Kubernetes instead of a standalone host.

## Kubernetes Deployment

### Prerequisites

- `kubectl` configured for your cluster
- Kubernetes 1.24+
- A StorageClass that supports `ReadWriteOnce` (the cluster default StorageClass works)
- cert-manager (for TLS certificates)
- ingress-nginx, the `kubernetes/ingress-nginx` controller (for ingress routing)

### Hosted Option

The [Hive Hub](https://hive.hivecommons.dev) provides hosted hives with OAuth-protected dashboards, a public registry, and cross-hive leaderboards. No cluster required.
The canonical hub address is now `https://hive.hivecommons.dev`; the legacy
`https://hive.kubestellar.io` hostname redirects during the cutover.
Start with the [hosted hub onboarding guide](hosted-hub.md) to sign in,
request a hosted hive, and finish first-run setup.

If you need to run your own private hub instead, see the
[self-hosted hub deployment guide](hub-deployment.md).

Just want to try it locally? See [Try Hive on kind](try-hive-on-kind.md).

### Self-Hosted Deployment

#### 1. Create the namespace

```bash
kubectl apply -f src/deploy/k8s/namespace.yaml
```

Or manually:

```bash
kubectl create namespace hive
```

#### 2. Create secrets

```bash
kubectl -n hive create secret generic hive-secrets \
  --from-literal=HIVE_GITHUB_TOKEN=ghp_... \
  --from-literal=HIVE_DASHBOARD_TOKEN="$(openssl rand -hex 32)"
```

The PAT needs the classic `repo` scope (`public_repo` for public-only repos),
plus `workflow` at L5/L6 if agent PRs may touch `.github/workflows/`. Scopes are
never validated at startup, so a wrong-scoped token fails later as a generic
GitHub 403 — see [Personal access token (PAT) scopes](github-app-setup.md#personal-access-token-pat-scopes).

The dashboard token is an opaque shared secret with no server-side strength
check — always generate it with a CSPRNG as above, never a hand-typed value.
See [Generating and rotating `HIVE_DASHBOARD_TOKEN`](env-vars.md#generating-and-rotating-hive_dashboard_token).

For GitHub App auth (recommended for production), add the private key:

```bash
kubectl -n hive create secret generic hive-secrets \
  --from-literal=HIVE_GITHUB_TOKEN=ghp_... \
  --from-file=gh-app-key.pem=/path/to/key.pem
```

With `github.app_id`/`key_file` set, the App path supplies repository
permissions and the PAT is only a fallback; see
[GitHub App setup](github-app-setup.md) for both paths.

#### 3. Create ConfigMap from hive.yaml

```bash
cp src/hive.yaml.example hive.yaml
# Edit hive.yaml: set your org, repos, agents, and governor config

kubectl create configmap hive-config -n hive --from-file=hive.yaml=hive.yaml
```

#### 4. Create PersistentVolumeClaim

Apply the provided PVC manifest:

```bash
kubectl apply -f src/deploy/k8s/pvc.yaml
```

The default PVC requests 10Gi with `ReadWriteOnce`. The Deployment runs one replica with
`strategy: Recreate`, so the old pod stops before the new one mounts `/data` and RWO is enough.

If your storage is NFS or another shared backend, you can use `ReadWriteMany` instead. Rollouts
still use `Recreate`, so this does not give zero-downtime updates:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: hive-data
  namespace: hive
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: nfs
  resources:
    requests:
      storage: 10Gi
```

#### 5. Deploy

```bash
kubectl apply -f src/deploy/k8s/dashboard-route-rbac.yaml
kubectl apply -f src/deploy/k8s/sandbox-job-rbac.yaml
kubectl apply -f src/deploy/k8s/deployment.yaml
kubectl apply -f src/deploy/k8s/service.yaml
```

`dashboard-route-rbac.yaml` creates the `hive` ServiceAccount the Deployment runs as.
Apply it first, or the ReplicaSet cannot create a pod.

The deployment runs a single replica. A startup probe and the readiness probe use `/api/health`,
and the liveness probe uses `/api/livez`. Resource defaults: 500m CPU / 512Mi memory (requests), 2 CPU / 2Gi memory (limits).

#### 6. Set up Ingress with TLS

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: hive
  namespace: hive
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/proxy-body-size: "50m"
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
spec:
  ingressClassName: nginx
  tls:
    - hosts:
        - hive.example.com
      secretName: hive-tls
  rules:
    - host: hive.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: hive
                port:
                  name: dashboard
```

Long timeouts are needed for SSE streaming connections to the dashboard.

The `nginx.ingress.kubernetes.io/*` annotations only work with ingress-nginx. Other controllers,
including F5's NGINX Ingress Controller, ignore them, so set the same body size and timeouts with
that controller's own options.

#### Quick apply (all manifests)

```bash
kubectl apply -f src/deploy/k8s/namespace.yaml
kubectl -n hive create secret generic hive-secrets \
  --from-literal=HIVE_GITHUB_TOKEN=ghp_...   # classic PAT: repo scope — see src/docs/github-app-setup.md#personal-access-token-pat-scopes
kubectl create configmap hive-config -n hive --from-file=hive.yaml=hive.yaml
kubectl apply -f src/deploy/k8s/pvc.yaml
kubectl apply -f src/deploy/k8s/dashboard-route-rbac.yaml
kubectl apply -f src/deploy/k8s/sandbox-job-rbac.yaml
kubectl apply -f src/deploy/k8s/deployment.yaml
kubectl apply -f src/deploy/k8s/service.yaml
```

### Ports

| Port | Purpose |
|------|---------|
| 3002 | Dashboard UI and `/api/*` (Service port `dashboard`) |

ttyd listens on 7681 inside the pod only. The Service does not expose it. Open the terminal
through the authenticated `/terminal` proxy on 3002.

### Volumes

| Mount Path | Purpose |
|------------|---------|
| `/etc/hive/hive.yaml` | Configuration (read-only, from ConfigMap) |
| `/data` | Persistent state: metrics, beads, logs |
| `/secrets` | GitHub App key and other secrets (read-only) |
