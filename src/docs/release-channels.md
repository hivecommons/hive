# Release Channels

Hive publishes three **release channels** — moving GHCR image tags an operator can point a hive at instead of a branch tag:

| Channel | Intended meaning |
|---|---|
| `stable` | Newest build promoted as generally safe to run. |
| `candidate` | A build believed good, awaiting soak before promotion to stable. |
| `edge` | The newest good build, with no soak period. |

> **Promotion policy:** the channels diverge by release line and maturity. Every green merge to **`v4`** retags **`candidate`**; **`stable`** advances later by digest through the scheduled/manual stable-promotion workflow after the [v4 stable soak and promotion policy](stable-soak-policy.md) passes. Merges to **`v5`** retag **`edge`**, so `edge` is an active-development v5 build, not a synonym for `stable`.

## How channels are published

Channels are **retags, not rebuilds**. Each release line's `docker.yml` workflow adds fast-moving channels as extra tags in the same `docker buildx imagetools create` call that publishes the branch's `-latest` and immutable short-SHA tags, so a channel always points at an already-built, multi-arch digest. Builds of branch `v4` publish `candidate`; the separate stable-promotion workflow later retags `stable` by candidate digest after the soak gate passes. Builds of branch `v5` publish `edge`. All three images get their line's channels in both published orgs:

- `ghcr.io/hivecommons/hive` and `ghcr.io/kubestellar/hive`
- `ghcr.io/hivecommons/hive-contributor` and `ghcr.io/kubestellar/hive-contributor`
- `ghcr.io/hivecommons/hive-hub` and `ghcr.io/kubestellar/hive-hub`

The `hivecommons` packages are mirror tags of the same manifest digest as the native `kubestellar` packages during the org transfer, so operators can verify or pin the digest against either registry. Only builds of the release branches (`v4`, `v5`) publish channels — a feature-branch build can never move a production channel.

Publishing is monotonic by workflow run number. Every successful multi-arch build receives its immutable short-SHA tag even if a newer merge has already reached the branch. If that exact short-SHA tag already exists, a re-run leaves it untouched. Moving tags (the branch's `-latest` tag and fast channels such as `candidate`/`edge`) advance only when that build is newer than the generation currently published; an older workflow that runs out of queue order publishes only any missing immutable tag. `stable` uses the same generation guard during digest promotion, so a delayed promotion cannot move it backwards over a newer stable. Registry inspection failures fail the publish or promotion job instead of producing a silent green skip.

Short-SHA tags are retained as a bounded rollback/debug window, not forever. The scheduled GHCR pruning workflow deletes only old package versions whose complete tag set is one or more 7-hex short-SHA tags, after 90 days. Versions still carrying any moving tag (`v4-latest`, `latest`, `stable`, `candidate`, `edge`, or future channel names) are never deleted by that cleanup.

## Switching a hive to a channel

From the hub dashboard's **My Hives** list, click the blue version pill on a hive row. The menu lists branches first, then a **Channels** section with the three channels (most stable first). Only the hive's **owner** can switch.

Under the hood this is the same endpoint as a branch switch — the channel name goes in the `branch` field verbatim:

```text
POST /api/saas/hives/{id}/switch-branch
{"branch": "stable"}
```

The hive's image is set to `ghcr.io/hivecommons/hive:stable` (via kubectl for reachable hosted spokes, or delivered on the next heartbeat otherwise). The switch is considered complete when the spoke's heartbeat reports an image ref whose tag matches the channel ([#3761](https://github.com/hivecommons/hive/pull/3761)).

Because a channel tag is a moving (mutable) tag, a channel-tracking hive gets the same floating-tag auto-upgrade treatment as a `-latest` branch tag: upgrades roll the pod but keep the `:stable` image string rather than pinning a SHA ([#3757](https://github.com/hivecommons/hive/pull/3757)).

## The version pill: `stable (v4)`

A channel is a moving pointer, so the dashboard shows what it currently points at. The pill on a channel-tracking hive reads, for example:

- `stable (v4)` — the channel currently resolves to a build of branch `v4`;
- `stable (49e53e6)` — the channel resolves to a digest the hub could not attribute to a tracked branch (short digest shown);
- `stable (?)` — the channel tag could not be resolved on GHCR at all.

Resolution is live: the hub HEADs the GHCR manifests for each tracked branch's `-latest` tag and each channel tag, and matches digests ([#3742](https://github.com/hivecommons/hive/pull/3742)). Results are cached for 5 minutes; an unresolved refresh is never cached, so a transient GHCR blip retries on the next poll rather than latching `unknown` for the TTL ([#3721](https://github.com/hivecommons/hive/pull/3721)). If channel rows render as `unknown`, grep the hub log for `channel resolve:` — each failure path logs a WARN naming the cause (token failure, 401/403 package permission, 404 tag never published).

The **My Hives** page also shows a `Release channels:` block above the per-branch `Latest available images:` rows, mapping each channel to its currently resolved branch/digest. Each branch row also carries a compact per-line image-pulls bar chart (package pulls landing during each of that line's release windows; `—` when the line has no closed window yet), and the header's "Pulls per release" chart follows the **active** line — the branch `stable` currently resolves to — rather than any hard-coded branch.

## Persistence: the tracked channel is durable

The hive's tracked channel is stored hub-side in the per-hive metadata record (`tracked_channel` in `/data/saas/hives/<hive-id>/meta.json`, on the hub PVC). It is set when you switch to a channel and cleared when you switch to a plain branch. Two failure modes are specifically handled:

- **Heartbeats do not erase it.** A channel image is a build of `v4`, so the spoke heartbeats `git_branch=v4`; the pill overlays the persisted channel at read time instead of trusting the reported branch ([#3750](https://github.com/hivecommons/hive/pull/3750)).
- **Hub restarts do not drop an in-flight switch.** The in-memory switch instruction is re-armed from the persisted record on the next heartbeat, as long as the spoke is not mid-upgrade and its reported image tag differs from the tracked channel ([#3771](https://github.com/hivecommons/hive/pull/3771)). This also self-heals a hive whose delivered upgrade drifted it off the channel tag.

## Spoke navbar badge

A spoke running a channel image shows the channel in its own dashboard version badge — `stable (v4)` — with the tooltip `Tracking release channel stable (currently a v4 build)` ([#3762](https://github.com/hivecommons/hive/pull/3762)). The spoke learns its channel by reading its own Deployment's image tag via the in-cluster API; a spoke that cannot read its Deployment (for example a plain docker run) shows only the branch badge.

## Known limitations

- **Bulk actions cannot set a channel.** The bulk *Switch branch* action validates against real branches only and rejects channel names (`unknown branch`); it also never writes the tracked channel. Switching to a channel is per-hive.
- **A manual Upgrade on a channel-tracking hive transiently arms a branch-SHA target.** The upgrade handler targets the tracked *branch*'s latest SHA; the heartbeat re-arm drags the hive back to the channel tag on the next non-upgrading beat. Expect a short window where the pill says `stable (v4)` while an upgrade converges on a SHA.
- **"Behind" / drift comparison keys on the branch, not the channel digest.** A channel-tracking hive's up-to-date math still compares against its underlying branch head.

## Grouping hives by upgrade state

The My Hives **GROUP BY** selector includes an `Upgrade state` dimension ([#3805](https://github.com/hivecommons/hive/pull/3805)) with three buckets: `Queued (ready, not yet upgrading)` (auto-upgrade on and a target armed), `Upgrading`, and `Up to date`. Note that `Queued` requires auto-upgrade to be enabled — a hive that is behind latest with auto-upgrade off sorts under `Up to date`.
