# Hive lite enrollment

Hive lite is the five-minute, clusterless on-ramp for a repository. It creates a
hub-side enrollment record and starter advisory config without deploying a spoke
or putting a PAT in the enrolled repository.

## Prerequisites

- `gh` is installed and logged in for the target GitHub host.
- The Hive GitHub App is installed on the repo owner, or you know the
  installation ID. If the hub has the App key, it can auto-discover the
  installation ID.
- A hub login/token that can call the dashboard API.

## Enroll

```bash
export HIVE_DASHBOARD_TOKEN="$(unset GITHUB_TOKEN && gh auth token)"
hivectl --server https://hive.kubestellar.io enroll OWNER/REPO
```

Optional:

```bash
hivectl enroll OWNER/REPO --acmm-level 1
hivectl enroll OWNER/REPO --installation-id 123456
```

The command verifies `gh auth`, checks that the repo is reachable, registers the
repo with the hub, and prints the dashboard URL and next steps.

## What lite mode does

- Records the repo as `lite` in the hub registry.
- Stores a starter config for advisory lanes at ACMM L1-L2.
- Uses the hub's GitHub App installation plus `pkg/mint` scoped tokens.
- Keeps the enrolled repo zero-secret: no PAT, private key, or long-lived token.
- When the hub is running with `lite.execution: true`, periodically evaluates
  lite repos with a read-scoped installation token and stores an advisory report
  on the private hub registry entry. The scan is deterministic and checks ACMM
  L1-L2 readiness signals such as CI, tests, PR/issue templates, coverage
  signals, and agent instruction files.
- Optional per-enrollment `postAdvisoryIssue` can publish the same summary to a
  single Hive-managed issue in the enrolled repo. It is off by default and, when
  enabled, uses an issues-only token and updates the same issue instead of
  creating duplicates.

## What lite mode does not do yet

- No workflow shim is installed in the repository.
- No ACMM L3+ behavior; lite is capped at L2 advisory mode.
- No code-writing agents run for lite repos. Hub-hosted lite execution is
  advisory-only until the repo graduates to a full spoke.

## Graduation path

Use lite mode until advisory findings are useful and low-noise. Graduate to a
full spoke when the repo needs execution, private runtime access, custom
in-cluster dependencies, or ACMM levels above L2.
