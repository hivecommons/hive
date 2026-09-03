# Hive Upgrade Guide

This document records operator steps for major-version upgrades. It is a
planning artifact until the target release ships; the deployment manifests and
entrypoint in `src/deploy/` remain the source of truth.

---

## v4 → v5

> **Status**: v5 not yet released. No data-path migration is required by the
> current v4 deployment assets, but v5 work is changing how the container owns
> `/data` and how agents run under per-agent UIDs.

### Before upgrading

1. Read the target release notes and this file.
2. Back up the persistent `/data` volume/PVC with your platform snapshot tool or
   `hive-backup`.
3. If you maintain custom Kubernetes, Docker Compose, or Quadlet manifests,
   compare them with the checked-in assets before rolling the image:
   - Kubernetes: `src/deploy/k8s/deployment.yaml`
   - Docker Compose: `src/docker-compose.yaml`
   - Quadlet: `src/docs/podman-standalone-quadlet.md` and
     `src/docs/podman-quadlet-update-rollback.md`

### `/data` ownership and startup probes

The current entrypoint does **not** require operators to pre-`chown -R /data`.
It keeps the NFS/large-PVC protection by leaving the broad recursive ownership
walk guarded, then repairs the fixed set of root-phase paths non-recursively
(`src/deploy/entrypoint.sh`). Do not add an out-of-band recursive `chown` unless
a release note for the exact target version says to do so.

For Kubernetes, preserve the startup budget and capabilities from
`src/deploy/k8s/deployment.yaml` when carrying local overlays forward:

- `startupProbe` currently allows 150 seconds (`failureThreshold: 30`,
  `periodSeconds: 5`) before liveness restarts are armed.
- The pod must retain the capabilities the entrypoint and per-agent UID switch
  use (`CHOWN`, `SETUID`, `SETGID`, `SETPCAP`, `DAC_OVERRIDE`, `FOWNER`,
  `FSETID`, plus `NET_ADMIN` unless proxy egress is explicitly advisory).

For Docker Compose and Quadlet, keep the `/data` named volume/bind unchanged
across the image change. The Compose healthcheck already gives the container a
120-second `start_period`; the Quadlet rollout/rollback docs describe the same
state-preserving update model.

### Authentication configuration

`HIVE_GITHUB_TOKEN` remains accepted for lower-autonomy/dev deployments, but
GitHub App auth is preferred and required for ACMM levels that need workflow and
org-member permissions. Carry forward the same secret layout used by the deploy
assets:

- `HIVE_GITHUB_APP_ID`
- GitHub App private key at `/secrets/gh-app-key.pem` (referenced from
  `hive.yaml` as `github.key_file`)
- optional `HIVE_GITHUB_TOKEN` only where PAT mode is intentionally retained

### Rollback

Rollback to the previous image should keep the same `/data` volume. Before
rolling back across any future release that announces a `/data` schema migration,
verify the release note for an explicit downgrade step; the current Quadlet docs
mark schema-changing rollback as not exercised.

---

## v3 → v4

No data-path breaking changes. The `/data` PVC layout is unchanged.

**Token configuration**: `HIVE_GITHUB_TOKEN` is still accepted, but GitHub App
auth (`gh-app-key.pem` + `HIVE_GITHUB_APP_ID`) is preferred and required for
ACMM levels L3 and above.

Required GitHub App permissions:

| Permission | Level |
|---|---|
| Contents (read/write) | Required at all ACMM levels |
| Pull requests (read/write) | Required at L2+ |
| Issues (read/write) | Required at L2+ |
| Workflows (read/write) | Required at L3+ |
| Members (read) | Required at L3+ (org-level installs) |

---

## v2 → v3

See `CHANGELOG.md` for the v3.0.0 entry. The `hive.yaml` schema changed in v3:
the `agents[].acmm` field replaced the former `agents[].tier` field.

---

## General upgrade checklist

- [ ] Read the `CHANGELOG.md` entry for the target version.
- [ ] Check this file for any version-specific migration steps.
- [ ] Back up `/data` before upgrading.
- [ ] Compare local Kubernetes/Compose/Quadlet overrides with the checked-in
      deployment assets.
- [ ] Review open issues labelled `kind/breaking` in the target milestone.
- [ ] Test in a staging hive before upgrading production.

---

*This document is maintained by the strategist agent and project maintainers.
Last updated: 2026-09-02. Tracking issue: [#5554](https://github.com/hivecommons/hive/issues/5554).*
