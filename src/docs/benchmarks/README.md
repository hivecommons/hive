# Outcome benchmarks

Reproducible comparison of pull requests that went through Hive's review swarm (**mediated**) with pull requests merged without it (**unmediated**), computed from the per-PR evidence bundles a spoke already writes under `/data/evidence`. No new telemetry; every rate carries its n and a 95% Wilson interval, and n < 10 is printed as `not reported`.

## Reports

| Month | Report | Charts |
|---|---|---|
| 2026-10 | [outcomes-2026-10.md](outcomes-2026-10.md) | [bars](assets/outcomes-2026-10-bars.svg) · [trend](assets/outcomes-2026-10-trend.svg) |

Each report ships with `outcomes-YYYY-MM.json`, the computed `Report` the markdown, charts and README block are rendered from. `pkg/outcomebench` `TestCommittedReportMatchesReadme` re-renders the newest JSON and fails when the committed markdown, SVGs or the repository README's `<!-- outcomes:start -->…<!-- outcomes:end -->` block differ, so none of them can drift.

## Regenerate

```sh
# on a spoke, or anywhere with a copy of its evidence tree
src/scripts/outcome-benchmark.sh --root /data/evidence --month 2026-10 \
  --spend cost.json          # saved GET /api/cost, for cost per merged PR
  --hub hub.json             # saved GET /api/hub/evidence (admin session), optional
  --source "hive-oke spoke snapshot 2026-10-27" \
  --readme                   # also rewrite README.md's outcomes block
```

The script wraps `hivectl evidence benchmark`; see [hivectl.md](../hivectl.md#evidence--benchmark) for every flag. To take a read-only copy of a spoke's evidence: `kubectl exec deploy/hive -c hive -- tar czf - -C /data evidence | tar xzf -`.

## Metrics

| Metric | Definition |
|---|---|
| Revert rate | merged PRs the evidence linker tagged `reverted_by` ÷ merged PRs observed ≥ 14 days after merge |
| Change-failure rate | `reverted_by` or `followed_up_by` ÷ the same matured denominator |
| Median time-to-merge | lower median of merge − open time over merged PRs whose bundle recorded an open time |
| CI failure at merged head | captured `failure` ÷ captured success/failure on merged heads; not comparable when a cohort has a captured status on < 50 % of its merges |
| Changes requested pre-merge | mediated merged PRs with ≥ 1 `changes_requested` verdict on any head before the merge |
| Cost per merged PR | `/api/cost` estimated spend ÷ its hive-attributed merged-PR count, same window |

A PR is mediated when any of its heads carries a Hive verdict or a posted Hive review. Backfilled history is unmediated by construction, so the window must start no earlier than the first mediated merge or the comparison is confounded by time; the report states that date.

## Caveats

- Revert and follow-up links depend on the evidence linker having seen the later PR; a spoke that was down misses them.
- CI status is captured only for heads Hive reviewed, so the unmediated CI cell is a small sample until the sweeper records it for every merged head.
- Cost is an estimate: model prices are approximate and some models are unpriced.
- The hub's `GET /api/hub/evidence` needs an admin session; when it is not supplied the report says so and the numbers are one spoke's.
