# Try Hive on kind

Run a single-replica Hive on a local [kind](https://kind.sigs.k8s.io/) cluster.
The overlay is [`src/deploy/kustomize/overlays/kind/`](../deploy/kustomize/overlays/kind/):
the core [`k8s`](../deploy/k8s/) base, kind's default `standard` StorageClass,
and agents pointed at an external OpenAI-compatible endpoint.

Prerequisites: Docker (or Podman), `kind`, `kubectl`, a GitHub PAT
([scopes](github-app-setup.md#personal-access-token-pat-scopes)), and an
OpenAI-compatible endpoint plus API key.

## 1. Create the cluster

```bash
kind create cluster --name hive
```

## 2. Configure

Edit the `YOUR_*` placeholders in
`src/deploy/kustomize/overlays/kind/patch-configmap.yaml` (org/repo, bot login,
dashboard owner, OAuth client id) and set `governor.litellm.endpoint` and
`default_model` for your provider. This is the same `hive-config` ConfigMap the
base ships; the overlay replaces its `hive.yaml`.

## 3. Apply the overlay and create the secret

```bash
kubectl apply -k src/deploy/kustomize/overlays/kind

kubectl -n hive create secret generic hive-secrets \
  --from-literal=HIVE_GITHUB_TOKEN=ghp_... \
  --from-literal=HIVE_DASHBOARD_TOKEN="$(openssl rand -hex 32)" \
  --from-literal=litellm_api_key=sk-... \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n hive rollout restart deploy/hive
kubectl -n hive rollout status deploy/hive --timeout=600s
```

## 4. Port-forward and sign in

```bash
kubectl -n hive port-forward svc/hive 3002:3002
curl -fsS http://localhost:3002/api/health
```

Open <http://localhost:3002> and sign in with the GitHub device flow (the
`github.oauth_client_id` you set in step 2).

## 5. Clean up

```bash
kind delete cluster --name hive
```

## kind gotchas

- **StorageClass.** kind's default class is `standard`; the overlay pins it on
  the `hive-data` PVC. `ReadWriteOnce` is enough — the Deployment is
  `replicas: 1` with `strategy: Recreate`.
- **`NET_ADMIN`.** The base requests `CAP_NET_ADMIN` for the forced-egress
  iptables gate. kind grants it; do not drop it. See
  [net-admin-requirement.md](net-admin-requirement.md).
- **Netfilter modules.** The gate needs `xt_mark` and `xt_REDIRECT` on the
  *host* kernel (kind nodes share it). If the pod exits 77, run
  `sudo modprobe xt_mark xt_REDIRECT xt_owner` on the host and restart the pod
  (see [#9757](https://github.com/hivecommons/hive/issues/9757)).
- **Image size.** `ghcr.io/hivecommons/hive:stable` is large; the first pull can
  take minutes, so use the long rollout timeout above. To avoid re-pulling into
  each new cluster: `docker pull` it once, then
  `kind load docker-image ghcr.io/hivecommons/hive:stable --name hive`.
- **No inference component.** The overlay omits
  [`src/deploy/inference`](../deploy/inference/): it needs Gateway API Inference
  Extension CRDs and downloads a model. The base's `HIVE_LLMD_ENDPOINT` is
  removed for the same reason; agents use the external `litellm` endpoint.

Smoke test: `bash src/deploy/test_kind_quickstart.sh` (skips if `kind` or
`kubectl` is missing).
