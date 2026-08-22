# Release Channels

Hive publishes three **release channels** — moving GHCR image tags an operator can point a hive at instead of a branch tag:

| Channel | Intended meaning |
|---|---|
| `stable` | Newest build promoted as generally safe to run. |
| `candidate` | A build believed good, awaiting soak before promotion to stable. |
| `edge` | The newest good build, with no soak period. |

> **Note — current promotion policy:** today all three channels are retagged on **every merge to `v4`**, so `stable`, `candidate`, and `edge` all point at the same digest as `v4-latest`. The distinct meanings above take effect only once a soak/promotion policy lands in CI (see [#3702](https://github.com/kubestellar/hive/pull/3702)). Treat the channel names as forward-looking track selection, not as a guarantee of differing maturity yet.

## How channels are published

Channels are **retags, not rebuilds**. The `docker.yml` workflow adds `stable`, `candidate`, and `edge` as extra tags in the same `docker buildx imagetools create` call that publishes `v4-latest` and the immutable short-SHA tag, so a channel always points at an already-built, multi-arch digest. All three images get all three channels:

- `ghcr.io/kubestellar/hive`
- `ghcr.io/kubestellar/hive-contributor`
- `ghcr.io/kubestellar/hive-hub`

Only builds of branch `v4` publish channels — a feature-branch build can never move a production channel.

## Switching a hive to a channel

From the hub dashboard's **My Hives** list, click the blue version pill on a hive row. The menu lists branches first, then a **Channels** section with the three channels (most stable first). Only the hive's **owner** can switch.

Under the hood this is the same endpoint as a branch switch — the channel name goes in the `branch` field verbatim:

```text
POST /api/saas/hives/{id}/switch-branch
{"branch": "stable"}
```

The hive's image is set to `ghcr.io/kubestellar/hive:stable` (via kubectl for reachable hosted spokes, or delivered on the next heartbeat otherwise). The switch is considered complete when the spoke's heartbeat reports an image ref whose tag matches the channel ([#3761](https://github.com/kubestellar/hive/pull/3761)).

Because a channel tag is a moving (mutable) tag, a channel-tracking hive gets the same floating-tag auto-upgrade treatment as a `-latest` branch tag: upgrades roll the pod but keep the `:stable` image string rather than pinning a SHA ([#3757](https://github.com/kubestellar/hive/pull/3757)).

## The version pill: `stable (v4)`

A channel is a moving pointer, so the dashboard shows what it currently points at. The pill on a channel-tracking hive reads, for example:

- `stable (v4)` — the channel currently resolves to a build of branch `v4`;
- `stable (49e53e6)` — the channel resolves to a digest the hub could not attribute to a tracked branch (short digest shown);
- `stable (?)` — the channel tag could not be resolved on GHCR at all.

Resolution is live: the hub HEADs the GHCR manifests for each tracked branch's `-latest` tag and each channel tag, and matches digests ([#3742](https://github.com/kubestellar/hive/pull/3742)). Results are cached for 5 minutes; an unresolved refresh is never cached, so a transient GHCR blip retries on the next poll rather than latching `unknown` for the TTL ([#3721](https://github.com/kubestellar/hive/pull/3721)). If channel rows render as `unknown`, grep the hub log for `channel resolve:` — each failure path logs a WARN naming the cause (token failure, 401/403 package permission, 404 tag never published).

The **My Hives** page also shows a `Release channels:` block above the per-branch `Latest available images:` rows, mapping each channel to its currently resolved branch/digest. Each branch row also carries a compact per-line image-pulls bar chart (package pulls landing during each of that line's release windows; `—` when the line has no closed window yet), and the header's "Pulls per release" chart follows the **active** line — the branch `stable` currently resolves to — rather than any hard-coded branch.

## Persistence: the tracked channel is durable

The hive's tracked channel is stored hub-side in the per-hive metadata record (`tracked_channel` in `/data/saas/hives/<hive-id>/meta.json`, on the hub PVC). It is set when you switch to a channel and cleared when you switch to a plain branch. Two failure modes are specifically handled:

- **Heartbeats do not erase it.** A channel image is a build of `v4`, so the spoke heartbeats `git_branch=v4`; the pill overlays the persisted channel at read time instead of trusting the reported branch ([#3750](https://github.com/kubestellar/hive/pull/3750)).
- **Hub restarts do not drop an in-flight switch.** The in-memory switch instruction is re-armed from the persisted record on the next heartbeat, as long as the spoke is not mid-upgrade and its reported image tag differs from the tracked channel ([#3771](https://github.com/kubestellar/hive/pull/3771)). This also self-heals a hive whose delivered upgrade drifted it off the channel tag.

## Spoke navbar badge

A spoke running a channel image shows the channel in its own dashboard version badge — `stable (v4)` — with the tooltip `Tracking release channel stable (currently a v4 build)` ([#3762](https://github.com/kubestellar/hive/pull/3762)). The spoke learns its channel by reading its own Deployment's image tag via the in-cluster API; a spoke that cannot read its Deployment (for example a plain docker run) shows only the branch badge.

## Known limitations

- **Bulk actions cannot set a channel.** The bulk *Switch branch* action validates against real branches only and rejects channel names (`unknown branch`); it also never writes the tracked channel. Switching to a channel is per-hive.
- **A manual Upgrade on a channel-tracking hive transiently arms a branch-SHA target.** The upgrade handler targets the tracked *branch*'s latest SHA; the heartbeat re-arm drags the hive back to the channel tag on the next non-upgrading beat. Expect a short window where the pill says `stable (v4)` while an upgrade converges on a SHA.
- **"Behind" / drift comparison keys on the branch, not the channel digest.** A channel-tracking hive's up-to-date math still compares against its underlying branch head.

## Grouping hives by upgrade state

The My Hives **GROUP BY** selector includes an `Upgrade state` dimension ([#3805](https://github.com/kubestellar/hive/pull/3805)) with three buckets: `Queued (ready, not yet upgrading)` (auto-upgrade on and a target armed), `Upgrading`, and `Up to date`. Note that `Queued` requires auto-upgrade to be enabled — a hive that is behind latest with auto-upgrade off sorts under `Up to date`.
