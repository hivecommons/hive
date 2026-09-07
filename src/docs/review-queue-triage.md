# Hold-gated review queue triage policy

> **Status: proposed.** This page is the reviewable artifact for
> [#6183](https://github.com/hivecommons/hive/issues/6183) (hold-gated review
> queue has no triage signal). Nothing here changes agent or governor behavior
> until maintainers settle the open decisions below; merging this page adopts
> the triage *convention*, and the mechanism options are listed in
> cheapest-first order.

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

## Mechanism options (maintainer decision, cheapest first)

1. **Presentational sort only.** The queue snapshot
   (`last-actionable.json`, dashboard PR list) orders by class then age.
   Zero label churn; the convention lives in this page and the sort.
2. **`review-priority` label.** The governor applies
   `review-priority: high` to T0 PRs at open time from lane + title class,
   making the ordering visible to plain `gh` searches and third-party
   tooling. One label, applied mechanically, never by the authoring agent.
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
- It does not apply to human-authored PRs (e.g. adopter feature PRs), which
  maintainers sequence by RFC state, not by this table.

## Open decisions for maintainers

1. Which mechanism tier (1, 1+2, or 1+2+3) to adopt.
2. The cap values for option 3, if adopted.
3. Whether T1 refactors should outrank T2 at all, or age-only within a
   merged T1/T2 class.

Evidence trail: queue measurements in
[#6183](https://github.com/hivecommons/hive/issues/6183); lane/label
conventions in [agent-configuration.md](agent-configuration.md); the
hold-gate contract in the ACMM policy matrix
([acmm-policy-matrix.md](acmm-policy-matrix.md)).
