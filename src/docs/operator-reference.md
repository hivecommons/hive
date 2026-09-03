# Hive operator reference

This page is a concise operator reference for fields and runtime knobs that are
easy to miss in `hive.yaml.example`. It was checked against `pkg/config/config.go`
and `cmd/hive/main.go` on branch `v4`.

For the full centralized environment variable table, including hub, backup,
inference, deployment, contributor, and legacy helper-script variables, see
[Environment variable reference](env-vars.md).

## Minimum required configuration

Most of `hive.yaml.example` is optional. The smallest config the hive will start
with (enforced by `Config.Validate`) is:

- **`project.org`** — the GitHub org the hive works on. Startup crash-loops with
  `project.org is required` if missing.
- **at least one repo** under `project.repos` (bare repo name; the org is
  `project.org`).
- **GitHub credentials** — one of `github.token`, `github.app_id` (App auth), or
  `github.forge`. Missing all three fails with
  `github.token, github.app_id or github.forge is required`.
- **at least one agent** under `agents:` (a bare `name: { backend, model }` is
  enough; defaults fill in the rest — see [agent-configuration.md](agent-configuration.md)).

Everything else — `governor` cadences, `knowledge`, `notifications`,
`dashboard.auth_token`, gateways, `hub` — is optional and has working defaults.
A minimal `hive.yaml`:

```yaml
project:
  org: my-org
  repos: [my-repo]
github:
  token: ${HIVE_GITHUB_TOKEN}
agents:
  scanner:
    backend: copilot
    model: claude-sonnet-4-6
```

## Configuration blocks

Top-level YAML keys accepted by `config.Config`:

| Block | Purpose | Notes |
|---|---|---|
| `project` | Managed GitHub org, repos, primary repo, AI author. | Required for normal operation. |
| `policies` | Optional git/local policy prompt source. | Hot-reloaded when configured. |
| `agents` | Agent definitions and behavior metadata. | Per-agent overlays may also live under `data.agents_dir`. |
| `governor` | Cadences, labels, sensing, health, budgets, inference gateways, trajectory review. | See notable fields below. |
| `github` | PAT or GitHub App credentials and forge URLs. | Use one auth method. |
| `notifications` | ntfy, Slack, and Discord webhooks. | All optional; see [notifications.md](notifications.md). |
| `dashboard` | Web UI port, snapshots, auth token, frame allowlist, authorized users. | `auth_token` can come from `HIVE_DASHBOARD_TOKEN`. |
| `agent_sandbox` | Podman-rootless sandbox launcher for hub/pod agents (`pkg/sandbox`). | Opt-in and **two-gate**: this block's own `enabled: true` sandboxes nothing by itself — each agent also needs `sandbox.enabled: true` under `agents.<name>`. The dashboard's Security tab writes only this global flag, so enabling it there alone can leave every agent unconfined — but the tab now shows this: `security.sandboxWarnings` in `GET /api/config/governor` carries `config.AgentSandboxGateWarnings`'s diagnosis (also logged at WARN at boot/reload), rendered both in the page's coherence-warnings box and inline under the toggle, naming the still-unconfined agents and the fixing key (#4918). See [sandbox-isolation.md](sandbox-isolation.md) and the [getting-started confinement section](getting-started.md#where-agents-actually-run-read-this-before-l3). |
| `data` | Metrics, logs, session, and agent overlay directories. | Defaults are `/data/...` in containers. |
| `knowledge` | Wiki layers, vaults, git/document sources, primer, curator, bead synthesizer. | Disabled unless `enabled: true`. |
| `hub` | Hub/spoke hosted-hive metadata. | Usually provisioner-owned. |
| `hive_id` | Stable spoke identifier. | Usually provisioner-owned. |
| `acmm_level` | Current ACMM pack level. | May be set by hub/dashboard. |
| `variables` | Trusted variable resolver definitions. | Env-only substitution works without this block. |
| `removed_agents` | Persistent tombstones for deleted agents. | Dashboard/overlay-owned; do not seed casually. |

## Notable fields

| Field | Default / behavior | Operator note |
|---|---|---|
| `governor.labels.automerge` | Defaults to `lgtm`. | Label applied when a merger/owner queues a PR for Hive auto-merge-on-green. |
| `governor.trajectory.enabled` | Defaults to enabled. | The lane no-ops until a reviewer endpoint and model resolve from `governor.trajectory` or `governor.litellm`. |
| `dashboard.snapshot_frame_ancestors` | Empty list means CSP `frame-ancestors 'none'`. | Entries must be exact `https://` origins; paths, wildcards, credentials, query, and fragments are rejected. |
| `dashboard.authorized_users` | Empty means no per-user direct-route allowlist. | Entries can be `user` or `user:role`; roles are `read`, `read-write`, `merger`, `owner`. |
| `dashboard.public_url` | Empty means OAuth redirect URIs (Linear agent install, OpenRouter funding) fall back to `hub.dashboard_url`, then the request's forwarded/`Host` origin. | Set to this dashboard's externally reachable origin (`https://hive.example.com`, no path/query) on a standalone hive whose callback is published on a different hostname or whose ingress rewrites `Host`. Must be an absolute `http(s)://` origin; a trailing slash is trimmed and anything else fails config load. See [linear-agent.md](linear-agent.md#setup). |
| `variables.security.*` | Deny by default. | `allow_exec`, `allow_http`, and GitHub prompt-source allowlists are honored only from the trusted seed, not dashboard overlays. |
| `data.claude_sessions_dir` | `/data/home/.claude/projects` | Where the dashboard reads Claude Code session JSONL for per-agent token/cost accounting. Point it at the agents' real session directory if you relocate `HOME`. |
| `data.copilot_sessions_dir` | `/data/home/.copilot/session-state` | Same, for the Copilot CLI backend's session state. |

For runtime precedence and provenance, see [config-layering.md](config-layering.md).

## Image provenance and tags

Pre-built images are published by [`.github/workflows/docker.yml`](../../.github/workflows/docker.yml) to `ghcr.io/kubestellar/hive` (plus `hive-contributor` and `hive-hub`) and mirrored to the matching `ghcr.io/hivecommons/*` packages. While `kubestellar` is the native publishing org before the Hive Commons transfer, the workflow retags the already-built digest into `hivecommons`, so both orgs serve digest-identical manifest lists for the same tag. A build of the mainline branch `v4` publishes, in one multi-architecture manifest operation:

- `ghcr.io/kubestellar/hive:v4-latest` and `ghcr.io/hivecommons/hive:v4-latest` — rolling tags for the current HEAD of `origin/v4`;
- `ghcr.io/kubestellar/hive:<git-short-sha>` and `ghcr.io/hivecommons/hive:<git-short-sha>` — immutable per-commit tags;
- `ghcr.io/kubestellar/hive:stable`, `:candidate`, `:edge` and the matching `ghcr.io/hivecommons/hive:*` tags — the moving **release channels** (retags of the same digest; see [release-channels.md](release-channels.md)).

PR and short-lived branch builds compile the image as a CI gate but only long-lived branches push tags, and only `v4` moves the release channels. Before tagging, the workflow verifies its SHA is still branch HEAD, so a stale queued build cannot move a rolling tag backward.

> **Note:** `v2-latest` was the rolling tag of the retired `v2` branch. Do not use it for new deployments — prefer `stable` for production, or pin a digest. (`src/docker-compose.yaml` was bumped off it in #4206; standalone image references now come from one source of truth, [`src/deploy/standalone-images.sh`](../deploy/standalone-images.sh).)

### One source of truth for standalone assets

Standalone deployment assets do not carry their own image references. They take
them from [`src/deploy/standalone-images.sh`](../deploy/standalone-images.sh),
which names the Hive, gateway, and auto-update-profile images in fully
qualified form. Change a reference there rather than in an individual asset.

Fully qualified is deliberate: `podman auto-update` refuses a short name, and
Podman's Quadlet generator warns on one, so a reference that resolves through
the host's `unqualified-search-registries` is not portable across the two
runtimes. Docker may still spell the same image without the `docker.io/`
prefix — the contract test compares the normalized forms, so the two spellings
are equal as long as they name the same image. Digest pins are carried through
unchanged and are checked for as well.

[`src/deploy/test_standalone_image_refs.sh`](../deploy/test_standalone_image_refs.sh)
runs in CI and fails the build when an asset stops agreeing with that file,
when a digest pin is dropped, or when the retired `v2-latest` tag reappears. It
already covers the Podman asset paths, so drift detection switches on by itself
when those assets land.

### Choosing a tag

- **Production:** `stable`, or pin a digest for full reproducibility.
- **Following mainline:** `v4-latest`.
- **Reproducing an incident:** the `<git-short-sha>` tag from the workflow run.

### Verify and pin a digest

```bash
docker buildx imagetools inspect ghcr.io/kubestellar/hive:stable
docker pull ghcr.io/kubestellar/hive@sha256:<digest>
```

In Compose, replace the tag with the digest form:

```yaml
services:
  hive:
    image: ghcr.io/kubestellar/hive@sha256:<digest>
```

To relate an image to source, compare the `<git-short-sha>` tag published by the same workflow with commits on `v4`, or inspect the Docker workflow run for the commit SHA that produced the digest.

## Governor cadence and budget

- Agent cadences are evaluated from persisted state: the last-kick map lives in `/data/hive-state.json` and is honored across pod restarts — a Deployment roll does **not** re-kick every cadenced agent at boot ([#3817](https://github.com/hivecommons/hive/pull/3817)). A fresh install (no persisted state) still kicks every cadenced agent on the first eval. There is no global default interval; a zero/absent interval means the agent is never cadence-kicked.
- The governor token budget uses a rolling window of `governor.budget.period_days` (default 7 days), with a soft warning at `governor.budget.critical_pct` (default 90%). When spend reaches the limit, kicks are suppressed for all agents except those explicitly budget-exempt.
- The **provider** spending limit is a separate signal from the token budget above ([#4294](https://github.com/hivecommons/hive/issues/4294)): the token budget counts what the hive spends, while this is the inference gateway refusing to spend more money — a LiteLLM key past its daily dollar cap, a project out of quota, an account out of credit. It is detected from the gateway's own error body (never from a bare 429, which stays on the ordinary retry path), raises an error-level dashboard alert naming the limit that was hit, and withholds every agent kick while it is in force. It does **not** pause agents: pause state stays a human decision.
- Recovery from a provider spending limit is automatic, via a probe. Withholding kicks also withholds the inference calls that would reveal the provider is serving again, so the hive suppresses only while the last refusal is recent and then lets a single kick through to test the gateway; the probe re-arms suppression the moment it is released, so at most one probe run flies per interval. A still-clipped key refuses the probe and suppression resumes for another interval; once the provider's window resets the probe succeeds, normal kicking resumes with no operator action, and a one-time recovery notification is sent (the entering notification is likewise sent once per clip, not once per cycle). Tune with `governor.provider_budget.probe_interval_s` (default 1800 — 30 minutes):

```yaml
governor:
  provider_budget:
    probe_interval_s: 1800
```

  Lower it to resume sooner after a reset at the cost of a rebuffed run per probe; raise it to waste less while noticing later.

## Fleet breaker

The dashboard top bar includes a fleet breaker: a circular pause/play control plus a status pill. The `GET /api/breaker` state is readable by authenticated viewers, but engaging or releasing the breaker is owner-only (`POST /api/breaker/engage`, `POST /api/breaker/release`).

Engage pauses every agent that is currently running and is not `on_demand`. Agents already paused before engage are skipped and keep their original pause reason. Release resumes only the exact agents the breaker paused and still owns (`PausedTrigger == fleet-breaker`); if an operator manually re-pauses an agent while the breaker is engaged, release leaves it paused. The breaker state is persisted so a crash/restart can still release the same captured set.

Fixes #2953.

## `HIVE_GITHUB_TOKEN` permissions

`github.token: ${HIVE_GITHUB_TOKEN}` creates the main GitHub client when a GitHub
App is not configured. The code reads issues, PRs, commits, contents, checks,
commit statuses, contributors, and search results; it can also create/update
issues and comments, add/remove/create labels, open PRs, approve PRs for the
merge queue, and squash-merge queued PRs.

Minimum practical PAT permissions for full PAT-authenticated operation:

- **Classic PAT:** `repo` for private repositories (`public_repo` is enough only
  when every managed repo is public and no private org data is needed).
- **Fine-grained PAT:** select the managed repositories and grant:
  - Contents: read/write (branch pushes by agents and content reads)
  - Pull requests: read/write (list, open, approve, merge)
  - Issues: read/write (list, comments, labels, advisory issue)
  - Checks: read-only and Commit statuses: read-only (CI/merge gating)
  - Metadata: read-only (required by GitHub)

If you use a GitHub App, configure equivalent repository permissions on the App
installation instead of broadening the PAT.

## `cmd/hive` flags and environment

### Startup

| Name | Source | Purpose |
|---|---|---|
| `--config` | flag | Path to `hive.yaml`; default `/etc/hive/hive.yaml` unless `HIVE_CONFIG` is set. |
| `HIVE_CONFIG` | env | Sets the **default** of `--config`. An explicit `--config` outranks it — and the image ships one (`CMD ["--config", "/etc/hive/hive.yaml"]`), so `entrypoint.sh` appends `--config "$HIVE_CONFIG"` to the launch argv when the variable is set. Without that append the variable is inert in the container ([#4973](https://github.com/hivecommons/hive/issues/4973)). |
| `HIVE_MODE=hub` | env | Starts the hub server instead of a spoke dashboard. |
| `HIVE_HUB_PORT` | env | Hub listen port in hub mode; default `3001`. |
| `HIVE_SINGLETON_LOCK` | env | Internal/escape hatch for the process singleton lock path; value `off` disables the guard. |

### GitHub and dashboard auth

| Name | Purpose |
|---|---|
| `HIVE_GITHUB_TOKEN` | Main PAT fallback when `github.token` is empty; also used for token identity/fleet stats fallback. Wrong-scope tokens fail with generic 403s at request time — see [github-app-setup.md](github-app-setup.md#personal-access-token-pat-scopes) for the required scopes per ACMM tier. |
| `GH_APP_KEY_FILE` | GitHub App private-key path fallback when `github.key_file` is empty. |
| `HIVE_DASHBOARD_TOKEN` | Shared dashboard/API token fallback for `dashboard.auth_token`. Any non-empty string is accepted with no strength check — generate one with `openssl rand -hex 32`; see [env-vars.md](env-vars.md#generating-and-rotating-hive_dashboard_token). |

### Hosted-spoke and fleet metadata

| Name | Purpose |
|---|---|
| `HIVE_HUB_URL` | Hub URL override for heartbeat/spoke registration. |
| `HIVE_CLUSTER_ID` | Hosted cluster ID override. |
| `HIVE_ID` | Hive ID override. |
| `HIVE_LEVEL` | ACMM level bootstrap/override used by hosted flows. |
| `HIVE_COVERAGE_BADGE_URL` | Optional coverage badge URL surfaced in dashboard status. |

### Dashboard metrics

| Name | Purpose |
|---|---|
| `HIVE_METRICS_ENABLED` | Registers Prometheus `/metrics` when set to `1`, `true`, `yes`, or `on`; off by default (route not registered — scrapes 404) because it exposes estimated cost data. |
| `HIVE_METRICS_TOKEN` | **Required whenever metrics are enabled** ([#3804](https://github.com/hivecommons/hive/pull/3804)): scrapers must send `Authorization: Bearer <token>` (configure Prometheus `bearer_token`). Enabled-but-tokenless serves 403 with an error naming both variables, and the hive logs a startup warning; the cost/agent series are never served unauthenticated. |

> **Note on `tokens_24h`:** despite its name, the per-spoke heartbeat field
> `tokens_24h` (stored on the hub as `totalTokens24h`) is a **cumulative total**,
> not a rolling 24-hour window. Read it as lifetime token consumption for the
> spoke. See [token-tracking.md](token-tracking.md) for the full heartbeat and
> `/api/saas/usage` rollup details.

### Inference endpoint fallbacks

| Name | Default | Purpose |
|---|---|---|
| `HIVE_VLLM_ENDPOINT` | `http://hive-vllm-svc.hive-inference.svc.cluster.local:8000` | Comma-separated vLLM endpoint list. |
| `HIVE_LLMD_ENDPOINT` | `http://hive-llm-d-epp.hive-inference.svc.cluster.local:8000` | Comma-separated llm-d endpoint list. |
