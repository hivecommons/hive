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

The overlay's `hive.yaml` reads the PAT through `github.token:
${HIVE_GITHUB_TOKEN}`, so the Secret must carry that key. `kubectl apply -k` does
not manage `hive-secrets` (the base ships
[`secret.yaml`](../deploy/k8s/secret.yaml) only as a template), so you can
re-apply the overlay after config changes without losing these keys.

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

## Testing v6 changes

The overlay defaults to `ghcr.io/hivecommons/hive:stable`, which is a v5 build. To test something that merged to `v6`, switch to `edge` before applying:

```bash
cd src/deploy/kustomize/overlays/kind
kustomize edit set image ghcr.io/hivecommons/hive:edge
docker pull ghcr.io/hivecommons/hive:edge
kind load docker-image ghcr.io/hivecommons/hive:edge --name hive
```

Check which commit the image was built from with
`podman inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' ghcr.io/hivecommons/hive:edge`.
See [release-channels.md](release-channels.md#testing-v6-changes).

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

  **Docker 29 and later** (with containerd image store): `kind load docker-image`
  fails on the multi-arch image with `ctr: content digest ... not found` because
  `docker save` exports the index without un-pulled platform layers. Use this
  instead:

  ```bash
  docker save --platform linux/amd64 ghcr.io/hivecommons/hive:stable \
    | docker exec -i hive-control-plane ctr -n k8s.io images import --all-platforms -
  ```

  (Alternatively, save to an archive with `--platform` and use `kind load image-archive`.)

  **Important:** The base Deployment uses `imagePullPolicy: Always`, so the loaded
  image won't be used offline without an overlay patch. Set `imagePullPolicy: IfNotPresent`:

  ```yaml
  # kustomization.yaml or an overlay
  patches:
    - target:
        kind: Deployment
        name: hive
      patch: |-
        - op: replace
          path: /spec/template/spec/containers/0/imagePullPolicy
          value: IfNotPresent
  ```
- **Config fix after a failed first boot.** The first boot writes
  `/data/hive.yaml.runtime` from the ConfigMap, and later boots read that file
  instead of the ConfigMap. If the pod crash-looped on a bad config, fixing
  `patch-configmap.yaml` and re-applying is not enough: also run
  `kubectl -n hive delete pvc hive-data` (this deletes all Hive state; then
  re-apply the overlay) or remove `/data/hive.yaml.runtime`, then restart the
  pod.
- **No inference component.** The overlay omits
  [`src/deploy/inference`](../deploy/inference/): it needs Gateway API Inference
  Extension CRDs and downloads a model. The base's `HIVE_LLMD_ENDPOINT` is
  removed for the same reason; agents use the external `litellm` endpoint.

Smoke test: `bash src/deploy/test_kind_quickstart.sh` (skips if `kind` or
`kubectl` is missing).
