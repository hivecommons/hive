# v4 → v5 forward-port sync policy

> **Status: active.** This page documents the v4→v5 sync practice used by
> [#5845](https://github.com/hivecommons/hive/pull/5845),
> [#6046](https://github.com/hivecommons/hive/pull/6046), and
> [#6053](https://github.com/hivecommons/hive/pull/6053). Tracking issue:
> [#6051](https://github.com/hivecommons/hive/issues/6051).

Hive develops on two lines ([ROADMAP.md](../../ROADMAP.md)): `v4` is the
supported stable default branch, and `v5` is the RFC-gated next generation.
Most day-to-day work targets `v4` ([CONTRIBUTING.md](../../CONTRIBUTING.md)),
so `v5` must be topped up by **forward-port sync PRs**. Historically these
have been ad hoc: #4058, #4087, #4313, #5017 (436 commits), #5472, #5845,
#6046, and #6053. This page makes the trigger, owner, procedure, and review
expectations explicit because the [v5 GA bar](https://github.com/hivecommons/hive/blob/v5/src/docs/v5-ga.md)
requires green `v5` gates over a soak window and a v4→v5 migration guide —
both of which degrade as drift grows.

## Why drift is a GA risk, not housekeeping

- **Sync size grows superlinearly.** The longer the gap, the more v4 changes
  land in files v5 has split or refactored, turning a mechanical merge into
  manual re-application of intent.
- **Review quality collapses with size.** A very large sync PR cannot be
  reviewed line-by-line; without a stated review contract it is merged on
  trust.
- **The GA soak clock resets.** "Required `v5` build/test gates are green for
  the agreed soak window" cannot be satisfied while v5 alternates between
  stale and mid-merge.

## Sync trigger (cadence)

Open a sync PR when **either** condition holds, whichever comes first:

1. A **v4 release tag** is cut (for example `v4.17.1`), or
2. One week has passed since the previous completed sync.

Small, frequent syncs are the point: they keep each PR reviewable and keep
v5's CI signal meaningful. The drift counter should usually return to zero; if
it does not, the remaining commits must be named as intentional v5-only
skips/divergences in the PR body.

## Ownership

- Syncs are owned by the maintainer group. Each sync PR still has a **single
  named driver** — the person or automation account that opens it — responsible
  for getting it green and merged.
- Hive's `ci-maintainer` agent may open or refresh the sync PR, but maintainers
  own review, conflict decisions, approvals, and merge readiness.
- A sync PR that goes red and idle for more than a few days should be treated
  as an incident against the GA bar, not background noise: fix it, or close and
  restart from the current tips.

## Procedure (merge-commit top-up)

The established style is a **merge commit**, not a cherry-pick series:

1. Branch from the current `v5` tip, then merge the current `v4` tip.
2. Resolve conflicts by **preserving v5's layout** and re-applying v4 intent in
   the files' new homes. Where a v4 change has no v5 counterpart because the
   subsystem was removed or deliberately replaced, skip it and say why in the
   PR body.
3. Keep v5's own policy where the release lines intentionally differ: version
   strings, release tags, image channels, branch names, and v5-only docs remain
   v5-shaped even when the v4 change touched adjacent code.
4. Ensure the merge brings v5 up to the **current `origin/v4` tip** unless the
   PR body explicitly lists intentional skips. Do not stop merely because drift
   is below a threshold.
5. Sign off the merge commit (`git commit -s`). The DCO check runs on sync
   merges like any other PR.
6. Run the v5 verification checklist before opening the PR or before marking it
   ready for review.

## Verification checklist

Before merge, a sync PR must have:

- `go test -race ./pkg/... ./cmd/...` passing from `src/` on the v5 branch.
- The kick-governor tests passing. If they are included in the package command
  above, record that; otherwise run their explicit package/test selector and
  record it in the PR body.
- The docs link check passing: `python3 src/scripts/check-docs-links.py`.
- Required GitHub checks green (Playwright/skipped lanes are not blockers when
  they are not required for the sync).
- DCO present on the merge commit.

## Confirming zero delta

After fetching both branches, this command reports how many v4 commits are not
reachable from v5:

```sh
git rev-list --count origin/v5..origin/v4
```

A clean sync reports `0`. A non-zero result is allowed only when every remaining
commit is listed in the PR body as an intentional v5 divergence or skip.

## PR body contract

Every sync PR body must include:

- The **v4 range** covered, including the final v4 tip SHA.
- A **ported-changes list**: each v4 PR in the range, either ported (with a
  note when it landed in a relocated file) or explicitly skipped with reason.
- Any **intentional divergences** introduced during conflict resolution,
  especially v5-only version/tag/image-channel policy.
- The verification commands and results from the checklist above.
- The zero-delta result, or the named/skipped commits that explain a non-zero
  result.

## Review contract

Reviewers of a sync PR are **not** expected to re-review v4 content that
already passed v4 review. They are expected to verify:

1. The ported-changes list is complete against
   `git log --oneline origin/v5..origin/v4` for the stated range.
2. Conflict resolutions in split/refactored files preserve v4 intent while
   keeping v5's layout and release-line policy.
3. Explicitly skipped changes have a stated reason.
4. The verification checklist is green and the DCO sign-off is present.

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
