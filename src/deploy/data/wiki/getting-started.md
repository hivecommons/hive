---
title: Getting Started with Hive
tags: [overview, onboarding]
---

# Getting Started with Hive

Hive is an autonomous multi-agent system that manages software repositories
using AI-powered agents. Each agent has a specific role and operates within
governance boundaries set by the **Governor**.

This page is the entry point. It gets a new operator from zero to a running
hive, points contributors at the build/test path, and links the reference
material for everything after that.

> **Note on links.** This vault is copied to `/data/wiki/` when the container
> starts, so it does not sit inside a checkout of the repository. Links out of
> the vault are therefore absolute URLs to the `v4` branch on GitHub rather
> than relative paths, which would not resolve at runtime.

## Core Concepts

- **Governor** -- evaluates repository state (open issues, PRs, SLA breaches)
  and decides which agents to wake and how often.
- **Modes** -- the governor selects a mode (surge, busy, quiet, idle) based on
  queue depth. Each mode defines cadences for every agent.
- **Beads** -- structured logs of each agent session. Beads capture what the
  agent was asked, what it did, and the outcome.
- **Knowledge Layer** -- a wiki of facts (patterns, gotchas, regressions)
  extracted from merged PRs and fed back to agents as primer context.
- **ACMM Level** -- six levels (L1-L6) that decide what agents are permitted to
  do, from advisory-only observations up to opening and auto-merging pull
  requests. You raise the level as you build trust in the output; the goal is
  trust, not level.

## Run a Hive (Docker Compose)

Docker Compose is the default standalone runtime. Podman is a parallel
supported choice — see the
[Podman quick start](https://github.com/hivecommons/hive/blob/v4/README.md#quick-start-podman).

**Prerequisites**

- Docker Engine 24+ with the Compose v2 plugin (`docker compose`, not the
  legacy `docker-compose`)
- Linux, macOS, or Windows (WSL2) on `amd64` or `arm64`
- `git`, `openssl`, and a GitHub token (PAT or App) for the org the hive will
  work on

```bash
git clone https://github.com/hivecommons/hive.git
cd hive

cp src/hive.yaml.example src/hive.yaml
```

Edit `src/hive.yaml` and set at least `project.org`, `project.repos`, and
`project.ai_author` for the org and repositories the hive should manage.

Now write the environment file. **It must be `src/.env`, not a `.env` at the
repository root.** Because `-f src/docker-compose.yaml` makes `src/` the
project directory, Compose reads `.env` from there — the same place the compose
file's own `./hive.yaml` and `./secrets` mounts resolve against. A root `.env`
is read by nothing, and since both paths are gitignored, neither git nor
Compose warns you: the hive starts and then 401s on every GitHub call, which
looks like a bad token rather than an unread file.

```bash
# Replace with your own token. Never commit this file.
echo "HIVE_GITHUB_TOKEN=ghp_REPLACE_ME" > src/.env

# REQUIRED. The dashboard's auth proxy enforces this token and refuses to
# start without one, so the gateway on :3001 would proxy to a port nothing is
# listening on.
printf 'HIVE_DASHBOARD_TOKEN=%s\n' "$(openssl rand -hex 32)" >> src/.env

docker compose -f src/docker-compose.yaml up -d
```

A classic PAT needs `repo` scope; see
[github-app-setup.md](https://github.com/hivecommons/hive/blob/v4/src/docs/github-app-setup.md#personal-access-token-pat-scopes)
for the App path and the full scope list.

## Verify Your Hive Is Healthy

The gateway publishes port 3001 whether or not the proxy behind it came up, so
confirm the endpoint answers rather than assuming the port is enough:

```bash
curl -sf http://127.0.0.1:3001/api/health     # -> {"status":"ok"}
```

If that returns nothing, check the containers and their logs — the two
services are named `hive` and `hive-gateway`:

```bash
docker compose -f src/docker-compose.yaml ps
docker logs hive
docker logs hive-gateway
```

A gateway that is up while `hive` is unhealthy is almost always the missing
`HIVE_DASHBOARD_TOKEN` or a `.env` written to the wrong directory. For anything
else, see
[troubleshooting.md](https://github.com/hivecommons/hive/blob/v4/src/docs/troubleshooting.md).

The dashboard is then at `http://localhost:3001`.

## The Agents

A hive deploys a roster of specialized agents, each with a distinct role:
`scanner`, `quality`, `guide`, `brainstorm`, `ci-maintainer`, `architect`,
`supervisor`, `outreach`, `sec-check`, and `strategist`. How many are active
depends on your ACMM level, and what each one is permitted to do depends on its
policy mode at that level.

See [agents.md](agents.md) in this vault for what each agent does and how to
add a custom one.

## For Contributors

If you are here to change Hive itself rather than run it:

- [Getting started as a first-time contributor](https://github.com/hivecommons/hive/blob/v4/docs/getting-started-contributing.md)
  -- the end-to-end path for a first PR.
- [CONTRIBUTING.md](https://github.com/hivecommons/hive/blob/v4/CONTRIBUTING.md)
  -- branches, DCO sign-off, and PR format.
- [Local development](https://github.com/hivecommons/hive/blob/v4/docs/development.md)
  -- Go version, `go build ./...`, `go test ./...`, and the helper recipes.

Use `v4` as the PR base for ordinary Hive development. Most changes need no
cluster: `cd src && go build ./...` is enough to iterate on Go code, and
docs-only fixes need no toolchain at all.

## Key References

- [Reference architecture](https://github.com/hivecommons/hive/blob/v4/src/docs/architecture.md)
  -- how the governor, agents, and dashboard fit together.
- [ACMM policy matrix](https://github.com/hivecommons/hive/blob/v4/src/docs/acmm-policy-matrix.md)
  -- the full per-level, per-agent permission table.
- [Zero to automation](https://github.com/hivecommons/hive/blob/v4/src/docs/getting-started.md)
  -- the narrative guide to climbing the ACMM levels.
- [Agent configuration](https://github.com/hivecommons/hive/blob/v4/src/docs/agent-configuration.md)
  -- every field of an `agents:` entry in `hive.yaml`.
- [Environment variables](https://github.com/hivecommons/hive/blob/v4/src/docs/env-vars.md)
  -- the compiled reference for `HIVE_*` and related variables.
- [Operator reference](https://github.com/hivecommons/hive/blob/v4/src/docs/operator-reference.md)
  -- runtime knobs, image provenance, and tags.
- [hivectl](https://github.com/hivecommons/hive/blob/v4/src/docs/hivectl.md)
  -- the non-interactive CLI client (`hivectl system health`, `system status`).
- [Documentation index](https://github.com/hivecommons/hive/blob/v4/src/docs/README.md)
  -- everything else: operations, snapshots, contributor relay, design notes.

## Editing This Wiki

This vault is an Obsidian-compatible directory of markdown files. You can:

1. Edit files directly on disk
2. Open the vault in Obsidian and enable the **Obsidian Git** community plugin
3. Push changes to the configured git remote -- Hive will pull them
   automatically every 60 seconds

The files shipped in the image are starter knowledge, not fixed product
documentation. Replace or extend them with the runbooks, gotchas, and policies
that your agents should be primed with.
