# Provisioning a Hosted Hive

> **Self-hosting a standalone hive (no hub)?** Most of this guide is about
> **hub-attached** hosted hives on a managed fleet (a hub-reachable cluster and
> a heartbeat-only cluster, a shared hub, placeholder pools). If instead you are
> standing up your **own** hub-less hive on your **own** cluster against your
> **own** repos, read
> **[Standalone / self-hosted (no hub)](#standalone--self-hosted-no-hub)** below
> and skip the hub-only Path A / Path B, `meta.json`, and placeholder-pool
> sections — none of them apply to you.

This guide documents how hosted hives are provisioned, covering both paths:

- **Automated provisioning** — the hub creates everything itself, on any cluster
  the hub can reach with `kubectl`. This is the normal path.
- **Manual provisioning** — hand-applied manifests, required when the hub has
  **no network path** to the target cluster (a *heartbeat-only* cluster). This
  is the situation on any **heartbeat-only** cluster.

Every command and manifest below was executed and verified while standing up a
pool of ten placeholder hives — five on a **heartbeat-only** cluster (manual)
and five on a **hub-reachable** cluster (automated) — so the procedure is real,
not aspirational. Where a step has a non-obvious failure mode, it is called out
as a **Gotcha**.

---

## Background: how the two clusters differ

The provisioning path is dictated entirely by whether the hub can `kubectl` to
the target cluster.

| | **Hub-reachable cluster** (vanilla Kubernetes) | **Heartbeat-only cluster** (OpenShift) |
|---|---|---|
| Hub reachability | Hub **can** `kubectl` → **automated** provisioning | Hub is **heartbeat-only** → **manual** provisioning |
| Dashboard routing | nginx **Ingress**, host `<id>.hive.hivecommons.dev` | OpenShift **Route**, host `<id>.apps.<your-cluster-domain>` |
| Auth in front of the dashboard | Hub's nginx ingress runs `auth-url` → `/api/saas/auth-check`, injecting `X-Hive-User` / `X-Hive-Role`. Spokes need **no** own OAuth. | No hub auth proxy. Each spoke runs its **own** GitHub device-flow login (`oauth_client_id`, `hub_proxied: false`). |
| Pod security | Standard Kubernetes; empty `securityContext`. | OpenShift SCC. The pod **must** run under the `anyuid` SCC (the entrypoint `chown`s the PVC as root). Without it the pod lands on `restricted-v2` and crash-loops. |
| Storage | RWX volume, default storage class. | RWX on `ocs-storagecluster-cephfs`. |
| Which methods it serves | **Public** methods (claude / copilot / gemini — subscription CLIs). | **Private** methods (litellm / vllm / llm-d — self-hosted inference). |

> **Why methods map to clusters.** Public-method hives run subscription CLIs and
> need no in-cluster inference, so they live on the hub's own (hub-reachable)
> cluster. Private-method hives point at a self-hosted inference endpoint that
> lives on the heartbeat-only cluster, so the hive runs there too — and that
> cluster is heartbeat-only, which is exactly why it needs the manual path.

---

## Prerequisites (both paths)

- `kubectl` access with a context per cluster (one for the hub-reachable
  cluster, one for the heartbeat-only cluster).
- The hub is running on the **hub-reachable** cluster in namespace `hive-hub`,
  backed by a RWX PVC named `hive-hub-data-rwx` mounted at `/data`. The hub's
  SaaS store lives at `/data/saas/hives/<id>/meta.json`; its fleet registry at
  `/data/hub-registry.json`.
- For manual (heartbeat-only cluster) provisioning: the
  `system:openshift:scc:anyuid` ClusterRole must exist (it does by default on
  OpenShift).

---

## Standalone / self-hosted (no hub)

This section is the **complete** path for the hub-less scenario: an operator
("Joe") running their **own** hive on their **own** firewalled cluster, against
their **own** repos, with **self-hosted inference** (litellm / vllm / llm-d) and
**no attachment** to `hub.kubestellar.io`. Everything after this section (Path A,
Path B, `meta.json`, placeholder pools, claiming, upgrade-via-hub) is
**hub-only** and does **not** apply — skip it.

### What "standalone" means

A standalone hive has **no hub**. In `src/pkg/config/config.go` the `hub:` block
is a `HubConfig` whose zero value has `Enabled: false`, so a config that simply
omits `hub:` is already hub-less. Concretely, a standalone hive needs **none** of
the hub plumbing the rest of this guide describes:

- **No `HIVE_HUB_SECRET`, no `HIVE_HUB_URL`, no derived `HIVE_HEARTBEAT_KEY` /
  `HIVE_SESSION_KEY` / `HIVE_SSO_KEY`** — those authenticate a spoke to a hub; a
  hive with nothing to heartbeat to needs none of them.
- **No `meta.json` / My-Hives / hub SaaS record** — there is no hub SaaS store.
- **No claim / placeholder-pool / Upgrade steps** — placeholders and hub-driven
  auto-upgrade are hub features. You upgrade the image yourself (below).

Because there is no hub auth proxy in front of it, a standalone hive enforces its
own dashboard login: `dashboard.hub_proxied: false` (the default) plus a
`dashboard.authorized_users` allowlist and a `github.oauth_client_id` for the
device-flow login. All three are covered in the swap checklist.

### Install method: Kustomize

Use the **Kustomize** overlays — a peer already built a base and a standalone
overlay, so this is the least-effort, git-trackable path (no operator, no Helm
values to reverse-engineer). The overlay layers two in-repo bases — the core
workload (`src/deploy/k8s`) and an in-cluster OpenAI-compatible inference backend
(`src/deploy/inference`, llama.cpp serving Qwen2.5-0.5B) — so you get a working
model endpoint out of the box. See the overlay and its README here:

- **Standalone overlay** —
  [github.com/hivecommons/hive/tree/v4/src/deploy/kustomize/overlays/standalone](https://github.com/hivecommons/hive/tree/v4/src/deploy/kustomize/overlays/standalone)
  (annotated `patch-configmap.yaml`, `patch-pvc-storageclass.yaml`,
  `patch-advisory-mode.yaml`, and a
  [`README.md`](https://github.com/hivecommons/hive/blob/v4/src/deploy/kustomize/overlays/standalone/README.md)
  with the full swap list).
- **Filled-in example** ("Joe on Spyre") —
  [github.com/hivecommons/hive/tree/v4/src/deploy/kustomize/overlays/standalone/example-joe-spyre](https://github.com/hivecommons/hive/tree/v4/src/deploy/kustomize/overlays/standalone/example-joe-spyre).
  Every value there is a placeholder — copy the shape, don't apply it verbatim.

There are two flows. Pick based on whether you just want to *see it run* or
you're doing a *real* deployment.

**1. Remote quickstart (see the shape).** Kustomize can build straight from
GitHub — no clone:

```bash
kubectl apply -k "https://github.com/hivecommons/hive//src/deploy/kustomize/overlays/standalone?ref=v4"
```

(Note the `//` between the repo and the sub-path — kustomize needs it to find
the directory inside the repo; a single slash does not resolve.)

This applies the overlay with its **PLACEHOLDER** values (`YOUR_ORG`,
`YOUR_OAUTH_CLIENT_ID`, `YOUR_RWX_STORAGE_CLASS`, …). It is only for seeing the
manifest shape come up — it will **not** be a working hive, because the repo,
storage class, OAuth app, and inference endpoint are all placeholders you must
swap first.

**2. Real path (clone → edit → apply).** This is the path you actually deploy,
because you **must** swap values before it works:

```bash
git clone -b v4 https://github.com/hivecommons/hive.git
cd hive/src/deploy/kustomize/overlays/standalone
# Edit the placeholders — see "What you must swap" below:
#   patch-configmap.yaml       (org/repos, owner login, OAuth client id, litellm endpoint)
#   patch-pvc-storageclass.yaml (your storage class — the PVC is RWO; RWX not needed)
# Review the rendered manifests, then apply:
kubectl kustomize .
kubectl apply -k .
kubectl -n hive rollout status deploy/hive
kubectl -n hive-inference rollout status deploy/vllm
```

**Upgrading the image.** The base tracks `ghcr.io/hivecommons/hive:stable`. To
pin (so upgrades are deliberate — there is no hub to auto-upgrade you), resolve
the channel you reviewed to a **digest** and write it in from the overlay
directory, then re-apply:

```bash
TOKEN=$(curl -s "https://ghcr.io/token?scope=repository:hivecommons/hive:pull" | jq -r .token)
DIGEST=$(curl -sI -H "Authorization: Bearer $TOKEN" \
  -H "Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json" \
  https://ghcr.io/v2/hivecommons/hive/manifests/stable \
  | awk 'tolower($1)=="docker-content-digest:" {print $2}' | tr -d '\r')
kustomize edit set image ghcr.io/hivecommons/hive=ghcr.io/hivecommons/hive@"$DIGEST"
kubectl apply -k .
```

> **Which image tags exist.** `ghcr.io/hivecommons/hive` carries the channel
> tags (`stable`, `candidate`, `edge`, `v4-latest`) and a short-SHA tag per
> merge. A `vX.Y.Z` **image** tag exists only when the automated tagged-release
> workflow (`.github/workflows/tagged-release.yml`, see
> [Tagged releases](releases.md)) has cut that version — it retags the merge's
> short-SHA images. That workflow landed after the `v4.0.0` git tag, so there is
> **no `:v4.0.0` image**; `newTag: v4.0.0` is an `ImagePullBackOff`. Pin a
> version tag only after confirming it on the
> [Releases page](https://github.com/hivecommons/hive/releases) or with
> `curl -sI -H "Authorization: Bearer $TOKEN" https://ghcr.io/v2/hivecommons/hive/manifests/vX.Y.Z`
> (200 = exists). Digests always work.

**Storage.** The base `hive-data` PVC is `ReadWriteOnce`. That is correct for
the single-replica standalone hive — any block storage class works; you do
**not** need an RWX class. (RWX is only relevant to the hub-provisioned path
further down, whose template runs a surge rollout.)

**On OpenShift.** The standalone overlay does not create a Route or grant any
SCC. Two more pieces supply the OpenShift-only deltas:

- The
  [`overlays/openshift`](https://github.com/hivecommons/hive/tree/v4/src/deploy/kustomize/overlays/openshift)
  overlay — a `Route` exposing the `dashboard` port (3002) and the `anyuid` SCC
  RoleBinding (the pod starts as root to set up the ACMM iptables and chown the
  PVC).
- The
  [`overlays/openshift-netadmin`](https://github.com/hivecommons/hive/tree/v4/src/deploy/kustomize/overlays/openshift-netadmin)
  overlay — the dedicated `hive-netadmin` SCC + RoleBinding, applied **once by a
  cluster-admin**. This one is **not optional on OpenShift**: the base
  deployment *adds* capabilities (`NET_ADMIN` + the su-exec set), and no stock
  SCC — `anyuid` included — permits adding them, so without this grant the pod
  is rejected at admission (`unable to validate against any security context
  constraint`). The SCC is `restricted-v2` loosened by exactly what the base
  deployment declares (see the manifest comments) — not `privileged`. See
  [Does your cluster grant NET_ADMIN?](#does-your-cluster-grant-net_admin) to
  check your cluster first.

A standalone deployment on OpenShift composes **standalone + these platform
deltas** (the SCC grant is applied separately, by cluster-admin:
`oc apply -k src/deploy/kustomize/overlays/openshift-netadmin`). For the
app-level overlays, two equivalent ways:

- **Combined overlay (recommended):** in your own copy of the standalone
  `kustomization.yaml`, add `route.yaml` and `anyuid-scc-rolebinding.yaml` as
  additional resources — this is exactly what the
  [`example-joe-spyre`](https://github.com/hivecommons/hive/tree/v4/src/deploy/kustomize/overlays/standalone/example-joe-spyre)
  overlay does (it adds its own `route.yaml`; add the SCC binding the same way).
  Set the Route `spec.host` to your cluster's apps domain (or drop `host` to let
  OpenShift auto-generate one).
- **Apply both:** `kubectl apply -k <standalone>` then
  `kubectl apply -k <openshift>` — but note the openshift overlay re-includes the
  `src/deploy/k8s` base, so prefer the combined form to avoid re-applying the
  base twice.

### Keeping your overlay private

The example overlay in the public repo (`example-joe-spyre`) is a *template* —
it carries placeholder values and is meant to be read, not applied. Your real
overlay will contain private hostnames, OAuth client IDs, cluster domain names,
and org names you absolutely do not want in a public fork. The right pattern is
a **private repository that references the public hive base remotely**, so you
get upstream updates without ever committing private values anywhere public.

#### Why not fork?

Forking `hivecommons/hive` as a private repo is the instinct, but it's the
wrong tool for this job:

- A fork carries the entire hive source tree — 100k+ lines of Go, frontend, CI,
  tests. You'd be maintaining a fork of code you don't own and don't change.
- Pulling upstream updates into a fork requires `git merge upstream/v4` — merge
  conflicts on code you didn't write, against a codebase that moves fast.
- Your private values (hostnames, tokens, OAuth IDs) are mixed into the same
  repo as all that source history. One accidental visibility flip and they're
  public.

The right model is a **tiny private repo that contains *only* your values**, and
references the public overlay by URL + pinned tag. No code you don't own, no
merge conflicts, no accidental exposure.

#### Repository layout

Create a **private** repo (on your GHE instance, a private GitHub.com repo,
GitLab, Gitea — whatever you control). The structure is minimal:

```
my-hive-config/                     ← your private repo
├── overlays/
│   └── spyre/                      ← one subdirectory per cluster/environment
│       ├── kustomization.yaml      ← references the public base by URL
│       ├── patch-configmap.yaml    ← your real org/repo/OAuth values
│       ├── patch-pvc-storageclass.yaml
│       └── route.yaml              ← OpenShift only; your cluster's apps domain
└── README.md                       ← internal runbook: how to install/upgrade
```

If you have more than one cluster (staging, production, a second team's Spyre),
add another subdirectory per cluster — each references the same upstream base:

```
overlays/
├── spyre-staging/
│   ├── kustomization.yaml
│   └── patch-configmap.yaml
└── spyre-prod/
    ├── kustomization.yaml
    └── patch-configmap.yaml
```

#### `kustomization.yaml` — reference the public base by URL

The key insight: kustomize can fetch remote bases at `apply` time — you never
clone the hive repo on your machine or your CI runner. Pin to a **release tag**,
not a branch, so every apply is deterministic:

```yaml
# overlays/spyre/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
  # ── Public hive standalone overlay, pinned to a reviewed git ref ──────────
  # kubectl/kustomize fetches this from GitHub at apply time — no local clone.
  # Bump the ref= value in a git commit when you want to upgrade; review the
  # diff of what changed upstream before you do. A git TAG (v4.0.0) or a full
  # commit SHA is immutable; a branch (v4) moves under you.
  - https://github.com/hivecommons/hive//src/deploy/kustomize/overlays/standalone?ref=v4.0.0
  #
  # OpenShift only: add your Route here alongside the remote base.
  # On plain Kubernetes, use an Ingress resource instead.
  - route.yaml

# Pin the image for extra safety. Without this the base tracks
# `ghcr.io/hivecommons/hive:stable`; pinning means upgrades are deliberate —
# you bump this in git, review, and apply. Pin by DIGEST: a git tag does not
# imply an image tag (there is no `:v4.0.0` image — see "Upgrading the image"
# above for how to resolve `stable` to a digest and how to check whether a
# `vX.Y.Z` image tag exists before using one).
images:
  - name: ghcr.io/hivecommons/hive
    newName: ghcr.io/hivecommons/hive
    digest: sha256:<digest-you-resolved-from-stable>

patches:
  - path: patch-configmap.yaml
  - path: patch-pvc-storageclass.yaml
  # Uncomment ONLY if your cluster denies NET_ADMIN — see "The NET_ADMIN
  # decision" section below. Leaving this commented keeps the fail-closed gate.
  # - path: patch-advisory-mode.yaml
```

> **`ref=` branch vs tag:** `?ref=v4` follows the branch — every `apply` may
> get a different manifest from the last one. `?ref=v4.0.0` (a git tag) or
> `?ref=<full-sha>` is immutable — the same apply produces the same result
> every time. For a production cluster, always pin a tag or SHA; bump it
> deliberately in a git commit so you have a record of every upgrade. Git tags
> are listed at https://github.com/hivecommons/hive/tags — note they are a
> different thing from image tags (see "Which image tags exist" above).

> **Double slash `//` in the URL:** kustomize requires `//` to separate the
> repo root from the path inside it. `github.com/hivecommons/hive//src/...` is
> correct; `github.com/hivecommons/hive/src/...` (single slash) won't resolve
> the sub-path correctly.

#### `patch-configmap.yaml` — your private values

Copy the annotated template from
[`overlays/standalone/patch-configmap.yaml`](https://github.com/hivecommons/hive/blob/v4/src/deploy/kustomize/overlays/standalone/patch-configmap.yaml)
into your private repo, then replace every `YOUR_*` placeholder with real
values. The full patch — with comments stripped to show only what you actually
need to swap:

```yaml
# overlays/spyre/patch-configmap.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: hive-config
  namespace: hive
data:
  hive.yaml: |
    project:
      org: your-github-org             # the GitHub org (or user) that owns your repos
      repos:
        - your-repo                    # repo(s) hive manages; list all of them
      ai_author: your-bot-login        # GitHub login agents use when authoring PRs
      primary_repo: your-repo          # must appear in repos: above

    acmm_level: 2                      # 2 = plan+approve gating; 3 = fully autonomous

    hub:
      enabled: false                   # standalone; no hub connection

    agents:
      supervisor:
        enabled: true
        backend: litellm               # routes to governor.litellm.endpoint below
        beads_dir: /data/beads/supervisor
        clear_on_kick: true
      scanner:
        enabled: true
        backend: litellm
        beads_dir: /data/beads/scanner
        clear_on_kick: true
      architect:
        enabled: true
        backend: litellm
        beads_dir: /data/beads/architect
        clear_on_kick: false

    governor:
      eval_interval_s: 300
      litellm:
        endpoint: http://litellm-svc.llm-gateway.svc.cluster.local:4000
        api_key_env: HIVE_LITELLM_API_KEY
        default_model: your-model-name

    dashboard:
      port: 3002
      hub_proxied: false
      authorized_users:
        - your-github-login:owner      # first entry is the owner; add more as :member

    github:
      oauth_client_id: YOUR_OAUTH_CLIENT_ID   # from your GitHub OAuth App settings
```

This file is safe to commit to your private repo — it has no secrets.
OAuth client IDs are not secret (they're visible in the browser redirect URL);
OAuth client **secrets** live in Kubernetes Secrets, not here.

#### `patch-pvc-storageclass.yaml` — storage class

One value to swap:

```yaml
# overlays/spyre/patch-pvc-storageclass.yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: hive-data
  namespace: hive
spec:
  storageClassName: ocs-storagecluster-cephfs   # ← a storage class on your cluster
```

To find the right class name:

```bash
kubectl get storageclass
# Look for one marked (default) or annotated storageclass.kubernetes.io/is-default-class=true
# The base PVC is ReadWriteOnce, so any block class works (AKS managed-csi, EBS, RBD, ...).
# On Spyre/ODF clusters, ocs-storagecluster-cephfs (RWX) also works — RWX is allowed, not required.
```

#### `route.yaml` — OpenShift only

```yaml
# overlays/spyre/route.yaml
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: hive
  namespace: hive
spec:
  host: hive.apps.your-cluster.example.com    # ← your cluster's apps domain
  port:
    targetPort: dashboard
  to:
    kind: Service
    name: hive
  tls:
    termination: edge
    insecureEdgeTerminationPolicy: Redirect
```

Drop `spec.host` to let OpenShift auto-generate a hostname from the cluster's
`apps.*` wildcard. Add it back once you know the generated name and want to
pin it. On plain Kubernetes, replace this with an `Ingress` object pointing at
the `hive` service's `dashboard` port (3002).

#### Secrets — keep them out of git entirely

The `hive-secrets` Kubernetes Secret holds your GitHub App private key (PEM),
OAuth client secret, and — if your LiteLLM endpoint requires auth — the API
key. These must **never** be committed to git, even a private repo.

**Option 1 — Manual one-time `kubectl create` (simplest)**

Create the Secret directly on your cluster once, before or right after the
first `apply -k`. Kustomize won't delete a Secret it didn't create, so this
survives all future `apply -k` runs:

```bash
# GitHub App PEM + OAuth client secret
kubectl create secret generic hive-secrets -n hive \
  --from-file=github_app_private_key=./your-app.pem \
  --from-literal=HIVE_GITHUB_OAUTH_CLIENT_SECRET=your-oauth-secret

# If your LiteLLM endpoint requires an API key:
kubectl patch secret hive-secrets -n hive \
  --type=merge \
  -p '{"stringData":{"HIVE_LITELLM_API_KEY":"your-litellm-key"}}'
```

To update a key later: `kubectl patch secret hive-secrets -n hive --type=merge -p '{"stringData":{"HIVE_GITHUB_OAUTH_CLIENT_SECRET":"new-secret"}}'`

**Option 2 — Sealed Secrets (safe to commit, GitOps-native)**

If you want everything in git (including secrets), [Bitnami Sealed Secrets](https://github.com/bitnami-labs/sealed-secrets) encrypts Secrets with the cluster's public key — only that cluster can decrypt. The encrypted `SealedSecret` object is safe to commit:

```bash
# Install controller once (cluster-admin):
kubectl apply -f https://github.com/bitnami-labs/sealed-secrets/releases/latest/download/controller.yaml

# Seal your secret:
kubectl create secret generic hive-secrets -n hive \
  --from-file=github_app_private_key=./your-app.pem \
  --from-literal=HIVE_GITHUB_OAUTH_CLIENT_SECRET=your-oauth-secret \
  --dry-run=client -o yaml \
  | kubeseal --controller-namespace kube-system -o yaml \
  > overlays/spyre/sealed-hive-secrets.yaml

# Add to kustomization.yaml:
#   resources:
#     - sealed-hive-secrets.yaml
git add overlays/spyre/sealed-hive-secrets.yaml
git commit -m "Seal hive-secrets for spyre cluster"
```

**Option 3 — External Secrets Operator (fetch from Vault / AWS Secrets Manager / etc.)**

If your org already uses a secrets store, add an `ExternalSecret` object to
your overlay. The ESO controller fetches the values at sync time. The
`ExternalSecret` manifest contains only paths/keys — no actual secrets — and is
safe to commit.

> **Do not** put a `Secret` object with `stringData:` values in your
> `kustomization.yaml` `secretGenerator` or as a raw resource — kustomize
> base64-encodes but does not encrypt, and the values end up in your git history.

#### Day-to-day workflow

**First install (from scratch):**

```bash
# Clone your private repo (not the hive repo):
git clone git@github.example.com:your-org/my-hive-config.git
cd my-hive-config

# Preview — see every manifest kustomize will send to the cluster:
kubectl kustomize overlays/spyre | less

# Create the secret first (Option 1 above), then apply:
kubectl apply -k overlays/spyre/
kubectl -n hive rollout status deploy/hive     # wait for Ready
kubectl -n hive rollout status deploy/vllm     # inference backend
```

**Changing a config value** — e.g. adding an authorized user or changing the
LiteLLM endpoint. Edit your private repo, commit, push, re-apply — the cluster
converges:

```bash
# Edit overlays/spyre/patch-configmap.yaml, then:
git add overlays/spyre/patch-configmap.yaml
git commit -m "Add jane to authorized_users"
git push
kubectl apply -k overlays/spyre/
# The ConfigMap updates in-place; hive picks up the change on its next
# config reload cycle (or force one: kubectl rollout restart deploy/hive -n hive)
```

**Upgrading hive to a new version:**

```bash
# 1. Read the release notes for the new tag:
#    https://github.com/hivecommons/hive/releases

# 2. Bump the ref and the image pin in kustomization.yaml:
#    resources:
#      - https://github.com/hivecommons/hive//src/deploy/kustomize/overlays/standalone?ref=<new git tag or sha>
#    images:
#      - name: ghcr.io/hivecommons/hive
#        digest: sha256:<digest resolved from stable, or from the release's image tag>
#    (use `newTag: vX.Y.Z` only after confirming that IMAGE tag exists — see
#    "Which image tags exist" above)

# 3. Preview the diff — see exactly what changes upstream:
kubectl kustomize overlays/spyre | diff - <(kubectl kustomize overlays/spyre 2>/dev/null) || true
# Or simpler: use `kustomize build` and diff the YAML files manually.

# 4. Commit and apply:
git add overlays/spyre/kustomization.yaml
git commit -m "Upgrade hive to v4.3.0"
git push
kubectl apply -k overlays/spyre/
kubectl -n hive rollout status deploy/hive
```

**Rolling back** — apply is idempotent; revert your commit and re-apply:

```bash
git revert HEAD   # or git checkout <old-sha> -- overlays/spyre/kustomization.yaml
kubectl apply -k overlays/spyre/
```

#### If you cannot reach github.com at apply time (air-gapped cluster)

Some clusters have no egress to the public internet at all — `kubectl apply -k
https://github.com/...` will time out. In that case, vendor the base files into
your private repo:

```bash
# One-time: clone the public hive repo at the pinned tag and copy just the
# kustomize overlay you need into a vendor/ directory:
git clone -b v4.2.0 https://github.com/hivecommons/hive.git hive-upstream
cp -r hive-upstream/src/deploy/kustomize overlays/spyre/vendor/
rm -rf hive-upstream

# Update kustomization.yaml to use the local path instead of the URL:
#   resources:
#     - vendor/kustomize/overlays/standalone   ← local, no network needed

git add overlays/spyre/vendor/
git commit -m "Vendor hive kustomize base at v4.2.0"
```

To upgrade: repeat the copy step for the new tag, commit the vendor diff, and
apply. The diff in your PR shows exactly what changed upstream — a useful forced
review gate even when you have egress.

#### Checklist: is my overlay complete?

Before the first `apply -k`, verify:

- [ ] `patch-configmap.yaml` — no `YOUR_*` placeholders remaining
- [ ] `patch-pvc-storageclass.yaml` — `storageClassName` is a real class on your cluster (`kubectl get storageclass`)
- [ ] `route.yaml` (OpenShift) — `spec.host` is reachable from your browser
- [ ] `hive-secrets` Secret exists in the `hive` namespace (`kubectl get secret hive-secrets -n hive`)
- [ ] GitHub OAuth App is registered at the right forge (`github.com` or your GHE host) and its callback URL is `https://<your-route>/github/callback`
- [ ] If OpenShift: `openshift-netadmin` overlay applied by a cluster-admin, or `patch-advisory-mode.yaml` uncommented (see [The NET_ADMIN decision](#the-net_admin-decision-stated-precisely))
- [ ] `kubectl kustomize overlays/spyre/ | grep YOUR_` returns nothing

### What you must swap

Every value below is a placeholder in the overlay. Cited location is where the
key is defined (config struct) and where you edit it (overlay file).

| Swap | Default / placeholder | Where you edit it | Config location (`src/pkg/config/config.go`) |
|---|---|---|---|
| RWX/RWO `storageClassName` | `YOUR_RWX_STORAGE_CLASS` (example uses `ocs-storagecluster-cephfs`) | `patch-pvc-storageclass.yaml` | base `hive-data` PVC (`src/deploy/k8s/pvc.yaml`) — RWO is fine for the default single replica |
| Route host domain (OpenShift) | `hive.apps.joes-cluster.example.com` (example) | `route.yaml` `spec.host` | `route.openshift.io/v1` Route |
| `project.org` / `repos` / `primary_repo` | `YOUR_ORG` / `YOUR_REPO` | `patch-configmap.yaml` (`project:`) | `ProjectConfig` (`Org`, `Repos`, `PrimaryRepo`, `AIAuthor`) |
| `dashboard.authorized_users` | `YOUR_GITHUB_LOGIN:owner` | `patch-configmap.yaml` (`dashboard:`) | `DashboardConfig.AuthorizedUsers` — first entry is the owner |
| `github.oauth_client_id` | `YOUR_OAUTH_CLIENT_ID` | `patch-configmap.yaml` (`github:`) | `GitHubConfig.OAuthClientID`. The built-in default (`DefaultOAuthClientID`) is a **github.com** Hive App — if you use GitHub Enterprise (e.g. `github.your-company.com`) you must register your **own** OAuth App there and also set `project.forge` / `github.forge` to your host |
| litellm `endpoint` + API key | in-cluster inference Service | `patch-configmap.yaml` (`governor.litellm.endpoint`) + the `hive-secrets` Secret | `LiteLLMConfig` under `Config.Governor` (`Endpoint`, `APIKeyEnv` → default `HIVE_LITELLM_API_KEY`, `APIKeyFile` → default `/secrets/litellm_api_key`, `DefaultModel`) |

Also populate the base `hive-secrets` Secret with a GitHub App PEM **or**
`HIVE_GITHUB_TOKEN`, and — if your inference endpoint needs auth — the key named
by `governor.litellm.api_key_env` (default `HIVE_LITELLM_API_KEY`).

### The NET_ADMIN decision (stated precisely)

The base deployment **already requests** the `NET_ADMIN` capability
(`src/deploy/k8s/deployment.yaml`) — the entrypoint uses it to install the
forced-proxy-egress `iptables` REDIRECT that enforces the ACMM MITM egress proxy.

- **Cluster GRANTS `NET_ADMIN` (most do):** nothing to do — the full,
  fail-closed egress gate comes up automatically. This is the recommended
  default.
- **Cluster DENIES `NET_ADMIN`:** the entrypoint **FATALs** (refusing to start
  with an advisory-only capability model — `src/deploy/entrypoint.sh`) and the
  pod crash-loops, with **exit code 77** when the missing capability is the
  cause (see the checks section below for what 77 means and how to read it).
  Two remedies:
  1. **Grant the capability (preferred — full gate).** On OpenShift, a
     cluster-admin applies the
     [`overlays/openshift-netadmin`](https://github.com/hivecommons/hive/tree/v4/src/deploy/kustomize/overlays/openshift-netadmin)
     overlay (dedicated `hive-netadmin` SCC + RoleBinding); on plain Kubernetes,
     ensure the namespace's Pod Security admission allows the capability (see
     below).
  2. **Deliberately run degraded.** Set `HIVE_PROXY_ADVISORY_OK=true` via the
     **commented-out**
     [`patch-advisory-mode.yaml`](https://github.com/hivecommons/hive/blob/v4/src/deploy/kustomize/overlays/standalone/patch-advisory-mode.yaml)
     — uncomment its `- path: patch-advisory-mode.yaml` line in your
     `kustomization.yaml`.

**Security trade-off (one honest sentence):** in advisory-only mode forced
proxy egress becomes advisory — the MITM egress gate is best-effort rather than
enforced, so an agent holding a raw token could bypass the proxy policy —
acceptable for a single-tenant hive whose owner controls every repo and agent,
but not a control you should silently disable on a shared one.

### Does your cluster grant NET_ADMIN?

The question every firewalled-cluster adopter hits first. Three checks, from
authoritative to empirical — run them **before** deploying and you'll know
which of the two remedies above (if either) you need.

**1. Ask OpenShift directly (`scc-subject-review` dry-run).** This is the
authoritative answer: it evaluates the *actual* pod spec against every SCC the
`hive` ServiceAccount can use, without creating anything:

```bash
# From your (placeholder-swapped) overlay directory — build the real manifests
# and let OpenShift judge them as the hive SA:
kubectl kustomize . > /tmp/hive-rendered.yaml
oc -n hive policy scc-subject-review -z hive -f /tmp/hive-rendered.yaml
```

Read the `ALLOWED BY` column for the `hive` Deployment: an SCC name (e.g.
`hive-netadmin`) means admission will pass under that SCC; `<none>` means **no
SCC admits the pod** — deploying now would leave the ReplicaSet stuck with
`unable to validate against any security context constraint` events and no pod
at all.

**2. Which SCCs allow the capability, and who can use them?**

```bash
# SCCs that would permit adding NET_ADMIN (allowedCapabilities contains it or "*"),
# plus privileged SCCs (which imply all capabilities):
oc get scc -o json | jq -r '.items[]
  | select(((.allowedCapabilities // []) | index("NET_ADMIN") or index("*"))
           or .allowPrivilegedContainer == true)
  | .metadata.name'

# For each candidate, is the hive SA allowed to use it?
oc adm policy who-can use scc/hive-netadmin
# Look for system:serviceaccount:hive:hive in the output.
```

On a stock cluster the first query returns only `privileged` / `node-exporter`
style SCCs the hive SA cannot (and should not) use — that is the "cluster
denies" case until `hive-netadmin` is applied. On plain Kubernetes (no SCC API)
the equivalent gate is [Pod Security admission](https://kubernetes.io/docs/concepts/security/pod-security-standards/):
check `kubectl get ns hive -o jsonpath='{.metadata.labels}'` — an
`pod-security.kubernetes.io/enforce: baseline` (or `restricted`) label denies
`NET_ADMIN`; the capability needs `privileged` enforcement (or no enforcement)
on the namespace.

**3. The 30-second probe pod (empirical).** Runs as the same ServiceAccount,
requests only `NET_ADMIN`, prints its own capability bounding set, and exits:

```bash
oc -n hive run netadmin-probe --restart=Never --image=busybox:1.36 \
  --overrides='{"spec":{"serviceAccountName":"hive","containers":[{"name":"netadmin-probe","image":"busybox:1.36","command":["sh","-c","grep CapBnd /proc/self/status"],"securityContext":{"capabilities":{"drop":["ALL"],"add":["NET_ADMIN"]}}}]}}'
oc -n hive logs netadmin-probe && oc -n hive delete pod netadmin-probe
```

Two outcomes: the `run` is **rejected at admission** (`unable to validate
against any security context constraint`) — the cluster denies the capability;
or the pod runs and prints `CapBnd: 0000000000001000` — bit 12 (`0x1000`,
`CAP_NET_ADMIN`) is set and the cluster grants it. (The probe requests *only*
`NET_ADMIN`; the real deployment also adds the su-exec capability set, so treat
check 1 as the authoritative pre-flight and this probe as the quick smoke
test.)

**Reading the failure after the fact — the exit-77 signature.** If the pod was
*admitted* but the container's bounding set still lacks `CAP_NET_ADMIN` (e.g.
an admission webhook stripped the capability, or a hand-edited manifest dropped
the request), the entrypoint refuses to start and exits with a **distinct code,
77** (sysexits.h `EX_NOPERM`), after logging:

```
[entrypoint] FATAL: refusing to start. Grant NET_ADMIN + install iptables, or set HIVE_PROXY_ADVISORY_OK=true to deliberately run in advisory mode.
[entrypoint] FATAL: CAP_NET_ADMIN is not in the container's capability bounding set — exiting 77 (EX_NOPERM) rather than 1.
```

```bash
kubectl -n hive get pod -l app.kubernetes.io/name=hive \
  -o jsonpath='{.items[0].status.containerStatuses[0].lastState.terminated.exitCode}'
```

`77` means precisely "grant the capability (or opt into advisory mode)"; any
*other* cause of the same FATAL (no `iptables` binary, netfilter lock
contention) exits `1`, since granting `NET_ADMIN` would not fix those. Full
rationale: [net-admin-requirement.md](net-admin-requirement.md).

### Firewall / egress (locked-down cluster)

A standalone hive needs **no hub egress**. The minimal outbound allowlist is:

- **Your repo's GitHub API host** — `api.github.com`, or your GHE instance's
  `<host>/api/v3`.
- **`ghcr.io`** (and its blob backend) — for the image pull.
- **Your inference endpoint** — if it's in-cluster (the shipped
  `src/deploy/inference` backend, or your own litellm/vllm Service) this is
  **cluster-internal traffic, not external egress** at all.

Hub hosts (`hub.kubestellar.io`) are **not** required. For the full egress matrix
(ports, hosts, when each is needed) see
[network-requirements.md](network-requirements.md); for the capability rationale
see [net-admin-requirement.md](net-admin-requirement.md).

### Agent backend

Standalone uses a **self-hosted** agent backend, **not** the subscription-CLI
backends (`copilot` / `claude`) the internal fleet's base ConfigMap uses. The
overlay sets each agent's `backend: litellm` (valid enum values:
`litellm` | `vllm` | `llm-d` | `watsonx`). The litellm connection config lives
under `governor:` (`LiteLLMConfig` on `Config.Governor`):

```yaml
governor:
  litellm:
    endpoint: http://vllm-svc.hive-inference.svc.cluster.local:8000  # your OpenAI-compatible proxy
    api_key_env: HIVE_LITELLM_API_KEY   # env var NAME holding the key (default)
    default_model: qwen2.5-0.5b-instruct
```

Swap `backend: vllm` to hit `HIVE_VLLM_ENDPOINT` directly. Hive does not
default this to an in-cluster Service; set it only when the target cluster can
resolve and reach that endpoint (hosted provisioning injects it from the
cluster `inference_endpoint` setting). Verify any key against
`src/pkg/config/config.go` before adding it.

---

## Path A — Automated provisioning (hub-reachable cluster)

The hub exposes `POST /api/saas/hives` (`handleCreateHive`). It generates the
hive ID, creates the namespace + RBAC + PVC + Service + Ingress + Deployment,
and writes the SaaS `meta.json` — all automatically.

### A.1 Call the API as the hub admin

`handleCreateHive` authenticates via the `hive_hub_user` cookie, whose value is
simply the username, validated against the SaaS user store. From **inside the
hub pod** (localhost), that is all you need:

```bash
HUB_POD=$(kubectl --context <hub-reachable-cluster> -n hive-hub get pods -l app=hive-hub \
  --no-headers | grep Running | awk '{print $1}' | head -1)

kubectl --context <hub-reachable-cluster> -n hive-hub exec "$HUB_POD" -- curl -s -X POST \
  -H "Cookie: hive_hub_user=<hub-admin-login>" \
  -H "Content-Type: application/json" \
  -d '{
        "org": "myorg",
        "repos": "myrepo",
        "primary_repo": "myrepo",
        "project_name": "My Hive",
        "acmm_level": 2,
        "cluster_id": "<hub-reachable-cluster>",
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
 "subdomain":"hosted-myorg-myrepo-ab12.hive.hivecommons.dev"}
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
| `cluster_id` | the hub-reachable cluster's ID (the `defaultClusterID`). |
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

## Path B — Manual provisioning (heartbeat-only cluster)

The hub cannot reach a heartbeat-only cluster, so every object is applied by hand
with `kubectl --context <heartbeat-only-cluster>`. The full set, in order:

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
CTX=<heartbeat-only-cluster>    # kubectl context for the target cluster
ID=hosted-myorg-myrepo          # the hive ID
NS=hive-hosted-$ID              # namespace is always hive-hosted-<id>
ROUTE_HOST=$ID.apps.<your-cluster-domain>
IMAGE=ghcr.io/hivecommons/hive:v4-latest   # what the hub provisioner stamps by default (saas_provision.go); use `stable` for production
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
live user-visible outage on the heartbeat-only pool — the hub minted links at
`<id>.hive.hivecommons.dev` while the spoke's Route served
`<id>.apps.<your-cluster-domain>`.

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
> bind `default`. Measured live: the hub-reachable cluster is 22× `default`; one
> heartbeat-only cluster is 2× `default` **and** 41× `hive-sa`; another spoke
> cluster is 5× `hive-sa`. Binding the wrong one fails silently — the pod runs,
> the lookup is denied, and you get the 503
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
      primary_repo: myrepo            # (available-<cluster>-<date>-<suffix>), which is what the hub
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
        - owner-github-login:owner    # a POOL PLACEHOLDER lists the pool owner: <hub-admin-login>:owner
      hub_proxied: false              # heartbeat-only cluster has no hub auth proxy → direct device-flow
    hub:
      enabled: true
      url: https://hive.hivecommons.dev
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

> **Gotcha — `hub_proxied` must be `false` on a heartbeat-only cluster.** A
> heartbeat-only cluster has no hub nginx auth proxy. If `hub_proxied` is `true`,
> dashboard sign-in enters an OAuth redirect loop. On the hub-reachable cluster
> it is the opposite — the hub proxy handles auth.

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
        # HIVE_*_KEY vars instead (the hub derives them per-hive; the exact
        # derivation lives in src/pkg/hub/hub_keys.go — heartbeat is HMAC-based
        # while session/SSO are now Ed25519 asymmetric seeds, so do NOT assume a
        # single HMAC(master, "hive-<domain>-v1") formula) and omit
        # HIVE_HUB_SECRET entirely.
        #
        # HUB-ONLY: this whole HIVE_HUB_SECRET / derived-key block is irrelevant
        # to a standalone (hub-less) hive — see "Standalone / self-hosted (no
        # hub)" at the top of this guide.
        - { name: HIVE_HUB_SECRET, value: "${HUB_SECRET}" }
        - { name: HIVE_HUB_URL,    value: "https://hive.hivecommons.dev" }
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
> heartbeat freshness and crash-looped healthy spokes on firewalled
> (heartbeat-only) clusters. Heartbeat freshness is reported by `/api/health/deep` under the
> `hub_heartbeat` check, and the hub greys the hive's dot on its own.
> Spokes deployed before this fix need a redeploy (or a probe patch) to stop
> restarting on hub outages.

> **Gotcha — some clusters enforce an `owner` label.** A `ValidatingAdmissionPolicy`
> on the heartbeat-only cluster requires an `owner` label on `PersistentVolumeClaim`, `ConfigMap`,
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
HUB_POD=$(kubectl --context <hub-reachable-cluster> -n hive-hub get pods -l app=hive-hub \
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
  "cluster_id": "<heartbeat-only-cluster>"
}
JSON

kubectl --context <hub-reachable-cluster> -n hive-hub exec "$HUB_POD" -- \
  sh -c "mkdir -p /data/saas/hives/${ID}"
kubectl --context <hub-reachable-cluster> -n hive-hub cp /tmp/meta.json \
  hive-hub/"$HUB_POD":/data/saas/hives/${ID}/meta.json
```

**Visibility rule.** `handleMyHives` shows a hive to a user when the user is its
`owner`, has a role on it, **or** is the hub admin — even if the
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

- **Heartbeat-only-cluster** placeholders serve **private** methods —
  provisioned manually (Path B) and left running at **`replicas: 1`**.
- **Hub-reachable-cluster** placeholders serve **public** methods — provisioned
  via the API (Path A) and left running at **`replicas: 1`**.

> **Gotcha — placeholder IDs are date + random suffix, NOT sequential integers.**
> The live pool uses `hosted-available-<cluster>-<YYMMDD>-<suffix>`, where
> `<suffix>` is a short unique token (4 lowercase base36 chars) — e.g.
> `hosted-available-<cluster>-260731-fikn`, `hosted-available-<cluster>-260806-1bo3`.
> **Do not** use sequential numbers like `hosted-available-<cluster>-01`: they
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
> kubectl --context <hub-reachable-cluster> -n hive-hub exec "$HUB_POD" -- ls /data/saas/hives/   # existing hive IDs
> kubectl --context <heartbeat-only-cluster> get ns | grep hive-hosted-hosted-available-<cluster>  # existing namespaces
> ```

Provision each placeholder as above at **`replicas: 1`**. The pod boots
immediately, lands on the `anyuid` SCC, and heartbeats to the hub as an
available slot ready to be claimed. Both pools stay at `replicas: 1`:

```bash
# manual (heartbeat-only cluster): apply the Deployment at replicas: 1 (it boots and heartbeats).
# If you applied it at 0, scale it up:
kubectl --context <heartbeat-only-cluster> -n "$NS" scale deploy/hive --replicas=1

# automated (hub-reachable cluster): the API already provisions at replicas=1 — leave it there.
```

> **Gotcha — do NOT scale placeholders to 0.** A `replicas: 0` slot is **not
> assignable**: the claim/assign path delivers the claimant's config to a
> **running, heartbeating** spoke, so a slot that isn't booted can never be
> claimed. Every assignable slot in the live pool runs at `replicas: 1`,
> `ready 1/1`. (Earlier guidance to "scale to 0" was incorrect.)

Give every placeholder a `meta.json` with **`owner: <hub-admin-login>`**
(admin-only visibility), **`status: "available"`**, and **`acmm_level: 2`**. Set
`org` to a **unique** value per slot — `available-<cluster>-<YYMMDD>-<suffix>`
mirroring the `id`'s date-suffix — with **empty** `repos: []` and
`primary_repo: ""` (they get their real values when the slot is claimed). The
`meta.json` also carries `github_host` when the placeholders target GitHub
Enterprise (`github.your-company.com`), not `github.com`:

```json
{
  "id": "hosted-available-<cluster>-260806-1bo3",
  "owner": "<hub-admin-login>",
  "project_name": "Available slot (private) 260806-1bo3",
  "org": "available-<cluster>-260806-1bo3",
  "repos": [],
  "primary_repo": "",
  "acmm_level": 2,
  "status": "available",
  "created_at": "",
  "subdomain": "hosted-available-<cluster>-260806-1bo3.apps.<your-cluster-domain>",
  "claim_delivered": false,
  "auto_upgrade": true,
  "is_public": false,
  "cluster_id": "<heartbeat-only-cluster>",
  "github_host": "github.your-company.com"
}
```

> **The `org` must be unique per slot** (mirror the `id`'s date-suffix) so each
> renders as a distinct **"Available slot (private) `<date>-<suffix>`"** row in
> the hub. A literal shared `org: "placeholder"` (with `repos: ["placeholder"]`)
> makes every slot render identically as "placeholder / placeholder" —
> indistinguishable — and is wrong.

### Verify a provisioned placeholder

A correct heartbeat-only-cluster placeholder runs at `replicas: 1` with its pod
up (`Running`, `ready`, `restartCount: 0`) on the `anyuid` SCC, its `hive-anyuid`
binding points at its **own** namespace, its PVC is `Bound`, its `livez` returns
200, and its hub `meta.json` exists. This set verified all ten placeholders in
the 2026-08-06 batch:

```bash
ID=hosted-available-<cluster>-<date>-<suffix>; NS=hive-hosted-$ID
kubectl --context <heartbeat-only-cluster> -n $NS get deploy hive -o jsonpath='{.spec.replicas}'          # must be 1
POD=$(kubectl --context <heartbeat-only-cluster> -n $NS get pod -l app=hive -o jsonpath='{.items[0].metadata.name}')
kubectl --context <heartbeat-only-cluster> -n $NS get pod $POD -o jsonpath='{.status.phase}'              # must be Running
kubectl --context <heartbeat-only-cluster> -n $NS get pod $POD -o jsonpath='{.metadata.annotations.openshift\.io/scc}'  # must be anyuid
kubectl --context <heartbeat-only-cluster> -n $NS get pod $POD -o jsonpath='{.status.containerStatuses[0].ready}'       # must be true
kubectl --context <heartbeat-only-cluster> -n $NS get pod $POD -o jsonpath='{.status.containerStatuses[0].restartCount}' # must be 0
kubectl --context <heartbeat-only-cluster> -n $NS exec $POD -c hive -- curl -s -o /dev/null -w '%{http_code}' localhost:3002/api/livez  # must be 200
kubectl --context <heartbeat-only-cluster> -n $NS get rolebinding hive-anyuid -o jsonpath='{.subjects[0].namespace}'  # must equal $NS
kubectl --context <heartbeat-only-cluster> -n $NS get pvc hive-data -o jsonpath='{.status.phase}'         # must be Bound
kubectl --context <hub-reachable-cluster> -n hive-hub exec $HUB_POD -- test -f /data/saas/hives/$ID/meta.json  # must exist
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
forever** — e.g. `hive-hosted-hosted-available-<cluster>-01-placeholder-bb95` — even
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
kubectl --context <hub-reachable-cluster> get ns \
  -l hive.kubestellar.io/hive-name=tradingasbuddies
```

Given a namespace, find the hive that owns it:

```bash
kubectl --context <hub-reachable-cluster> get ns hive-hosted-hosted-available-<cluster>-01-placeholder-bb95 \
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
kubectl --context <hub-reachable-cluster> -n hive-hub exec "$HUB_POD" -- \
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
| Dashboard sign-in redirect loop (heartbeat-only cluster) | `hub_proxied: true` on a cluster with no hub auth proxy | Set `hub_proxied: false` on the PVC overlay |
| Hive online but **Upgrade** → `hive not found` | No `meta.json` on the hub | Create `/data/saas/hives/<id>/meta.json` |
| Hive online, but **My Hives → Dashboard** returns a branded **503**; the link reads `<hive-id>.<hub-host>` instead of the spoke cluster's own domain | Missing `hive-route-reader` RBAC, so the spoke can't read its own Route/Ingress and the hub falls back to the hub-wildcard host, which has no backend for a hive on another cluster | Apply the `hive-route-reader` Role + RoleBinding, binding the SA the hive Deployment actually uses (see [B.2](#hive-route-reader--why-the-dashboard-link-503s-without-it)), then `rollout restart deploy/hive` |
| Not visible in My Hives | No `meta.json`, or `owner` doesn't match | Create/patch `meta.json` with the right `owner` |
| ConfigMap edits have no effect | PVC overlay is authoritative | Edit `/data/hive.yaml.dashboard`, not the ConfigMap |
| User has `read-write` in Manage Access but login fails with `device-flow login rejected: user not authorized` | Grant written to `/data/hive.yaml.dashboard` after the pod booted; the running authorizer only rebuilds `authorized_users` at startup (common right after provisioning / cross-cluster migration onto a fresh PVC) | Confirm the user is in the pod's `/etc/hive/hive.yaml`, then `rollout restart deploy/hive` so the authorizer rebuilds |
| Admission warnings on apply | Cluster requires an `owner` label | Add `owner: <login>` to every `metadata.labels` |
| Placeholder rows all render as "placeholder / placeholder" | `meta.json` used the literal `org: "placeholder"` | Give each slot a unique `org: available-<cluster>-<date>-<suffix>` with `repos: []` |
