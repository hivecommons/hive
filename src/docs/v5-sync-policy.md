# v4 → v5 forward-port sync policy

> **Status: historical (superseded 2026-09-21).** `v4` has been feature-frozen
> since 2026-09-21 (policy [#6346](https://github.com/hivecommons/hive/issues/6346);
> see [CONTRIBUTING.md](../../CONTRIBUTING.md#branches)), so bulk forward-port
> sync PRs no longer run: `v5` is the active development branch and v4→v5
> movement is cherry-pick-only for security/critical fixes. The procedure below
> is kept as the record of how the pre-freeze syncs were done.
>
> This page documented the v4→v5 sync practice used by
> [#5845](https://github.com/hivecommons/hive/pull/5845),
> [#6046](https://github.com/hivecommons/hive/pull/6046), and
> [#6053](https://github.com/hivecommons/hive/pull/6053). Tracking issue:
> [#6051](https://github.com/hivecommons/hive/issues/6051).

Before the freeze Hive developed on two lines ([ROADMAP.md](../../ROADMAP.md)):
`v4` was the supported stable default branch, and `v5` the RFC-gated next
generation. Most day-to-day work targeted `v4` ([CONTRIBUTING.md](../../CONTRIBUTING.md)),
so `v5` had to be topped up by **forward-port sync PRs**. Historically these
have been ad hoc: #4058, #4087, #4313, #5017 (436 commits), #5472, #5845,
#6046, and #6053. This page makes the trigger, owner, procedure, and review
expectations explicit because the [v5 GA bar](https://github.com/hivecommons/hive/blob/v5/src/docs/v5-ga.md)
requires green `v5` gates over a soak window and a v4→v5 migration guide —
both of which degrade as drift grows.

The post-freeze lifecycle decision lives in the
[v5 GA bar](v5-ga.md#v4-lifecycle-policy-accepted): accepted on 2026-09-09
([#6346](https://github.com/hivecommons/hive/issues/6346)), v4 freezes when the
Release-train rows are green or on the recorded date, and future v4→v5 movement
becomes cherry-pick-only with per-commit DCO sign-off.

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

## Automated top-up workflows (`v5-topup.yml` / `v6-topup.yml`)

The trigger above is implemented mechanically
([#7297](https://github.com/hivecommons/hive/issues/7297)):
[`.github/workflows/v5-topup.yml`](../../.github/workflows/v5-topup.yml)
opens the v4 → v5 forward-merge PR automatically after every **successful
"Tagged Release" run on `v4`**, with a daily scheduled run (`17 6 * * *` UTC)
as a safety net for drift that arrives without a release tag (dependency
bumps, revert traffic), plus `workflow_dispatch` for manual runs.
`v6-topup.yml` does the same for the v5 → v6 hop. Three points of policy are
encoded in the workflow rather than left to the operator:

- **Conflicts are a human signal, not a retry.** When the merge conflicts,
  the workflow fails instead of opening a PR
  ([#7199](https://github.com/hivecommons/hive/issues/7199)) — a
  forward-merge that resolves itself wrongly is worse than one that waits.
  Nothing is pushed in that case, so re-running after a manual top-up is
  safe.
- **Feature freeze disarms it.** Once the repository variable
  `V4_FEATURE_FREEZE` is set, the `top-up` job is skipped on every trigger —
  release, schedule, and manual dispatch alike — and a `frozen` job records
  why in the run summary. See the
  [v4 freeze runbook](v4-freeze-runbook.md); `v6-topup` is unaffected.
- **Workflow-file deltas need the `TOPUP_PUSH_TOKEN` secret.** The job's
  `GITHUB_TOKEN` can never push commits touching `.github/workflows/**`, so
  a sync range carrying workflow changes (dependabot action-pin bumps on
  `v4` are the common case) requires the repo secret `TOPUP_PUSH_TOKEN` — a
  fine-grained PAT with **Contents: write + Workflows: write**
  ([#7959](https://github.com/hivecommons/hive/issues/7959)). When the
  secret is set, the checkout pushes with it; when it is not, a preflight
  step fails early — before anything is pushed — naming the touched workflow
  files and the manual sync recipe. An operator standing up a fork or a new
  release line must provision this secret or accept that those top-ups need
  a hand-carried sync.

### Top-up PRs land with a merge commit, never a squash or rebase

A squash or rebase keeps v5's *content* on v6 but not v5's *history*, so
`git merge-base origin/v5 origin/v6` stays at the previous top-up and every
later forward-merge replays the whole range against v6's already-adapted copies
of the same files. That is what happened after #18689 was squash-merged: every
`v6-topup.yml` run then hit 109 conflicted files (17 of them `add/add`) that no
resolver pattern covers
([#18931](https://github.com/hivecommons/hive/issues/18931)). Three layers
keep it from recurring:

- **Merge method.** Heads matching `sync/vN-to-vM*` or `topup/*` must be merged
  with the merge-commit method. The hive's own merge paths already upgrade a
  squash or rebase of such a PR to a merge commit
  (`ResolveForwardMergeMethod`). In the native merge queue, a group that does
  not contain a forward-merge PR's own head commit (what a squash or rebase
  queue produces) fails `native-queue-policy` with a message naming the
  violation instead of the generic "not contained" one. A human merging in the
  GitHub UI must choose **Create a merge commit**.
- **Ancestry check.** Every PR the workflow opens ends with a marker line,
  `<!-- hive:topup carried=<sha> -->`, naming the v5 head it carries (refreshed
  on every push to the PR branch). On each run the workflow finds the newest
  merged PR on `sync/v5-to-v6`, checks that the marker is genuine (the PR was
  opened by the workflow identity, and the sha is the second parent of that
  PR's own head commit) and that the sha is an ancestor of `origin/v6`
  (`.github/scripts/v6-topup-ancestry.sh`). If it is not, the previous top-up
  was squashed or rebased and the run says so in a warning and the job
  summary.
- **Self-healing.** When the ancestry was lost and the carried head is still
  v5 history, the run starts the top-up branch with
  `git merge -s ours --no-ff --signoff <carried head>` (history only, no file
  changes, signed off by the bot as the author of that merge commit), then
  merges `origin/v5` as usual with the autoresolver unchanged. If the carried
  head is no longer on v5 (v5 history was rewritten) the job fails fast and
  nothing is pushed. If no verified marker exists (PRs from before #18931 and
  hand-carried top-ups carry none), nothing can be checked and the merge runs
  as before; the conflict report then prints the merge base and the manual
  recipe, whose first step is the ancestry check:

  ```
  git merge-base --is-ancestor <v5 head the last top-up carried> origin/v6 || echo LOST
  git checkout -B sync/v5-to-v6 origin/v6
  git merge -s ours --no-ff --signoff <that v5 head>   # only when LOST
  git merge --no-ff --signoff origin/v5
  ```

#### DCO on top-up PRs

The inherited v5 commits keep the author and sign-off they had on v5. A squash
that GitHub authors as `Andy Anderson <andy@clubanderson.com>` while the
trailer says `Andrew Anderson <andy@clubanderson.com>` (or an image-bump
squash signed by `hive-release[bot]` and the noreply identity) fails the
probot `DCO` check, and nothing pushed to the top-up branch can change a v5
commit. Nobody signs off on another author's behalf, and the gate is not
loosened:

- **Plain automated top-up** (`sync/v5-to-v6`, opened by the workflow, every
  commit v5-reachable except the bot's own merge of v5 onto v6): the narrow
  prow-DCO path of the native queue applies
  ([#17030](https://github.com/hivecommons/hive/issues/17030) decision B, see
  [native-merge-queue.md](native-merge-queue.md)). The bot signs off only its
  own merge commit.
- **Healed top-up** (it carries the ours-merge above) and **hand-carried
  top-ups**: decision B does not cover them. The ours-merge is a commit that is
  not on v5, and B requires every commit but the head to be reachable from v5;
  the policy then falls back to requiring probot `DCO`. The owner of each
  flagged commit remediates it with a `DCO Remediation Commit` under
  `.github/dco.yml` (`allowRemediationCommits.individual: true`,
  `thirdParty: false`), as #18929 did with two remediation commits by the
  author covering 15 inherited commits. Widening B to accept the bot's
  ancestry-only merge commit (its tree equals its first parent's) is a policy
  decision for the owner and is not made here.

The automation covers only the mechanical open. Ownership, the verification
checklist, the PR body contract, and the review contract below apply to an
automatically opened sync PR exactly as they do to a hand-opened one.

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

## Inherited DCO failures from v4

A v4→v5 sync PR can inherit DCO failures from commits that already landed on
`v4`. The current failure mode is Tide squash history: Tide builds the squash
commit from the PR title/body and the PR author's identity, so the resulting
commit can either drop `Signed-off-by:` entirely or keep a trailer for a
different identity. A common example is an author email such as
`andan02@gmail.com` with a retained trailer for `andy@clubanderson.com`; DCO
checks compare the trailer email to the commit author email, not just the human
name.

Do not hand-swap the `dco` / `no-dco` label (or Prow's `dco-signoff: yes|no` pair,
which coexist on Prow-gated repos) as the normal response — the hive rewrites it
from the commits on the next cycle anyway. Instead:

**Merge-method policy (decided by #6312):** `hive` PRs are merged by
**rebase**, not squash. Squash is not used on `v4`/`v5` because Tide rebuilds
the merge commit from PR metadata rather than the branch's own commits,
which is what drops or mismatches the `Signed-off-by:` trailer above. Once
[hivecommons/infra#3](https://github.com/hivecommons/infra/pull/3) lands,
Tide will merge `hivecommons/hive` PRs with `merge_method: rebase`
automatically; until then, maintainers merge by hand with
`gh pr merge --admin --rebase`. Because rebase lands every commit on the PR
branch verbatim (none are squashed into one), **every commit on a PR branch
must carry its own valid `Signed-off-by:` trailer** — not just the PR's final
or merge commit.

1. Identify the offending v4 squash commits with the post-merge checker:

   ```sh
   src/scripts/check-dco-trailers.sh 50 origin/v4
   ```

   Increase the window when the sync range is larger than the default.
2. Fix the source of the bad protected-branch commits. [#6312](https://github.com/hivecommons/hive/issues/6312)
   determined that a squash commit template cannot reliably fix this: Tide only
   has the PR title/body/author *login* available at merge time, not the
   author's DCO-signing email, so it cannot reconstruct a matching trailer.
   The fix is instead to merge `hivecommons/hive` by **rebase**, not squash:
   rebase replays each already-signed branch commit onto the base as-is, so
   every commit keeps its own valid `Signed-off-by:` trailer. That change is
   requested in [hivecommons/infra#3](https://github.com/hivecommons/infra/pull/3),
   which adds a `hivecommons/hive: rebase` override to Tide's
   `merge_method` config (the org-wide default for other hivecommons repos
   stays `squash`). Rebase requires PRs to be free of merge conflicts and
   `needs-rebase`-clean, which Tide's existing query already enforces via
   `missingLabels`. Until that Prow config change is applied and taken over by
   Tide, maintainers merge `hive` PRs by hand with
   `gh pr merge --admin --rebase` so the DCO trailer on every commit survives.
   That Prow/Tide configuration itself is outside this repository.
3. For the sync PR itself, use a real merge commit that preserves the original
   v4 SHAs. Never rewrite another contributor's commits and never add a
   sign-off on someone else's behalf; see
   [#6329](https://github.com/hivecommons/hive/issues/6329). If a maintainer
   performs a DCO override for the sync PR, record it as a PR comment listing
   every affected SHA and the reason the override is acceptable.
   For the v5 to v6 top-up PR (`sync/v5-to-v6`), the PR must carry the
   `tide/merge-method-merge` label (`v6-topup.yml` applies it) so Tide makes a
   merge commit instead of its default squash. A squashed top-up loses v5
   ancestry (single-parent commit on v6) and must be repaired with a
   `git merge -s ours` merge PR merged with `gh pr merge --merge`, because Tide
   cannot merge a PR with nothing to commit ([#19140](https://github.com/hivecommons/hive/issues/19140)).
   After every top-up merges, verify its ancestry with
   `git merge-base --is-ancestor <v5 head> origin/v6`.

Contributors can avoid the author/trailer mismatch class by configuring
`git user.email` to match the email they put in their `Signed-off-by:` trailer
before commits are merged or squashed.

### Recording a disposition for unfixable protected-branch history

Step 3 above forbids the two things that would "fix" a bad commit already on a
protected branch, so for that commit there is no fix — only a recorded
disposition. The post-merge monitor
(`.github/workflows/dco-post-merge.yml`) has two instruments for recording one,
and they differ in blast radius:

| | `DCO_ALLOWLIST_EMAILS` | `DCO_WAIVED_COMMITS` |
| --- | --- | --- |
| Accepts | an **identity** (a sign-off email) | **one historical commit** (full 40-char SHA) |
| Clears `mismatched-signoff` | yes | yes |
| Clears `missing-signoff` | **no** — it is only consulted once a trailer has been found | yes |
| Applies to future commits | **yes**, indefinitely | never — a SHA names history that already exists |
| Example | `raul.mturrubiates@gmail.com`, accepted in [#6554](https://github.com/hivecommons/hive/issues/6554) | `b798382a…`, accepted in [#6576](https://github.com/hivecommons/hive/issues/6576) |

Prefer the SHA waiver. It is the narrower instrument, it matches what step 3
already asks for ("listing every affected SHA and the reason"), and it is the
only one that can clear a commit that carries no trailer at all. Reach for the
email allowlist only when the intent really is to accept an identity's alternate
address going forward.

Before reaching for either, check whether the checker can resolve the commit
itself. A squash whose author is a GitHub noreply address
(`<id>+<login>@users.noreply.github.com`) and whose sign-off is the same
contributor's real address is accepted as `same-person-noreply` when the
branch's history (last `DCO_SAME_PERSON_WINDOW` commits, default 500) carries
local evidence of the link — the same name authored under the sign-off
address before, or both identities appear on one earlier commit. Each
acceptance is printed with the evidence commit; no GitHub API call is made.
`DCO_SAME_PERSON_EVIDENCE_REFS` adds refs (for example a fetched
`refs/pull/N/head`) to that search for a local check. Only a contributor's
first such squash, which has no prior history, needs a waiver — and that
waiver is reported `STALE-WAIVER` once their next commit under that address
lands ([#15401](https://github.com/hivecommons/hive/issues/15401)).

Neither instrument is silent. A waived commit is reported on every run as
`WAIVED <sha> …` above the failures, the summary line counts waivers, and a
green run carrying them says `passed (with recorded waivers)` rather than
`passed` — so "nothing wrong" stays distinguishable from "we are accepting
known-bad history". A waiver whose commit is in the inspected window and no
longer failing is reported as `STALE-WAIVER` so it gets removed instead of
lingering as an unexamined exception; that report is advisory and does not fail
the run.

Every waiver entry must carry, as a YAML comment beside it: the commit subject,
the author, why it cannot be repaired, and the issue where the maintainer
accepted it. Abbreviated SHAs are rejected outright (exit 2) — a prefix is
ambiguous, and a waiver that silently matched nothing would leave the monitor
red while a maintainer believed the disposition had been recorded.

Check a waiver locally before proposing it:

```sh
DCO_WAIVED_COMMITS=<full-sha> src/scripts/check-dco-trailers.sh 50 origin/v4
```

`src/scripts/test-check-dco-trailers.sh` covers the checker itself and runs in
CI on any PR touching it.

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
