# Release Channels

A **release channel** is a moving label for a published container image.
You choose a label such as `stable` instead of pinning one build. Moving the
label does not rebuild the image or instantly update your running hive:
it still needs an upgrade or restart. Images are published in GitHub Container
Registry (**GHCR**).

| Channel | Intended meaning |
|---|---|
| `stable` | Newest build promoted as generally safe to run. |
| `candidate` | A v5 build awaiting the observation/waiting period (“soak”) before stable promotion. |
| `edge` | An active-development v6 build, with no soak period; not the newest v5 stable build. |

> **Promotion policy:** the channels diverge by release line and maturity. Every green merge to **`v5`** retags **`candidate`** (and `:latest`); **`stable`** advances later by digest through the scheduled/manual stable-promotion workflow after the [stable soak and promotion policy](stable-soak-policy.md) passes. Merges to **`v6`** retag **`edge`**, so `edge` is an active-development v6 build, not a synonym for `stable`. **`v4`** is a maintenance line: its builds publish only `v4-latest` and short-SHA tags, no channel (#7721 Phase 1).

**Choose `stable` for the conservative starting route.** Then
[switch as the owner](#switching-a-hive-to-a-channel), check the
[version pill](#the-version-pill-stable-v5), and follow the
[update timeline](#what-happens-to-your-hive-after-stable-moves).
Changing channels is **not** a digest-verified rollback; use the
[rollback runbook](release-rollback.md) when you need one specific prior build.

The hub's release-channel block also shows the stable auto-promotion state.
Hub admins see a play/pause control on the `stable` row: play (the default)
lets the hourly promotion workflow catch `stable` up to `candidate` after the
24-hour soak and maintained-hive smoke evidence; pause records who paused and
when, and the workflow skips without moving tags until resumed. Non-admins see
a read-only badge.

## Testing v6 changes

`stable` is promoted from `v5`, so the default `:stable` image in the v6 docs and manifests is a v5 build and does not contain fixes that merged only to `v6`. To test a v6 change, run `edge` instead; the defaults stay on `:stable` ([#11742](https://github.com/hivecommons/hive/issues/11742)).

- **Kubernetes / kind:** from the overlay directory, `kustomize edit set image ghcr.io/hivecommons/hive:edge`, then apply. On kind, `kind load docker-image ghcr.io/hivecommons/hive:edge --name hive` avoids re-pulling into each cluster.
- **Docker Compose:** set `image: ghcr.io/hivecommons/hive:edge` in `src/docker-compose.yaml`.
- **Podman Quadlet:** set `Image=ghcr.io/hivecommons/hive:edge` in `hive.container`.

To confirm which commit an image was built from, read its `org.opencontainers.image.revision` label:

```bash
podman inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' ghcr.io/hivecommons/hive:edge
```

## How channels are published

Channels are **retags, not rebuilds**. Each release line's `docker.yml` workflow adds fast-moving channels as extra tags in the same `docker buildx imagetools create` call that publishes the branch's `-latest` and immutable short-SHA tags, so a channel always points at an already-built, multi-arch digest. Builds of branch `v5` publish `candidate`; the separate stable-promotion workflow later retags `stable` to the digest for the newest build that crossed the 24-hour line after the soak gate passes. Builds of branch `v6` publish `edge`. All three images get their line's channels in both published orgs:

- `ghcr.io/hivecommons/hive` and `ghcr.io/kubestellar/hive`
- `ghcr.io/hivecommons/hive-contributor` and `ghcr.io/kubestellar/hive-contributor`
- `ghcr.io/hivecommons/hive-hub` and `ghcr.io/kubestellar/hive-hub`

The `kubestellar` packages are mirror tags of the same manifest digest as the native `hivecommons` packages during the org transfer, so operators can verify or pin the digest against either registry. Only builds of the release branches (`v5`, `v6`) publish channels — a feature-branch build can never move a production channel.

Publishing is monotonic by workflow run number. Every successful multi-arch build receives its immutable short-SHA tag even if a newer merge has already reached the branch. If that exact short-SHA tag already exists, a re-run leaves it untouched. Moving tags (the branch's `-latest` tag and fast channels such as `candidate`/`edge`) advance only when that build is newer than the generation currently published; an older workflow that runs out of queue order publishes only any missing immutable tag. `stable` uses the same generation guard during digest promotion, so a delayed promotion cannot move it backwards over a newer stable. Registry inspection failures fail the publish or promotion job instead of producing a silent green skip.

Short-SHA tags are retained as a bounded rollback/debug window, not forever. The scheduled GHCR pruning workflow deletes only old package versions whose complete tag set is one or more 7-hex short-SHA tags, after 90 days. Versions still carrying any moving tag (`v4-latest`, `latest`, `stable`, `candidate`, `edge`, or future channel names) are never deleted by that cleanup.

Pinning a hive back to one of those short-SHA builds for all three images, and verifying by digest that the pin landed on the running spoke, is the [digest-verifiable rollback](release-rollback.md) runbook. Switching channels (below) is not a rollback: a channel is a moving tag, and the switch is judged complete when the heartbeat reports a matching *tag*, not a matching digest.

## Switching a hive to a channel

**Who: the hive's owner, for a hub-managed hive.**

1. Open the hub dashboard's **My Hives** list and click the hive's blue version pill.
2. In **Channels**, choose `stable`, `candidate`, or `edge`; branches are listed separately.
3. Wait for rollout and heartbeat confirmation. A request or pending selector is intent, not proof the running image changed.
4. Check the version pill and running short SHA, then use the [timeline below](#what-happens-to-your-hive-after-stable-moves) if it is still waiting.

**Self-hosted operators:** change your runtime's image reference and perform
its pull/recreate or rollout procedure. A Podman Quadlet owner edits `Image=`
and uses the Podman lifecycle; the hosted selector does not manage that unit.

<details>
<summary>API and delivery reference for a hosted channel switch</summary>

Under the hood this is the same endpoint as a branch switch — the channel name goes in the `branch` field verbatim:

```text
POST /api/saas/hives/{id}/switch-branch
{"branch": "stable"}
```

The hive's image is set to `ghcr.io/hivecommons/hive:stable` (via kubectl for reachable hosted spokes, or delivered on the next heartbeat otherwise). The switch is considered complete when the spoke's heartbeat reports an image ref whose tag matches the channel ([#3761](https://github.com/hivecommons/hive/pull/3761)).

Because a channel tag is a moving (mutable) tag, a channel-tracking hive gets the same floating-tag auto-upgrade treatment as a `-latest` branch tag: upgrades roll the pod but keep the `:stable` image string rather than pinning a SHA ([#3757](https://github.com/hivecommons/hive/pull/3757)).

</details>

## The version pill: `stable (v5)`

A channel is a moving pointer, so the dashboard shows what it currently points at. The pill on a channel-tracking hive reads, for example:

- `stable (v5)` — the channel currently resolves to a build of branch `v5`;
- `stable (49e53e6)` — the channel resolves to a digest the hub could not attribute to a tracked branch (short digest shown);
- `stable (?)` — the channel tag could not be resolved on GHCR at all.

Resolution is live: the hub HEADs the GHCR manifests for each tracked branch's `-latest` tag and each channel tag, and matches digests ([#3742](https://github.com/hivecommons/hive/pull/3742)). Results are cached for 5 minutes; an unresolved refresh is never cached, so a transient GHCR blip retries on the next poll rather than latching `unknown` for the TTL ([#3721](https://github.com/hivecommons/hive/pull/3721)). If channel rows render as `unknown`, grep the hub log for `channel resolve:` — each failure path logs a WARN naming the cause (token failure, 401/403 package permission, 404 tag never published).

The **My Hives** page also shows a `Release channels:` block above the per-branch `Latest available images:` rows, mapping each channel to its currently resolved branch/digest. Each branch row also carries a compact per-line image-pulls bar chart (package pulls landing during each of that line's release windows; `—` when the line has no closed window yet), and the header's "Pulls per release" chart follows the **active** line — the branch `stable` currently resolves to — rather than any hard-coded branch.

## Channel distance: how much is queued for promotion

The visible counts compare each channel with the **next channel upstream**:
`stable` versus `candidate`, and `candidate` versus `edge`.
For example, `↓12 vs candidate` means candidate has 12 commits absent from
stable; it does not mean your hive will update in 12 minutes.

| Display | Immediate meaning |
|---|---|
| `↓N` / `↑N` | Commits absent here / commits unique here; both can appear for different branches. |
| `in sync with candidate` | Same commit, or the comparison returned no counts. |
| `↓N 3d 5h vs candidate` | For a pure-behind pair, the promoted build is that much older. |
| No distance | Comparison could not be resolved. It is not a verified zero. |

<details>
<summary>Hub-operator reference: branch divergence, timestamp calculation, cache, and API credentials</summary>

Each row in the `Release channels:` block also shows how far that channel has drifted from the stage **immediately upstream** of it in the promotion order ([#6418](https://github.com/hivecommons/hive/pull/6418)): `stable` is measured against `candidate`, `candidate` against `edge`. Edge has no upstream — it is where builds enter — so its row carries no distance. The upstream stage is derived from the promotion order rather than hardcoded, so a new track wires itself up. Measuring every channel against edge instead would roughly restate the sum of the hops and could not distinguish a starved soak from a stalled promotion; one hop per row keeps each number actionable.

How to read it:

- `↓N vs candidate` (amber) — N commits are on `candidate` that `stable` does not have: the promotion backlog for that hop.
- `↑N` (blue) — N commits are on this channel that its upstream does not have. Both arrows can appear at once: the channels follow different branches, and a branch synced from another both carries commits the other lacks and misses commits merged since the sync. GitHub's compare API reports this as *diverged*, and the UI shows **both** counts rather than collapsing them into a direction that does not exist. The tooltip spells out the full sentence.
- `in sync with candidate` — the two stages resolve to the same commit (or the compare returned no counts).
- `↓N 3d 5h vs candidate` — when a row is a plain ancestor of its upstream (compare status *behind*), the amber duration after the count is how much **older** the promoted build is than the upstream's: the difference between the two rows' commit timestamps. It appears only for that pure-behind case. A *diverged* pair (`candidate` on v5 vs `edge` on v6) shares no single line of history, so a time gap there would compare two unrelated clocks — those rows show commit counts only.
- **No distance shown at all** — the compare could not be resolved. This is deliberate: rendering `0` would read as "level with upstream", the one answer that must never be guessed, since it turns a stalled promotion into a healthy-looking row.

Each row also carries the **commit timestamp** of the build it points at (`<sha> 2026-09-21 16:51`, viewer's local time, minute precision; the tooltip has the UTC RFC3339 value). It is the committer date of that SHA, fetched hub-side (`pkg/hub/channel_commit_date.go`) and cached permanently. A row whose date could not be fetched shows no stamp rather than a placeholder.

Distances are computed hub-side (`pkg/hub/channel_distance.go`) via GitHub's compare API and cached permanently: the distance between two fixed commits is immutable, and a moved channel is a new SHA pair, so entries become unreferenced rather than stale — there is no TTL after which a shown distance could be wrong.

Both reads, like every other hub-originated `api.github.com` call (branch tips, commit messages, workflow runs), use the shared GitHub HTTP transport, so unchanged GitHub responses are revalidated with ETags and served from the hub cache on `304 Not Modified`. Configure **`HIVE_HUB_GITHUB_APP_ID`**, **`HIVE_HUB_GITHUB_INSTALLATION_ID`**, and **`HIVE_HUB_GITHUB_APP_KEY_FILE`** so those reads use the hub GitHub App installation quota. **`HIVE_HUB_GITHUB_TOKEN`** remains only as a legacy fallback; remove it after the App secret is mounted so the hub no longer spends an operator's personal PAT quota. See [`env-vars.md`](env-vars.md).

</details>

## Persistence: the tracked channel is durable

<details>
<summary>Hub implementation reference: persisted channel intent and restart recovery</summary>

The hive's tracked channel is stored hub-side in the per-hive metadata record (`tracked_channel` in `/data/saas/hives/<hive-id>/meta.json`, on the hub PVC). It is set when you switch to a channel and cleared when you switch to a plain branch. Two failure modes are specifically handled:

- **Heartbeats do not erase it.** A channel image still reports its built-from branch (currently `v5` for stable/candidate or `v6` for edge); the pill overlays the persisted channel at read time instead of treating that branch as the tracking choice ([#3750](https://github.com/hivecommons/hive/pull/3750)).
- **Hub restarts do not drop an in-flight switch.** The in-memory switch instruction is re-armed from the persisted record on the next heartbeat, as long as the spoke is not mid-upgrade and its reported image tag differs from the tracked channel ([#3771](https://github.com/hivecommons/hive/pull/3771)). This also self-heals a hive whose delivered upgrade drifted it off the channel tag.

</details>

## Spoke navbar badge

A spoke running a channel image shows both delivery dimensions in its own dashboard version badge: for example, `a1b2c3d · stable (v5) · floating`. The linked short SHA identifies the running build, `stable (v5)` identifies the release channel and built-from branch, and `floating` confirms that the Deployment follows a mutable tag. `candidate` and `edge` are displayed the same way when reported by the backend. A branch tag such as `v5-latest` renders as `a1b2c3d · v5 · floating`, while a SHA tag or digest pin renders as `a1b2c3d · v5 · pinned`.

The tooltip includes the running SHA, channel when present, built-from branch, authoritative Deployment image ref, and tracking mode. The spoke derives both channel and tracking mode server-side from its cached in-cluster Deployment image lookup; browser code does not infer mutability from tag strings. If the Deployment cannot be read (for example, during a plain `docker run`) or its image ref is malformed, the badge says `tracking unknown` and the API does not expose the untrusted ref. This preserves the distinction between unknown provenance and an intentional pin. The channel portion was introduced by [#3762](https://github.com/hivecommons/hive/pull/3762); the tracking detail is specified by [#6321](https://github.com/hivecommons/hive/issues/6321).

On a self-hosted Podman Quadlet spoke there is no Kubernetes Deployment to read. `bin/hive-podman-setup.sh` therefore writes the unit's own image metadata into `hive.env`, which `hive.container` already loads through `EnvironmentFile=`: `HIVE_SELF_IMAGE` carries the `Image=` reference and `HIVE_SELF_IMAGE_TRACKING` is `registry` when Quadlet registry auto-update is enabled for a non-digest image, otherwise `pinned`. The spoke dashboard uses those variables only when the Deployment lookup is unavailable, so Kubernetes spokes keep the Deployment-derived behaviour above. A Quadlet spoke running `Image=ghcr.io/hivecommons/hive:candidate` with registry auto-update shows `candidate · tracking registry` in the header and `Channel: candidate` in the release-status panel; a digest-pinned Quadlet image shows pinned tracking and the digest while leaving the channel unresolved. If `hive.env` lacks `HIVE_SELF_IMAGE` (for example, an install that predates it), the panel shows the built-from branch the version menu shows and says to rerun `bin/hive-podman-setup.sh` or `bin/hive-podman-update.sh` to record the image reference. A hive with no hub configured does not show the "hub has not been reached" staleness warning, because there is no heartbeat to judge.

## Spoke self-service selector

Hosted spoke dashboards that are already following a release-channel tag show a **Follow channel** selector in the release-status panel. Choosing `stable`, `candidate`, or `edge` calls the spoke-local `POST /api/release-channel`, which relays the request to the hub's existing `POST /api/saas/hives/{id}/switch-branch` path using the spoke's dashboard-token proof. The hub still performs the authorization, channel/tag validation, GHCR publishability check, tracked-channel persistence, and kubectl-or-heartbeat delivery.

The spoke UI keeps reported state and intent separate: after a selection it continues to show the channel observed from the Deployment image, plus a pending "switch requested" note, until the rollout/heartbeat lands on the requested tag. Spokes that are self-hosted, pinned, branch-tracking, missing hub credentials, or otherwise unresolved show an honest "selection unavailable" explanation rather than a dead control.

The panel's second row reports the last upgrade attempt (`releaseStatus.attempt` on `GET /api/version`). When the last spoke-side upgrade landed but the hive has since been rolled to a different commit outside the spoke upgrader — a `kubectl rollout restart`, a direct image bump, or a floating-tag pull — the row headlines the running commit as `↷ Running <sha> (rolled externally) · <age>`, where the age is the running build's start time (`runningSince`), not the superseded upgrade's. The superseded attempt moves to a secondary line, `Last spoke-side upgrade to <sha> superseded · <age>`, carrying its own timestamp; it is dimmed once it predates the running build. The two ages are never conflated: a hive restarted an hour ago reads "1h ago" even if its last spoke-side upgrade was a day earlier.

For self-hosted Podman Quadlet spokes the selector is intentionally unavailable even when `HIVE_SELF_IMAGE` resolves a channel, because there is no hub-managed Deployment image to patch. The panel says to change `Image=` in `hive.container`; after editing, reload/restart through the Podman lifecycle so `hive.env` carries the updated `HIVE_SELF_IMAGE` and tracking mode.

## What happens to your hive after `stable` moves

When the hub says that `stable` moved, that means the channel tag now points at
a newer image. Your hive still has to notice that target and roll onto it. The
normal path is:

1. **A build becomes eligible for `stable`.** The stable-promotion workflow runs
   about hourly and promotes the newest `v5` build that passed the
   [24-hour stable soak](stable-soak-policy.md). Moving `stable` is a retag of an
   already-built image, not a rebuild.
2. **The hub refreshes its channel answer.** The hub caches channel resolution
   for up to 5 minutes. After that refresh, `stable` resolves to the new commit
   in the hub's release-channel block and in per-hive upgrade targeting. If the
   row cannot resolve, the hub does not guess; check the hub log for
   `channel resolve:` warnings. For fast-moving branches, the deployable target
   is the newest recent commit whose per-SHA image is already published on GHCR;
   the branch head can still display as `building` until its own image lands.
3. **Your hive's upgrade policy decides when the rollout may start.**
   - **Hub-managed hosted spokes** are controlled by the hub. If upgrades are not
     `paused`, the hub arms the target when the hive is behind and delivers the
     instruction on the spoke's next heartbeat. Spokes heartbeat every 2 minutes.
     Instant policies may still wait for the 5-minute auto-upgrade debounce
     window, which collapses a burst of moving targets into one rollout. Daily
     and weekly policies wait for their configured window (`schedule`,
     `schedule_hour`, `schedule_timezone`, and, for weekly,
     `schedule_weekday`).
   - **Spoke-managed Kubernetes spokes** learn the `upgrade_to` target in their
     heartbeat response and patch their own Deployment. A Deployment that tracks
     `:stable` rolls by changing a pod-template annotation; with
     `imagePullPolicy: Always`, the replacement pod re-pulls the moving tag.
   - **Self-hosted Podman Quadlet spokes** follow the registry only when you opt
     in to Podman's auto-update drop-in. That enables `podman-auto-update.timer`;
     Podman's timer is daily by default.
   - **Self-hosted Kubernetes deployments that are not receiving hub/spoke
     upgrade instructions** do not roll just because a tag moved. The shipped
     manifest uses `imagePullPolicy: Always`, so the next Deployment restart or
     rollout re-pulls `:stable`.
4. **What you see while waiting.** The spoke Version panel shows `Next update`,
   `overdue since`, `none queued`, `paused`, or `unknown` from
   `/api/version.autoUpdate.nextUpdateAt` and `nextUpdateStatus`. The navbar
   version badge changes its short SHA only after the pod actually runs the new
   image. If the hive is stuck, `/api/version` also exposes
   `autoUpdate.state`; a retrying or failed upgrade includes the upgrade marker's
   `attempts` and `lastError`. A dashboard-triggered Upgrade that is accepted
   but produces no rollout or completion signal is marked failed after 15
   minutes instead of showing `Upgrading…` forever. If a daily or weekly
   window opened while the hive was behind but the hub held the upgrade (an
   unresolved channel, a claim still being delivered, a pending earlier
   upgrade, a full upgrade wave, the merge debounce, or a hive that has not
   heartbeated recently), the panel shows a `Held` row with that reason from
   `autoUpdate.heldReason` / `heldAt`, so a missed window is explained instead
   of only showing the previous upgrade. On a `stable` hive the `Next update`
   / `overdue since` line is the **channel's** next promotion estimate (the
   newest `v5` build's 24-hour soak deadline), not your hive's daily or weekly
   window; `overdue since` means a build crossed the soak line and the hub has
   not promoted it yet, and says nothing about whether your hive rolled.
   While a hub-managed spoke drains in-flight agent kicks before restarting
   (default 45 minutes, `hub.upgrade_drain_timeout_s`), the hub timeline
   shows the instruction as still in progress and the hub keeps re-delivering
   it on every heartbeat — including after a hub restart — until the spoke's
   drain deadline plus 20 minutes has passed. Spokes on builds newer than
   [#14322](https://github.com/hivecommons/hive/pull/14322) report the drain
   and are held for exactly the window they report; older spokes drain
   silently and are held for the default window
   ([#14856](https://github.com/hivecommons/hive/issues/14856)). Only after
   that does the hub record a `stale-upgrade-sweep` and retry. The whole
   lifecycle is also exposed as one state (`scheduled` → `draining` → `pulling` → `rolling` →
   `verifying` → `done` | `missed`) on `/api/status.upgradeProgress` and, per
   hive, on the hub's `/api/saas/my-hives` (`upgradeState`), with the drain
   countdown, the agents whose turns are holding it, and a persisted
   `upgradeHistory` of the last 14 attempts and held windows — see
   [operator-reference.md](operator-reference.md#scheduled-upgrade-progress-and-history).
5. **Need it now?** Once `stable` itself points at the desired build,
   **Upgrade now** skips the automatic-upgrade wait for that hive and arms the
   rollout immediately. It does not promote `stable`; it only follows the channel
   value that already exists.

With passing promotion gates and an unpaused instant upgrade policy, a rough
timing budget after a fix merges is: **24 hours of soak + up
to 1 hour for the promotion tick + up to 5 minutes for hub channel resolution +
up to one 2-minute heartbeat + the 5-minute debounce**, plus any paused/daily/
weekly policy window you configured.
Promotion holds, scheduling delays, rollout/drain time, and failures can make
the wait longer. This is not a delivery-time guarantee.

## Known limitations

- **Bulk actions cannot set a channel.** The bulk *Switch branch* action validates against real branches only and rejects channel names (`unknown branch`); it also never writes the tracked channel. Switching to a channel is per-hive.
- **A manual Upgrade on a channel-tracking hive resolves through the channel tag.** If `:stable`/`:candidate`/`:edge` already points at the commit the spoke is running, the hub answers `200 {"status":"current", ...}` — a clean "already up to date" no-op that arms nothing — rather than an error (a `409` here used to surface as "Upgrade failed" on both dashboards and was recorded as a failed self-upgrade on the spoke). Operators who need a newer build must wait for the channel to advance or switch the hive to a newer channel/branch tag.
- **The spoke dashboard offer uses that same target.** `/api/version` keeps reporting the branch tip for provenance, but the `behind` flag, behind count, and Upgrade button are measured against the hub-delivered upgrade policy when present. A `:stable` spoke therefore does not offer "Upgrade available → `<branch tip>`" while `:stable` itself still points at the running commit.

## How channels are published

**Release-publisher and hub-admin reference — not needed to choose a channel.**

The hub's release-channel block also shows stable auto-promotion state.
Only hub admins get its play/pause control. Play (the default) permits
promotion after the normal 24-hour soak and smoke evidence; pause records
who and when, and skips promotion until resumed. Other users see a badge.

Channels are **retags, not rebuilds**. Each release line's `docker.yml`
workflow adds fast-moving channels in the same `docker buildx imagetools create`
call as its branch `-latest` and immutable short-SHA tags. Each tag points
at an already-built multi-architecture digest (an exact image identifier).
v5 builds publish `candidate`; stable promotion later retags the newest
eligible build after its age and other gates pass. v6 builds publish `edge`.
All three images have their line's channels in both published orgs:

- `ghcr.io/hivecommons/hive` and `ghcr.io/kubestellar/hive`
- `ghcr.io/hivecommons/hive-contributor` and `ghcr.io/kubestellar/hive-contributor`
- `ghcr.io/hivecommons/hive-hub` and `ghcr.io/kubestellar/hive-hub`

The `kubestellar` packages mirror the same manifest digests during the org
transfer; either registry can be used to verify or pin. Only release branches
(`v5`, `v6`) publish channels, never feature branches.

Publishing is monotonic by workflow run number. Each successful multi-arch
build gets its immutable short-SHA tag even if a newer merge already landed.
A rerun leaves an existing exact short-SHA tag untouched. Moving tags advance
only for a build newer than the published generation; an out-of-order older
workflow publishes only missing immutable tags. Stable promotion uses the
same generation guard so it cannot overwrite a newer stable with an older
build. Registry-inspection failures fail the job, not silently skip it.

Short-SHA tags are a bounded rollback/debug window. Scheduled GHCR pruning
deletes package versions older than 90 days only when their complete tag set
contains solely 7-hex short-SHA tags. A version with any moving tag
(`v4-latest`, `latest`, `stable`, `candidate`, `edge`, or future channels)
is never deleted by that cleanup.

For rollback, pin all three images and verify running **digests**, using
[digest-verifiable rollback](release-rollback.md). A channel switch confirms
the heartbeat's matching **tag**, not a matching digest.

## Channel-aware upgrade targeting

Automatic upgrade targeting resolves **through the tag the spoke's Deployment tracks**, not around it ([#5994](https://github.com/hivecommons/hive/issues/5994), landed in [#6005](https://github.com/hivecommons/hive/pull/6005), `pkg/hub/channel_targeting.go`).

The stable soak policy made this necessary: per-merge publishes move only `candidate`, while `stable` advances later by digest. A spoke's Deployment tracks one image tag, and rolling the pod re-pulls that tag — nothing the hub instructs can make a restart land on a digest the tag does not carry. A hub that targets branch HEAD is therefore asking a `:stable` spoke to reach a digest its own tag is deliberately withholding; before the fix this looped 41 spokes into permanent `UPGRADE FAILED` (hub instructs a SHA, spoke rolls, re-pulls `:stable`, reports the SHA it started on, hub re-sends the identical instruction).

How targets are now resolved (`reachableUpgradeTarget`, used by the manual upgrade handler, the auto-upgrade sweep, and the heartbeat spoke-managed path):

- **Which channel a spoke is on:** the spoke's *reported image ref* leads, because it is what the kubelet will pull; the hub-side `tracked_channel` record is intent, and the two disagree exactly while a channel switch is still on the wire. `tracked_channel` remains the fallback only for spokes too old to report an image ref. Branch tags and SHA pins resolve to branch targeting, exactly as before.
- **Channel → commit:** the hub walks GHCR from the channel tag to the image index, picks the `linux/amd64` platform manifest (buildx attaches `unknown/unknown` provenance descriptors to the same index, so position is not enough), and reads the `org.opencontainers.image.revision` OCI label from the config blob — the only commit identity that survives a retag. Answers are cached for 5 minutes (`channelDigestTTL`); when a refresh fails, the last good answer is served for up to 4× that (`channelRevisionStaleGrace`, 20 minutes) since a channel moves at most hourly, then resolution is treated as failed.
- **Target = newest published image, not merely newest commit.** After resolving the channel's commit/branch, the hub checks the immutable short-SHA spoke image tags and advertises only the newest commit whose image is already present in GHCR. If the channel or branch pointer advances before `docker.yml` publishes that SHA, the hub walks back to the newest published ancestor; if none is verified, it withholds the upgrade and retries on the next poll. This makes a still-building image read as queued/waiting instead of a hard failed upgrade.
- **Unresolved channel = hold, loudly.** If the channel does not resolve to a commit, the hub instructs *nothing* for that spoke and logs a WARN — falling back to branch HEAD would be exactly the bug. Grep the hub log for `auto-upgrade held — the spoke's release channel did not resolve to a commit` (sweep) or `heartbeat: upgrade instruction withheld — the spoke's release channel did not resolve to a commit` (heartbeat path). The next cycle retries.
- **Downgrade guard.** A channel is a moving pointer, not a monotonic branch, so a spoke can legitimately sit *ahead* of the channel it tracks (rolled while the channel was further along, or switched from `candidate` moments ago). Such spokes are skipped rather than instructed to downgrade (`no upgrade — spoke is at or ahead of its release channel`).
- **"Up to date" is judged through the tag too.** The stale-latch recovery clears a floating-tag hive's upgrade latch once it runs the newest build *its tag can deliver* (`clearing upgrade latch — floating-tag hive is at latest`). Judging a `:stable` spoke against branch HEAD instead kept it latched for the whole soak window — the failure mode measured above.

## Grouping hives by upgrade state

The My Hives **GROUP BY** selector includes an `Upgrade state` dimension ([#3805](https://github.com/hivecommons/hive/pull/3805)) with three buckets: `Queued (ready, not yet upgrading)` (auto-upgrade on and a target armed), `Upgrading`, and `Up to date`. Note that `Queued` requires auto-upgrade to be enabled — a hive that is behind latest with auto-upgrade off sorts under `Up to date`.

## Release notes between two revisions

`GET /api/version/release-notes?from=<sha>&to=<channel-or-sha>` answers "what changes if I upgrade from here to there?" ([#11065](https://github.com/hivecommons/hive/issues/11065)). The dashboard reads `CHANGELOG.md` at both revisions (through the configured GitHub credential, falling back to the public raw endpoint) and returns the `## <date> (vX.Y.Z)` sections that exist at `to` but not at `from`, newest first, grouped Added / Changed / Fixed / Security / Deprecated. The diff comes from repo content, never version arithmetic, so channel and nightly heads work. When `to` is not a release tag, `changelog.d/` fragments that are new at `to` are listed under an `Unreleased (in this build)` section, categorised by filename prefix. If the notes cannot be fetched the endpoint still returns 200 with `source: "unavailable"` and a reason, so an upgrade is never blocked on it. Responses are cached per (from, to) pair for an hour (the cache holds at most 64 pairs, evicting the oldest) and capped at 400 entries (`truncated: true` when cut). Because each uncached build can cost a few hundred GitHub content reads against the hive's own credential, uncached builds are admitted at most 2 at a time and 8 per 10 minutes process-wide; a request past that limit still returns 200 with `source: "unavailable"` and a retry hint, while already-cached pairs keep being served.

When a hub-managed spoke is upgraded without the operator clicking anything, its dashboard shows a dismissible banner at the top of the overview for owners and mergers: "Upgraded <old> → <new>: N releases. What changed ▸" ([#11067](https://github.com/hivecommons/hive/issues/11067)). Expanding it opens these release notes in a read-only modal. It appears once per upgrade per browser and never on a first visit. See [Dashboard-triggered standalone upgrades](dashboard-standalone-upgrades.md#release-notes-before-and-after-an-upgrade).
