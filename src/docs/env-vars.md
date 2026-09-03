# Environment variable reference

This reference is compiled by hand from the Go source under `src/`, the deployment manifests, and the top-level helper scripts. The code is authoritative: `hive.yaml` may also expand arbitrary `${NAME}` placeholders through the config resolver, but only the variables below have built-in behavior.

## Core `hive` runtime

| Variable | Required | Default | Purpose |
|---|---:|---|---|
| `HIVE_CONFIG` | No | `/etc/hive/hive.yaml` | Default config path used before the `--config` flag is parsed; an explicit `--config` outranks it, so `entrypoint.sh` also appends `--config "$HIVE_CONFIG"` to the launch argv when it is set ([#4973](https://github.com/hivecommons/hive/issues/4973)). The dashboard also uses it when reporting config provenance — which is why the two must not disagree. |
| `HIVE_MODE` | No | spoke/dashboard mode | Set to `hub` to run the hub server instead of the spoke dashboard. |
| `HIVE_HUB_PORT` | No | `3001` | Hub listen port when `HIVE_MODE=hub`. |
| `HIVE_SINGLETON_LOCK` | No | `/var/run/hive-metrics/hive.singleton.lock` when available, otherwise OS temp dir | Overrides the process singleton lock path. Set exactly `off` only for local development where duplicate processes are intentional. |
| `HIVE_GITHUB_TOKEN` | Required unless GitHub App auth is configured | none | Main PAT fallback for `github.token`; also used by fleet/stat fallback paths and some deployment manifests. Missing PAT scopes surface as request-time 403s — see [Required PAT scopes](github-app-setup.md#personal-access-token-pat-scopes). |
| `GH_APP_KEY_FILE` | No | configured `github.key_file`, then `/data/gh-app-key.pem` or `/secrets/gh-app-key.pem` in provisioned paths | GitHub App private-key file fallback. |
| `DASHBOARD_AUTH_TOKEN` | No | none | Dashboard shared-token **value** used by Kubernetes/provisioned deployments; read before `HIVE_DASHBOARD_TOKEN` when `dashboard.auth_token` is empty. Same format rules as `HIVE_DASHBOARD_TOKEN` — see [Generating and rotating `HIVE_DASHBOARD_TOKEN`](#generating-and-rotating-hive_dashboard_token). |
| `HIVE_DASHBOARD_TOKEN` | No | none | Dashboard/API shared-token fallback and default `hivectl --token-env` variable. See [Generating and rotating `HIVE_DASHBOARD_TOKEN`](#generating-and-rotating-hive_dashboard_token). |
| `HIVE_DASHBOARD_COOKIE` | No | none | **Client-side only** - read by `hivectl tui`, never by the server. Cookie header value (e.g. `hive_session=...`) carrying a per-user session, for hives that do not accept the shared token: hub-hosted ones, and spokes with an `authorized_users` allowlist. See [hivectl.md, Credentials](hivectl.md#credentials). |
| `HIVE_AUTHORIZED_USERS` | No | none | Comma-separated direct-route dashboard allowlist, with optional `user:role` entries. Used when `dashboard.authorized_users` is empty. |
| `HIVE_REPO` | No | none | Bootstrap shortcut in `owner/repo` form; fills `project.org`, `project.repos`, and `project.primary_repo` if missing. |
| `HIVE_LEVEL` | No | config/pack value | ACMM level bootstrap/override used by hosted flows and the entrypoint pack selection. |
| `HIVE_ID` | No | config or generated id | Stable hive/spoke identifier override; passed through to launched agents. |
| `HIVE_CLUSTER_ID` | No | config or hub-provisioned value | Hosted cluster identifier override. |
| `HIVE_HUB_URL` | No | `hub.url` from config | Hub URL override for spoke heartbeats/registration. On the hub it is also the last environment variable consulted in the hub public-origin chain (see `HIVE_HUB_PUBLIC_URL`). |
| `HIVE_HUB_PUBLIC_URL` | No | `https://hive.kubestellar.io` | Hub canonical public origin used to derive OAuth/OIDC callback URLs, hub Open Graph/notification links, same-origin checks, and the registrable-domain scope for hub SSO cookies. Set this to the externally reachable hub URL when moving the public host; leave unset to preserve the current canonical host. |
| `HIVE_HUB_SPOKE_DOMAIN` | No | `hive.kubestellar.io` | Parent domain used for hub-provisioned spoke ingress hostnames (for example `<hive-id>.<domain>`). Kept separate from `HIVE_HUB_PUBLIC_URL` because the wildcard spoke domain may differ from the hub's own hostname. |
| `HIVE_HUB_LEGACY_COOKIE_DOMAIN` | No | none | Optional transition-only hub SSO cookie domain to expire alongside the active cookie domain while accepting any carried `hive_hub_user` cookie value during a host migration. Writes still target the domain derived from `HIVE_HUB_PUBLIC_URL`; unset disables the extra legacy-domain cleanup. |
| `HIVE_HUB_SECRET` | Required for spokes registered to a protected hub; optional for a standalone hub with `/data/saas/hub-secret.key` | `/data/saas/hub-secret.key` on the hub when present; no fallback for spoke heartbeat auth | Bearer secret for spoke heartbeats and hub/spoke SaaS APIs. |
| `HIVE_COVERAGE_BADGE_URL` | No | none | Optional coverage badge URL exposed in dashboard status. |
| `HIVE_BRANDING_CSS` | No | `<data>/branding/custom.css`, where `<data>` is the parent directory of the configured `data.agents_dir`, falling back to `/data` when that config field is empty — so the shipped default is `/data/branding/custom.css` | Absolute path of the operator override stylesheet the dashboard serves at `/branding/custom.css`. The index document links that URL unconditionally, so a missing file is the normal case and simply 404s. Read **per request**, so an edit takes effect on reload without a restart. The file is read whole with no ownership, mode, or size check — see [Branding a hive, Overriding the paths](branding.md#overriding-the-paths) before pointing this at any directory an agent can write. |
| `HIVE_BRANDING_JSON` | No | `branding.json` in the same directory as the resolved `HIVE_BRANDING_CSS` — so `/data/branding/branding.json` by default, and it follows `HIVE_BRANDING_CSS` when that is overridden | Absolute path of the branding strings file (`product_name`, `tagline`, `mark`, `title`) baked into the served SPA document. Read **once at startup**, because the index carries a precomputed gzip body and strong ETag; editing it needs a restart. A missing or malformed file is a no-op (a parse failure is logged and ignored). Because the strings are substituted into the served bytes, they are inside the document CSP hashes are computed over — see [branding.md](branding.md#overriding-the-paths). |
| `HIVE_WORK_DIR` | No | `/data/agents` | Agent manager working directory. |
| `HIVE_SHA` | No | build SHA | Passed to launched agents and used in hub upgrade/status paths. |
| `HIVE_ADVISORY_ISSUE` | No | none | Passed to launched agents so advisory findings can target a configured issue. |
| `HIVE_TTYD_PORT` | No | `7681` | Web terminal port used by the entrypoint and terminal proxy. |
| `HIVE_TTYD_CREDENTIAL` | No | `hive:<HIVE_DASHBOARD_TOKEN>` when a token is set, else none | ttyd basic-auth credential (`user:pass`) the entrypoint starts the web terminal with. Also read by `hivectl tui`'s remote attach ([#5644](https://github.com/hivecommons/hive/issues/5644)), which must present the same credential through the terminal proxy and derives the same default from `HIVE_DASHBOARD_TOKEN` — set it on the client only if the deployment overrode it on the server. |
| `HIVE_METRICS_ENABLED` | No | disabled | Registers Prometheus `/metrics` when set to `1`, `true`, `yes`, or `on`. Requires `HIVE_METRICS_TOKEN` — enabled-but-tokenless returns 403 ([#3804](https://github.com/hivecommons/hive/pull/3804)). |
| `HIVE_METRICS_TOKEN` | Yes when metrics enabled | none | Bearer token for `/metrics` (`Authorization: Bearer <token>`; Prometheus `bearer_token`). `/metrics` bypasses dashboard session auth, so this token is its only guard; the cost/agent series are never served without it. |
| `HIVE_METRICS_FILE` | No | `/var/run/hive-metrics/contribute.json` | Contributor metrics JSON file override. |
| `HIVE_COPILOT_INTEGRATION_ID` | No | compiled Copilot integration id | Overrides the integration id used by Copilot model discovery. |
| `HIVE_CONTRIBUTORS_DIR` | No | hub default | Contributor registry directory override. |
| `HIVE_FEDERATION_REGISTRY_PATH` | No | `/data/federation/registry.json` | Federation registry path override. |
| `HIVE_WEBHOOK_SECRET` | No | none | HMAC secret for the spoke `/webhook` channel. |
| `GITHUB_WEBHOOK_SECRET` | No | `/data/saas/webhook-secret.key` when present | Hub GitHub webhook HMAC secret. |
| `HIVE_DASHBOARD_URL` | No | none | Base URL the `hive tui` client targets (`pkg/tui/client`). A bad value surfaces as a request error on the first call, not at startup. On the hub it is also consulted (fourth) in the hub public-origin chain (see `HIVE_HUB_PUBLIC_URL`). |
| `HIVE_CONVERGENCE_MODE` | No | `convergence.mode` in `hive.yaml`, else `off` | Process-level override of the convergence mode (`off`, `shadow`, `enforce`) so an operator can flip shadow mode without editing `hive.yaml`. Any unrecognised value — a typo, or a mode this build does not know — resolves to `off`. |
| `HIVE_WATCHDOG_PAUSE` | No | unset (not paused) | Fleet-wide watchdog kill switch (`1`, `true`, `yes`, `on`). Read at every config resolve, so it takes effect without a restart. It can only ever REDUCE authority: it never turns a watchdog on and never promotes observe to heal. |
| `HIVE_DELEGATION_CHAIN_ENABLED` | No | disabled | Enables delegation chain minting (`1`, `true`, `yes`, `on` — same spelling as `HIVE_METRICS_ENABLED`). Read on each call rather than cached, so disabling it on a misbehaving spoke does not require a pod roll. |
| `HIVE_ALLOW_PRIVATE_GIT_SOURCE` | No | `false` | Opt-in to knowledge Git sources whose host resolves to a private/internal address (self-hosted GitLab and similar). Off by default as SSRF protection. |
| `HIVE_SHARED_AGENT_HOME` | No | per-agent HOME | Escape hatch (`1`) restoring the legacy shared-HOME layout for agents. |
| `HIVE_WORKSPACE_CLEANUP_ENABLED` | No | enabled | Set `0` to opt out of automatic agent workspace cleanup. |
| `HIVE_WORKSPACE_CLEANUP_INTERVAL` | No | `1h` | How often the workspace cleanup sweep runs (Go duration, e.g. `30m`). Unset, unparseable, or non-positive values fall back to the default. |
| `HIVE_WORKSPACE_CLEANUP_MAX_AGE` | No | `2h` | How old an entry under `/data/agents/*/` must be before the cleanup sweep removes it (Go duration, e.g. `6h`). Unset, unparseable, or non-positive values fall back to the default. |
| `HIVE_DOSSIER_CACHE_MAX_ENTRIES` | No | `512` | Caps each public dossier cache. Bounds username-spray memory while keeping normal contributor reuse hot. |

## Generating and rotating `HIVE_DASHBOARD_TOKEN`

`HIVE_DASHBOARD_TOKEN` (and the `dashboard.auth_token` config key it falls back
to) is an opaque shared secret. The server does **not** enforce any format:

- **Format**: any non-empty string is accepted. It is not parsed as a UUID,
  JWT, or hex value — it is compared byte-for-byte (in constant time) against
  the `Authorization: ******` value on each API request.
- **Validation**: there is no startup validation and no minimum-length or
  entropy check. A weak or predictable value is accepted silently, so the
  burden of picking a strong value is entirely on the operator.
- **What it protects**: on a self-hosted (non-direct-route) hive this token is
  the *only* API credential — it gates agent logs, kick controls, and config
  reads/writes, and it doubles as the server-to-server `X-Hive-Internal`
  credential used by the local proxy. Treat it like a root password for the
  hive. On direct-route or hub-proxied spokes identity is per-user and the
  shared token is server-to-server only.
- **Empty value**: leaving it unset leaves the dashboard API unauthenticated
  (unless direct-route per-user authorization is configured). Never deploy an
  internet-reachable hive without it.

### Precedence: `dashboard.auth_token`, `DASHBOARD_AUTH_TOKEN`, `HIVE_DASHBOARD_TOKEN`

Three sources can supply the same one shared token. The first non-empty value
wins and the rest are ignored:

1. `dashboard.auth_token` in `hive.yaml` (note that the shipped manifests set it
   to the `${HIVE_DASHBOARD_TOKEN}` placeholder, which the config resolver
   expands — so in those deployments the env var is what actually supplies it).
2. `DASHBOARD_AUTH_TOKEN` environment variable.
3. `HIVE_DASHBOARD_TOKEN` environment variable.

**Both env vars hold the token *value*, not a Kubernetes Secret name.** In
`src/deploy/k8s/deployment.yaml` the *name* of the Secret is `hive-secrets`; the
env var is populated from a key inside it via `secretKeyRef`. Setting either var
to something like `hive-secrets` configures that literal string as your
dashboard password.

Because both resolve to the same field, setting them to *different* values is
never useful — the lower-precedence one is silently discarded, which is a common
source of "I rotated the token but the old one still works" confusion. Pick one
variable per deployment and rotate that one.

Generate a strong value with a CSPRNG; 32 bytes (256 bits) of entropy is
recommended:

```sh
openssl rand -hex 32
# or
head -c 32 /dev/urandom | base64 | tr -d '=+/'
```

Placeholders like `your-dashboard-auth-token` in deployment examples must be
replaced — any string "works", but a guessable token is a full-access
credential.

**Rotation**: the token is read at process start and compared per request, so
rotating is: update the env var / Kubernetes Secret / `config.env`, then
restart the container or pod. The old token stops being accepted as soon as
the process restarts with the new value; there is no separate session
invalidation step (browser device-flow sessions use their own cookies and are
unaffected). Update any `hivectl` environments and other API clients to the
new value at the same time.

## Deployment entrypoint and proxy knobs

| Variable | Required | Default | Purpose |
|---|---:|---|---|
| `HIVE_API_PORT` | No | `3002` | Internal Go API port used by `src/deploy/entrypoint.sh`. |
| `HIVE_PROXY_PORT` | No | `3001` | Node reverse-proxy/front-door port used by `src/deploy/entrypoint.sh`. |
| `HIVE_STATIC_DIR` | No | `/opt/hive/proxy/public` | Static asset directory for the Node proxy. |
| `HIVE_PROXY_EGRESS_MARK` | No | `0x1112` | Packet mark exempted from the MITM egress redirect. |
| `HIVE_PROXY_ADVISORY_OK` | No | `false` | Allows the spoke to start when the forced-proxy egress redirect cannot be installed (no `CAP_NET_ADMIN`/iptables). Enforcement becomes advisory-only — agents can bypass the proxy. Also gates whether the Go proxy trusts a self-asserted `Proxy-Authorization` header as agent identity when its UID map is unavailable (N7, #3841) — off by default, an unidentified caller is treated as `ADVISORY` (writes blocked) rather than whatever name it claims. See [security-model.md](security-model.md#forced-proxy-egress-and-cap_net_admin). |
| `HIVE_TMUX_HISTORY_LIMIT` | No | `50000` | tmux scrollback depth applied when an agent session is created (positive integer; the authoritative knob for terminal scrollback and full-log capture). |
| `HIVE_TTYD_HISTORY_LIMIT` | No | `50000` | Defense-in-depth history-limit raise applied at browser attach time; only affects panes created after attach. |
| `HIVE_TMUX_PANE_WIDTH` | No | `200` | Column count agent tmux sessions are created with. A detached tmux session defaults to 80 columns because no attached client supplies a size. |
| `HIVE_KICK_LOG_DIR` | No | `/data/logs/kicks` | Root directory per-kick log archives are written under. On the persistent volume so archives survive restarts, pod rolls, and image upgrades. |
| `HIVE_KICK_LOG_RETENTION` | No | `10` | Archived kick logs kept per agent. `0` disables archiving entirely. |
| `HIVE_KICK_LOG_MAX_BYTES` | No | `67108864` (64 MiB) | Per-agent total size cap across archived kick logs. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | No | none | OTLP trace exporter endpoint. Tracing stays disabled while unset. |
| `HIVE_WIKI_GIT_URL` | No | none | Optional wiki vault URL cloned into `/data/vaults/hive-wiki` on first boot. |

## Inference, CLI backends, and agents

| Variable | Required | Default | Purpose |
|---|---:|---|---|
| `HIVE_VLLM_ENDPOINT` | No | `http://hive-vllm-svc.hive-inference.svc.cluster.local:8000` in code; `src/deploy/k8s/deployment.yaml` sets `http://vllm-svc.hive-inference.svc.cluster.local:8000` | Comma-separated vLLM endpoint list. |
| `HIVE_LLMD_ENDPOINT` | No | `http://hive-llm-d-epp.hive-inference.svc.cluster.local:8000` in code; `src/deploy/k8s/deployment.yaml` sets `http://llm-d-epp.hive-inference.svc.cluster.local:8000` | Comma-separated llm-d endpoint list. |
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
| `ANTHROPIC_API_KEY` | Required by `cmd/apiproxy` to inject an upstream key; agent inference backends receive a synthetic value | none | Anthropic-compatible **upstream** API key. Never used to authenticate callers of `cmd/apiproxy`. |
| `PROXY_AUTH_TOKEN` | **Required** by `cmd/apiproxy`; the binary exits at startup when unset and the proxy handler returns `503` | none | **Client auth token** callers must present to `cmd/apiproxy` via `Authorization: Bearer` or `X-Api-Key`. Validated in constant time and stripped before the upstream request. Mandatory because an unauthenticated proxy would let any co-resident loopback caller spend the host `ANTHROPIC_API_KEY`. |
| `CONTEXT7_API_KEY` | No | none | Optional key for Context7 knowledge API integration. |
| `GOOSE_PROVIDER` | No | Goose CLI default | Provider passed through Goose backend/model resolution. |
| `GOOSE_MODEL` | No | Goose CLI default | Model passed through Goose backend/model resolution and contributor relay fallback. |
| `HIVE_EXPLAIN_MODE` | No | `off` | **Fallback** for the hive-wide default agent explain mode (`off`, `brief`, `full`) — see [agent-configuration.md](agent-configuration.md#explain-mode-debugging-agent-behaviour). `governor.explain_mode` in `hive.yaml` (Settings → Governor → General in the dashboard) takes precedence; this variable applies only when that is unset. Either way it applies only to agents that leave `explain_mode` unset; an agent with an explicit value, including `off`, keeps it. Hive also injects the *resolved* mode into every agent process under this same name. An unrecognized value resolves to `off`. |
| `BD_DIR` | No | current directory | `bd` beads CLI data directory. |
| `BD_DASHBOARD_URL` | No | none | Dashboard URL used by `bd kb` integration. |
| `OPENAI_API_KEY` | No | none | OpenAI-compatible API key consulted by backend/model resolution. |
| `CODEX_API_KEY` | No | none | API key consulted for the Codex CLI backend. |
| `HIVE_AGENT_TOKEN_REFRESH_INTERVAL` | No | `40m` | Go duration overriding the per-agent token refresh interval. Invalid or non-positive values fall back to the default. |
| `HIVE_CREDENTIAL_WATCHDOG_INTERVAL` | No | `5m` | Go duration overriding how often the credential watchdog verifies each in-use backend credential file. `0` does NOT disable the watchdog — disabling is intentionally not offered. |
| `HIVE_COPILOT_SESSION_REFRESH_INTERVAL` | No | `10m` | Go duration overriding the Copilot session refresh interval. |
| `HIVE_COPILOT_SESSION_REFRESH_START_DELAY` | No | `30s` | Go duration overriding the delay before the first Copilot session refresh. |
| `HIVE_CLAUDE_DANGEROUSLY_ALLOW_HOST_STATE` | No | unset | Bypasses the Claude host-state isolation guard. As the name says, unsafe outside local development. |
| `HIVE_CONN_<NAME>_URL` | No | generated from agent connection config | Agent API connection URI variable when a connection omits `env_name`; `<NAME>` is the uppercased connection name with `-` replaced by `_`. |
| Custom connection auth env vars | No | none | If an agent API connection uses `auth.type: env`, Hive reads `auth.env_var` and injects that exact variable into the agent. |

## Linear agent integration

Part 2 of [RFC #4492](https://github.com/hivecommons/hive/issues/4492): the hive can join a Linear workspace as an agent (`actor=app` OAuth), receive `AgentSessionEvent` webhooks, and narrate work back as agent activities. Setup and verification steps live in [linear-agent.md](linear-agent.md).

| Variable | Required | Default | Purpose |
|---|---:|---|---|
| `LINEAR_API_KEY` | Yes for `work_source.type: linear` | none | Read-only Linear API key used by the Linear work-source adapter. Reference it from `hive.yaml` with `api_key: ${LINEAR_API_KEY}` rather than storing the secret directly. The same `${LINEAR_API_KEY}` form works when the work source is set from the dashboard: the reference is resolved from the hive's environment when the work source is built (an unset variable is a startup error), and only the reference is ever persisted. |
| `LINEAR_CLIENT_ID` | Yes for the Linear agent integration | none | OAuth client id of your Linear application (Linear → Settings → API → Applications). Without it the install endpoint returns 412 and the integration stays off. |
| `LINEAR_CLIENT_SECRET` | Yes for the Linear agent integration | none | OAuth ****** for the code exchange and token refresh. Secret — deliver via Kubernetes Secret / env, never config files. |
| `LINEAR_WEBHOOK_SECRET` | Yes for Linear webhooks | none | HMAC-SHA256 signing secret from the Linear app's webhook settings. The receiver **fails closed**: with this unset every delivery to `/api/linear/webhook` is rejected 401. |
| `LINEAR_AGENT_STORE` | No | `/data/linear-agent.json` | Path of the persisted install record (workspace identity + OAuth grant, mode 0600). Override for tests or non-container runs. |

Inside an **agent** session (set by the hive, never by the operator): ISSUES_ONLY+ agents receive `LINEAR_ACCESS_TOKEN` (the connected app's OAuth token, `Authorization: Bearer`) or, when no workspace is connected, `LINEAR_API_KEY` (the work-source key, bare `Authorization`). Advisory agents receive neither and have both stripped. See [linear-agent.md](linear-agent.md#github-issue-parity-agents-writing-to-linear).

## Hub, SaaS, alerts, and backups

| Variable | Required | Default | Purpose |
|---|---:|---|---|
| `HIVE_HUB_OAUTH_CLIENT_ID` | Required to enable hub OAuth | none | Enables hub `/login` and OAuth callback routes. |
| `HIVE_HUB_OAUTH_CLIENT_SECRET` | Required when hub OAuth is enabled | none | OAuth client secret used during callback token exchange. |
| `HIVE_HUB_SLACK_BOT_TOKEN` | No | none | Slack bot token for hub Slack notifications. |
| `HIVE_NTFY_SERVER` | No | none | ntfy server for hub auth-audit alerts. |
| `HIVE_NTFY_TOPIC` | No | none | ntfy topic for hub auth-audit alerts. |
| `HIVE_BACKUP_KEY` | Yes for hub DR backups; optional for spoke backups | none | 64-character hex AES-256 key. For spoke backups it is now a **fallback**: owners set the key from Governor Config → Security → Backup (`governor.backup.key_file`), which takes precedence and needs no deployment access. With no key from any source, backup creation fails rather than writing plaintext. |
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
| `HIVE_HUB_ADMIN_USERNAME` | No | none | Single hub admin username. Consulted alongside `HIVE_HUB_ADMINS`. |
| `HIVE_HUB_ADMINS` | No | none | Comma-separated hub admin usernames. |
| `HIVE_HUB_GITHUB_TOKEN` | No | none | Hub-side GitHub token used by the dibs public-repo check. |
| `HIVE_REACH_REPO_DIR` | No | none (GitHub compare API) | Local clone the reach ancestry check resolves against via `git merge-base --is-ancestor`. The hub image ships no clone, so the compare-API adapter is the default. |
| `HIVE_REACH_NEVER_RAN_DAYS` | No | `3` | Never-ran grace period in days (integer, > 0). Absent or invalid values fall back to the default. |
| `HIVE_PROVISION_WORKERS` | No | saved scale setting, else built-in default | Provision queue worker count. The saved dashboard scale setting takes precedence over this variable. |
| `HIVE_PROVISION_PER_CLUSTER` | No | saved scale setting, else built-in default | Maximum concurrent provisions per cluster. |
| `HIVE_KUBECTL_MAX_PER_CLUSTER` | No | saved scale setting, else built-in default | Maximum concurrent `kubectl` executions per cluster. |
| `HIVE_UPGRADE_WAVE_SIZE` | No | saved scale setting, else built-in default | Number of spokes upgraded per wave. |
| `HIVE_UPGRADE_DEBOUNCE_SECONDS` | No | built-in default | Debounce window before an upgrade wave starts. |
| `HIVE_UPGRADE_MAX_HOLD_SECONDS` | No | built-in default | Maximum time an upgrade may be held before proceeding. |
| `HIVE_ADVISORY_ISSUE_AGING_AFTER` | No | `24h` | Age (Go duration) after which a hive's advisory/issue output is bucketed `aging` in hub activity reporting (`pkg/hub/advisory_issue_activity.go`). Must be set **below** `HIVE_ADVISORY_ISSUE_STALE_AFTER`: if stale ≤ aging, **both** thresholds silently revert to their defaults. |
| `HIVE_ADVISORY_ISSUE_STALE_AFTER` | No | `72h` | Age (Go duration) after which advisory/issue output is bucketed `stale` — the operator-action threshold. Same pairing rule as above: a value ≤ the aging threshold makes both revert to defaults. |
| `HIVE_HUB_PUBLIC_URL` | No | none (chain continues) | First variable in the hub public-origin chain used to build notification deep links and to match the hub domain suffix. Precedence: `HIVE_HUB_PUBLIC_URL` → `HIVE_PUBLIC_URL` → `HIVE_HUB_BASE_URL` → `HIVE_DASHBOARD_URL` → `HIVE_HUB_URL`, then the compiled-in canonical public origin (links) or the default cluster domain (suffix match). |
| `HIVE_PUBLIC_URL` | No | none (chain continues) | Second variable in the hub public-origin chain — see `HIVE_HUB_PUBLIC_URL` for the full precedence order. |
| `HIVE_HUB_BASE_URL` | No | none (chain continues) | Third variable in the hub public-origin chain — see `HIVE_HUB_PUBLIC_URL` for the full precedence order. |

### Spoke-side derived keys

A hub-hosted spoke is provisioned with only the derived sub-keys it needs and
never receives the master `HIVE_HUB_SECRET`. When one of these is unset, the
spoke derives the same domain-separated sub-key from `HIVE_HUB_SECRET`, so a
spoke still rolling on an older Deployment keeps working. Both sources yield the
identical key, so hub verification succeeds either way; a lookup fails closed
only when neither is configured.

| Variable | Required | Default | Purpose |
|---|---:|---|---|
| `HIVE_HEARTBEAT_KEY` | No | derived from `HIVE_HUB_SECRET` | Spoke heartbeat signing sub-key. |
| `HIVE_SESSION_KEY` | No | derived from `HIVE_HUB_SECRET` | Spoke session-cookie signing sub-key. |
| `HIVE_INVITE_KEY` | No | derived from `HIVE_HUB_SECRET` | Per-hive contributor-invite signing key. Symmetric: the spoke both mints and verifies invite tokens with it. |
| `HIVE_TERMINAL_KEY` | No | self-derived per-hive from `HIVE_HUB_SECRET` + `HIVE_ID` | Per-hive terminal-assertion signing key. It never falls back to a fleet-uniform key. |
| `HIVE_SSO_PUBLIC_KEY` | No | none | Ed25519 **public** key a spoke verifies hub-minted SSO handoff tokens with. Holding only the public key, a spoke can verify but cannot mint. |
| `HIVE_SSO_PUBLIC_KEY_PREV` | No | none | Previous SSO public key, accepted during rotation so a spoke bridges a hub key change. |
| `HIVE_SSO_KEY` | No | none | Legacy symmetric SSO key, still read for one release so spokes on a pre-cutover Deployment keep working. |

### Hub login providers

`HIVE_HUB_OAUTH_CLIENT_ID`/`_CLIENT_SECRET` enable **GitHub** login. Additional human-login providers are OIDC-based and each is enabled by setting its client id ([#3664](https://github.com/hivecommons/hive/pull/3664)). Per provider `<P>` in `GOOGLE`, `IBMID`, `REDHAT`, `MICROSOFT`, `CUSTOM`:

| Variable | Required | Default | Purpose |
|---|---:|---|---|
| `HIVE_HUB_OIDC_<P>_CLIENT_ID` | Enables the provider | none | Provider is absent from the login picker until set. |
| `HIVE_HUB_OIDC_<P>_CLIENT_SECRET` | Yes when enabled | none | OIDC client secret. |
| `HIVE_HUB_OIDC_<P>_ISSUER` | Required for `IBMID` and `CUSTOM` | Google/Red Hat/Microsoft have built-in issuers | OIDC issuer URL (discovery + JWKS). |
| `HIVE_HUB_OIDC_<P>_SCOPES` | No | `openid email profile` | Space- or comma-separated scope override. |
| `HIVE_HUB_OIDC_<P>_DISPLAY` | No | provider name | Login button label override. |
| `HIVE_HUB_OIDC_<P>_SUBJECT_CLAIM` | No | `sub` (IBMid: `uid`) | Claim used as the stable subject. |
| `HIVE_HUB_OIDC_MICROSOFT_TENANT` | No | `organizations` | Entra tenant segment of the issuer URL. |
| `HIVE_HUB_OIDC_MICROSOFT_ALLOWED_TENANTS` | No | none | Restricts accepted Entra tenants. |

With two or more providers configured, `/login` renders a provider picker; with exactly one it redirects straight into it. A provider with a client id but missing/invalid issuer is **silently skipped** so it cannot take down login for the others — check the `hub login enabled providers=` startup log line when a button is missing.

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
| `HIVE_SESSION` | No | backend name (`AGENT_BACKEND`) | Optional session label for running multiple relays concurrently under one GitHub account. The hub keys task leases, assignment cooldowns, failure streaks, and ownership fences on `ContributorID#session`, so distinctly labeled relays hold independent task slots; auth, trust tier, model admission, and rate-limit accounting stay per-account. Sanitized to `[A-Za-z0-9._-]`, max 32 bytes. Set to the empty string to opt out (bare per-account identity, the historical single-session behavior). See `src/docs/contributor-relay.md`, "Running multiple backends under one account". |
| `HIVE_HEADLESS_STATUS_FILE` | No | `/tmp/contributor-headless-status.json` | Status file written by headless contributor relay. |
| `HIVE_CONTRIBUTOR_IMAGE` | No | `ghcr.io/hivecommons/hive-contributor:latest` | Image used by `just contribute-hive`. |
| `HIVE_CONTAINER_RUNTIME` | No | autodetect `docker` or `podman` | Container runtime override for contributor helpers. `just contribute-hive` also passes the runtime it resolved into the container, so the attach hints printed from inside it name the engine that actually launched it rather than assuming `docker` ([#5145](https://github.com/hivecommons/hive/issues/5145)). |
| `HIVE_CONTAINER_NAME` | No | `hive-contributor` | Set by `just contribute-hive` on the container it starts. The contributor entrypoint and relay read it for their attach hints; unset means the relay is running in local mode, where the hint is a plain `tmux attach`. |
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

## Credly badge integration (proposed — not implemented)

> **No code reads these variables.** The Credly integration is a design
> ([Credly badges](credly-badges.md)); Hive ships only the contributor-card
> placeholder and the milestone mapping. There are no live Credly API calls,
> credentials, or badge issuance. Setting these today has **no effect**.

They are listed here so the central reference does not appear to contradict
`credly-badges.md`, and so the names are reserved. Treat the table as a design
record until the feature ships — at which point these rows move into the
sections above and gain real defaults.

| Variable | Required | Default | Purpose |
|---|---:|---|---|
| `HIVE_CREDLY_ORG_ID` | n/a — proposed | none | Credly issuing organization id. |
| `HIVE_CREDLY_API_TOKEN` | n/a — proposed | none | Credly Issuer API token. A live issuing credential when the feature ships: supply it by environment or secret reference only, never in `hive.yaml` and never committed. Until then, unset leaves the card in placeholder mode. |
| `HIVE_CREDLY_TEMPLATES` | n/a — proposed | none | JSON map of milestone id → Credly badge template id. |

## Keeping this reference current

The code is authoritative and this table is hand-maintained, so it drifts unless
PRs update it. **If your change adds, renames, or removes an environment
variable lookup, update this file in the same PR.**

A CI guard (`TestEnvVarsDocDocumentsOnlyRealVariables`, in
`src/pkg/config/env_vars_doc_parity_test.go`) enforces **one** direction of
this: every variable given a table row here must actually appear in the
implementation, so the reference cannot document something nothing reads. It is
one-directional on purpose — env var names reach `os.Getenv` through package
constants, config-resolved struct fields, injected `getenv` parameters, and
local wrapper helpers, so no static check can enumerate the full set of
variables the code reads.

**The converse is therefore not enforced: adding a lookup without adding a row
here will not fail CI.** Keeping this file complete remains a human
responsibility, which is what the rest of this section is for.

What counts as a change that needs an entry:

- A new `os.Getenv` or `os.LookupEnv` call in `src/` — most live in
  `src/pkg/hub`, `src/pkg/dashboard`, `src/pkg/agent`, and `src/pkg/config`.
- A new variable referenced from a deployment manifest under `src/deploy/`, or
  from a top-level helper script in `bin/`.
- A change to an existing variable's default, or to whether it is required.

What each column means:

| Column | What to write |
|---|---|
| Variable | The exact name, in backticks. |
| Required | `No` for anything with a working default. Spell out the condition when it is conditional (see `HIVE_GITHUB_TOKEN`). |
| Default | The literal fallback value, or `none`. If the fallback is a lookup chain, describe the order — precedence is the part operators get wrong. |
| Purpose | What it controls and which component reads it. Link to a deeper section when the variable has real setup steps. |

Add the row to the section matching the component that reads the variable, and
run the searches below to confirm nothing else was missed.

One exception to "the code is authoritative": a **proposed** variable that no
code reads yet may be listed, but only in a section explicitly marked as such
(see [Credly badge integration](#credly-badge-integration-proposed--not-implemented)).
The marking is the whole point — an operator must never set a variable from this
file and have it silently do nothing. When the feature ships, move those rows
into the section for the component that reads them.

## Verification commands

The table above was cross-checked with these mechanical searches from the repository root:

```sh
rg 'os\.(Getenv|LookupEnv)\(' src --glob '*.go'
rg '\bHIVE_[A-Z0-9_]+\b|\bBD_DIR\b|\bGH_APP_KEY_FILE\b|\bAGENT_BACKEND\b' src bin config Justfile
rg '\b(ANTHROPIC|COPILOT|GOOSE|BOBSHELL|OCI|KUBERNETES|POD|NAMESPACE|DASHBOARD|PROXY|SLACK|DISCORD|NTFY)_[A-Z0-9_]+\b' src bin config Justfile
rg 'HIVE_[A-Z0-9_]+|BD_DIR|GH_APP_KEY_FILE|AGENT_BACKEND' src/deploy
```
