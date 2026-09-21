# Stable soak and promotion policy (v5 line)

This policy is enforced by CI: every successful `v5` image build retags
`candidate`, while a separate scheduled or manually dispatched promotion
workflow advances `stable` by digest only after the gate below passes.

## Goals

- Keep `candidate` fast: it should move on every green `v5` release build.
- Make `stable` deliberate: it should advance only after observable soak, or by a
  documented emergency exception.
- Preserve rollback safety with immutable short-SHA tags and digest evidence.
- Give operators clear expectations for bursty release days.

## Proposed promotion rule

A `v5` build may be promoted from `candidate` to `stable` only when all of these
conditions hold:

1. **Minimum soak:** the candidate digest has been the newest candidate for at
   least 24 hours.
2. **No superseding candidate:** if a newer `candidate` appears before the soak
   window ends, the timer restarts on the newer digest.
3. **Green release evidence:** build, lint, unit tests, changelog/release guards,
   and non-flaky required checks are passing or skipped by policy.
4. **No open blocker:** no open issue or PR label explicitly marks the candidate
   digest, release tag, or included fix set as `blocker`, `regression`, or
   `security` hold for the stable (v5) line.
5. **Operator smoke signal:** at least one maintained hive has reported a healthy
   heartbeat on the candidate digest, or the release captain records why a
   dashboard/heartbeat smoke is not applicable for that digest.

The release captain records the promoted digest, candidate tag, stable tag, soak
start/end time, and smoke evidence in the promotion PR or workflow summary.

## CI enforcement

The existing release build continues to publish immutable short-SHA tags and move
`candidate`; it no longer moves `stable` on every `v5` merge. The
`Promote Stable Channel` workflow runs hourly and can also be manually dispatched.
It:

1. resolves the current `candidate` digest for `hive`, `hive-contributor`, and
   `hive-hub`;
2. compares the candidate digest with the current `stable` digest;
3. reads the candidate's first-seen time from the successful `docker.yml` run
   number recorded in the image metadata;
4. requires successful `v2 CI` and `v2 Tests` workflow evidence for the
   candidate SHA;
5. requires no open **issue** labelled `release-blocker` — `blocker_count` in
   `src/scripts/promote-stable.sh` runs `gh issue list`, which does not return
   pull requests, so condition 4's "issue or PR" is enforced by CI only for the
   issue half. A `release-blocker` label on an open PR alone does not hold the
   gate; open an issue when you need the promotion stopped;
6. requires maintained-hive smoke evidence from the dispatch input or the
   `STABLE_SMOKE_EVIDENCE` repository variable; and
7. retags `stable` to the candidate digest only when the gate passes and the
   moving-tag generation is newer than the currently published `stable` tag.

The workflow writes the candidate digest, SHA, generation, first-seen time, age,
checks consulted, blocker count, smoke evidence, decision, and any exception note
to the GitHub Actions step summary. If the gate fails, the workflow leaves
`stable` unchanged with a human-readable reason such as `candidate age 3600s <
required 86400s (24h)` or `newer candidate superseded this digest before the soak
window completed`.

One hold is expected and benign: the candidate digest is pushed part-way
through its `docker.yml` run, so an hourly promotion that lands in that window
sees a candidate whose publishing run has not completed yet. Since v4.24.4
(`hold_with_reason` in `src/scripts/promote-stable.sh`,
[#6537](https://github.com/hivecommons/hive/issues/6537)) the workflow reports

> `candidate <digest> comes from docker.yml run <N>, which has not completed
> yet; re-evaluate on the next schedule`

and the next hourly schedule re-evaluates once the run finishes — no action is
needed. On builds **before v4.24.4** the same race instead killed the script
with exit 1 and *no output at all* (the empty `workflow_run_created_at` lookup
under `set -e`): a Promote Stable Channel run that failed with no step summary
and nothing in the log is this condition, not a broken gate.

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
exactly two of the five promotion conditions:

- **condition 1, minimum soak** — the candidate age is never compared against
  `soak-hours`; and
- **condition 5, operator smoke signal** — the `smoke_evidence` test is only
  reached on the non-exception path, so an exception promotes even when both the
  `smoke-evidence` input and `STABLE_SMOKE_EVIDENCE` are empty.

Everything else still holds, and the exception branch fails closed on each:
a missing `emergency-followup-issue`, non-green `v2 CI` / `v2 Tests` evidence, a
non-zero blocker count, a superseded candidate, and the monotonic generation
guard all leave `stable` where it was. A promotion that needs to bypass one of
*those* is not an emergency exception — fix the blocker or roll back instead.

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
   promotes a v5 candidate on the normal path — step-summary decision `promote`
   with reason `all stable soak promotion conditions passed`, no exception note:

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
