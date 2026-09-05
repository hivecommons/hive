# Hive Federation: Multi-Project Agent Swarm Network

## Overview

Hive Federation lets independent Hive instances publish themselves in a registry so contributors can discover projects and connect a local ClankeR contributor relay to one or more hubs. The registry is a directory, not a control plane: every hive keeps its own credentials, queue, agents, contributor registry, and trust policy.

## Current implementation

> This section was originally written while the source lived under the retired
> `v2/` tree (v2 was retired in August 2026). The endpoints it describes are
> live on the current `v4` branch, under `src/`.

The Go dashboard API implements the federation endpoints in `src/pkg/dashboard/api_contribute.go`:

- `GET /api/hives` — list registered hives.
- `POST /api/hives/register` — add or update a hive entry.
- `POST /api/hives/:id/heartbeat` — update live counts such as active contributors/agents and actionable work.
- `DELETE /api/hives/:id` — remove a hive.
- `POST /api/hives/onboard` — generate starter deployment/config text.

Registry storage defaults to `/data/federation/registry.json` and can be overridden for tests with `HIVE_FEDERATION_REGISTRY_PATH`.

## Project onboarding

A project maintainer installs/configures GitHub auth for their Hive, generates or writes the hive config (`hive.yaml`), deploys the Hive, and registers it:

```bash
curl -X POST https://hive.hivecommons.dev/api/hives/register \
  -H "Content-Type: application/json" \
  -d '{
    "project_name": "drasi",
    "org": "drasi-project",
    "hub_url": "wss://drasi-hive.example.com:3001/contribute",
    "dashboard_url": "https://drasi-hive.example.com:3001",
    "contact_email": "maintainer@example.com"
  }'
```

The starter endpoint can produce bootstrap text, but it is not a substitute for reviewing secrets, storage, ingress, and ACMM level before production.

## Contributor flow

A contributor can browse hives, then point ClankeR at one or more hubs:

```bash
just contribute-browse
HIVE_HUB=wss://drasi-hive.example.com:3001/contribute just contribute-hive
```

Multiple hubs are supported by comma-separated `HIVE_HUB` and `HIVE_REGISTRATION_TOKEN` values in the same order. The relay keeps separate WebSocket connections/heartbeats and works on one task at a time.

## Architecture

```
┌──────────────────────────┐
│ Federation registry      │
│ GET /api/hives           │
│ POST /api/hives/register │
│ POST /api/hives/:id/heartbeat
└──────────┬───────────────┘
           │ lists
    ┌──────┴──────┬──────────────┐
    ▼             ▼              ▼
┌─────────┐ ┌─────────┐  ┌─────────┐
│ KS Hive │ │ Drasi   │  │ Keptn   │
│ Hub     │ │ Hive Hub│  │ Hive Hub│
└────┬────┘ └────┬────┘  └────┬────┘
     │           │            │
  contributors connect directly to each hub
```

Each hive owns:

- GitHub App/PAT credentials.
- Agent fleet and ACMM level.
- Work queue and claims.
- Contributor registration and trust tier.
- Dashboard and `/contribute` WebSocket endpoint.

## Operational notes

- The registry does not proxy contributor traffic or mint credentials for remote hives.
- Heartbeats are implemented; operators still need to run the heartbeat sender or otherwise call the endpoint.
- Registration has validation and a maximum registry size, but production deployments should still place the public registry behind normal rate limiting and abuse controls.

## Design-future items

These are not complete today and should be treated as future design work:

- A polished web UI for joining hives from project cards.
- Screenshot-level GitHub App installation guide.
- Portable cross-hive contributor reputation attestations.
- Stronger public registry abuse controls beyond the current API validation and size cap.
