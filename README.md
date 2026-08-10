# Hive

AI agent orchestration for open source projects. A single Go binary enumerates GitHub issues and PRs, classifies them by complexity, and dispatches work to AI agents (Claude, Copilot, Gemini, Goose) on adaptive cadences governed by queue depth.

Hive separates decisions into two layers: a **deterministic pipeline** of shell scripts handles filtering, classification, merge-gating, and enforcement before any LLM sees the work. Agents only handle judgment calls — reading code, reasoning about fixes, writing PRs.

## Quick Start (Docker Compose)

```bash
git clone -b v2 https://github.com/kubestellar/hive.git
cd hive/v2

cp hive.yaml.example hive.yaml
export HIVE_GITHUB_TOKEN=ghp_...
docker compose up -d
```

Dashboard at `http://localhost:3001`.

To build from source instead of pulling the pre-built image:

```bash
docker compose build
docker compose up -d
```

## Kubernetes Deployment

### Prerequisites

- `kubectl` configured for your cluster
- Kubernetes 1.24+
- A StorageClass that supports `ReadWriteMany` (NFS recommended for zero-downtime rollouts)
- cert-manager (for TLS certificates)
- nginx-ingress (for ingress routing)

### Hosted Option

The [Hive Hub](https://hive.kubestellar.io) provides hosted hives with OAuth-protected dashboards, a public registry, and cross-hive leaderboards. No cluster required.

If you need to run your own private hub instead, see the v2
[self-hosted hub deployment guide](v2/docs/hub-deployment.md).

### Self-Hosted Deployment

#### 1. Create the namespace

```bash
kubectl apply -f v2/deploy/k8s/namespace.yaml
```

Or manually:

```bash
kubectl create namespace hive
```

#### 2. Create secrets

```bash
kubectl -n hive create secret generic hive-secrets \
  --from-literal=HIVE_GITHUB_TOKEN=ghp_... \
  --from-literal=HIVE_DASHBOARD_TOKEN=your-dashboard-auth-token
```

For GitHub App auth (recommended for production), add the private key:

```bash
kubectl -n hive create secret generic hive-secrets \
  --from-literal=HIVE_GITHUB_TOKEN=ghp_... \
  --from-file=gh-app-key.pem=/path/to/key.pem
```

#### 3. Create ConfigMap from hive.yaml

```bash
cp v2/hive.yaml.example hive.yaml
# Edit hive.yaml: set your org, repos, agents, and governor config

kubectl create configmap hive-config -n hive --from-file=hive.yaml=hive.yaml
```

#### 4. Create PersistentVolumeClaim

Apply the provided PVC manifest:

```bash
kubectl apply -f v2/deploy/k8s/pvc.yaml
```

The default PVC requests 10Gi with `ReadWriteOnce`. For zero-downtime rollouts with rolling updates, use an NFS-backed StorageClass with `ReadWriteMany`:

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
kubectl apply -f v2/deploy/k8s/deployment.yaml
kubectl apply -f v2/deploy/k8s/service.yaml
```

The deployment runs a single replica with liveness and readiness probes on `/api/health`. Resource defaults: 500m CPU / 512Mi memory (requests), 2 CPU / 2Gi memory (limits).

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

#### Quick apply (all manifests)

```bash
kubectl apply -f v2/deploy/k8s/namespace.yaml
kubectl -n hive create secret generic hive-secrets \
  --from-literal=HIVE_GITHUB_TOKEN=ghp_...
kubectl create configmap hive-config -n hive --from-file=hive.yaml=hive.yaml
kubectl apply -f v2/deploy/k8s/pvc.yaml
kubectl apply -f v2/deploy/k8s/deployment.yaml
kubectl apply -f v2/deploy/k8s/service.yaml
```

### Ports

| Port | Purpose |
|------|---------|
| 3001 | Dashboard (supports auth token) |
| 3002 | Internal API |
| 7681 | ttyd web terminal |

### Volumes

| Mount Path | Purpose |
|------------|---------|
| `/etc/hive/hive.yaml` | Configuration (read-only, from ConfigMap) |
| `/data` | Persistent state: metrics, beads, logs |
| `/secrets` | GitHub App key and other secrets (read-only) |

## Configuration

All v2 runtime config lives in a single `hive.yaml`. Environment variables are interpolated with `${VAR}` syntax. See [v2/hive.yaml.example](v2/hive.yaml.example) for the full reference, [v2/docs/env-vars.md](v2/docs/env-vars.md) for the centralized environment variable reference, [v2/docs/agent-configuration.md](v2/docs/agent-configuration.md) for agent configuration, [v2/docs/supervisor.md](v2/docs/supervisor.md) for the supervisor agent, [docs/backend-setup.md](docs/backend-setup.md) for CLI backends, [docs/inference-backends.md](docs/inference-backends.md) for model gateways, and [docs/migration-v1-v2.md](docs/migration-v1-v2.md) for v1→v2 migration.

The top-level deterministic shell pipeline uses a separate project file,
`config/hive-project.yaml.example`; see [config/README.md](config/README.md)
before running the top-level `bin/` scripts directly.

```yaml
project:
  org: your-org
  repos:
    - repo-one
    - repo-two
  primary_repo: repo-one
  ai_author: your-bot-user

agents:
  scanner:
    enabled: true
    backend: claude
    model: claude-sonnet-4-6
    beads_dir: /data/beads/scanner
    clear_on_kick: true

governor:
  eval_interval_s: 300
  modes:
    surge:
      threshold: 20
      scanner: 15m
      reviewer: pause
    busy:
      threshold: 10
      scanner: 15m
      reviewer: 1h
    quiet:
      threshold: 2
      scanner: 15m
      reviewer: 45m
    idle:
      threshold: 0
      scanner: 15m
      reviewer: 15m

hub:
  enabled: true
  url: https://hive.kubestellar.io
  contribute:
    enabled: true
```

### GitHub Auth

Use a personal access token or a GitHub App:

```yaml
github:
  token: ${HIVE_GITHUB_TOKEN}
```

```yaml
github:
  app_id: 12345
  installation_id: 67890
  key_file: /secrets/gh-app-key.pem
```

## ACMM Levels

Hive uses an **AI-native Capability Maturity Model** (ACMM) with six levels that control what agents are allowed to do:

| Level | Name | Agents | What agents can do |
|-------|------|--------|-------------------|
| L1 | Inception (Assisted) | 2 | Interactive advisor and project inception. Advisory beads only. |
| L2 | Advisory (Instructed) | 5 | Observe and report findings as dashboard beads. No GitHub interaction. |
| L3 | Quality-Gated (Measured) | 6 | Quality agent opens issues and hold-gated PRs. Others remain advisory. |
| L4 | Security-Aware (Adaptive) | 7 | All agents file issues. Quality, sec-check, and CI open hold-gated PRs. |
| L5 | Semi-Autonomous (Semi-Automated) | 9 | All agents open hold-gated PRs. Humans batch-review and approve. |
| L6 | Fully Autonomous | 10 | Agents open PRs and auto-merge on green CI. No hold label required. |

Each level defines per-agent **policy modes**: advisory (observe only), measured (file issues), holdgated (PRs with hold label), or full (auto-merge). See `v2/docs/acmm-policy-matrix.md` for the full matrix. Browse the [v2 docs index](v2/docs/README.md) for operations, contributor relay, snapshots, health checks, and design guides.

Operational references from the repository root include [hub disaster recovery](docs/HUB_DISASTER_RECOVERY.md), [federation design](docs/federation-design.md), [outreach antispam policy](docs/outreach-antispam.md), [macOS deployment notes](docs/macos.md), and [backend setup](docs/backend-setup.md). Worked examples live under [examples/](examples/README.md), including [KubeStellar skill and campaign configs](examples/kubestellar/README.md), [SQLite state backend notes](examples/sqlite-state.md), and [ACMM runtime fragments](examples/acmm/README.md).

## Architecture

Hive runs as a single container with three long-lived processes:

- **Go binary** (`hive`, `:3002`) — the brain. Runs the governor eval loop, the agent manager (tmux sessions), the dashboard API, an in-process MITM GitHub proxy, the hub heartbeat, and token tracking — all as goroutines.
- **Node.js proxy** (`:3001`) — the public front door. Reverse-proxies to the Go API with auth and path-rewrite, and streams SSE/WebSocket to the dashboard and web terminal.
- **ttyd** (`:7681`) — web terminal onto the agent tmux sessions.

The governor evaluates queue depth on a configurable interval and switches between four modes (`SURGE`, `BUSY`, `QUIET`, `IDLE`), each with per-agent cadences. A deterministic pipeline (Go + shell) filters, classifies, and merge-gates all GitHub work before any agent is kicked, and three independent layers — CLI tool denial, least-privilege scoped tokens, and a network-level MITM proxy — enforce what each agent may do, keyed off its ACMM-assigned mode.

```mermaid
flowchart LR
    github["GitHub<br/>issues · PRs"] --> gov["Governor<br/>(queue depth → mode → kick)"]
    gov --> pipe["Deterministic pipeline<br/>classify · merge-gate · enforce"]
    pipe --> agents["AI agents (tmux)<br/>Claude · Copilot · Gemini · Goose"]
    agents --> guard["Guardrails<br/>tool deny · scoped token · MITM proxy"]
    guard -->|"gated writes"| github
    agents -.-> beads["Beads ledger<br/>(git-backed work items)"]
    gov -.->|"heartbeat"| hub["Hive Hub<br/>registry · leaderboard"]
    dash["Dashboard :3001"] -.->|"SSE"| gov
```

**See [v2/docs/architecture.md](v2/docs/architecture.md) for the full reference architecture** — process model, the governor loop, the deterministic pipeline, layered guardrails, ACMM, beads, hub & spoke, and an end-to-end walkthrough, with Mermaid diagrams throughout. Operator safety references include [trajectory review](v2/docs/trajectory-review.md), [dashboard health checks](v2/docs/health-checks.md), [sandbox guardrails](v2/docs/sandbox-isolation.md), [manual provisioning](v2/docs/manual-provisioning.md), [cross-cluster migration](v2/docs/cross-cluster-migration.md), and [config layering](v2/docs/config-layering.md). The dashboard API reference is published as [dashboard/openapi.json](dashboard/openapi.json).

## Contribute to a Hive

Community members can contribute compute to any hive through **ClankeR**, the
contributor relay — it hands tasks from a hive's backlog to the CLI agent
running on your own machine:

```bash
brew install just gh
git clone -b v2 https://github.com/kubestellar/hive && cd hive
just contribute-setup claude
just contribute-hive
```

Supported CLIs: Claude Code, GitHub Copilot, Pi, Goose, Bob. Contributors start as newcomer (rate-limited) and auto-promote based on completed tasks. Your credentials never leave your machine.

A relay can subscribe to multiple hives with comma-separated `HIVE_HUB` and matching `HIVE_REGISTRATION_TOKEN` values, and operators can delegate selected spoke roles through **Acting as** / `HIVE_AGENT_ROLE`. See [v2/docs/contributor-relay.md](v2/docs/contributor-relay.md) and [v2/docs/contributor-trust-and-roles.md](v2/docs/contributor-trust-and-roles.md).

See the [Hive Hub contribute page](https://hive.kubestellar.io) for details.

## Contributing

See the [Hive Hub](https://hive.kubestellar.io) to browse registered hives, view leaderboards, and find hives accepting contributions.

To contribute to Hive itself, see [CONTRIBUTING.md](CONTRIBUTING.md) and open issues or PRs on this repository.

## Security

Please see [SECURITY.md](SECURITY.md) for the vulnerability disclosure process. Do not report security vulnerabilities through public issues or pull requests.

---

Apache 2.0
