# Hive

AI agent orchestrator for GitHub repositories. A single Go binary that enumerates issues and PRs, classifies them by complexity, and dispatches work to AI agents (Claude, Copilot, Gemini, Goose) on adaptive cadences.

## Quick Start (Docker)

### Option A: Pre-built image (recommended)

```bash
git clone https://github.com/hivecommons/hive.git
cd hive

cp src/hive.yaml.example src/hive.yaml
# Edit src/hive.yaml — set your org, repos, and agent config

# src/.env, NOT ./.env — `-f src/docker-compose.yaml` makes `src/` the project
# directory, so that is where Compose reads `.env`. A root `.env` is ignored
# silently, and both paths are gitignored, so nothing warns you.
echo "HIVE_GITHUB_TOKEN=ghp_..." > src/.env
# REQUIRED: the dashboard's auth proxy refuses to start without it.
printf 'HIVE_DASHBOARD_TOKEN=%s\n' "$(openssl rand -hex 32)" >> src/.env

docker compose -f src/docker-compose.yaml up -d

# Dashboard at http://localhost:3001
curl -sf http://127.0.0.1:3001/api/health     # -> {"status":"ok"}
```

### Option B: Build from source

```bash
git clone https://github.com/hivecommons/hive.git
cd hive

cp src/hive.yaml.example src/hive.yaml
# Edit src/hive.yaml — set your org, repos, and agent config

# src/.env, NOT ./.env — `-f src/docker-compose.yaml` makes `src/` the project
# directory, so that is where Compose reads `.env`. A root `.env` is ignored
# silently, and both paths are gitignored, so nothing warns you.
echo "HIVE_GITHUB_TOKEN=ghp_..." > src/.env
# REQUIRED: the dashboard's auth proxy refuses to start without it.
printf 'HIVE_DASHBOARD_TOKEN=%s\n' "$(openssl rand -hex 32)" >> src/.env

docker compose -f src/docker-compose.yaml build
docker compose -f src/docker-compose.yaml up -d
```

### Pre-built image

The default `src/docker-compose.yaml` uses the pre-built image `ghcr.io/kubestellar/hive:stable`, the operator-blessed release channel. To build from source instead, run `docker compose -f src/docker-compose.yaml build` before `docker compose -f src/docker-compose.yaml up -d`.

Every standalone image reference comes from one source of truth, [`deploy/standalone-images.sh`](deploy/standalone-images.sh), so the Docker assets and the Podman assets cannot drift apart. Change a reference there, not in an individual asset.

For tag provenance, digest pinning, and the release/tagging flow, see [docs/operator-reference.md#image-provenance-and-tags](docs/operator-reference.md#image-provenance-and-tags).

### Troubleshooting

- **Rancher Desktop / Lima**: Volume mounts from `/tmp` may fail silently (file appears as directory inside container). Clone the repo under your home directory instead.
- **Gateway won't start**: The gateway depends on the hive health check (120s start period). If it times out, run the hive container directly: `docker run -d --name hive --cap-add NET_ADMIN -p 3001:3001 -v ./src/hive.yaml:/etc/hive/hive.yaml -e HIVE_GITHUB_TOKEN=$HIVE_GITHUB_TOKEN ghcr.io/kubestellar/hive:stable` (the `--cap-add NET_ADMIN` enables the full forced-proxy-egress gate — see below).
- **Forced-proxy-egress gate is degraded / `SO_MARK unavailable` in logs**: the container runs fine without `NET_ADMIN`, but the proxy's SO_MARK self-exemption needs it. Grant `NET_ADMIN` (`--cap-add NET_ADMIN` for docker/podman, `securityContext.capabilities.add: ["NET_ADMIN"]` for Kubernetes) for the full egress gate; without it the spoke stays in best-effort/degraded egress mode. See [docs/net-admin-requirement.md](docs/net-admin-requirement.md).
- **Policy clone errors in logs**: The example config has a placeholder policy repo. Comment out the `policies:` section in `src/hive.yaml` if you don't have a custom policy repo.

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

See [docs/hivectl.md](docs/hivectl.md) for the full command reference. The architecture overview also calls out `hivectl` as the non-interactive API client for automation. For raw dashboard endpoints, see [docs/api-reference.md](docs/api-reference.md).

See [docs/README.md](docs/README.md) for the full v2 documentation index, including [notifications](docs/notifications.md) and image provenance in the operator reference.

## Kubernetes

```bash
# Create the namespace, config, secret, and storage
kubectl apply -f deploy/k8s/namespace.yaml
kubectl -n hive create secret generic hive-secrets \
  --from-literal=HIVE_GITHUB_TOKEN=ghp_...
kubectl create configmap hive-config -n hive --from-file=hive.yaml=src/hive.yaml
kubectl apply -f deploy/k8s/pvc.yaml
kubectl apply -f deploy/k8s/deployment.yaml
kubectl apply -f deploy/k8s/service.yaml
kubectl apply -f deploy/k8s/dashboard-route-rbac.yaml
```

`dashboard-route-rbac.yaml` lets the spoke report `route_exists` in heartbeats.
See [docs/health-checks.md](docs/health-checks.md) for listener probes,
hub-fronted URL probing, and alert hysteresis.

## Configuration

All config lives in a single `hive.yaml`. Environment variables are interpolated with `${VAR}` syntax. See `src/hive.yaml.example` for the commented example, [docs/operator-reference.md](docs/operator-reference.md) for operator-only knobs, token scopes, and image provenance, [docs/config-layering.md](docs/config-layering.md) for runtime precedence/provenance, [docs/agent-configuration.md](docs/agent-configuration.md) for agent fields and ACMM packs, [AGENT-DEFINITION.md](AGENT-DEFINITION.md) for the portable agent YAML format, [../docs/backend-setup.md](../docs/backend-setup.md) for CLI backend setup, and [../docs/migration-v1-v2.md](../docs/migration-v1-v2.md) for migration from v1.

```yaml
project:
  org: your-org
  repos:
    - repo-one
    - repo-two
  primary_repo: repo-one
  ai_author: your-bot-user

ioscan:
  enabled: true        # default: true; scans untrusted text before agent kicks
  fail_mode: open      # open (default) redacts; closed blocks Critical injection kicks
  canaries: false      # default: false; plant per-kick canaries and audit/block leaks
```

### Custom stylesheets

The ClankeR leaderboard, the spoke dashboard, and the read-only `/snapshot`
preview accept a shareable sanitized CSS theme from a public GitHub repo:

```text
/contribute/leaderboard?style=owner/repo/path/to/theme.css@ref
/?style=owner/repo/path/to/theme.css@ref
/snapshot?style=owner/repo/path/to/theme.css@ref
```

See [docs/custom-stylesheets.md](docs/custom-stylesheets.md) for sanitizer
rules, `report=1`, scoping, and examples. See
[docs/snapshots.md](docs/snapshots.md) for the public snapshot page and
frame-ancestor sharing configuration.

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


For portable agent overlays and dashboard imports/exports, see [AGENT-DEFINITION.md](AGENT-DEFINITION.md).

## ClankeR contributor relay

ClankeR lets contributors lend their own AI CLI subscription to a hive through
`/contribute`. Contributors start as `newcomer`, auto-promote to `contributor`
after 5 PR-backed completions and `trusted` after 20, and may receive
maintainer-granted tiers such as `merger`. The merger tier can queue **other
people's** PRs for Hive's auto-merge-on-green sweep, never its own.

Relays can subscribe to multiple hubs by setting comma-separated `HIVE_HUB` and
matching `HIVE_REGISTRATION_TOKEN` lists. Operators can also allow contributors
to act as selected spoke roles with `HIVE_AGENT_ROLE` / **Acting as**,
profile-level grant chips, and `hub.contribute_delegatable_roles`.

See [docs/contributor-relay.md](docs/contributor-relay.md) and
[docs/contributor-trust-and-roles.md](docs/contributor-trust-and-roles.md).

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

## Contributor relay image

`Dockerfile.contributor` and `compose-contributor.yaml` build/run the ClankeR contributor image used by `just contribute-hive`. The image contains the relay scripts plus common CLIs; compose mounts your local contributor config read-only and selects `AGENT_BACKEND`. See [../docs/backend-setup.md](../docs/backend-setup.md#contributor-relay-image).

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

See [../docs/inference-backends.md](../docs/inference-backends.md) for setup and troubleshooting, and [deploy/inference/README.md](deploy/inference/README.md) for the sample in-cluster vLLM-compatible deployment.

## Knowledge

A 4-layer wiki system (powered by [llm-wiki](https://github.com/geronimo-iia/llm-wiki)) that primes agents with relevant facts on each kick. Layers are merged by precedence: personal > project > org > community. See the [knowledge system design](docs/design/knowledge-system.md) for curator, git-source indexing, and primer behavior.

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

Configure via the dashboard (Strategy Lab panel, ACMM level 4+) or the `/api/nous/*` endpoints directly — there is no `nous:` block in `hive.yaml.example`; Nous has no YAML config surface. See [docs/strategy-lab.md](docs/strategy-lab.md) for the experiment lifecycle, configuration options, and gate-decision flow. The dashboard REST API is described by [../dashboard/openapi.json](../dashboard/openapi.json).

## Further reading

- [documentation index](docs/README.md) — all operator, contributor, and design docs.
- [Cross-cluster migration](docs/cross-cluster-migration.md) — move a hive between clusters.
- [Manual provisioning](docs/manual-provisioning.md) — hosted/spoke provisioning and hub access.
- [Config layering](docs/config-layering.md) — ConfigMap vs PVC overlay precedence.
- [Network requirements](docs/network-requirements.md) and [TLS setup](docs/tls-setup.md) — firewall, port, and HTTPS guidance.
- [Token tracking](docs/token-tracking.md) and [security notes](docs/security.md) — cost/usage rollups and log redaction.
- [GitHub App setup](docs/github-app-setup.md) — production App auth and `/gh-setup`.
- [Knowledge curator](docs/knowledge-curator.md) — automatic fact extraction and promotion settings.
- [Trajectory review](docs/trajectory-review.md) — trajectory safety lane.
- [Credly badges](docs/credly-badges.md) — planned badge integration placeholder.

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
the hive verifies and saves the installation ID automatically. See
[GitHub App setup](docs/github-app-setup.md) for permissions, key handling, and
the callback flow.

## Deployment Examples

The `deploy/` directory contains pre-built configurations for common deployment patterns. Copy the one that matches your use case to `hive.yaml` and adjust the `github:` section. For Kubernetes or OpenShift installs that need more than the quick-start manifests, follow [manual provisioning](docs/manual-provisioning.md).

| File | Use Case |
|------|----------|
| `deploy/hive-quickstart.yaml` | Quick-start evaluation roster targeting `kubestellar/hive-tester` with all ten built-in agent lanes enabled under `acmm_level: 1` |
| `deploy/architect-only.yaml` | Single `architect` agent managing KubeStellar Console repos — Docker on Proxmox LXC, ntfy notifications |
| `deploy/hive-level2-knuckle.yaml` | ACMM L2 advisory config for `projectbluefin/knuckle` — agents observe and report, no issues or PRs |
| `deploy/hive-level3-knuckle.yaml` | ACMM L3 measured config for `projectbluefin/knuckle` — quality uses the measured policy for testing issues, other agents stay advisory |

## Additional references

- [Operator reference](docs/operator-reference.md) — config blocks, server flags/env, GitHub token scopes.
- [Troubleshooting](docs/troubleshooting.md) — container logs, config validation, agent sessions, dashboard auth, and GitHub credential checks.
- [Config layering](docs/config-layering.md) — effective config provenance and precedence.
- [Dashboard API reference](docs/api-reference.md) — generated route index.
- [apiproxy](docs/apiproxy.md) — model API proxy purpose and deployment notes.

## Build from Source

```bash
go build -o hive ./cmd/hive
./hive --config ./hive.yaml
```
