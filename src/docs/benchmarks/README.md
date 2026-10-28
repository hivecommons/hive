# Outcome benchmarks

Reproducible comparison of pull requests that went through Hive's review swarm (**mediated**) with pull requests merged without it (**unmediated**), computed from the per-PR evidence bundles a spoke already writes under `/data/evidence`. No new telemetry; every rate carries its n and a 95% Wilson interval, and n < 10 is printed as `not reported`.

## Reports

| Month | Report | Charts |
|---|---|---|
| 2026-10 | [outcomes-2026-10.md](outcomes-2026-10.md) | [bars](assets/outcomes-2026-10-bars.svg) · [trend](assets/outcomes-2026-10-trend.svg) · README cards `assets/outcomes-2026-10-card-*.{svg,png}` |

Each report ships with `outcomes-YYYY-MM.json`, the computed `Report` the markdown, charts, README cards and README block are rendered from, plus the inputs that are not derivable from the evidence tree: `cost-YYYY-MM.json` (saved `GET /api/cost`), `stories-YYYY-MM.json` (curated caught → fixed → merged examples, timestamps taken from the evidence bundles) and `red-team-YYYY-MM.json` (prompt-injection evaluation counts from [ioscan-red-team.md](../ioscan-red-team.md)). `pkg/outcomebench` `TestCommittedReportMatchesReadme` re-renders the newest JSON and fails when the committed markdown, SVGs, cards or the repository README's `<!-- outcomes:start -->…<!-- outcomes:end -->` block differ, so none of them can drift.

## README cards

The repository README shows this data as plain-language cards (one number, one sentence, a footer naming the base and month), with the detailed charts and intervals kept in the monthly report. Rules the generator enforces for the README:

- A production-count card is drawn only when its base is at least `--readme-min-base` (default 200), separate from the n ≥ 10 report threshold. The 43-merge revert bucket, for example, stays in the report.
- The checks-passing card is drawn only when a check result is recorded on at least 80 % of reviewed merges; below that the pass rate would describe a biased subset.
- No statistical vocabulary (intervals, medians, cohorts, n=) appears in the README or on a card; `TestReadmeVocabulary` enforces the list and `TestReadmeProseBudget` keeps README prose under 600 words and 10 sections.
- Every card is drawn twice from the same instructions: a real-text SVG (the vector source) and a 2× PNG in dark and light. The README embeds the PNG (`<picture>` + `prefers-color-scheme`), because GitHub renders SVG text in whatever monospace the reader has and filters differ by browser; the PNG is rasterised in Go with an embedded IBM Plex Mono (SIL OFL 1.1, `pkg/outcomebench/fonts/`), no external renderer. Flat colours only: no gradient, glow or filter. Each PNG carries the SHA-256 of its SVG in a `tEXt` chunk; the drift test checks that hash and the dimensions rather than pixels, which differ slightly by platform.
- The three stat cards (accuracy, security, cost) are the same height, one figure each, so the README reads as a set.
- The prompt-injection card comes from Hive's own evaluation set, says so on the card, and is not subject to the minimum base because it is not a production count.

Each report's "What the README shows and leaves out" section records, per card, whether it was drawn and on what base.

## Regenerate

```sh
# on a spoke, or anywhere with a copy of its evidence tree
src/scripts/outcome-benchmark.sh --root /data/evidence --month 2026-10 \
  --spend cost-2026-10.json  # saved GET /api/cost, for cost per merged PR
  --stories stories-2026-10.json   # curated caught → fixed → merged examples
  --red-team red-team-2026-10.json # prompt-injection evaluation counts
  --hub hub.json             # saved GET /api/hub/evidence (admin session), optional
  --source "hive-oke spoke snapshot 2026-10-27" \
  --readme                   # also rewrite README.md's outcomes block
```

The script wraps `hivectl evidence benchmark`; see [hivectl.md](../hivectl.md#evidence--benchmark) for every flag. After changing the card layout or copy, redraw the committed assets and README block from the newest committed JSON without the evidence tree: `src/scripts/outcome-benchmark.sh --rerender --readme` (`hivectl evidence benchmark --from-json`). To take a read-only copy of a spoke's evidence: `kubectl exec deploy/hive -c hive -- tar czf - -C /data evidence | tar xzf -`.

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
