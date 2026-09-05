# v4 → v5 forward-port sync policy

> **Status: proposed.** This page documents the sync practice already used by
> [#5845](https://github.com/hivecommons/hive/pull/5845) and
> [#6046](https://github.com/hivecommons/hive/pull/6046) and proposes cadence
> and ownership rules around it. Tracking issue:
> [#6051](https://github.com/hivecommons/hive/issues/6051).

Hive develops on two lines ([ROADMAP.md](../../ROADMAP.md)): `v4` is the
supported stable default branch, and `v5` is the RFC-gated next generation.
Almost all day-to-day work targets `v4`
([CONTRIBUTING.md](../../CONTRIBUTING.md)), so `v5` continuously falls behind
and must be topped up by **forward-port sync PRs**. Historically these have
been ad hoc: #4058, #4087, #4313, #5017 (436 commits), #5472, #5845, #6046
(249 files). This page makes the trigger, owner, procedure, and review
expectations explicit, because the [v5 GA bar](https://github.com/hivecommons/hive/blob/v5/src/docs/v5-ga.md)
requires green `v5` gates over a soak window and a v4→v5 migration guide —
both of which degrade as drift grows.

## Why drift is a GA risk, not housekeeping

- **Sync size grows superlinearly.** The longer the gap, the more v4 changes
  land in files v5 has split or refactored, turning a mechanical merge into
  manual re-application of intent (see the "reapplies v4 intent in the new
  homes" work in #6046).
- **Review quality collapses with size.** A 250-file sync PR cannot be
  reviewed line-by-line; without a stated review contract it is merged on
  trust.
- **The GA soak clock resets.** "Required `v5` build/test gates are green for
  the agreed soak window" cannot be satisfied while v5 alternates between
  stale and mid-merge.

## Sync trigger (cadence)

Open a sync PR when **either** condition holds, whichever comes first:

1. A **v4 minor release** is tagged (for example `v4.16.0`), or
2. `git rev-list --count origin/v5..origin/v4` exceeds **50 commits**.

At current v4 velocity this means roughly weekly syncs. Small, frequent syncs
are the point: they keep each PR reviewable and keep v5's CI signal meaningful.

## Ownership

- Each sync PR has a **single named owner** (the person who opens it) who is
  responsible for driving it to green and merged, including follow-up fixes.
- Maintainers should rotate this duty rather than letting it default to
  whoever notices the drift. The rotation itself is a maintainer agreement,
  recorded on the tracking issue for the sync in question.
- A sync PR that goes red and idle for more than a few days should be treated
  as an incident against the GA bar, not background noise: fix, or close and
  restart from the current tips.

## Procedure (merge-commit top-up)

The established style — used by #5845 and #6046 — is a **merge commit**, not a
cherry-pick series:

1. Branch from `v5`, then `git merge origin/v4`.
2. Resolve conflicts by **preserving v5's layout** and re-applying v4 intent
   in the files' new homes. Where a v4 change has no v5 counterpart (removed
   subsystem), drop it and say so in the PR body.
3. Ensure the merge brings v5 up to the **current `origin/v4` tip**, so the
   drift counter returns to zero, not merely below threshold.
4. Sign off the merge commit (`git commit -s`). The DCO check runs on sync
   merges like any other PR — the red DCO on #6046 is the failure mode this
   line exists to prevent.
5. Run the v5 test suite locally before opening the PR; a sync PR opened red
   transfers the debugging cost to reviewers.

## PR body contract

Every sync PR body must include:

- The **v4 range** covered (`v4.X.Y…v4.X.Z`, plus tip SHA).
- A **ported-changes list**: each v4 PR in the range, either ported (with a
  note when it landed in a relocated file) or explicitly skipped with reason.
- Any **intentional divergences** introduced during conflict resolution.

## Review contract

Reviewers of a sync PR are **not** expected to re-review v4 content that
already passed v4 review. They are expected to verify:

1. The ported-changes list is **complete** against
   `git log --oneline origin/v5..origin/v4` for the stated range.
2. Conflict resolutions in split/refactored files preserve v4 intent.
3. Explicitly skipped changes have a stated reason.
4. v5 CI is green and the DCO sign-off is present.

This narrower contract is what makes a large-but-honest sync mergeable without
pretending it received line-by-line review.

## Relationship to other documents

- [ROADMAP.md](../../ROADMAP.md) — release-line trajectory; the v4→v5
  migration path is part of the v5 GA bar.
- [`src/docs/v5-ga.md` (v5 branch)](https://github.com/hivecommons/hive/blob/v5/src/docs/v5-ga.md)
  — the GA checklist this policy protects; tracker
  [#6016](https://github.com/hivecommons/hive/issues/6016).
- [Release channels](release-channels.md) — `v5` publishes `edge`; a stale or
  red v5 makes `edge` misleading.
- [CONTRIBUTING.md](../../CONTRIBUTING.md) — contributor-facing branch
  guidance (target `v4`); this page is the maintainer-facing counterpart for
  how that work reaches `v5`.
