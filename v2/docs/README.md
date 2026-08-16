# Hive documentation

Documentation for the current Hive line (branch `v4`; the code and docs live under the historically named `v2/` directory — the doubled `v2` in repo paths is the directory name, not a branch). The `v2` branch was retired in August 2026.

Start with [Architecture](architecture.md) for the system overview, then use the topic guides below.

## Operations

- [Manual provisioning](manual-provisioning.md) — heartbeat-only cluster provisioning, hub access roles, and common gotchas.
- [Self-hosted hub deployment](hub-deployment.md) — `HIVE_MODE=hub`, hub storage, heartbeat secrets, and SaaS spoke registration.
- [`CAP_NET_ADMIN` and self-hosted spokes](net-admin-requirement.md) — the container runs with or without `NET_ADMIN`; granting it (`--cap-add NET_ADMIN` / `securityContext.capabilities.add`) enables the full forced-proxy-egress gate, and what the degraded best-effort mode means without it.
- [Config layering](config-layering.md) — how ConfigMap seed, PVC dashboard overlay, and runtime config interact.
- [Operator reference](operator-reference.md) — top-level config blocks, hive flags/env, GitHub token scopes, and image provenance.
- [Release channels](release-channels.md) — `stable`/`candidate`/`edge` moving image tags, switching a hive to a channel, and the `stable (v4)` version pill.
- [The `auto-update` Compose profile](auto-update-profile.md) — what unattended Watchtower updates cost you, what the Docker socket proxy does and does **not** fix, and why Kubernetes should not use this profile at all.
- [Environment variable reference](env-vars.md) — centralized list of runtime, deployment, hub, backup, and contributor environment variables.
- [Troubleshooting](troubleshooting.md) — container logs, config validation, agent tmux sessions, dashboard auth, and GitHub credential checks.
- [Cross-cluster migration](cross-cluster-migration.md) — the manual procedure for moving a hive between clusters.
- [Dashboard route and health checks](health-checks.md) — `dashboard-route-rbac.yaml`, `route_exists`, listener probes, and alert behavior.
- [Network and port requirements](network-requirements.md) — inbound ports, proxy paths, egress, and firewall guidance.
- [TLS, HTTPS, and certificates](tls-setup.md) — termination patterns and certificate ownership.
- [Security notes](security.md) — log scrubbing and secret redaction guarantees/limits.
- [Token collection and usage tracking](token-tracking.md) — session JSONL, `/api/cost`, and hub usage rollups.
- [Notifications](notifications.md) — ntfy, Slack, and Discord alert channels, plus the two-way [Discord bot](../../discord/README.md).
- [Public snapshots](snapshots.md) — read-only `/snapshot`, custom CSS, and frame-ancestor sharing.
- [hivectl](hivectl.md) — command-line client for the dashboard API.
- [`bd` beads CLI](beads-cli.md) — work-ledger and knowledge command reference for operators and contributors.
- [Backup and restore](backup-restore.md) — `hive-backup`, Kubernetes CronJob, and spoke backup scope.
- [Deployment helper scripts](deployment-scripts.md) — Proxmox LXC and blue-green Compose helpers.
- [`bin/` pipeline script index](../../bin/README.md) — map of the 45 deterministic pipeline and operational shell/Python scripts, grouped by function.
- [Dashboard API reference](api-reference.md) — pragmatic route index for dashboard and hub endpoints.
- [Dashboard OpenAPI spec](../../dashboard/openapi.json) — machine-readable REST API reference for integrations.
- [ioscan status](ioscan.md) — the untrusted-input scanner/canary feature (live and default-on in v4).
- [Deployment scripts](../deploy/README.md) — inventory of v2 deployment helpers, including dashboard TTY panes and `hive-panes`.

## Contributors and access

- [ClankeR contributor relay](contributor-relay.md) — local contributor setup, multi-hub subscriptions, and role requests.
- [Contributor trust tiers and delegated agent roles](contributor-trust-and-roles.md) — newcomer/contributor/trusted/merger/advisor semantics, **Acting as**, grants, and delegatable roles.
- [Credly badges](credly-badges.md) — planned integration design; currently a placeholder mapping only.

## Configuration and agents

- [Agent configuration](agent-configuration.md) — agent fields, methods, models, pins, cadences, caveman mode, and ACMM packs.
- [Supervisor agent](supervisor.md) — supervisor policy modes, bead roles, and when to enable the orchestration lane.
- [Custom dashboard stylesheets](custom-stylesheets.md) — operator-supplied CSS for the dashboard and public snapshot.
- [Portable AgentDefinition format](../AGENT-DEFINITION.md) — standalone YAML schema for importing/exporting agent definitions.
- [Knowledge curator](knowledge-curator.md) — automatic fact extraction and promotion knobs.
- [Agent peer-awareness logging (pluk)](agent-logging.md) — pluk log format, `hive-panes`, availability, and retention.
- [Strategy Lab (Nous)](strategy-lab.md) — experiment lifecycle, dashboard/API configuration, fast-fail bounds, and the gate-decision flow. No `nous:` block in `hive.yaml`.
- [GitHub App setup](github-app-setup.md) — app creation, permissions, Setup URL, and `/gh-setup`.
- [ACMM policy matrix](acmm-policy-matrix.md) — capability levels and policy modes.
- [Inception](inception.md) — operator guide to the L1 brainstorm/inception workflow: phases, API, and template variables.
- [ACMM policy fragments](../../examples/acmm/README.md) — per-level ACMM policy references.
- [Sandbox isolation and agent guardrails](sandbox-isolation.md) — isolation layers and operator guardrail notes.
- [Per-agent gh restrictions](../../config/restrictions/README.md) — file-based wrapper denials in `/etc/hive/restrictions/`.
- [Podman rootless CI](podman-rootless-ci.md) — rootless Podman contract for `contribute-hive`.
- [CLI backend setup](../../docs/backend-setup.md) — setup notes for Claude, Copilot, Goose, Bob, Pi, Codex, and Aider.
- [Inference backends](../../docs/inference-backends.md) — vLLM, llm-d, LiteLLM, and Model Gateway troubleshooting.
- [apiproxy](apiproxy.md) — Anthropic-compatible proxy logging and deployment notes.
- [v1 to v2 migration](../../docs/migration-v1-v2.md) — migration checklist and rollback notes.

## Architecture and design

- [Architecture](architecture.md) — process model, governor loop, guardrails, hub/spoke, and walkthrough.
- [CNCF reference architecture](cncf-reference-architecture.md) — CNCF submission/reference template.
- [Knowledge system design](design/knowledge-system.md) — llm-wiki layers, subscriptions, and APIs.
- [Trajectory review](trajectory-review.md) — trajectory safety lane and review signals.

## Historical/design notes

Some documents describe planned or design-only work rather than live features. Those pages are marked at the top, for example [Credly badges](credly-badges.md).

## Security (v4)

- [Security model — operator guide](security-model.md) — Ed25519-only sessions/SSO, per-hive keys, master key rotation, forced proxy egress and `CAP_NET_ADMIN`, privilege model, and supply-chain posture.
- [Security threat model](security-threat-model.md) — actors, boundaries, layered defenses, known gaps, and reporting.
- [Architecture Decision Records](adr/README.md) — lightweight ADR process and records 0001-0010.
- [Intent verification](intent-verification.md) — tier-based change authorization for merge eligibility.
- [Rootless Podman CI seam](podman-rootless-ci.md) — documented test intent and static contract for contributor-container runtime handling.
