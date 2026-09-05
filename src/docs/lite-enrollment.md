# Hive lite enrollment

Hive lite is the five-minute, zero-repo-secret on-ramp for a repository. Lite is
still a **spoke** flow: repositories live in a spoke's `hive.yaml` `project.repos`
list. The hub tracks spokes through heartbeats and returns config callbacks; it
does not register or scan repositories itself.

## Prerequisites

- `gh` is installed and logged in for the target GitHub host.
- The Hive GitHub App is installed on the repo owner, or you know the
  installation ID. If the hub has the App key, it can auto-discover the
  installation ID for hosted lite spokes.
- A hub login/token that can call the dashboard API.

## Enroll

```bash
export HIVE_DASHBOARD_TOKEN="$(unset GITHUB_TOKEN && gh auth token)"
hivectl --server https://hive.hivecommons.dev enroll OWNER/REPO
```

Optional:

```bash
hivectl enroll OWNER/REPO --acmm-level 1
hivectl enroll OWNER/REPO --installation-id 123456
```

The command verifies `gh auth` and repository access, then asks the hub to place
the repo on a spoke:

1. If you already own a spoke for the repo's org, the hub updates that spoke's
   desired project config and delivers the changed repo list through the normal
   heartbeat `project_config` callback.
2. If no suitable spoke exists, the hub requests a hosted lite spoke using the
   hosted-hive provisioning path. The hosted lite spoke starts with the repo in
   its own `hive.yaml`, advisory lanes enabled, and ACMM capped at L2.

## What lite mode does

- Stores the repo on a spoke, never as a hub-side repo registry entry.
- Marks hosted lite spokes as `lite` spoke metadata so the UI can explain the
  profile.
- Uses the Hive GitHub App plus `pkg/mint` scoped tokens from the spoke.
- Keeps the enrolled repo zero-secret: no PAT, private key, or long-lived token
  is written to the repository.
- Runs normal spoke-side advisory behavior at ACMM L1-L2.

## What lite mode does not do

- The hub does not run a lite repo scanner or store lite repo reports.
- No workflow shim is installed in the repository.
- No ACMM L3+ behavior; lite is capped at L2 advisory mode.

## Graduation path

Use lite mode until advisory findings are useful and low-noise. Graduate to a
full spoke profile when the repo needs code-writing agents, private runtime
access, custom in-cluster dependencies, or ACMM levels above L2.
