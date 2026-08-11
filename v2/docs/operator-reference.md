# Hive operator reference

This page is a concise operator reference for fields and runtime knobs that are
easy to miss in `hive.yaml.example`. It was checked against `pkg/config/config.go`
and `cmd/hive/main.go` on v2.

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
| `variables.security.*` | Deny by default. | `allow_exec`, `allow_http`, and GitHub prompt-source allowlists are honored only from the trusted seed, not dashboard overlays. |
| `data.claude_sessions_dir` | `/data/home/.claude/projects` | Where the dashboard reads Claude Code session JSONL for per-agent token/cost accounting. Point it at the agents' real session directory if you relocate `HOME`. |
| `data.copilot_sessions_dir` | `/data/home/.copilot/session-state` | Same, for the Copilot CLI backend's session state. |

For runtime precedence and provenance, see [config-layering.md](config-layering.md).

## Image provenance for `ghcr.io/kubestellar/hive:v2-latest`

The pre-built Docker image used by `v2/docker-compose.yaml` is built by [`.github/workflows/docker.yml`](../../.github/workflows/docker.yml). On the `v2` branch, that workflow publishes a rolling multi-architecture manifest tag:

- `ghcr.io/kubestellar/hive:v2-latest`
- `ghcr.io/kubestellar/hive:<git-short-sha>`

### What updates the tag

The workflow runs on pushes to every non-dependabot/non-copilot branch and on `workflow_dispatch`. PR and short-lived branch builds still compile the image as a CI gate, but the `gate` job only pushes to GHCR for long-lived branches listed in the workflow (`v2`, `v3`, `mk`, `dd`) or for manual `workflow_dispatch` runs.

For the main Hive image, the build job uses `v2/Dockerfile` and passes these build arguments:

```text
GIT_HASH=${{ github.sha }}
GIT_BRANCH=${{ github.ref_name }}
```

The merge job then combines the per-architecture digests into a manifest list. Before tagging, it verifies that the workflow SHA is still the current HEAD of the branch; stale queued builds skip tagging instead of moving `v2-latest` backward.

### Stability guarantee

`v2-latest` is a rolling branch tag for the current HEAD of `origin/v2` after the Docker workflow succeeds. It is not immutable and is intended for quick starts and continuously updated hives. Pin production deployments to a digest when you need a reproducible image.

### Verify and pin a digest

Inspect the tag digest:

```bash
docker buildx imagetools inspect ghcr.io/kubestellar/hive:v2-latest
```

Pull by digest after choosing the manifest digest you want:

```bash
docker pull ghcr.io/kubestellar/hive@sha256:<digest>
```

In Compose, replace the tag with the digest form:

```yaml
services:
  hive:
    image: ghcr.io/kubestellar/hive@sha256:<digest>
```

To relate an image to source, compare the `<git-short-sha>` tag published by the same workflow with commits on the `v2` branch, or inspect the Docker workflow run for the commit SHA that produced the digest.

### Changelog and release notes

The rolling image follows merged changes on the `v2` branch. Use the GitHub commit/PR history for the exact source change set behind a SHA tag, and use repository releases or changelog files only when this repository publishes them for a specific release line.

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
| `HIVE_CONFIG` | env | Overrides the default config path before `--config` is parsed. |
| `HIVE_MODE=hub` | env | Starts the hub server instead of a spoke dashboard. |
| `HIVE_HUB_PORT` | env | Hub listen port in hub mode; default `3001`. |
| `HIVE_SINGLETON_LOCK` | env | Internal/escape hatch for the process singleton lock path; value `off` disables the guard. |

### GitHub and dashboard auth

| Name | Purpose |
|---|---|
| `HIVE_GITHUB_TOKEN` | Main PAT fallback when `github.token` is empty; also used for token identity/fleet stats fallback. |
| `GH_APP_KEY_FILE` | GitHub App private-key path fallback when `github.key_file` is empty. |
| `HIVE_DASHBOARD_TOKEN` | Shared dashboard/API token fallback for `dashboard.auth_token`. |

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
| `HIVE_METRICS_ENABLED` | Enables unauthenticated Prometheus `/metrics` when set to `1`, `true`, `yes`, or `on`; off by default because it exposes estimated cost data. |
| `HIVE_METRICS_TOKEN` | Optional bearer token for `/metrics`. When set, scrapers must send `Authorization: Bearer <token>` (configure Prometheus `bearer_token`); when empty, `/metrics` stays open. Set it to stop the cost/agent series leaking to anyone on the pod network. |

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
