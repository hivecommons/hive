# Quality report and README badges

The README badges read a small generated report instead of numbers typed by hand. A scheduled workflow, [`quality-report.yml`](../../.github/workflows/quality-report.yml), builds the report with [`quality-report.py`](../../.github/scripts/quality-report.py) and publishes it to the `quality-badges` branch. Each badge is a shields.io endpoint badge that reads one file from that branch.

## What is published

The `quality-badges` branch holds a single commit that is replaced on every refresh. It contains:

- `report.json`: every entry with its status, value, source link and `as_of` timestamp, plus the top-level `generated_at`.
- `tests.json`, `coverage.json`, `acmm.json`, `security.json`, `scorecard.json`, `outcomes.json`: one file per badge in the shields.io endpoint schema (`schemaVersion`, `label`, `message`, `color`).

## The badges

| Badge | Value comes from | Fresh for |
|---|---|---|
| tests | Latest completed [`v2-tests.yml`](../../.github/workflows/v2-tests.yml) run on `v5` that succeeded or failed: `passing` or `failing`. | 72 hours |
| coverage | Statement coverage computed from the `coverage-profile-*` artifact that [`coverage-hourly.yml`](../../.github/workflows/coverage-hourly.yml) uploads. It is the same ratio `go tool cover -func` reports as the total. | 48 hours |
| ACMM level | `overall_level` in the public evaluation JSON named by the `QUALITY_ACMM_URL` repository variable (the shape of the dashboard `/api/acmm/evaluation` response). With the variable unset the badge shows `unknown`. | 7 days |
| security scan | Latest completed [`go-security-analysis.yml`](../../.github/workflows/go-security-analysis.yml) run on `v5` (`govulncheck`, `gosec`, lint): `clean` or `findings`. | 7 days |
| scorecard | Score the supply-chain scorecard published for the repository after [`scorecard.yml`](../../.github/workflows/scorecard.yml) runs. | 14 days |
| revised, then approved | `Followup.FixedAndApproved` in the newest `src/docs/benchmarks/outcomes-YYYY-MM.json`: the share of pull requests sent back for changes that later had an updated version approved, shown with its counts. Not shown below n = 10. | 62 days |

The freshness windows, colour bands and the n = 10 minimum are named constants at the top of `quality-report.py`. Each can be overridden with a `QUALITY_*` environment variable.

## Unknown and stale

- A value that cannot be computed is `unknown`. Nothing is guessed.
- A value whose source is older than its window is `stale`. The last value stays in `report.json` under `detail`, never on the badge.
- A badge cannot notice that the report file itself stopped refreshing. The report is rebuilt every six hours, after each coverage measurement, and on demand, and every run replaces the branch commit, so the commit date on `quality-badges` shows the last refresh.

## Refresh

- Schedule: every six hours.
- After each [Coverage Hourly](../../.github/workflows/coverage-hourly.yml) run completes.
- On demand: run the `Quality report` workflow from the Actions tab.

The workflow uses only the default token, with `contents: write` on the publishing job alone and read access elsewhere. It is not a required check, and its jobs are marked `continue-on-error`, so a failed refresh never blocks a merge. A change to the workflow or script runs the offline tests and a dry-run generation on the pull request without publishing.
