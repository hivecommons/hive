# Hive documentation

Documentation for the current Hive line (branch `v4`; the code and docs live under the `src/` directory). The `v2` branch was retired in August 2026.

Start with [Architecture](architecture.md) for the system overview, then use the topic guides below.

## Operations

- [Manual provisioning](manual-provisioning.md) — heartbeat-only cluster provisioning, hub access roles, and common gotchas.
- [Self-hosted hub deployment](https://github.com/kubestellar/hive/blob/v4/src/docs/hub-deployment.md) — `HIVE_MODE=hub`, hub storage, heartbeat secrets, and SaaS spoke registration.
- [`CAP_NET_ADMIN` and self-hosted spokes](https://github.com/kubestellar/hive/blob/v4/src/docs/net-admin-requirement.md) — the container runs with or without `NET_ADMIN`; granting it (`--cap-add NET_ADMIN` / `securityContext.capabilities.add`) enables the full forced-proxy-egress gate, and what the degraded best-effort mode means without it.
- [Config layering](https://github.com/kubestellar/hive/blob/v4/src/docs/config-layering.md) — how ConfigMap seed, PVC dashboard overlay, and runtime config interact.
- [Operator reference](https://github.com/kubestellar/hive/blob/v4/src/docs/operator-reference.md) — top-level config blocks, hive flags/env, GitHub token scopes, and image provenance.
- [Release channels](release-channels.md) — `stable`/`candidate`/`edge` moving image tags, switching a hive to a channel, and the `stable (v4)` version pill.
- [The `auto-update` Compose profile](https://github.com/kubestellar/hive/blob/v4/src/docs/auto-update-profile.md) — what unattended Watchtower updates cost you, what the Docker socket proxy does and does **not** fix, and why Kubernetes should not use this profile at all.
- [Environment variable reference](https://github.com/kubestellar/hive/blob/v4/src/docs/env-vars.md) — centralized list of runtime, deployment, hub, backup, and contributor environment variables.
- [Troubleshooting](troubleshooting.md) — container logs, config validation, agent tmux sessions, dashboard auth, and GitHub credential checks.
- [Cross-cluster migration](https://github.com/kubestellar/hive/blob/v4/src/docs/cross-cluster-migration.md) — the manual procedure for moving a hive between clusters.
- [Dashboard route and health checks](https://github.com/kubestellar/hive/blob/v4/src/docs/health-checks.md) — `dashboard-route-rbac.yaml`, `route_exists`, listener probes, and alert behavior.
- [Network and port requirements](https://github.com/kubestellar/hive/blob/v4/src/docs/network-requirements.md) — inbound ports, proxy paths, egress, and firewall guidance.
- [TLS, HTTPS, and certificates](https://github.com/kubestellar/hive/blob/v4/src/docs/tls-setup.md) — termination patterns and certificate ownership.
- [Security notes](https://github.com/kubestellar/hive/blob/v4/src/docs/security.md) — log scrubbing and secret redaction guarantees/limits.
- [Token collection and usage tracking](https://github.com/kubestellar/hive/blob/v4/src/docs/token-tracking.md) — session JSONL, `/api/cost`, and hub usage rollups.
- [Notifications](https://github.com/kubestellar/hive/blob/v4/src/docs/notifications.md) — ntfy, Slack, and Discord alert channels, plus the two-way [Discord bot](https://github.com/kubestellar/hive/blob/v4/discord/README.md).
- [Public snapshots](https://github.com/kubestellar/hive/blob/v4/src/docs/snapshots.md) — read-only `/snapshot`, custom CSS, and frame-ancestor sharing.
- [hivectl](hivectl.md) — command-line client for the dashboard API.
- [`bd` beads CLI](https://github.com/kubestellar/hive/blob/v4/src/docs/beads-cli.md) — work-ledger and knowledge command reference for operators and contributors.
- [Backup and restore](https://github.com/kubestellar/hive/blob/v4/src/docs/backup-restore.md) — `hive-backup`, Kubernetes CronJob, spoke backup scope, and setting the backup encryption key from Governor Config (hosted flow).
- [Deployment helper scripts](https://github.com/kubestellar/hive/blob/v4/src/docs/deployment-scripts.md) — Proxmox LXC and blue-green Compose helpers.
- [`bin/` pipeline script index](https://github.com/kubestellar/hive/blob/v4/bin/README.md) — map of the 45 deterministic pipeline and operational shell/Python scripts, grouped by function.
- [Dashboard API reference](https://github.com/kubestellar/hive/blob/v4/src/docs/api-reference.md) — pragmatic route index for dashboard and hub endpoints.
- [Dashboard OpenAPI spec](https://github.com/kubestellar/hive/blob/v4/dashboard/openapi.json) — machine-readable REST API reference for integrations.
- [ioscan status](https://github.com/kubestellar/hive/blob/v4/src/docs/ioscan.md) — the untrusted-input scanner/canary feature (live and default-on in v4).
- [Deployment scripts](https://github.com/kubestellar/hive/blob/v4/src/deploy/README.md) — inventory of v2 deployment helpers, including dashboard TTY panes and `hive-panes`.

## Contributors and access

- [ClankeR contributor relay](contributor-relay.md) — local contributor setup, multi-hub subscriptions, and role requests.
- [Contributor trust tiers and delegated agent roles](https://github.com/kubestellar/hive/blob/v4/src/docs/contributor-trust-and-roles.md) — newcomer/contributor/trusted/merger/advisor semantics, **Acting as**, grants, and delegatable roles.
- [Credly badges](https://github.com/kubestellar/hive/blob/v4/src/docs/credly-badges.md) — planned integration design; currently a placeholder mapping only.

## Configuration and agents

- [Agent configuration](agent-configuration.md) — agent fields, methods, models, pins, cadences, caveman mode, and ACMM packs.
- [Governor mode thresholds](https://github.com/kubestellar/hive/blob/v4/src/docs/governor-thresholds.md) — how idle/quiet/busy/surge thresholds scale with repo count, the `threshold_scaling` curves, and when explicit thresholds win.
- [Supervisor agent](https://github.com/kubestellar/hive/blob/v4/src/docs/supervisor.md) — supervisor policy modes, bead roles, and when to enable the orchestration lane.
- [Custom dashboard stylesheets](https://github.com/kubestellar/hive/blob/v4/src/docs/custom-stylesheets.md) — operator-supplied CSS for the dashboard and public snapshot.
- [Portable AgentDefinition format](https://github.com/kubestellar/hive/blob/v4/src/AGENT-DEFINITION.md) — standalone YAML schema for importing/exporting agent definitions.
- [Knowledge curator](https://github.com/kubestellar/hive/blob/v4/src/docs/knowledge-curator.md) — automatic fact extraction and promotion knobs.
- [Agent peer-awareness logging (pluk)](https://github.com/kubestellar/hive/blob/v4/src/docs/agent-logging.md) — pluk log format, `hive-panes`, availability, and retention.
- [Strategy Lab (Nous)](https://github.com/kubestellar/hive/blob/v4/src/docs/strategy-lab.md) — experiment lifecycle, dashboard/API configuration, fast-fail bounds, and the gate-decision flow. No `nous:` block in `hive.yaml`.
- [GitHub App setup](https://github.com/kubestellar/hive/blob/v4/src/docs/github-app-setup.md) — app creation, permissions, Setup URL, and `/gh-setup`.
- [ACMM policy matrix](acmm-policy-matrix.md) — capability levels and policy modes.
- [Inception](https://github.com/kubestellar/hive/blob/v4/src/docs/inception.md) — operator guide to the L1 brainstorm/inception workflow: phases, API, and template variables.
- [ACMM policy fragments](https://github.com/kubestellar/hive/blob/v4/examples/acmm/README.md) — per-level ACMM policy references.
- [Sandbox isolation and agent guardrails](https://github.com/kubestellar/hive/blob/v4/src/docs/sandbox-isolation.md) — isolation layers and operator guardrail notes.
- [Per-agent gh restrictions](https://github.com/kubestellar/hive/blob/v4/config/restrictions/README.md) — file-based wrapper denials in `/etc/hive/restrictions/`.
- [Podman rootless CI](https://github.com/kubestellar/hive/blob/v4/src/docs/podman-rootless-ci.md) — rootless Podman contract for `contribute-hive`.
- [CLI backend setup](https://github.com/kubestellar/hive/blob/v4/docs/backend-setup.md) — setup notes for Claude, Copilot, Goose, Bob, Pi, Codex, and Aider.
- [Inference backends](https://github.com/kubestellar/hive/blob/v4/docs/inference-backends.md) — vLLM, llm-d, LiteLLM, and Model Gateway troubleshooting.
- [apiproxy](https://github.com/kubestellar/hive/blob/v4/src/docs/apiproxy.md) — Anthropic-compatible proxy logging and deployment notes.
- [v1 to v2 migration](https://github.com/kubestellar/hive/blob/v4/docs/migration-v1-v2.md) — migration checklist and rollback notes.

## Architecture and design

- [Architecture](architecture.md) — process model, governor loop, guardrails, hub/spoke, and walkthrough.
- [CNCF reference architecture](https://github.com/kubestellar/hive/blob/v4/src/docs/cncf-reference-architecture.md) — CNCF submission/reference template.
- [Knowledge system design](https://github.com/kubestellar/hive/blob/v4/src/docs/design/knowledge-system.md) — llm-wiki layers, subscriptions, and APIs.
- [Trajectory review](https://github.com/kubestellar/hive/blob/v4/src/docs/trajectory-review.md) — trajectory safety lane and review signals.

## Historical/design notes

Some documents describe planned or design-only work rather than live features. Those pages are marked at the top, for example [Credly badges](https://github.com/kubestellar/hive/blob/v4/src/docs/credly-badges.md).

## Security (v4)

- [Security model — operator guide](security-model.md) — Ed25519-only sessions/SSO, per-hive keys, master key rotation, forced proxy egress and `CAP_NET_ADMIN`, privilege model, and supply-chain posture.
- [Security threat model](https://github.com/kubestellar/hive/blob/v4/src/docs/security-threat-model.md) — actors, boundaries, layered defenses, known gaps, and reporting.
- [Architecture Decision Records](https://github.com/kubestellar/hive/blob/v4/src/docs/adr/README.md) — lightweight ADR process and records 0001-0010.
- [Intent verification](https://github.com/kubestellar/hive/blob/v4/src/docs/intent-verification.md) — tier-based change authorization for merge eligibility.
- [Rootless Podman CI seam](https://github.com/kubestellar/hive/blob/v4/src/docs/podman-rootless-ci.md) — documented test intent and static contract for contributor-container runtime handling.
