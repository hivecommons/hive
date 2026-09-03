# Standalone (self-hosted, hub-less) Hive overlay

This Kustomize overlay deploys a **self-hosted Hive** — the "Joe" scenario: an
operator running their own Hive against their own repos, with **no connection to
the kubestellar hub**. It layers on the two in-repo bases:

| Base | What it provides |
|------|------------------|
| `../../../k8s` | Core Hive workload: namespace, ConfigMap, Secret, PVC, RBAC, Deployment, Service. |
| `../../../inference` | An in-cluster OpenAI-compatible inference backend (llama.cpp serving Qwen2.5-0.5B) plus EPP RBAC — so you get a working model endpoint without an external provider. |

The overlay only adds **deltas**; it never edits the bases. A concrete,
filled-in copy lives in [`example-joe-spyre/`](./example-joe-spyre) for
reference.

## What you MUST swap

All placeholders are UPPER_SNAKE_CASE. Search for them before applying:

| Placeholder | File | What to put |
|-------------|------|-------------|
| `YOUR_ORG`, `YOUR_REPO` | `patch-configmap.yaml` (`project:`) | Your GitHub org and repo(s). `primary_repo` must be one of `repos`. |
| `YOUR_BOT_LOGIN` | `patch-configmap.yaml` (`project.ai_author`) | The GitHub login the agents author PRs as. |
| `YOUR_GITHUB_LOGIN` | `patch-configmap.yaml` (`dashboard.authorized_users`) | Your GitHub login. First entry is the owner (read-write). |
| `YOUR_OAUTH_CLIENT_ID` | `patch-configmap.yaml` (`github.oauth_client_id`) | Client ID of a GitHub OAuth App for device-flow dashboard login. For GHE, also set `github.forge` / `project.forge` to your host. |
| litellm `endpoint` | `patch-configmap.yaml` (`governor.litellm.endpoint`) | Your OpenAI-compatible endpoint. Defaults to the in-cluster inference Service. |
| `YOUR_RWX_STORAGE_CLASS` | `patch-pvc-storageclass.yaml` | A storage class on your cluster (RWO is fine for the default single replica). |
| Route host (OpenShift) | see below | If you also want a Route, add the openshift overlay or set `spec.host`. |

If the dashboard's OAuth callbacks (`/linear/callback` for the Linear agent,
`/openrouter/callback` for OpenRouter funding) are published on a different
hostname than the one you open the dashboard on — or your ingress rewrites the
`Host` header on the way in (Traefik with a fixed upstream `Host`, a Cloudflare
Tunnel "HTTP Host Header") — also set `dashboard.public_url` in
`patch-configmap.yaml` to the externally reachable origin
(`https://hive.example.com`, no path). Without it the install leg and the
callback leg can derive different `redirect_uri`s and the provider rejects the
code exchange. Do not use `hub.dashboard_url` for this on a standalone hive;
see [docs/linear-agent.md](../../../../docs/linear-agent.md#setup).

Also populate `hive-secrets` (from the base) with a GitHub App PEM **or**
`HIVE_GITHUB_TOKEN`, and — if your litellm/inference endpoint needs auth —
the key named by `governor.litellm.api_key_env` (default `HIVE_LITELLM_API_KEY`)
or mounted at `api_key_file` (default `/secrets/litellm_api_key`).

## The NET_ADMIN decision

The base Deployment requests `CAP_NET_ADMIN`. The Hive entrypoint uses it to
install a forced-proxy-egress `iptables` REDIRECT that enforces the ACMM MITM
proxy (the security gate that stops an agent bypassing the proxy with a raw
token).

- **Cluster GRANTS NET_ADMIN (most do):** do nothing. The full gate comes up
  automatically. This is the recommended, fail-closed default.
- **Cluster DENIES NET_ADMIN:** two possible signatures, two remedies.
  - *OpenShift admission rejection* — the pod never starts; `oc -n hive get
    events` shows `unable to validate against any security context constraint`
    (no stock SCC, `anyuid` included, permits the capabilities the base
    deployment adds). **Remedy:** a cluster-admin applies the
    `../openshift-netadmin` overlay (dedicated `hive-netadmin` SCC + RoleBinding)
    — this is the real fix; the full gate then comes up.
  - *Runtime FATAL* — the pod starts but crash-loops with
    `FATAL: refusing to start ... capability model would be advisory-only` and
    **exit code 77** (EX_NOPERM) when the container's bounding set lacks
    `CAP_NET_ADMIN` (any other cause of that FATAL exits 1). **Remedy** if you
    cannot grant the capability: start anyway in **advisory-only** mode by
    uncommenting the `- path: patch-advisory-mode.yaml` line in
    `kustomization.yaml` (sets `HIVE_PROXY_ADVISORY_OK=true`). **Trade-off:**
    forced proxy egress becomes advisory — best-effort, not enforced — so an
    agent could bypass the proxy. Only do this if you accept that.

Not sure which case you're in? Run the checks in
`src/docs/manual-provisioning.md` ("Does your cluster grant NET_ADMIN?"), and
see `src/docs/net-admin-requirement.md` for the full explanation.

## Install

```bash
# Review the rendered manifests first:
kubectl kustomize src/deploy/kustomize/overlays/standalone

# Apply:
kubectl apply -k src/deploy/kustomize/overlays/standalone

# Watch it come up:
kubectl -n hive rollout status deploy/hive
kubectl -n hive-inference rollout status deploy/vllm
```

## Pinning / upgrading the image

The base tracks `ghcr.io/hivecommons/hive:stable`. Pin so that upgrades are
deliberate — there is no hub to auto-upgrade a standalone hive.

**Which tags actually exist on `ghcr.io/hivecommons/hive`:**

- Channel tags (`stable`, `candidate`, `edge`, `v4-latest`) and an immutable
  7-character short-SHA tag for every merge to `v4`. These are what `docker.yml`
  publishes, and they are the only tags that are guaranteed to exist.
- `vX.Y.Z` **image** tags are produced only by the automated
  [tagged-release workflow](../../../../docs/releases.md)
  (`.github/workflows/tagged-release.yml`), which retags the just-published short-SHA
  images with the version it cut. That workflow landed *after* the `v4.0.0`
  **git** tag, so **there is no `ghcr.io/hivecommons/hive:v4.0.0` image** — a
  `newTag: v4.0.0` pin goes straight to `ImagePullBackOff`. Before pinning a
  version tag, confirm the image exists:

  ```bash
  # Anonymous pull token, then ask the registry for that tag's manifest.
  TOKEN=$(curl -s "https://ghcr.io/token?scope=repository:kubestellar/hive:pull" | jq -r .token)
  curl -sI -H "Authorization: Bearer $TOKEN" \
    -H "Accept: application/vnd.oci.image.index.v1+json" \
    https://ghcr.io/v2/hivecommons/hive/manifests/v4.1.0 | head -1   # 200 = exists, 404 = not published
  ```

  Version tags that do exist are listed on the
  [GitHub Releases page](https://github.com/kubestellar/hive/releases) (each
  release attaches the SBOM of the image it tagged).

**Pin by digest (works today, always immutable — recommended).** Resolve the
channel you reviewed to its digest, then write it into `kustomization.yaml`
(git-tracked) from this overlay directory:

```bash
TOKEN=$(curl -s "https://ghcr.io/token?scope=repository:kubestellar/hive:pull" | jq -r .token)
DIGEST=$(curl -sI -H "Authorization: Bearer $TOKEN" \
  -H "Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json" \
  https://ghcr.io/v2/hivecommons/hive/manifests/stable \
  | awk 'tolower($1)=="docker-content-digest:" {print $2}' | tr -d '\r')
echo "$DIGEST"   # sha256:...

kustomize edit set image \
  ghcr.io/hivecommons/hive=ghcr.io/hivecommons/hive@"$DIGEST"
```

(`docker buildx imagetools inspect ghcr.io/hivecommons/hive:stable` or
`crane digest ghcr.io/hivecommons/hive:stable` print the same digest if you
have those tools.)

**Pin by version tag** — only once you have confirmed the tag exists as above:

```bash
kustomize edit set image ghcr.io/hivecommons/hive=ghcr.io/hivecommons/hive:vX.Y.Z
```

Either form writes an `images:` entry into `kustomization.yaml`. To upgrade,
bump the digest/tag in git and re-apply:

```bash
kubectl apply -k src/deploy/kustomize/overlays/standalone
```

## Storage

The base PVC (`../../../k8s/pvc.yaml`) is **`ReadWriteOnce`**, which is correct
for the default single replica — any block storage class (AKS `managed-csi`,
EBS, Ceph RBD, …) works. `patch-pvc-storageclass.yaml` only sets
`storageClassName`; you do **not** need an RWX class. The base Deployment rolls
with `strategy: Recreate`, so old and new pods never mount `/data` at the same
time — RWX only matters if you change that to a surge rollout.

## Exposing the dashboard

This overlay does not create an Ingress or Route. On OpenShift, use the
`../openshift` overlay (or add a Route with your `spec.host`) which targets the
base Service `hive` port `dashboard` (3002). On plain Kubernetes, add your own
Ingress to the same Service/port.
