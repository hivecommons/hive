# Migrating a deployment from v4 to v5

This guide is for operators running a Hive deployment built from the `v4` line
who need to move it to `v5`, the supported stable line.

Every claim below is sourced from this repository or from the public tracker
issues linked inline. Where the repository does not document a path — notably a
Helm chart — this guide says so rather than inventing one.

## The short version

v5 is the current stable line. Since the 2026-09-21 cut-over, `stable`,
`candidate`, and `latest` resolve to v5 builds; `edge` resolves to v6; and v4
publishes only `v4-latest` plus immutable short-SHA tags
([#7721](https://github.com/hivecommons/hive/issues/7721),
[release channels](release-channels.md)).

v4 is feature-frozen and receives security and critical fixes only until
**2026-12-21**, its announced EOL date
([#8168](https://github.com/hivecommons/hive/issues/8168),
[ROADMAP.md](../../ROADMAP.md#v4--maintenance-line)). If you are still running
`v4-latest`, a v4 short-SHA tag, or manifests copied from the v4 branch, plan an
operator-owned move to `stable` or `candidate` before that date.

The config-file part is intentionally low-risk: comparing `src/hive.yaml.example`
on `origin/v4` with this branch found **no removed or renamed documented keys**.
The v5 example only adds opt-in/defaulted keys. No breaking v5 config-key change
is recorded in the v5 changelog entries through v5.4.4.

What changes most operators must care about:

| Area | v4 | v5 |
| --- | --- | --- |
| Production channel | `stable` pointed at v4 before 2026-09-21 | `stable` points at v5 and advances by digest after soak |
| Candidate channel | v4 | v5 |
| Edge channel | v5 before the cut-over | v6 active-development builds |
| Global `latest` tag | v4 before the cut-over | v5; prefer `stable` or a digest for production |
| v4 tags | `v4-latest`, short SHA, and release channels before the cut-over | `v4-latest` and short SHA only; no release channel |
| v4 PR intake | normal stable-line intake before freeze | only `security`, `agent/security`, `priority/critical-urgent`, or `v4-freeze-exempt` passes `freeze-gate` |
| `/data` | persistent state volume | same volume; no v5.0.0 `/data` schema migration is recorded |

## Who needs to migrate

Migrate if any of these describe your deployment:

- Your image reference is `ghcr.io/hivecommons/hive:v4-latest`, a v4 short SHA,
  or a local image built from the `v4` branch.
- Your Kubernetes, Docker Compose, or Podman assets were copied from the v4
  branch and still point at v4 tags.
- You were tracking `edge` only to get early v5 builds. `edge` is now v6, so
  switch to `stable` or `candidate` for v5.
- You still send feature PRs to `v4`. Post-freeze v4 PRs need one of the
  freeze-gate labels named above and should be security/critical only; ordinary
  feature/docs/operability work should target `v5`.

If you already track `stable`, your next normal roll moved you to v5 after the
2026-09-21 channel re-base. Still run the verification steps below so you know
which digest is actually serving.

## Timeline

| Date | Event |
| --- | --- |
| 2026-09-21 | Hub Admin emergency cut-over moved `stable`, `candidate`, and `latest` to v5 and `edge` to v6; v4 feature freeze took effect at SHA `c354a5008` ([#7721](https://github.com/hivecommons/hive/issues/7721)). |
| 2026-09-21 | First v5 `stable` promotion used `soak-hours=0`; post-hoc soak evidence and rollback digests are tracked in [#8062](https://github.com/hivecommons/hive/issues/8062). |
| 2026-09-22 | v4 EOL date set to **2026-12-21** ([#8168](https://github.com/hivecommons/hive/issues/8168)). |
| 2026-12-21 | v4 EOL: no further v4 releases or image retags are expected after this date ([ROADMAP.md](../../ROADMAP.md#v4--maintenance-line)). |

## Configuration deltas

The documented v5 config schema is additive over the v4 example. These keys or
comments are new in `src/hive.yaml.example`; none are required to boot v5 with a
v4 config:

- `project.paused_repos` — optional run-state seed for pausing one repository.
- top-level `auto_merge.allow_unprotected_base` and `auto_merge.no_ci_ok` —
  explicit safety exceptions for merge-request handling.
- `github.self_authorization_hold` and `project.repo_policies[].self_authorization_hold`
  — optional controls for the self-authorization hold.
- `agents.<name>.repos` — optional per-agent repository scoping.
- `quality.formal` — optional formal-verification capability for the quality
  lane at ACMM L5/L6.
- `project.writing_guide` now also reaches contributor relay task prompts.

The v5 changelog also records additional opt-in/defaulted runtime knobs, such as
`github.app_signed_commits`, `sandbox.runtime: job`,
`data.session_retention_days`, `HIVE_CONTRIBUTE_SKIP_LABELS` /
`hub.contribute_skip_labels`, and contributor quota/pool settings. These are not
breaking requirements for an existing v4 config.

## Before upgrading

1. Read the target v5 release notes in [CHANGELOG.md](../../CHANGELOG.md) and
   the major-version notes in [UPGRADE.md](../../UPGRADE.md#v4--v5).
2. Back up the persistent `/data` volume/PVC. The checked-in guidance points to
   [backup and restore](backup-restore.md) and says to keep the same `/data`
   volume across upgrade and rollback.
3. Record the current image reference and, if possible, its digest. Moving tags
   are not proof of what is running.
4. If your hive heartbeats to the hosted hub, set `hub.url` or `HIVE_HUB_URL` to
   `https://hive.hivecommons.dev`; do not rely on legacy compiled fallbacks
   ([UPGRADE.md](../../UPGRADE.md#hosted-hub-domain-cutover)).
5. Compare local overlays with the v5 deployment assets you actually use:
   `src/deploy/k8s/deployment.yaml`, `src/docker-compose.yaml`, or the Podman
   Quadlet docs linked from the root [README](../../README.md#quick-start).

Do **not** add an out-of-band recursive `chown -R /data`. The v5 upgrade notes
state that the entrypoint repairs the fixed root-phase paths non-recursively and
that no v5.0.0 `/data` schema migration is recorded.

## Kubernetes deployment

The repository documents raw Kubernetes manifests under `src/deploy/k8s/` and a
root README [Kubernetes deployment](../../README.md#kubernetes-deployment). It
does **not** document a Helm chart in `src/docs`; if you maintain a private Helm
chart, apply the same image, secret, ConfigMap, PVC, probe, and capability
changes to your chart values/templates.

1. Back up the PVC that backs `/data`.
2. Update your image reference to a v5 channel or digest. For production, use
   `ghcr.io/hivecommons/hive:stable`; for a pre-stable canary, use
   `ghcr.io/hivecommons/hive:candidate`; for reproducibility, pin
   `ghcr.io/hivecommons/hive@sha256:<digest>`.
3. Keep the existing `/data` PVC mounted. Do not replace it with an empty PVC.
4. Carry forward the v5 startup probe budget and capabilities from
   `src/deploy/k8s/deployment.yaml`: the upgrade notes name a 150-second
   startup budget and the capabilities `CHOWN`, `SETUID`, `SETGID`, `SETPCAP`,
   `DAC_OVERRIDE`, `FOWNER`, `FSETID`, plus `NET_ADMIN` unless proxy egress is
   explicitly advisory.
5. Keep the same GitHub credential layout or move deliberately to GitHub App
   auth: `HIVE_GITHUB_APP_ID`, private key at `/secrets/gh-app-key.pem`, and an
   optional `HIVE_GITHUB_TOKEN` only where PAT mode is intentional.
6. Apply the updated Deployment/ConfigMap/Secret and wait for rollout:

   ```bash
   kubectl apply -f src/deploy/k8s/deployment.yaml
   kubectl -n hive rollout status deployment/hive
   ```

7. If this is a hosted spoke managed by a hub, prefer the hub's channel switcher
   or `POST /api/saas/hives/{id}/switch-branch` with `{"branch":"stable"}` as
   documented in [release channels](release-channels.md#switching-a-hive-to-a-channel),
   so the hub records the selected channel.

## Docker Compose deployment

The root README documents Docker Compose as the default standalone runtime
([Quick Start: Docker Compose](../../README.md#quick-start-docker-compose)).

1. Back up the Compose volume or bind mount that stores `/data`.
2. Keep your existing `src/.env` and `src/hive.yaml`, then compare them with the
   v5 examples.
3. Change the `hive` service image to `ghcr.io/hivecommons/hive:stable`,
   `ghcr.io/hivecommons/hive:candidate`, or a digest pin. Standalone image
   references are centralized in `src/deploy/standalone-images.sh`; change your
   local override rather than editing unrelated assets.
4. Recreate the service:

   ```bash
   docker compose -f src/docker-compose.yaml pull hive
   docker compose -f src/docker-compose.yaml up -d hive
   ```

5. Leave `/data` mounted unchanged. If you enabled the optional Watchtower
   auto-update profile, remember that a digest pin is inert to Watchtower while
   a moving channel tag can be updated.

## Podman / single-host deployment

The repository's documented single-host Podman path is Quadlet, not a separate
single-binary release ([Quick Start: Podman](../../README.md#quick-start-podman)).
The same image-selection rules apply: use `stable` for production v5,
`candidate` for a v5 canary, or a digest for reproducibility.

1. Back up the volume or host path mounted as `/data`.
2. Update `Image=` in your Quadlet unit or its drop-in to the v5 channel/digest.
   The Podman docs also provide `bin/hive-podman-update.sh` for pin/rollback
   workflows.
3. Reload units and restart through systemd, then run the lifecycle probe from
   the README:

   ```bash
   systemctl --user daemon-reload
   systemctl --user restart hive-gateway.service
   bin/hive-podman-lifecycle-probe.sh check
   ```

For a local source build instead of the published image, the README builds the
same image name the unit uses:

```bash
podman build -t ghcr.io/hivecommons/hive:stable -f src/Dockerfile .
```

## Verification

Run these checks after the rollout.

1. The health endpoint answers from the route you expose:

   ```bash
   curl -sf https://<your-hive-host>/api/health
   ```

   The documented healthy response is `{"status":"ok"}` for standalone hosts;
   Kubernetes liveness/readiness probes also use `/api/health`.

2. Read the version endpoint and confirm the reported branch/channel/image match
   your intent:

   ```bash
   curl -sf https://<your-hive-host>/api/version
   ```

   Look for the version string, `short`, `branch`, optional `channel`,
   `tracking`, and `imageRef` fields. A channel-tracking v5 deployment should
   show `stable` or `candidate`, not `edge` unless you intentionally moved to
   v6.

3. Read `/api/status` and inspect `releaseLineLag`:

   ```bash
   curl -sf https://<your-hive-host>/api/status
   ```

   The dashboard status payload includes `releaseLineLag`; if it cannot be
   measured it must be `unknown`, not silently treated as healthy zero. Treat an
   exceeded threshold or an unknown value as something to investigate before
   declaring a fleet migration complete.

4. In the dashboard, the version pill should identify the v5 channel or image
   you selected. The release-channel docs describe pills such as `stable (v5)`
   and the separate `edge` v6 line.

5. Confirm agents and repository cards still reflect the same configured org,
   repos, and state. A changed setting can take a heartbeat cycle to propagate.

## Rollback

Prefer digest-verifiable rollback over moving-tag rollback.
[release-rollback.md](release-rollback.md) is the operator runbook and covers
hosted spokes, self-managed Kubernetes, Docker Compose, Podman Quadlet, the hub
image, and contributor relays.

Important sourced constraints:

- Verify rollback by digest, never by tag.
- Resolve and record the manifest-list digest for every image you need:
  `hive`, `hive-contributor`, and `hive-hub`.
- Hosted spokes should use the hub's `POST /api/saas/hives/{id}/pin-digest`
  operation when available; it records who pinned what and prevents channel
  re-arm/auto-upgrade from undoing the rollback.
- The emergency v5 `stable` promotion issue records pre-move rollback digests
  for the v4 `stable` state, but comments on [#8062](https://github.com/hivecommons/hive/issues/8062)
  note those digests are held by short-SHA tags and enter the 90-day GHCR prune
  window once moving tags leave them.
- Keep the same `/data` volume across rollback unless a future release note
  names an explicit downgrade step.

For a hosted-spoke pin through the hub:

```text
POST /api/saas/hives/{id}/pin-digest
{"digest": "sha256:<hive-digest>", "reason": "rollback: <why>"}
```

For self-managed Kubernetes, Compose, and Podman, follow the deployment-specific
sections in [release-rollback.md](release-rollback.md#step-3-write-the-pin-by-digest-for-each-image).

## Getting help

- File issues in [hivecommons/hive](https://github.com/hivecommons/hive/issues)
  with your deployment shape, target image ref, `/api/health`, `/api/version`,
  and relevant `/api/status.releaseLineLag` output.
- For channel semantics, start with [release-channels.md](release-channels.md).
- For rollback, start with [release-rollback.md](release-rollback.md).
- For major-version notes, start with [UPGRADE.md](../../UPGRADE.md#v4--v5).

## Limits

- No production v4→v5 upgrade was performed to write this guide. It summarizes
  the repository's checked-in docs, changelog, examples, and the public tracker
  issues named above.
- The repository did not provide Helm-specific installation docs under
  `src/docs`; Helm users must translate the documented Kubernetes manifest
  changes into their own chart.
- The config compatibility statement covers the documented `hive.yaml.example`
  schema and v5 changelog entries, not arbitrary private keys in an operator's
  local config.
