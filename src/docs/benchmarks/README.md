# Outcome benchmarks

Monthly reports comparing the outcomes of pull requests Hive mediated with those merged without it
([#14577](https://github.com/hivecommons/hive/issues/14577)). Each `outcomes-YYYY-MM.md` here is generated, never
hand-edited: it is the output of `hivectl evidence benchmark` over a spoke's evidence root
(see [Review evidence bundle](../review-evidence.md#outcome-benchmark) and [hivectl](../hivectl.md)).

## Regenerating a report

On a spoke (or any host holding a copy of its `/data/evidence`), with `hivectl` on `PATH`:

```bash
src/scripts/outcome-benchmark.sh 2026-10                                   # writes src/docs/benchmarks/outcomes-2026-10.md
src/scripts/outcome-benchmark.sh --fleet-spend-usd 412.50 2026-10          # also report cost per merged mediated PR
src/scripts/outcome-benchmark.sh --root ./evidence-copy --out - 2026-10    # another root, Markdown on stdout
```

The script pins the window to the UTC calendar month (`--since YYYY-MM-01T00:00:00Z --until <next month>-01T00:00:00Z`)
and calls `hivectl evidence benchmark`. `HIVECTL` picks the binary, `EVIDENCE_ROOT` the root and `FLEET_SPEND_USD` the
optional spend input.

## Reproducibility

The report is a pure function of the bundles on disk and the flags: no wall clock appears in it, PRs are folded with
the same `evidence.FoldPRs` the summary API and `hivectl evidence export` use, and every aggregate is order
independent. Re-running the script for the same month on the same evidence root produces the same file byte for
byte; a diff between two runs means the evidence changed (for example a revert or an issue reopen was linked after
the first run, or a backfill added history).

## Reading a report

- **Mediated** PRs carry at least one Hive reviewer verdict or a review Hive posted; **unmediated** PRs carry
  neither. The split is observational, not randomised; the `control_window` on/off cut, when bundles carry it, is
  the prospectively assigned arm.
- Rates carry 95% Wilson intervals. Any cell with fewer than 10 PRs reads `not reported` with its n.
- Metrics evidence schema v1 does not capture (post-merge CI re-runs, human `changes_requested` reviews) are listed
  as not available rather than approximated.
- The report's own Methodology section defines every metric.
