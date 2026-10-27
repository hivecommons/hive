# Outcome benchmark — 2026-10

Generated 2026-10-27 from hive's own spoke (hive-oke) evidence snapshot taken 2026-10-27. Window: merged 2026-10-01 → 2026-11-01; maturation 14 days (observed as of 2026-10-27).

Pull requests that went through Hive's review swarm (**mediated**: at least one recorded verdict or posted review) versus pull requests merged without it (**unmediated**). Every rate carries its n and a 95% Wilson interval; anything with n < 10 is `not reported`.

## Headline

| Metric | Mediated | Unmediated |
|---|---|---|
| Merged PRs in window | 1455 | 2224 |
| Revert rate (matured) | 0.0% [0.0–8.2] (n=43) | 0.1% [0.0–0.4] (n=1513) |
| Change-failure rate (revert or follow-up fix, matured) | 2.3% [0.4–12.1] (n=43) | 1.1% [0.7–1.7] (n=1513) |
| Median time-to-merge | 0.44 h (n=1455) | 0.19 h (n=2224) |
| CI failure at merged head | 6.9% [5.3–8.9] (n=798) | 27.4% [17.9–39.6] (n=62) — captured on only 3% of merges, not comparable |
| Had a `changes_requested` verdict before merge | 33.7% [31.3–36.1] (n=1455) | — (no verdicts by definition) |
| Cost per merged PR | ≈$2.13 est. (n=2869 merged, 2026-08-08 → 2026-10-27) | — |

![mediated vs unmediated](assets/outcomes-2026-10-bars.svg)

### Reading the numbers

- Revert rates are indistinguishable at this n: 0.0% [0.0–8.2] (n=43) mediated vs 0.1% [0.0–0.4] (n=1513) unmediated (overlapping intervals).
- Median time-to-merge is 0.44 h (n=1455) for mediated vs 0.19 h (n=2224) for unmediated: review adds about 15 minutes to the median PR.
- 33.7% [31.3–36.1] (n=1455) of mediated merged PRs were flagged `changes_requested` on an earlier head before merging — findings an unmediated PR would have carried to main unseen.
- CI at merged head is only captured for reviewed heads (mediated 6.9% [5.3–8.9] (n=798)); the unmediated cell is not comparable.
- Estimated agent spend is ≈$2.13 per hive-attributed merged PR (n=2869).

## Trend by month

Whole evidence history, not just the window; months with n < 10 are gaps.

![trend](assets/outcomes-2026-10-trend.svg)

| Month | Mediated merged | Mediated revert | Mediated TTM | Unmediated merged | Unmediated revert | Unmediated TTM |
|---|---|---|---|---|---|---|
| 2026-04 | 0 | not reported (n=0) | not reported (n=0) | 184 | 0.0% [0.0–2.0] (n=184) | 0.01 h (n=184) |
| 2026-05 | 0 | not reported (n=0) | not reported (n=0) | 432 | 0.0% [0.0–0.9] (n=432) | 0.01 h (n=432) |
| 2026-06 | 0 | not reported (n=0) | not reported (n=0) | 943 | 0.1% [0.0–0.6] (n=943) | 0.01 h (n=943) |
| 2026-07 | 0 | not reported (n=0) | not reported (n=0) | 611 | 0.0% [0.0–0.6] (n=611) | 0.09 h (n=611) |
| 2026-08 | 0 | not reported (n=0) | not reported (n=0) | 1881 | 0.1% [0.0–0.4] (n=1881) | 0.31 h (n=1881) |
| 2026-09 | 0 | not reported (n=0) | not reported (n=0) | 3088 | 0.1% [0.1–0.3] (n=3088) | 0.34 h (n=3088) |
| 2026-10 | 1455 | 0.0% [0.0–8.2] (n=43) | 0.44 h (n=1455) | 2224 | 0.1% [0.0–0.4] (n=1513) | 0.19 h (n=2224) |

## Scale

- 13060 evidence bundles (10 unreadable) folding to 11571 pull requests across 10 repositories; 15054 verdicts recorded.
- Merged in window by repository: hivecommons/hive 2451, hivecommons/docs 223, hivecommons/dibs 179, hivecommons/rationguard 172, hivecommons/pluk 155, hivecommons/promptargs 147, hivecommons/hivecommons.github.io 145, hivecommons/hotshot 122, hivecommons/homebrew-hive 78, hivecommons/crux 7.
- Merged in window by author kind — mediated: agent 857, bot 70, human 528; unmediated: agent 1186, bot 103, human 935.
- First mediated merge in the evidence: 2026-10-09.
- Hub `GET /api/hub/evidence` was not consulted (it requires an admin session); numbers are this spoke's evidence only.

## Methodology

- **Source.** Only the per-PR evidence bundles the spoke already writes under `/data/evidence` (schema v1), folded per PR with `pkg/evidence.FoldPRs`. No new telemetry.
- **Cohorts.** A PR is *mediated* if any head carries a Hive verdict or a posted Hive review; otherwise *unmediated*. Backfilled history is unmediated by construction, so the window must start no earlier than the first mediated merge or the comparison is confounded by time.
- **Revert / change-failure.** Denominator is merged PRs observed at least 14 days after merge. A revert is a later PR the evidence linker tagged `reverted_by`; change-failure adds `followed_up_by` fixes.
- **Time-to-merge.** Lower median of merge time minus open time, over merged PRs whose bundle recorded an open time.
- **CI failure at head.** Share of merged heads with a captured `failure` status among heads with a captured success/failure. The unmediated cohort rarely has a captured status (Hive only records CI for heads it reviews), so its cell reports coverage and is not comparable when coverage is low.
- **Changes requested.** Share of mediated merged PRs with at least one `changes_requested` verdict on any head before the merge — findings the author had a chance to address pre-merge. Whether the later head fixed the finding, or a human overrode it, is not verified here.
- **Cost.** `/api/cost` estimated spend divided by its hive-attributed merged-PR count over the same window; model prices are estimates and some models are unpriced, so treat it as an upper-bound order of magnitude.
- **Intervals.** 95% Wilson score; n < 10 is never reported.
- **Regenerate.** `src/scripts/outcome-benchmark.sh --month 2026-10` (see `docs/benchmarks/README.md`).
