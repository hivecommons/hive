# Migrating from Hive v1 to v2

> **Historical documentation.** Both ends of this migration are retired: v2 was
> retired in August 2026. This page is kept for operators still running v1, who
> will land on a retired v2 and should continue with
> [`src/docs/migration-v2-v4.md`](../src/docs/migration-v2-v4.md). For the
> current line (branch `v4`; code under `src/`), start with the
> [`src/docs/README.md`](../src/docs/README.md) index.

This guide is practical rather than automatic: v2 changes the runtime layout, config model, and agent policy system enough that you should migrate deliberately.

## Before you start

1. Stop v1 agents cleanly.
2. Back up the hub/dashboard data and any persistent `/data` or `/etc/hive` volumes. If you use the hosted hub, export the registry/contributor state before changing deployments.
3. Save a copy of your v1 `hive.yaml`, agent policy files, secrets, GitHub App IDs, and private key mounts.
4. Create a new v2 branch/deployment and test it before pointing production traffic at it.

## What changes

| Area | v1 | v2 |
| --- | --- | --- |
| Layout | Top-level scripts and dashboard pieces were the main operator surface. | Runtime code and examples live under `src/`; root docs still link to shared concepts. |
| Config | Mostly one static `hive.yaml`. | Layered config: seed `/etc/hive/hive.yaml`, dashboard overlay `/data/hive.yaml.dashboard`, per-agent files in `/data/agent-configs/`, runtime snapshot, and secret dirs. |
| Agents | Hand-built rosters and policies. | ACMM packs generate/merge curated rosters, modes, templates, and cadences. |
| Prompts | Local policy files. | Policy checkout, embedded defaults in `src/policies`, dashboard/`hivectl` prompt edits, and optional allowlisted GitHub prompt sources. |
| Backends | Mainly subscription CLIs. | CLI backends plus OpenAI-compatible inference gateways (`vllm`, `llm-d`, `litellm`, `watsonx`). |
| Contributor flow | Local worker scripts. | ClankeR contributor relay, `Dockerfile.contributor`, `compose-contributor.yaml`, and `just contribute-*` commands. |

## What carries over

- GitHub org/repo lists and the primary repo.
- GitHub App identity, installation ID, and private key, or PAT-based auth if you still use it.
- Agent intent: scanner, ci-maintainer, quality, guide, architect, security, supervisor, strategist, and outreach all map to v2 agents.
- Custom prompts, after renaming them to v2 `kick_template` files or importing them through the dashboard/`hivectl`.
- Knowledge/wiki content if mounted or exported into the v2 knowledge layer.

## Suggested migration path

1. Check out v2 and copy the example config:

   ```bash
   git clone -b v2 https://github.com/hivecommons/hive.git hive-v2
   cd hive-v2/src
   cp hive.yaml.example hive.yaml
   ```

2. Port `project:` and `github:` first. Keep secrets as env vars or files under `/secrets`; do not paste key values into YAML.
3. Pick an ACMM level close to your current operating model. Start lower than production autonomy; L2/L3 is a safer first boot than L5/L6.
4. Port custom agents one at a time. Preserve `backend`, `model`, `beads_dir`, `clear_on_kick`, and cadence intent, but use v2 mode names and templates.
5. Copy or configure policy files. Prefer a policy checkout under `/data/policies/...` so prompt changes do not require rebuilding Hive.
6. Start v2 with one or two agents enabled. Verify dashboard health, GitHub auth, model discovery, and a manual kick.
7. Enable the rest of the roster, then raise ACMM level only after CI and merge gates behave as expected.

## Validation checklist

- `hivectl system status` returns healthy.
- Dashboard shows the expected agents and ACMM modes.
- Each enabled backend reports authenticated or has a configured gateway endpoint.
- A manual kick uses the expected policy template.
- GitHub writes happen only at the intended ACMM level.
- Backups and rollback instructions are stored outside the new deployment.

## Rollback

Keep the v1 deployment and data untouched until v2 has run through at least one full agent cycle. If rollback is needed, stop v2, restore the old DNS/ingress/service target, and restart v1 with the backed-up config and volumes.
