# Hive

AI agent orchestrator for GitHub repositories. A single Go binary that enumerates issues and PRs, classifies them by complexity, and dispatches work to AI agents (Claude, Copilot, Gemini, Goose) on adaptive cadences.

## Quick Start (Docker)

### Option A: Pre-built image (recommended)

```bash
git clone -b v2 https://github.com/kubestellar/hive.git
cd hive/v2

cp hive.yaml.example hive.yaml
# Edit hive.yaml — set your org, repos, and agent config

export HIVE_GITHUB_TOKEN=ghp_...
docker compose up -d

# Dashboard at http://localhost:3001
```

### Option B: Build from source

```bash
git clone -b v2 https://github.com/kubestellar/hive.git
cd hive/v2

cp hive.yaml.example hive.yaml
# Edit hive.yaml — set your org, repos, and agent config

export HIVE_GITHUB_TOKEN=ghp_...
docker compose build
docker compose up -d
```

### Pre-built image

The default `docker-compose.yaml` uses the pre-built image `ghcr.io/kubestellar/hive:v2-latest`. To build from source instead, run `docker compose build` before `docker compose up -d`.

### Troubleshooting

- **Rancher Desktop / Lima**: Volume mounts from `/tmp` may fail silently (file appears as directory inside container). Clone the repo under your home directory instead.
- **Gateway won't start**: The gateway depends on the hive health check (120s start period). If it times out, run the hive container directly: `docker run -d --name hive -p 3001:3001 -v ./hive.yaml:/etc/hive/hive.yaml -e HIVE_GITHUB_TOKEN=$HIVE_GITHUB_TOKEN ghcr.io/kubestellar/hive:v2-latest`
- **Policy clone errors in logs**: The example config has a placeholder policy repo. Comment out the `policies:` section in `hive.yaml` if you don't have a custom policy repo.

## Command-line client

`hivectl` is the non-interactive client for a running Hive dashboard API. Build
it from this repository:

```bash
mkdir -p bin
go build -o bin/hivectl ./cmd/hivectl

bin/hivectl system status
bin/hivectl agent list
bin/hivectl knowledge search "user journey"
```

When dashboard authentication is enabled, `hivectl` reads
`HIVE_DASHBOARD_TOKEN` by default. It supports JSON, YAML, table, and JSONL
output, stdin-based imports, and explicit confirmation for destructive
operations.

See [docs/hivectl.md](docs/hivectl.md) for the full command reference.

## Kubernetes

```bash
# Create the namespace, config, secret, and storage
kubectl apply -f deploy/k8s/namespace.yaml
kubectl -n hive create secret generic hive-secrets \
  --from-literal=HIVE_GITHUB_TOKEN=ghp_...
kubectl create configmap hive-config -n hive --from-file=hive.yaml=hive.yaml
kubectl apply -f deploy/k8s/pvc.yaml
kubectl apply -f deploy/k8s/deployment.yaml
kubectl apply -f deploy/k8s/service.yaml
```

## Configuration

All config lives in a single `hive.yaml`. Environment variables are interpolated with `${VAR}` syntax. See `hive.yaml.example` for the full reference.

```yaml
project:
  org: your-org
  repos:
    - repo-one
    - repo-two
  primary_repo: repo-one
  ai_author: your-bot-user
```

### Custom stylesheets

The ClankeR leaderboard, the spoke dashboard, and the read-only `/snapshot`
preview accept a shareable custom stylesheet parameter:

`/contribute/leaderboard?style=owner/repo/path/to/theme.css@ref`
`/?style=owner/repo/path/to/theme.css@ref`
`/snapshot?style=owner/repo/path/to/theme.css@ref`

The `@ref` suffix is optional and defaults to the repo's `HEAD`. Hive only
accepts the `owner/repo/path.css` triplet form, fetches public GitHub raw content
server-side without credentials, sanitizes CSS, strips external imports/URLs, and
serves it from same-origin endpoints with a 128 KiB size cap. The sanitizer keeps
normal declarations including custom properties (`--x`/`var(--x)`), attribute and
pseudo selectors, gradients, `calc()`/`clamp()`/modern color functions, and the
recursive at-rules `@media`, `@supports`, `@container`, and `@keyframes`.
`@font-face` blocks are kept only when every `src` URL is same-origin/relative or
`data:`. It removes `@import`, external or protocol-relative `url()` fetches,
CSS escape sequences, `image-set()`, and legacy executable CSS vectors such as
`expression()`, `behavior`, and `-moz-binding`.

Sanitized style responses include `X-Hive-Style-Dropped: N` when anything was
removed. Add `&report=1` to `/api/style` or `/api/leaderboard/style` to get JSON
with the sanitized CSS and a short list of sanitizer reasons. Leaderboard CSS is
scoped to `#tab-leaderboard`; dashboard and snapshot CSS is scoped to
`#hive-dashboard-root`, which deliberately leaves login/setup overlays outside
the custom-theme surface. `/api/style` is public so unauthenticated snapshots can
load sanitized CSS, and the existing `style-src 'self'` CSP remains sufficient.

## Agents

Seven agents ship as defaults (scanner, ci-maintainer, quality, architect, supervisor, outreach, sec-check). Enable or disable each in config:

```yaml
agents:
  scanner:
    enabled: true
    backend: claude        # claude | copilot | gemini | goose | vllm | llm-d | litellm
    model: claude-sonnet-4-6
    caveman_mode: full     # lite | full | ultra | wenyan — compresses output ~65%
    beads_dir: /data/beads/scanner
    clear_on_kick: true
```

### Adding a Custom Agent

Add a block under `agents:` and a cadence entry under each governor mode:

```yaml
agents:
  my-agent:
    enabled: true
    backend: claude
    model: claude-sonnet-4-6
    beads_dir: /data/beads/my-agent
    clear_on_kick: true

governor:
  modes:
    surge:
      my-agent: pause
    busy:
      my-agent: 1h
    quiet:
      my-agent: 30m
    idle:
      my-agent: 15m
```

Place a policy file in the agent's working directory, or use a git config repo for hot-reloadable policies:

```yaml
policies:
  repo: https://github.com/your-org/hive-config
  path: agents/
  poll_interval: 5m
```

## Governor

The governor evaluates queue depth every `eval_interval_s` seconds and switches between four modes — **SURGE**, **BUSY**, **QUIET**, **IDLE** — each with its own cadences per agent. Agents can be paused in any mode by setting their cadence to `pause`.

```yaml
governor:
  eval_interval_s: 300
  modes:
    surge:
      threshold: 20    # queue depth >= 20
    busy:
      threshold: 10
    quiet:
      threshold: 2
    idle:
      threshold: 0
```

## Inference Backends

Besides CLI backends (claude, copilot, …), agents can run against
self-hosted, OpenAI-compatible inference backends: **vllm**, **llm-d**, and
**litellm**. The agent still launches the Claude CLI (bare mode); the hive's
inference translator converts Anthropic Messages API calls to OpenAI format
and forwards them to the backend. The MITM proxy stays in the path, so mode
enforcement, logging, and per-agent attribution are unchanged.

Endpoints for vllm/llm-d come from `HIVE_VLLM_ENDPOINT` /
`HIVE_LLMD_ENDPOINT` (in-cluster defaults exist). LiteLLM is configured
under `governor.litellm` — via the dashboard's Governor Config → LiteLLM
tab, or in YAML:

```yaml
governor:
  litellm:
    endpoint: https://litellm.example.com   # HIVE_LITELLM_ENDPOINT overrides
    api_key_env: HIVE_LITELLM_API_KEY       # env var NAME — never the key value
    api_key_file: /secrets/litellm_api_key  # or a mounted key file (wins over env)
    default_model: gpt-4o
    ca_bundle: /secrets/litellm-ca.pem      # optional PEM for a private CA
    local_proxy: false                      # run the bundled litellm binary locally
```

The API key is resolved at use time (file → named env var →
`HIVE_LITELLM_API_KEY`) and is never written to `hive.yaml` or exposed via
the API. Model discovery uses the backend's `/v1/models` (with bearer auth
for litellm); `HIVE_LITELLM_MODELS` is a comma-separated fallback list.
With `local_proxy: true`, hive supervises a bundled `litellm` process on
loopback (config at `/data/litellm/config.yaml`) and routes through it
instead of the remote endpoint — agents never bypass the Go translator.

## Knowledge

A 4-layer wiki system (powered by [llm-wiki](https://github.com/geronimo-iia/llm-wiki)) that primes agents with relevant facts on each kick. Layers are merged by precedence: personal > project > org > community.

```yaml
knowledge:
  enabled: true
  engine: llm-wiki
  layers:
    - type: personal
      path: ~/.hive/wiki
    - type: project
      path: .hive/wiki
  primer:
    max_facts: 25
    merge_strategy: precedence
```

## Strategy Lab (Nous)

An experiment framework that lets you test configuration changes (models, cadences, thresholds) against live data before committing them. Experiments run in a sandbox with rollback on failure.

Configure via the dashboard or the `/api/nous/*` endpoints. See `hive.yaml.example` for available options.

## Ports and Volumes

| Port | Purpose |
|------|---------|
| 3001 | Dashboard (public, auth) |
| 3002 | Internal API |
| 7681 | ttyd terminal |

| Volume | Purpose |
|--------|---------|
| `/etc/hive/hive.yaml` | Config (read-only) |
| `/data` | Metrics, beads, logs, state |
| `/secrets` | GitHub App key (if using App auth) |

## GitHub Auth

Use a personal access token (simplest) or a GitHub App (recommended for production):

```yaml
github:
  token: ${HIVE_GITHUB_TOKEN}
  # Or GitHub App:
  # app_id: 12345
  # installation_id: 67890
  # key_file: /secrets/gh-app-key.pem
```

For GitHub App setup, configure the app's **Setup URL** to
`https://<hive-host>/gh-setup` and enable **Redirect on update**. After an
operator installs the app from the dashboard banner, GitHub redirects back and
the hive verifies and saves the installation ID automatically.

## Deployment Examples

The `deploy/` directory contains pre-built configurations for common deployment patterns. Copy the one that matches your use case to `hive.yaml` and adjust the `github:` section.

| File | Use Case |
|------|----------|
| `deploy/hive-quickstart.yaml` | Full ACMM L1 roster (10 agents) targeting `kubestellar/hive-tester` — safe for evaluation and level testing |
| `deploy/architect-only.yaml` | Single `architect` agent managing KubeStellar Console repos — Docker on Proxmox LXC, ntfy notifications |
| `deploy/hive-level2-knuckle.yaml` | ACMM L2 advisory config for `projectbluefin/knuckle` — agents observe and report, no issues or PRs |
| `deploy/hive-level3-knuckle.yaml` | ACMM L3 measured config for `projectbluefin/knuckle` — quality opens testing issues and hold-gated PRs, other agents stay advisory |

## Build from Source

```bash
go build -o hive ./cmd/hive
./hive --config ./hive.yaml
```
