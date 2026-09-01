# Hive Upgrade Guide

This document describes breaking changes and migration steps for each major version boundary.
It is a **planning artifact** and will be kept current as each release ships.

---

## v4 → v5

> **Status**: v5 not yet released. This section documents known breaking changes
> identified during v5 development so operators can plan ahead.

### UID-isolation entrypoint: chown walks entire /data PVC on first start

**Affected**: all operators upgrading an existing PVC from v4 to v5.

**Symptom**: the startup probe fires before the chown walk completes, the pod is
killed and restarted, and the walk restarts from the beginning — causing a death
loop. The same loop occurs on rollback from v5 back to v4 if the ownership was
already mutated.

**Root cause**: the v5 entrypoint script runs `chown -R` over the entire `/data`
PVC to enforce per-agent UID isolation. On a PVC with significant history this
walk can take longer than the startup probe's `failureThreshold × periodSeconds`.

**Mitigation (until fixed upstream)**:

1. Before upgrading, increase your startup probe tolerance:
   ```yaml
   startupProbe:
     failureThreshold: 60   # was 10
     periodSeconds: 10
   ```
2. Let the pod start once successfully (ownership now matches).
3. Restore original probe values and redeploy.

**Permanent fix (tracked in #5554, #5525)**:
The entrypoint should skip `chown` when the ownership already matches the target
UID, making the operation idempotent and O(1) on subsequent starts.

---

## v3 → v4

No data-path breaking changes. The `/data` PVC layout is unchanged.

**Token configuration**: `HIVE_GITHUB_TOKEN` is still accepted but GitHub App
auth (`gh-app-key.pem` + `HIVE_GITHUB_APP_ID`) is now preferred and required
for ACMM levels L3 and above.

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

- [ ] Read the `CHANGELOG.md` entry for the target version
- [ ] Check this file for any version-specific migration steps
- [ ] Back up `/data` PVC before upgrading (snapshot or `hive-backup`)
- [ ] Review open issues labelled `kind/breaking` in the target milestone
- [ ] Test in a staging hive before upgrading production

---

*This document is maintained by the strategist agent and the project maintainers.
Last updated: 2026-09-01. Tracking issue: [#5554](https://github.com/kubestellar/hive/issues/5554).*
