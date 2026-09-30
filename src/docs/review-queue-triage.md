# Hold-gated review queue triage policy

> **Status: option 1 implemented; option 2 implemented but off by
> default; option 3 open.** This page is the reviewable artifact for
> [#6183](https://github.com/hivecommons/hive/issues/6183) (hold-gated review
> queue has no triage signal) and
> [#9590](https://github.com/hivecommons/hive/issues/9590) (one ranked queue
> across agent and contributor PRs). The presentational sort (mechanism
> option 1 below) ships in `pkg/github/review_priority.go`:
> `last-actionable.json`, the dashboard PR list, and the on-hold tooltip
> order PRs by class then age and expose the class as `review_class`. The
> [PR review queue](#pr-review-queue) (`pkg/github/review_queue.go`) ranks
> every open PR - agent- and contributor-authored alike - and option 2 is
> available behind `review.priority_labels`. Nothing here changes agent or
> governor work selection; option 3 waits on the maintainer decisions below.

At ACMM L5 hold-gated, agent PR throughput permanently exceeds human review
throughput by design: every agent PR carries the `hold` label and waits for a
human. What is not by design is that the resulting queue is **undifferentiated**.
Measured 2026-09-07 ~21:55 UTC:

- **54 open hold-gated PRs** (#6079 … #6221); zero merges to `v4` since the
  automated v4.18.1 release gate at 2026-09-05 09:09 UTC (~61h).
- Lane mix: 27 quality (test coverage), 11 architect (dead-code refactors),
  10 guide (docs), 3 scanner (**runtime bug fixes**), 2 ci-maintainer (CI
  fixes), 1 strategist (planning).
- The 5 fixes — e.g. #6118 (leaked proxy goroutine / atomic stall timeout),
  #6094 (claude local-mode write roots), #6179, #6191, #6109 — sit interleaved
  with coverage and docs PRs of identical apparent urgency.

The cost of review latency is asymmetric across those classes; a FIFO or
random walk through the queue spends scarce review minutes on low-risk
coverage PRs while production-relevant fixes age.

## Triage classes

Reviewers work the queue in class order, oldest-first within a class:

| Class | Contents | Typical lanes | Why this rank |
|-------|----------|---------------|---------------|
| **T0 — fixes** | runtime bug fixes, security fixes, CI/pipeline fixes | scanner, ci-maintainer | latency has production cost; fixes are also the smallest diffs |
| **T1 — behavior-adjacent** | refactors (even dead-code deletion), planning docs that gate other decisions | architect, strategist | wrong-merge cost is real but bounded; refactors go stale fastest as the tree moves |
| **T2 — additive** | test coverage, reference docs | quality, guide | near-zero wrong-merge cost; safe to age |

Classification is derivable from existing metadata — lane label plus the
conventional title prefix (`fix:` / `refactor:` / `docs:` / `test:` /
`planning:`) — so no new agent behavior is required.

Contributor and maintainer PRs rarely carry a lane label, so for a PR with
**no `agent/*` lane label** the same title-prefix and label signals are tried
first, and only when both are silent is the class derived from the changed
paths (`ClassifyContributorReviewClass`): a PR whose every path is a test
file is T2, one whose every path is documentation is T1, and anything else
stays unclassified (ranked with T1). Paths never make a PR a fix. Paths come
from the duplicate sweep's head-SHA cache, so ranking never costs a GitHub
call; a head the sweep has not fingerprinted simply has no path signal. PRs
with a lane label classify exactly as before.

## PR review queue

The queue (#9590) is one ranked list per hive covering every open PR in the
governed repos - the actionable list and the held list - whoever opened it.
The order is computed, never asked of a model, and is total and deterministic
for the same inputs:

1. **Triage class** - T0 fixes, then T1 refactors/docs/unclassified, then T2
   tests.
2. **Confidence band** from the review swarm's 0-5 score
   ([review-swarm.md](review-swarm.md#confidence-score)) for the PR's
   **current head**: *safe* (4-5), then *needs attention* (1-3), then *do not
   merge* (0). A PR with no verdict for its current head - never reviewed, or
   pushed to since - has no score and ranks as *needs attention*, never
   *safe*.
3. **CI state** - green, then pending, then red.
4. **Age** - oldest first; then repo and number as a final tiebreak.

Each entry carries `reasons` in that order (for example `T0 fix (from title
prefix)`, `not reviewed at this head: confidence unknown, ranked as needs
attention`, `CI red: build`, `open 3d`), plus context that does not move the
rank (`contributor PR (not opened by a hive agent)`, `on hold`, `has merge
conflicts`).

Where it shows up:

- **`GET /api/review/queue?limit=N&offset=M`** - the ranked queue with
  reasons, paged (`limit` 1-200, default 50; `total` and `has_more` for
  walking it). Reads only the last enumeration snapshot and the verdict
  artifact; never calls GitHub.
- **`last-actionable.json`** - each PR in `prs.items` and `prs.held` carries
  `review_rank` (its 1-based queue position), `review_priority` and
  `review_rank_reasons`. The lists keep their existing class-then-age order.
- **`review-priority/*` labels** - option 2, off by default; see below.

The queue ranks contributor PRs, but ranking is not reviewing: whether the
review swarm reviews a contributor PR is still governed by
`review.all_authors` ([review-swarm.md](review-swarm.md#who-may-be-pushed-to-all_authors-versus-fix_human_prs)).

## Mechanism options (maintainer decision, cheapest first)

1. **Presentational sort only.** The queue snapshot
   (`last-actionable.json`, dashboard PR list) orders by class then age.
   Zero label churn; the convention lives in this page and the sort.
2. **`review-priority` label.** *Implemented, off by default*
   (`review.priority_labels: true`, or Governor -> Features -> Review Gate ->
   "Label PRs with their review-queue priority"). Each eval cycle the
   governor gives every open PR exactly one of `review-priority/high` (T0),
   `review-priority/normal` (T1 and unclassified) or `review-priority/low`
   (T2), derived from the queue's rank and removing any stale
   `review-priority/*` label, making the ordering visible to plain `gh`
   searches and third-party tooling. Applied mechanically by the hive, never
   by the authoring agent, on contributor PRs too. The labels are never
   created - create them in each governed repo first; a repo without them is
   skipped and logged. At most 20 PRs are relabelled per cycle, top of the
   queue first, and a PR already in step costs no API call.
3. **Soft per-lane queue caps.** A lane stops opening new T2 PRs while more
   than *N* of its PRs are unreviewed (proposed starting point: N=10 for
   quality, N=5 elsewhere). This bounds preflight cost — every agent kick
   must diff intended work against the whole occupied set, so queue length
   is a per-cycle tax on every lane — and converts review starvation into
   reduced production instead of unbounded backlog.

Options compose: 1 is the floor, 2 makes it portable, 3 caps the backlog.
Adopting only option 1 is sufficient to end fix starvation.

## What this policy does *not* do

- It does not let any agent merge, remove `hold`, or reorder anything a
  human sees except by the published sort.
- It does not promise review SLAs. It only says: *when* review minutes are
  spent, spend them T0-first.
- It does not decide merge order for maintainers: human-authored PRs (e.g.
  adopter feature PRs) are classified and ranked like any other so they get a
  triage signal, and maintainers remain free to sequence them by RFC state.
- It does not review, approve, request changes on, push to, merge or close
  any PR. The rank is an ordering and, with option 2 on, a label.

## Open decisions for maintainers

1. Which mechanism tier (1, 1+2, or 1+2+3) to adopt. Option 2 ships
   default-off, so adopting it is a per-hive toggle.
2. The cap values for option 3, if adopted.
3. Whether T1 refactors should outrank T2 at all, or age-only within a
   merged T1/T2 class.

Evidence trail: queue measurements in
[#6183](https://github.com/hivecommons/hive/issues/6183); lane/label
conventions in [agent-configuration.md](agent-configuration.md); the
hold-gate contract in the ACMM policy matrix
([acmm-policy-matrix.md](acmm-policy-matrix.md)).
