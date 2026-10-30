# Outcome benchmarks

Reproducible comparison of pull requests that went through Hive's review swarm (**mediated**) with pull requests merged without it (**unmediated**), computed from the per-PR evidence bundles a spoke already writes under `/data/evidence`. No new telemetry; every rate carries its n and a 95% Wilson interval, and n < 10 is printed as `not reported`.

## Reports

| Month | Report | Charts |
|---|---|---|
| 2026-10 | [outcomes-2026-10.md](outcomes-2026-10.md) | [bars](assets/outcomes-2026-10-bars.svg) · [trend](assets/outcomes-2026-10-trend.svg) · README cards `assets/outcomes-2026-10-card-*.{svg,png}` |

Each report ships with `outcomes-YYYY-MM.json`, the computed `Report` the markdown, charts and legacy cards are rendered from, plus the inputs that are not derivable from the evidence tree: `cost-YYYY-MM.json` (saved `GET /api/cost`), `stories-YYYY-MM.json` (curated caught → fixed → merged examples, timestamps taken from the evidence bundles) and `red-team-YYYY-MM.json` (prompt-injection evaluation counts from [ioscan-red-team.md](../ioscan-red-team.md)). `pkg/outcomebench` `TestCommittedReportAssets` re-renders the newest JSON and checks the committed report, SVGs and cards.

## Root README

The root README is hand-written. Its selected snapshot charts, source links
and renderer live in [docs/readme](../../../docs/readme/README.md).
`TestReadmeSnapshotCharts` checks the counts and readable notes.
`TestReadmeGraphicsManifest` checks source and SVG hashes without requiring
Node.js in the Go test runner. `TestReadmeProseBudget` keeps the approved
copy under 600 words and 10 sections.

Run `node docs/readme/render-charts.mjs` from the repository root to redraw
those graphics. Benchmark regeneration does not replace the README.

## Legacy benchmark cards

The report generator still emits its original cards for existing consumers.
These are not the root README's graphics. Their rules remain:

- A production-count card is drawn only when its base is at least `--readme-min-base` (default 200), separate from the n ≥ 10 report threshold. The 43-merge revert bucket, for example, stays in the report.
- The checks-passing card is drawn only when a check result is recorded on at least 80 % of reviewed merges; below that the pass rate would describe a biased subset.
- No statistical or marketing vocabulary appears on a card; `TestReadmeVocabulary` enforces the list.
- Every legacy card is drawn twice from the same instructions: a real-text SVG and a 2× PNG in dark and light. The PNG is rasterised in Go with an embedded IBM Plex Mono (SIL OFL 1.1, `pkg/outcomebench/fonts/`), with no external renderer. Each PNG carries the SHA-256 of its SVG in a `tEXt` chunk; the drift test checks that hash and the dimensions rather than pixels, which differ slightly by platform.
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
  --source "hive-oke spoke snapshot 2026-10-27"
```

The script wraps `hivectl evidence benchmark`; see [hivectl.md](../hivectl.md#evidence--benchmark) for every flag. Redraw the committed report and legacy cards without the evidence tree using `src/scripts/outcome-benchmark.sh --rerender`. The wrapper rejects `--readme` with instructions for the separate README renderer. The underlying CLI still supports marked legacy documents, but the root README deliberately has no legacy outcomes markers. To take a read-only copy of a spoke's evidence: `kubectl exec deploy/hive -c hive -- tar czf - -C /data evidence | tar xzf -`.

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
