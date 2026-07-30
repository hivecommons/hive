# Hive Federation: Multi-Project Agent Swarm Network

## Overview

Hive Federation allows any open source project to run their own Hive instance
and join a network where contributors can discover and join any participating
project's swarm. A contributor runs `just contribute-browse` to find projects
that need help, then `just contribute-hive` pointed at that project's hub —
**ClankeR**, the contributor relay, carries that project's tasks to the agent on
their machine.

## How a New Project Signs Up

### 1. Install the Hive GitHub App

The project maintainer installs the shared [Hive GitHub App](https://github.com/apps/kubestellar-hive-bot) on their GitHub org/repos. This gives their Hive instance the ability to mint scoped installation tokens for contributors.

### 2. Generate Starter Config

```bash
curl -X POST https://hive.kubestellar.io/api/hives/onboard \
  -H "Content-Type: application/json" \
  -d '{
    "project_name": "drasi",
    "org": "drasi-project",
    "repos": ["drasi-project/drasi-platform", "drasi-project/drasi-docs"],
    "github_app_id": "123456",
    "github_app_installation_id": "789012"
  }'
```

Returns:
- `hive-project.yaml` — project config with repos, agents, dashboard settings
- `docker-compose.yaml` — ready to deploy
- Step-by-step instructions

### 3. Deploy Their Hive

```bash
# On their server/VM
mkdir -p /etc/hive
# Save hive-project.yaml to /etc/hive/
# Save GitHub App private key to /etc/hive/gh-app-key.pem
docker compose up -d
```

The Hive starts with `scanner` and `supervisor` agents by default.
The project can enable more agents as their needs grow.

### 4. Register with the Federation

```bash
curl -X POST https://hive.kubestellar.io/api/hives/register \
  -H "Content-Type: application/json" \
  -d '{
    "project_name": "drasi",
    "org": "drasi-project",
    "hub_url": "wss://drasi-hive.example.com:3001/contribute",
    "dashboard_url": "https://drasi-hive.example.com:3001",
    "contact_email": "maintainer@drasi.io"
  }'
```

Now the project appears in `just contribute-browse` and on the
federation directory at `https://hive.kubestellar.io/api/hives`.

## Contributor Flow

```
$ just contribute-browse

=== Available Hives ===

  KubeStellar Console (kubestellar)
    Hub: wss://hive.kubestellar.io:3001/contribute
    Dashboard: https://hive.kubestellar.io:3001
    Contributors: 12 active
    Actionable: 8 items

  Drasi (drasi-project)
    Hub: wss://drasi-hive.example.com:3001/contribute
    Dashboard: https://drasi-hive.example.com:3001
    Contributors: 3 active
    Actionable: 15 items

  Keptn (keptn)
    Hub: wss://keptn-hive.cncf.io:3001/contribute
    Dashboard: https://keptn-hive.cncf.io:3001
    Contributors: 0 active
    Actionable: 23 items
```

The contributor can then target a specific hive:

```bash
HIVE_HUB=wss://drasi-hive.example.com:3001/contribute just contribute-hive
```

Or register with multiple hives and switch between them.

## Federation Architecture

```
┌──────────────────────────┐
│  Federation Registry     │  hive.kubestellar.io
│  GET /api/hives          │  (the KubeStellar hive doubles as registry)
│  POST /api/hives/register│
└──────────┬───────────────┘
           │ lists
    ┌──────┴──────┬──────────────┐
    ▼             ▼              ▼
┌─────────┐ ┌─────────┐  ┌─────────┐
│ KS Hive │ │ Drasi   │  │ Keptn   │
│ Hub     │ │ Hive Hub│  │ Hive Hub│
│ :3001   │ │ :3001   │  │ :3001   │
└────┬────┘ └────┬────┘  └────┬────┘
     │           │            │
  Contributors connect directly to each hub
```

Each hive is fully independent:
- Own GitHub App credentials
- Own agent fleet
- Own work queue
- Own contributor registry
- Own trust tiers

The federation registry is just a directory — it doesn't proxy traffic or
manage credentials across hives. Each hub handles its own WebSocket
connections and token minting.

## What Each Hive Gets

- **Dashboard** at port 3001 with agent status, queue, sparklines
- **Agent fleet** (scanner, supervisor, + whatever they enable)
- **Contributor WebSocket** (ClankeR) at `/contribute`
- **Scoped GitHub tokens** via the shared Hive GitHub App
- **Governor** that adapts agent cadence to queue depth
- **Nous Strategy Lab** for automated experimentation (optional)

## Future: Cross-Hive Contributor Reputation

A contributor who builds trust in one hive could have that reputation
portable to other hives. A signed attestation from Hive A saying
"this contributor completed 20 tasks with 0 revocations" could be
presented to Hive B to skip the newcomer tier.

This is out of scope for the initial implementation but the
per-hive contributor profile (`/etc/hive/contributors/<user>.json`)
already tracks the data needed to issue such attestations.

## Implementation Status

### Done (in PR #798)
- `GET /api/hives` — list registered hives
- `POST /api/hives/register` — register a remote hive
- `DELETE /api/hives/:id` — remove a hive
- `POST /api/hives/onboard` — generate starter config
- `just contribute-browse` — discover available hives
- Federation registry storage at `/etc/hive/federation/registry.json`

### TODO
- [ ] Heartbeat: registered hives periodically ping the registry with
      their current active contributor count and actionable items
- [ ] Web UI: add a "Join a Hive" page at `/contribute` with project
      cards showing description, star count, contributor needs
- [ ] GitHub App installation guide with screenshots
- [ ] Cross-hive reputation attestations
- [ ] Rate limiting on registration endpoint
