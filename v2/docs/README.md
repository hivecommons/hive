# Hive v2 documentation

Start with [Architecture](architecture.md) for the system overview, then use the topic guides below.

## Operations

- [Manual provisioning](manual-provisioning.md) — heartbeat-only cluster provisioning, hub access roles, and common gotchas.
- [Config layering](config-layering.md) — how ConfigMap seed, PVC dashboard overlay, and runtime config interact.
- [Operator reference](operator-reference.md) — top-level config blocks, hive flags/env, and GitHub token scopes.
- [Cross-cluster migration](cross-cluster-migration.md) — moving a hive between clusters without losing state.
- [Dashboard route and health checks](health-checks.md) — `dashboard-route-rbac.yaml`, `route_exists`, listener probes, and alert behavior.
- [Network and port requirements](network-requirements.md) — inbound ports, proxy paths, egress, and firewall guidance.
- [TLS, HTTPS, and certificates](tls-setup.md) — termination patterns and certificate ownership.
- [Security notes](security.md) — log scrubbing and secret redaction guarantees/limits.
- [Token collection and usage tracking](token-tracking.md) — session JSONL, `/api/cost`, and hub usage rollups.
- [Public snapshots](snapshots.md) — read-only `/snapshot`, custom CSS, and frame-ancestor sharing.
- [hivectl](hivectl.md) — command-line client for the dashboard API.
- [`bd` beads CLI](beads-cli.md) — work-ledger and knowledge command reference for operators and contributors.
- [Backup and restore](backup-restore.md) — `hive-backup`, Kubernetes CronJob, and spoke backup scope.
- [Deployment helper scripts](deployment-scripts.md) — Proxmox LXC and blue-green Compose helpers.
- [Dashboard API reference](api-reference.md) — pragmatic route index for dashboard and hub endpoints.
- [Dashboard OpenAPI spec](../../dashboard/openapi.json) — machine-readable REST API reference for integrations.
- [ioscan status](ioscan.md) — v2 status of the untrusted-input scanner/canary feature.

## Contributors and access

- [ClankeR contributor relay](contributor-relay.md) — local contributor setup, multi-hub subscriptions, and role requests.
- [Contributor trust tiers and delegated agent roles](contributor-trust-and-roles.md) — newcomer/contributor/trusted/merger/advisor semantics, **Acting as**, grants, and delegatable roles.
- [Credly badges](credly-badges.md) — planned integration design; currently a placeholder mapping only.

## Configuration and agents

- [Agent configuration](agent-configuration.md) — agent fields, methods, models, pins, cadences, caveman mode, and ACMM packs.
- [Knowledge curator](knowledge-curator.md) — automatic fact extraction and promotion knobs.
- [GitHub App setup](github-app-setup.md) — app creation, permissions, Setup URL, and `/gh-setup`.
- [ACMM policy matrix](acmm-policy-matrix.md) — capability levels and policy modes.
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

Some documents describe planned or design-only work rather than live features. Those pages are marked at the top, for example [Credly badges](credly-badges.md). `ioscan` is also documented as absent from v2 HEAD until code is reintroduced.
