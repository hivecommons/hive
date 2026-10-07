# Stable chases candidate and is always 24 hours behind it (v5 line)

Stable chases candidate and is always 24 hours behind it: at each evaluation,
`stable` is the newest `v5` build whose `docker.yml` run completed at least
`SOAK_HOURS` ago — the build `candidate` pointed at 24 hours ago — regardless of
how many newer candidate builds exist.

## Goals

- Keep `candidate` fast: it should move on every green `v5` release build.
- Keep `stable` predictable: when `v5` is busy and the promotion workflow runs
  hourly, `stable` lags `candidate` by 24 to 25 hours, not by a quiet period.
- Make longer lag explicit: green evidence, `release-blocker`, and smoke-signal
  gates are the only normal reasons `stable` stays farther behind, and the
  workflow names the gate and build holding it.
- Preserve rollback safety with immutable short-SHA tags and digest evidence.

## Promotion rule

Stable chases candidate and is always 24 hours behind it. In this document a
*build* is one successful `docker.yml` run on `v5`; its *generation* is that
run's number and its *digest* is the image manifest list it published for each
image (`hive`, `hive-contributor`, and `hive-hub`). `candidate` is the newest
build pointer. `stable` promotes an already-built digest; it does not rebuild.

At each hourly evaluation, the workflow finds the newest build newer than the
current `stable` generation whose own `docker.yml` completion time is at least
`SOAK_HOURS` old. That is the build `candidate` pointed at 24 hours ago. No quiet
period is ever required: if `v5` keeps merging, newer builds may keep moving
`candidate`, but the build that just crossed the 24-hour line remains the one
considered for `stable`.

The workflow moves `stable` to that build only when these hard gates pass:

1. **Digest integrity:** all three image digests for the chosen generation still
   exist in GHCR and carry matching `org.opencontainers.image.revision` and
   `io.kubestellar.hive.github-actions-run-number` labels.
2. **Green release evidence:** build, lint, unit tests, changelog/release guards,
   and non-flaky required checks are passing or skipped by policy for the chosen
   build's commit.
3. **No open blocker:** no open issue label explicitly marks the candidate
   digest, release tag, or included fix set as a `release-blocker` for the stable
   (v5) line.
4. **Maintained-hive smoke signal:** at least one maintained hive has reported a
   healthy heartbeat with zero crash restarts in the soak window. The preferred
   evidence is a maintained hive on the exact build being promoted. When `v5` is
   busy and the exact build has already been superseded, the workflow may use a
   healthy maintained hive on any later `candidate` build in the same monotonic
   `v5` docker.yml lineage — not only the current one, since on a busy day
   `candidate` moves faster than spokes auto-update (#10042). The hub reports
   each candidate-channel hive's build generation; surviving a later build is
   conservative smoke evidence for an older build in the same line, and it is
   never used to justify a younger build. If the hub is reachable but returns no
   maintained hive summaries, the smoke-signal gate holds the selected build
   until evidence is available.

If any hard gate fails, the run exits successfully without moving `stable` and
prints which gate is holding which build. Those gates are the only normal reasons
`stable` can lag more than 24 to 25 hours on the hourly cadence.

Worked example: `stable` is B1. During the next day, `docker.yml` publishes B2,
B3, … B52 and `candidate` ends at B52. At time T, B52 is only minutes old, but
B37 is the newest build whose own `docker.yml` completion time is at least 24
hours old. The workflow evaluates B37. If B37's digests, evidence, blockers, and
smoke all pass, `stable` moves to B37, even though `candidate` is B52. On each
later hourly run, `stable` moves again whenever another newer build crosses the
24-hour line.

Two promotion runs never execute at the same time: `promote-stable.yml` declares
a GitHub Actions `concurrency` group (`stable-promotion-v5`,
`cancel-in-progress: false`), so a scheduled or manually dispatched run waits
for any in-progress run to finish before it starts. Because repository-wide
`schedule:` delivery is starved, the workflow also triggers on completion of
`Tagged Release` (`workflow_run`, v5 only); a `gate` job skips the run when
another ran within the last 30 minutes. GitHub serialises the
runs; the workflow itself does not need an atomic primitive. As a second,
independent guard, immediately before publishing the run re-reads each image's
current `stable` generation and requires it to be unchanged from the decision
read and lower than the chosen build's generation. That re-read is not atomic
with the tag move and is not relied on to be; it only catches a `stable`
generation that moved between the decision and the publish step (for example a
manual retag). If the re-read fails, the run exits successfully without moving
`stable`; the next scheduled run re-evaluates from the new `stable` generation.

The release captain records the promoted digest, selected build, stable tag, soak
start/end time, and smoke evidence in the workflow summary or promotion PR.

Promotion moves the `stable` tag; see
[What happens to your hive after `stable` moves](release-channels.md#what-happens-to-your-hive-after-stable-moves)
for when an individual hive follows that tag.

## CI enforcement

The existing release build continues to publish immutable short-SHA tags and move
`candidate`; it no longer moves `stable` on every `v5` merge. The
`Promote Stable Channel` workflow runs hourly and can also be manually dispatched.
It:

1. reads the current `stable` generation for `hive`, `hive-contributor`, and
   `hive-hub`;
2. lists successful `docker.yml` runs on `v5` newest-to-oldest;
3. skips builds whose own `docker.yml` completion time has not yet crossed the
   configured soak line, remembering the newest unsoaked build's `eligible_at`;
4. selects the first build that has crossed the line — the newest build
   `candidate` pointed at 24 hours ago;
5. resolves that build's three short-SHA image tags by digest and verifies their
   revision and generation labels match that run;
6. checks required release evidence, open `release-blocker` issues, and
   maintained-hive smoke evidence for that build; and
7. retags all three `stable` images to the chosen build's digests only after the
   stable-generation compare-and-set passes.

The hub dashboard's release-channel block has a play/pause control on the
`stable` row for hub admins. Play is the default: scheduled runs advance `stable`
to the newest build that crossed the 24-hour line, so when `v5` is busy and the
cron is hourly, `stable` stays 24 to 25 hours behind `candidate`. No quiet period
is required. Pause records the admin and timestamp, shows "paused" next to any
behind count, and stops scheduled and manual stable-promotion runs until an admin
resumes.

The public GET endpoint exposes only non-secret channel state, `eligible_at`, an
`eligible_build` object (`sha`, `generation`, `built_at`, and digest when known),
and maintained-hive smoke summaries; the PUT toggle is hub-admin gated and audit
logged. Stable-channel hive rows and spoke dashboards use this same `eligible_at`
calculation for their "Next update" ETA. When there is no eligible or soaking
build ahead of stable, they say that no update is queued instead of hiding the
field.

Hives on the `stable` channel also receive the hub's expected time of the next
promotion as `next_update_at` in the heartbeat upgrade policy
([#10256](https://github.com/hivecommons/hive/issues/10256)). It is omitted
(unknown) while stable is paused, when there is no build newer than `stable`, or
when the channels have not resolved.

The workflow writes the selected build digest, SHA, generation, build completion
time and age, checks consulted, blocker count, smoke evidence, decision, and any
exception note to the GitHub Actions step summary. If no build has crossed the
line, the workflow leaves `stable` unchanged and exits successfully with a reason
such as `no eligible build has completed the 24h soak yet; newest unsoaked build
<sha> generation <N> completed <time> and is eligible_at <time>`. If a hard gate
holds the selected build, the reason names the gate and build, for example
`green evidence gate is holding build <sha> generation <N>` or `release-blocker
gate is holding build <sha> generation <N>`.

## Emergency promotion exception

Manual dispatch may set `emergency-exception-reason` to shorten the 24-hour soak.
The reason must name the risk of waiting, the checks that passed, and the
rollback digest, and `emergency-followup-issue` must name the follow-up issue for
skipped soak or smoke evidence. The exception still requires the candidate to be
current, required workflows to be green, no open `release-blocker`, and the
monotonic generation guard to pass. When an emergency promotion succeeds, the
workflow records the exception in the step summary and comments on the follow-up
issue.

### What the exception actually waives

`normalized_decision` in `src/scripts/promote-stable.sh` checks the emergency
branch *before* the soak-age and smoke-evidence branches, so an exception waives
exactly two promotion requirements:

- **the 24-hour chase line** — the selected build age is never compared
  against `soak-hours`; and
- **the smoke-signal gate** — the `smoke_evidence` test is only
  reached on the non-exception path, so an exception promotes even when both the
  `smoke-evidence` input and `STABLE_SMOKE_EVIDENCE` are empty.

Everything else still holds, and the exception branch fails closed on each:
a missing `emergency-followup-issue`, non-green `v2 CI` / `v2 Tests` evidence, a
non-zero blocker count, missing image metadata, and the monotonic stable
compare-and-set all leave `stable` where it was. A promotion that needs to
bypass one of *those* is not an emergency exception — fix the blocker or roll
back instead.

Those two waived conditions are exactly what the follow-up issue owes after the
fact, which is what makes it a *post-hoc soak*, not a waiver.

### Closing the follow-up issue

The follow-up issue is closed on evidence, not on elapsed time alone. Collect
all four:

1. **Soak, served rather than staged.** 24 hours after the promotion, no open
   issue labelled `release-blocker` names the promoted digest, tag, or fix set:

   ```bash
   gh issue list -R hivecommons/hive --label release-blocker --state open
   ```

   This is the same query the gate runs, so an empty result is the condition
   the skipped soak would have been checking — measured against the digest
   operators actually ran rather than one sitting in `candidate`.

2. **The smoke signal the exception skipped.** At least one maintained hive
   tracking `stable` reports a healthy heartbeat on the promoted digest. On the
   hub's **My Hives** page the version pill reads `stable (v5)`; if it reads
   `stable (<short digest>)` the hub could not attribute the digest to a tracked
   branch, and if it reads `stable (?)` the channel tag did not resolve at all
   (see [release-channels.md](release-channels.md#the-version-pill-stable-v4)).
   Record which hive reported it.

3. **The gate works unassisted.** The next scheduled `promote-stable.yml` run
   promotes a v5 build on the normal path — step-summary decision `promote`
   with reason `all stable chases-candidate promotion gates passed`, no
   exception note:

   ```bash
   gh run list -R hivecommons/hive --workflow promote-stable.yml --event schedule --limit 5
   ```

   A promotion is only proven repeatable once the gate has moved `stable`
   without a dispatch input.

4. **Downstream unblocked.** Whatever the exception was taken to unblock has
   proceeded.

Then add the outcome to the ledger below in the PR that closes the issue, so
the record outlives the Actions step summary.

### Recorded exceptions

The step summary is the primary record while it exists, but Actions logs and
summaries age out, and the follow-up issue is prose. This table is the durable
copy. Add a row in the same PR that closes the follow-up issue.

| Promoted | Reason | Follow-up | Run |
|---|---|---|---|
| 2026-09-21, `e6f4da7` (v5) | [#7721](https://github.com/hivecommons/hive/issues/7721) Phase 1 channel cut-over: once [#8058](https://github.com/hivecommons/hive/pull/8058) stopped the v4 lane publishing `candidate`, a 24h soak would have left `stable` and `candidate` on different release lines | [#8062](https://github.com/hivecommons/hive/issues/8062) | [35632232764](https://github.com/hivecommons/hive/actions/runs/35632232764) |

Digests for that row, as resolved from GHCR:

| Image | `stable` after (v5 `e6f4da7`) | `stable` before |
|---|---|---|
| `hive` | `sha256:c75ba962736c7b27ead64723193fc63c35f928b257488fa717eeed3607d8946a` | `sha256:647197c5cf733fd1f635d3d089bf50a31889773c7f9eb6989fb212f71f657db0` (v4 `896140f`) |
| `hive-contributor` | `sha256:4258219243ef9f55ff51840ee161c6c5ee796dd186773611339641452fcc8f96` | `sha256:c8ab0a7ef4c70fccbbe5b516df3e392f50b9f0102d541e13583cc56754935f96` (v4 `c09a040`) |
| `hive-hub` | `sha256:4d095bc56d8db4bb801c1e49f588635876e8410a215583809810556ab996b4bd` | `sha256:4c1741c154ad870970ebfdb4d75370efff70ede8f54533b5ea769e7393d716b3` (v4 `c09a040`) |

The `ghcr.io/kubestellar/*` mirrors carry the same three `stable` digests, so the
cross-org mirror step of that run landed.

### What the "before" column has to get right

**The workflow does not record it for you.** `promote` in
`src/scripts/promote-stable.sh` reads each image's current `stable` digest into
a loop-local `image_stable_digest`, uses it only to decide whether *any* image
still differs from `candidate`, and then writes a single `- Stable digest:` line
to the step summary — the literal string `mixed-or-not-promoted` whenever the
three do not already all equal `candidate`, which is every run that actually
promotes. So on a successful promotion the summary names the digest being
promoted **to** and never the three being promoted **from**. Resolve the three
`stable` digests from the registry *before* dispatching an emergency promotion
and paste them into the follow-up issue; afterwards the old values are only
recoverable by hunting short-SHA tags, which is how the row above was
reconstructed.

**A rollback target is three digests, and they need not share a commit.** The
row above is the worked example: the `stable` this promotion replaced was not a
single v4 build. `hive` was still on `896140f` while `hive-contributor` and
`hive-hub` had already advanced to `c09a040`, because the gate resolves each
image's `candidate` independently and the three images are separate build jobs
([release-rollback.md](release-rollback.md#what-you-can-roll-back-to)). Record
one digest **per image** — never one short SHA for all three. An operator who
instead took `896140f` from a "rollback = v4 `896140f`" label and resolved it
for all three images, which is the procedure
[release-rollback.md](release-rollback.md#step-1-resolve-the-target-digests)
teaches, would put two of them on a build `stable` never served.

**The rollback target has a shelf life.** Once `stable` moves off a digest, that
digest keeps registry protection only through whatever moving tag it still
carries. For the row above there is none: `stable` left for v5 and `v4-latest`
has advanced past both v4 commits, so all three "before" digests are held only by
their short-SHA tags and enter the 90-day GHCR prune window
([release-channels.md](release-channels.md#how-channels-are-published)). An
emergency exception whose follow-up issue stays open past that window has no
rollback left. Close the follow-up, or pin the target with a durable tag.
