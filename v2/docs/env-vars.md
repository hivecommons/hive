# Environment variable reference

This reference is generated from the v2 source, deployment manifests, and the top-level helper scripts. The code is authoritative: `hive.yaml` may also expand arbitrary `${NAME}` placeholders through the config resolver, but only the variables below have built-in behavior.

## Core `hive` runtime

| Variable | Required | Default | Purpose |
|---|---:|---|---|
| `HIVE_CONFIG` | No | `/etc/hive/hive.yaml` | Default config path used before the `--config` flag is parsed. The dashboard also uses it when reporting config provenance. |
| `HIVE_MODE` | No | spoke/dashboard mode | Set to `hub` to run the hub server instead of the spoke dashboard. |
| `HIVE_HUB_PORT` | No | `3001` | Hub listen port when `HIVE_MODE=hub`. |
| `HIVE_SINGLETON_LOCK` | No | `/var/run/hive-metrics/hive.singleton.lock` when available, otherwise OS temp dir | Overrides the process singleton lock path. Set exactly `off` only for local development where duplicate processes are intentional. |
| `HIVE_GITHUB_TOKEN` | Required unless GitHub App auth is configured | none | Main PAT fallback for `github.token`; also used by fleet/stat fallback paths and some deployment manifests. |
| `GH_APP_KEY_FILE` | No | configured `github.key_file`, then `/data/gh-app-key.pem` or `/secrets/gh-app-key.pem` in provisioned paths | GitHub App private-key file fallback. |
| `DASHBOARD_AUTH_TOKEN` | No | none | Kubernetes/provisioned secret name for the dashboard shared token; used before `HIVE_DASHBOARD_TOKEN` when `dashboard.auth_token` is empty. |
| `HIVE_DASHBOARD_TOKEN` | No | none | Dashboard/API shared-token fallback and default `hivectl --token-env` variable. |
| `HIVE_AUTHORIZED_USERS` | No | none | Comma-separated direct-route dashboard allowlist, with optional `user:role` entries. Used when `dashboard.authorized_users` is empty. |
| `HIVE_REPO` | No | none | Bootstrap shortcut in `owner/repo` form; fills `project.org`, `project.repos`, and `project.primary_repo` if missing. |
| `HIVE_LEVEL` | No | config/pack value | ACMM level bootstrap/override used by hosted flows and the entrypoint pack selection. |
| `HIVE_ID` | No | config or generated id | Stable hive/spoke identifier override; passed through to launched agents. |
| `HIVE_CLUSTER_ID` | No | config or hub-provisioned value | Hosted cluster identifier override. |
| `HIVE_HUB_URL` | No | `hub.url` from config | Hub URL override for spoke heartbeats/registration. |
| `HIVE_HUB_SECRET` | Required for spokes registered to a protected hub; optional for a standalone hub with `/data/saas/hub-secret.key` | `/data/saas/hub-secret.key` on the hub when present; no fallback for spoke heartbeat auth | Bearer secret for spoke heartbeats and hub/spoke SaaS APIs. |
| `HIVE_COVERAGE_BADGE_URL` | No | none | Optional coverage badge URL exposed in dashboard status. |
| `HIVE_WORK_DIR` | No | `/data/agents` | Agent manager working directory. |
| `HIVE_SHA` | No | build SHA | Passed to launched agents and used in hub upgrade/status paths. |
| `HIVE_ADVISORY_ISSUE` | No | none | Passed to launched agents so advisory findings can target a configured issue. |
| `HIVE_TTYD_PORT` | No | `7681` | Web terminal port used by the entrypoint and terminal proxy. |
| `HIVE_METRICS_ENABLED` | No | disabled | Registers the Prometheus `/metrics` endpoint when set to `1`, `true`, `yes`, or `on`. The endpoint is open unless `HIVE_METRICS_TOKEN` is also set. |
| `HIVE_METRICS_TOKEN` | No | none | Optional bearer token guarding `/metrics` (`pkg/dashboard/metrics_prometheus.go`). When set, scrapers must send `Authorization: Bearer <token>` (Prometheus `bearer_token`); when empty, `/metrics` stays open for backward compatibility. Set it so the estimated cost/agent series are not readable by anyone on the pod network. |
| `HIVE_METRICS_FILE` | No | `/var/run/hive-metrics/contribute.json` | Contributor metrics JSON file override. |
| `HIVE_ALLOW_PRIVATE_GIT_SOURCE` | No | `false` | Exact `true` opts in to knowledge Git sources whose host resolves to a private/internal address (`pkg/knowledge/gitsource.go`), e.g. an in-cluster GitLab. Default is fail-closed as SSRF protection, mirroring the document-import guard. |
| `HIVE_COPILOT_INTEGRATION_ID` | No | compiled Copilot integration id | Overrides the integration id used by Copilot model discovery. |
| `HIVE_PROXY_PROOF_REQUIRED` | No | `false` | Requires the internal proxy proof header when set to `true`. |
| `HIVE_CONTRIBUTORS_DIR` | No | hub default | Contributor registry directory override. |
| `HIVE_FEDERATION_REGISTRY_PATH` | No | `/data/federation/registry.json` | Federation registry path override. |
| `HIVE_WEBHOOK_SECRET` | No | none | HMAC secret for the spoke `/webhook` channel. |
| `GITHUB_WEBHOOK_SECRET` | No | `/data/saas/webhook-secret.key` when present | Hub GitHub webhook HMAC secret. |

## Deployment entrypoint and proxy knobs

| Variable | Required | Default | Purpose |
|---|---:|---|---|
| `HIVE_API_PORT` | No | `3002` | Internal Go API port used by `v2/deploy/entrypoint.sh`. |
| `HIVE_PROXY_PORT` | No | `3001` | Node reverse-proxy/front-door port used by `v2/deploy/entrypoint.sh`. |
| `HIVE_STATIC_DIR` | No | `/opt/hive/proxy/public` | Static asset directory for the Node proxy. |
| `HIVE_PROXY_EGRESS_MARK` | No | `0x1112` | Packet mark exempted from the MITM egress redirect. |
| `HIVE_WIKI_GIT_URL` | No | none | Optional wiki vault URL cloned into `/data/vaults/hive-wiki` on first boot. |

## Inference, CLI backends, and agents

| Variable | Required | Default | Purpose |
|---|---:|---|---|
| `HIVE_VLLM_ENDPOINT` | No | `http://hive-vllm-svc.hive-inference.svc.cluster.local:8000` in code; `v2/deploy/k8s/deployment.yaml` sets `http://vllm-svc.hive-inference.svc.cluster.local:8000` | Comma-separated vLLM endpoint list. |
| `HIVE_LLMD_ENDPOINT` | No | `http://hive-llm-d-epp.hive-inference.svc.cluster.local:8000` in code; `v2/deploy/k8s/deployment.yaml` sets `http://llm-d-epp.hive-inference.svc.cluster.local:8000` | Comma-separated llm-d endpoint list. |
| `HIVE_LITELLM_ENDPOINT` | No | YAML `governor.litellm.endpoint`; unset means LiteLLM is unregistered unless `local_proxy` is true | Runtime LiteLLM base URL override. |
| `HIVE_VLLM_API_KEY` | No | none | Default bearer token for vLLM model discovery when no backend-specific `api_key_env` or file resolves. |
| `HIVE_LLMD_API_KEY` | No | none | Default bearer token for llm-d model discovery when no backend-specific `api_key_env` or file resolves. |
| `HIVE_LITELLM_API_KEY` | No | `/secrets/litellm_api_key` or `/data/secrets/litellm_api_key` may be used first | Default LiteLLM API key environment variable. |
| `HIVE_VLLM_MODELS` | No | static fallback aliases | Comma-separated vLLM model IDs used when discovery returns none. |
| `HIVE_LLMD_MODELS` | No | static fallback aliases | Comma-separated llm-d model IDs used when discovery returns none. |
| `HIVE_LITELLM_MODELS` | No | static fallback aliases | Comma-separated LiteLLM model IDs used when discovery returns none. |
| `HIVE_BOB_API_KEY` | Required only for Bob agents in pods unless a key file is mounted or saved on `/data` | `/secrets/bob_api_key` or `/data/secrets/bob_api_key` may be used first | Hive-side Bob API key source. The value is injected into Bob as `BOBSHELL_API_KEY`. |
| `HIVE_BOB_API_URL` | No | `https://api.us-east.bob.ibm.com` | Bob key-test endpoint base URL override. |
| `BOBSHELL_API_KEY` | Required by Bob CLI when Hive injects or contributor mode uses Bob | none | API key name read by bobshell itself. |
| `COPILOT_GITHUB_TOKEN` | No | dashboard device-flow token file, if present | Copilot completion/model-discovery token and explicit agent injection. |
| `CODEX_API_KEY` | No | none | API key read by agent credential probing (`pkg/agent/authprobe.go`) for the Codex CLI backend; either this or `OPENAI_API_KEY` makes Codex API-key mode count as configured. |
| `OPENAI_API_KEY` | No | none | OpenAI-compatible API key consulted by `pkg/agent/authprobe.go` for Codex API-key mode, alongside `CODEX_HOME/auth.json` entries stored under the same key. |
| `ANTHROPIC_API_KEY` | Required by `cmd/apiproxy` unless `PROXY_AUTH_TOKEN` is set; agent inference backends receive a synthetic value | none | Anthropic-compatible API key for apiproxy auth or CLI compatibility. |
| `PROXY_AUTH_TOKEN` | No | `ANTHROPIC_API_KEY` | Preferred auth token for `cmd/apiproxy`. |
| `CONTEXT7_API_KEY` | No | none | Optional key for Context7 knowledge API integration. |
| `GOOSE_PROVIDER` | No | Goose CLI default | Provider passed through Goose backend/model resolution. |
| `GOOSE_MODEL` | No | Goose CLI default | Model passed through Goose backend/model resolution and contributor relay fallback. |
| `BD_DIR` | No | current directory | `bd` beads CLI data directory. |
| `BD_DASHBOARD_URL` | No | none | Dashboard URL used by `bd kb` integration. |
| `HIVE_CONN_<NAME>_URL` | No | generated from agent connection config | Agent API connection URI variable when a connection omits `env_name`; `<NAME>` is the uppercased connection name with `-` replaced by `_`. |
| Custom connection auth env vars | No | none | If an agent API connection uses `auth.type: env`, Hive reads `auth.env_var` and injects that exact variable into the agent. |

## Hub, SaaS, alerts, and backups

| Variable | Required | Default | Purpose |
|---|---:|---|---|
| `HIVE_HUB_OAUTH_CLIENT_ID` | Required to enable hub OAuth | none | Enables hub `/login` and OAuth callback routes. |
| `HIVE_HUB_OAUTH_CLIENT_SECRET` | Required when hub OAuth is enabled | none | OAuth client secret used during callback token exchange. |
| `HIVE_HUB_SLACK_BOT_TOKEN` | No | none | Slack bot token for hub Slack notifications. |
| `HIVE_NTFY_SERVER` | No | none | ntfy server for hub auth-audit alerts. |
| `HIVE_NTFY_TOPIC` | No | none | ntfy topic for hub auth-audit alerts. |
| `HIVE_BACKUP_KEY` | Yes for hub DR backups and browser spoke backups | none | 64-character hex or base64 AES-256 key. Unset makes backup creation fail. |
| `HIVE_BACKUP_BUCKET` | Required for OCI upload mode | none | OCI Object Storage bucket for hub backup archives. |
| `HIVE_BACKUP_DATA_DIR` | No | `/data` | Hub data directory used by `hive-backup`. |
| `HIVE_BACKUP_RETENTION` | No | `30` | Number of backup archives to retain. |
| `HIVE_BACKUP_OCI_ENDPOINT` | No | OCI SDK default endpoint | OCI Object Storage endpoint override. |
| `HIVE_HUB_NAMESPACE` | No | current/default namespace | Hub namespace override for backup collection. |
| `HIVE_KUBECONFIG_DIR` | No | `/etc/hive/kubeconfigs` | Directory containing per-cluster kubeconfigs for remote spoke backup collection. |
| `HIVE_SPOKE_BACKUP_DATA_DIR` | No | `/data` | Spoke data directory for on-demand spoke backup. |
| `OCI_TENANCY_OCID` | Required for OCI FSS/Object Storage flows | none | OCI tenancy OCID. |
| `OCI_USER_OCID` | Required for OCI FSS/Object Storage flows | none | OCI user OCID. |
| `OCI_FINGERPRINT` | Required for OCI FSS/Object Storage flows | none | OCI API key fingerprint. |
| `OCI_PRIVATE_KEY` | Required for OCI FSS/Object Storage flows | none | OCI API private key PEM content. |
| `OCI_REGION` | Required for Object Storage backup | none | OCI Object Storage region. |
| `OCI_FSS_REGION` | Required for OCI FSS provisioning unless region is otherwise configured | none | OCI File Storage region. |
| `OCI_COMPARTMENT_ID` | Required for OCI FSS provisioning | none | OCI compartment OCID. |
| `OCI_AVAILABILITY_DOMAIN` | Required for OCI FSS provisioning | none | OCI availability domain. |
| `OCI_MOUNT_TARGET_ID` | Required for OCI FSS provisioning | none | OCI mount target OCID. |
| `OCI_EXPORT_SET_ID` | Required for OCI FSS provisioning | none | OCI export set OCID. |

## Kubernetes downward API and platform variables

| Variable | Required | Default | Purpose |
|---|---:|---|---|
| `POD_NAMESPACE` | No | `NAMESPACE`, then `default` | Preferred downward-API namespace value for dashboard/spoke identity. |
| `NAMESPACE` | No | `default` | Fallback namespace when `POD_NAMESPACE` is unset. |
| `KUBERNETES_SERVICE_HOST` | Set by Kubernetes | none | Used with `KUBERNETES_SERVICE_PORT` to detect in-cluster execution and build API URLs. |
| `KUBERNETES_SERVICE_PORT` | Set by Kubernetes | none | Kubernetes API service port. |
| `HOME` | Set by OS/container | none | Used to locate CLI credentials in several backend probes. |
| `PATH` | Set by OS/container | none | Used by helper tests and inherited by subprocesses. |

## Contributor relay and top-level helper scripts

| Variable | Required | Default | Purpose |
|---|---:|---|---|
| `HIVE_HUB` | Required after registration for contributor relay; `just` can discover/set it | `wss://hive.kubestellar.io/contribute` in `Justfile`; `wss://hive.kubestellar.io:3001/contribute` in compose/relay defaults | Contributor WebSocket hub URL. Comma-separated values are supported with matching `HIVE_REGISTRATION_TOKEN` entries. |
| `HIVE_REGISTRATION_TOKEN` | Yes for contributor relay | none | Contributor registration token. Comma-separated values match `HIVE_HUB` by position. |
| `AGENT_BACKEND` | No | `claude` | Contributor/agent CLI backend selector. |
| `AGENT_MODEL` | No | backend default, or `GOOSE_MODEL` for Goose fallback | Contributor/agent model override. |
| `CONTRIBUTOR_MODE` | No | `interactive` | Contributor relay mode: `interactive` uses tmux; `headless` uses one-shot CLI execution for supported backends. |
| `HIVE_HEADLESS_STATUS_FILE` | No | `/tmp/contributor-headless-status.json` | Status file written by headless contributor relay. |
| `HIVE_CONTRIBUTOR_IMAGE` | No | `ghcr.io/kubestellar/hive-contributor:latest` | Image used by `just contribute-hive`. |
| `HIVE_CONTAINER_RUNTIME` | No | autodetect `docker` or `podman` | Container runtime override for contributor helpers. |
| `HIVE_SKIP_VERSION_CHECK` | No | `false` | Skips `just` version freshness check when set to `true`. |
| `HIVE_SKIP_PULL` | No | `false` | Skips contributor image pull when set to `true`. |
| `HIVE_KEEP_CONTAINER` | No | remove failed contributor container | Keeps failed contributor containers for debugging when set to `true`. |
| `HIVE_PROJECT_CONFIG` | No | `/etc/hive/hive-project.yaml` | Path read by `bin/hive-config.sh` for deterministic pipeline/project metadata. |
| `HIVE_PROJECT_YAML` | No | `/etc/hive/hive-project.yaml`, then first example found | Path read directly by pipeline stages. |
| `HIVE_RUNTIME_CONFIG` | No | `/etc/hive/hive-runtime.yaml` | Runtime overlay read by `bin/hive-config.sh`. |
| `HIVE_REPO_DIR` | No | `/tmp/hive` | Hive checkout path used by top-level deployment scripts. Must not be empty or `/`. |
| `HIVE_BIN` | No | `/usr/local/bin` | Directory containing helper binaries such as `gh-app-token.sh`. |
| `HIVE_REPOS` | Required by v1/top-level supervisor scripts if project config is absent | none | Space-separated repos for legacy/top-level script workflows. |
| `HIVE_BACKENDS` | No | `copilot` in bootstrap example | Backend list for legacy/top-level script setup. |
| `HIVE_MODEL_SERVICES` | No | none | Optional local model stack selector in legacy bootstrap scripts. |
| `HIVE_AUTO_INSTALL` | No | script default | Controls CLI auto-install in legacy bootstrap scripts. |
| `AGENT_SESSION_NAME` | No | `hive` in `config/agent.env.example` | tmux session name for legacy/top-level supervised agent scripts. |
| `AGENT_LOOP_PROMPT` | No | example executor prompt | Prompt sent by legacy/top-level supervisor scripts. |
| `AGENT_READY_MARKER` | No | configured in `agent.env` | TUI marker used by legacy/top-level supervisor readiness checks. |
| `AGENT_AUTO_APPROVE_PHRASE` | No | none | Legacy/top-level supervisor phrase used to auto-dismiss a known prompt. |
| `AGENT_LOG_FILE` | No | configured in `agent.env` | Legacy/top-level heartbeat/healthcheck log path. |
| `NTFY_TOPIC` | No | `hive` in `bin/kick-agents.sh`; blank disables `bin/notify.sh` | ntfy topic for top-level script notifications. |
| `NTFY_SERVER` | No | `https://ntfy.sh` | ntfy server for top-level script notifications. |
| `SLACK_WEBHOOK` | No | none | Slack incoming webhook for top-level script notifications. |
| `DISCORD_WEBHOOK` | No | none | Discord webhook for top-level script notifications. |

## Verification commands

The table above was cross-checked with these mechanical searches from the repository root:

```sh
rg 'os\.(Getenv|LookupEnv)\(' v2 --glob '*.go'
rg '\bHIVE_[A-Z0-9_]+\b|\bBD_DIR\b|\bGH_APP_KEY_FILE\b|\bAGENT_BACKEND\b' v2 bin config Justfile
rg '\b(ANTHROPIC|COPILOT|GOOSE|BOBSHELL|OCI|KUBERNETES|POD|NAMESPACE|DASHBOARD|PROXY|SLACK|DISCORD|NTFY)_[A-Z0-9_]+\b' v2 bin config Justfile
rg 'HIVE_[A-Z0-9_]+|BD_DIR|GH_APP_KEY_FILE|AGENT_BACKEND' v2/deploy
```
